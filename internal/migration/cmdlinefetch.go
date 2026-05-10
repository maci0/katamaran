package migration

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const maxPodLogLineSize = 8 * 1024 * 1024

// maxPodLogScanBytes bounds how much log data we will fetch/scan looking for
// migration markers. The markers are emitted near source-job startup and are
// capped individually by maxMarkerB64Size, so scanning beyond this is wasted
// I/O on chatty pods.
const maxPodLogScanBytes = 16 * 1024 * 1024

// maxMarkerB64Size caps the base64 payload accepted from a single marker
// line before we attempt to allocate the decoded buffer. 6 MiB of base64
// decodes to ~4.5 MiB — a comfortable headroom over real cmdline /
// VMConfig payloads while preventing a malicious or runaway pod log from
// driving an unbounded allocation.
const maxMarkerB64Size = 6 * 1024 * 1024

// podRefDNSRe matches a single DNS-1123 label (used for both the
// namespace and the pod name in <namespace>/<pod> references).
// Defense-in-depth: the values are URL-escaped before going on the wire,
// but rejecting non-conforming inputs early avoids spurious apiserver
// round-trips on bogus references.
var podRefDNSRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

const (
	cmdlineMarker     = "KATAMARAN_CMDLINE_B64="
	vmConfigMarker    = "KATAMARAN_VMCONFIG_B64="
	agentConfigMarker = "KATAMARAN_AGENTCONFIG_B64="
)

// podLogClient bundles the apiserver pod-log fetch parameters: the
// authenticated HTTP client, the resolved log URL, the bearer token,
// and the parsed <namespace, pod> tuple.
type podLogClient struct {
	client   *http.Client
	endpoint string
	token    string
	ns       string
	pod      string
}

// newPodLogClient builds an authenticated HTTP client and the apiserver
// pod-log URL for the given <ns>/<pod> ref. Centralises the SA token + CA
// bundle plumbing shared by every apiserver-driven pod-log fetch.
func newPodLogClient(ref string) (*podLogClient, error) {
	ns, pod, err := parsePodRef(ref)
	if err != nil {
		return nil, err
	}
	host, port, err := resolveAPIServerHostPort()
	if err != nil {
		return nil, err
	}
	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("read service account token: %w", err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	caBytes, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read service account CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBytes) {
		return nil, fmt.Errorf("CA file %s did not contain any PEM certificates", caPath)
	}
	hc := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				MinVersion: tls.VersionTLS12,
			},
		},
		// Refuse to follow redirects: the apiserver pod-log endpoint does
		// not redirect on the happy path, and a redirect away from the
		// in-cluster apiserver could divert the bearer token to an
		// untrusted host.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	q := url.Values{}
	q.Set("container", "katamaran")
	q.Set("limitBytes", fmt.Sprint(maxPodLogScanBytes))
	endpoint := fmt.Sprintf("https://%s/api/v1/namespaces/%s/pods/%s/log?%s",
		net.JoinHostPort(host, port), url.PathEscape(ns), url.PathEscape(pod), q.Encode())
	return &podLogClient{client: hc, endpoint: endpoint, token: token, ns: ns, pod: pod}, nil
}

// fetchCmdlineFromPodLog retrieves the source QEMU cmdline that the
// source binary stamped on its own pod log as a
// `KATAMARAN_CMDLINE_B64=<base64>` line. ref is `<namespace>/<podname>`.
// Returns the path of a local file containing the decoded bytes — the
// caller (dest's spawnReplayedQEMU) treats it the same as the legacy
// --replay-cmdline path.
//
// Why pod-log instead of a stager pod with hostPath:
//
//   - Eliminates the SPDY exec dance (read source pod -> create
//     stager on dest node -> write file -> delete stager). That flow
//     ran in a goroutine inside katamaran-mgr; if mgr restarted
//     mid-flight the goroutine died and the dest job had no cmdline
//     to replay.
//   - Reduces RBAC: the dest job's SA needs only `pods/log get`. No
//     pods/exec, no Pod create/delete on the controller.
//   - Source pod log lifetime is bounded by the source Job's TTL
//     (5 min default), which always outlives a single migration.
//
// The fetch retries up to a 5-minute deadline because the source pod
// may not have started by the time the dest is up.
func fetchCmdlineFromPodLog(ctx context.Context, ref string) (string, error) {
	pc, err := newPodLogClient(ref)
	if err != nil {
		return "", err
	}
	defer pc.client.CloseIdleConnections()

	deadline, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	slog.Info("Fetching source QEMU cmdline from pod log", "endpoint", pc.endpoint, "marker", cmdlineMarker)
	timeoutErr := func() error {
		return fmt.Errorf("source pod %s/%s did not emit %s within timeout", pc.ns, pc.pod, cmdlineMarker)
	}
	for attempt := 1; ; attempt++ {
		select {
		case <-deadline.Done():
			return "", timeoutErr()
		default:
		}
		markers, bytesScanned, err := scanPodLogMarkers(deadline, pc.client, pc.endpoint, pc.token, cmdlineMarker)
		if err != nil {
			logPodLogFetchRetry("pod-log fetch attempt failed", attempt, "error", err)
		} else if b64 := markers[cmdlineMarker]; b64 != "" {
			if len(b64) > maxMarkerB64Size {
				return "", fmt.Errorf("KATAMARAN_CMDLINE_B64 marker too large: %d bytes (max %d)", len(b64), maxMarkerB64Size)
			}
			decoded, derr := base64.StdEncoding.DecodeString(b64)
			if derr != nil {
				return "", fmt.Errorf("decode KATAMARAN_CMDLINE_B64: %w", derr)
			}
			slog.Info("Decoded source QEMU cmdline from pod log", "attempt", attempt, "bytes", len(decoded))
			return writeCmdlineTempFile(decoded)
		} else {
			logPodLogMarkerMissing(attempt, bytesScanned)
		}
		// Retry every 2s. Either the pod isn't up yet (404), the
		// cmdline marker hasn't been emitted (apiserver returns the
		// log so far), or a transient apiserver error.
		select {
		case <-deadline.Done():
			return "", timeoutErr()
		case <-time.After(2 * time.Second):
		}
	}
}

func logPodLogFetchRetry(message string, attempt int, attrs ...any) {
	attrs = append([]any{"attempt", attempt}, attrs...)
	if attempt == 1 || attempt%15 == 0 {
		slog.Warn(message, attrs...)
		return
	}
	slog.Debug(message, attrs...)
}

func logPodLogMarkerMissing(attempt int, bytesScanned int64) {
	attrs := []any{"attempt", attempt, "bytes_scanned", bytesScanned}
	if attempt%15 == 0 {
		slog.Warn("pod-log fetch returned no marker yet", attrs...)
		return
	}
	slog.Debug("pod-log fetch returned no marker yet", attrs...)
}

// fetchVMConfigFromPodLog retrieves the VMConfig emitted by the source
// binary as KATAMARAN_VMCONFIG_B64 and KATAMARAN_AGENTCONFIG_B64 markers.
// Returns nil slices if not found (best-effort, non-fatal).
func fetchVMConfigFromPodLog(ctx context.Context, ref string) (vmConfig, agentConfig []byte) {
	pc, err := newPodLogClient(ref)
	if err != nil {
		slog.Warn("Cannot build pod-log client for VMConfig fetch", "ref", ref, "error", err)
		return nil, nil
	}
	defer pc.client.CloseIdleConnections()

	markers, _, err := scanPodLogMarkers(ctx, pc.client, pc.endpoint, pc.token, vmConfigMarker, agentConfigMarker)
	if err != nil {
		slog.Warn("Failed to fetch source pod log for VMConfig", "error", err)
		if len(markers) == 0 {
			return nil, nil
		}
	}

	decode := func(name, b64 string) []byte {
		if b64 == "" {
			return nil
		}
		if len(b64) > maxMarkerB64Size {
			slog.Warn(name+" marker exceeds max size; ignoring", "size", len(b64), "max", maxMarkerB64Size)
			return nil
		}
		decoded, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			slog.Warn("Failed to decode "+name+" marker from pod log", "ref", ref, "error", err)
			return nil
		}
		return decoded
	}
	return decode("VMConfig", markers[vmConfigMarker]), decode("AgentConfig", markers[agentConfigMarker])
}

// parsePodRef splits "<ns>/<name>" into its components and rejects
// values whose namespace or name does not look like a DNS-1123 label.
// The DNS check is defense-in-depth: callers ultimately pass the pieces
// to url.PathEscape before the request hits the apiserver, but a strict
// up-front check keeps clearly bogus references (whitespace, control
// characters, path traversal) from reaching that boundary at all.
func parsePodRef(ref string) (string, string, error) {
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid pod ref %q (expected <namespace>/<name>)", ref)
	}
	ns, pod := parts[0], parts[1]
	if len(ns) > 253 || !podRefDNSRe.MatchString(ns) {
		return "", "", fmt.Errorf("invalid pod ref %q: namespace must be a DNS-1123 label", ref)
	}
	if len(pod) > 253 || !podRefDNSRe.MatchString(pod) {
		return "", "", fmt.Errorf("invalid pod ref %q: pod name must be a DNS-1123 label", ref)
	}
	return ns, pod, nil
}

// scanPodLogMarkers fetches the pod's log via the apiserver and scans it
// line-by-line for marker values. Non-2xx responses are surfaced as errors
// so callers can retry.
func scanPodLogMarkers(ctx context.Context, client *http.Client, endpoint, token string, markers ...string) (map[string]string, int64, error) {
	found := make(map[string]string, len(markers))
	if len(markers) == 0 {
		return found, 0, nil
	}
	type markerSpec struct {
		text  string
		bytes []byte
	}
	specs := make([]markerSpec, 0, len(markers))
	for _, marker := range markers {
		if marker == "" {
			continue
		}
		specs = append(specs, markerSpec{text: marker, bytes: []byte(marker)})
	}
	if len(specs) == 0 {
		return found, 0, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	// apiserver's pod-log endpoint returns 406 for `Accept: text/plain`;
	// it picks the encoding itself, so leave the header off (Go's
	// default `*/*` works for both wget and our client).
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("apiserver returned %d for %s", resp.StatusCode, endpoint)
	}

	limited := &io.LimitedReader{R: resp.Body, N: maxPodLogScanBytes + 1}
	var bytesScanned int64
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 0, 64*1024), maxPodLogLineSize)
	for scanner.Scan() {
		line := scanner.Bytes()
		bytesScanned += int64(len(line) + 1)
		if bytesScanned > maxPodLogScanBytes {
			return found, bytesScanned, fmt.Errorf("pod log scan exceeded %d bytes", maxPodLogScanBytes)
		}
		for _, spec := range specs {
			if _, ok := found[spec.text]; ok {
				continue
			}
			if i := bytes.Index(line, spec.bytes); i >= 0 {
				found[spec.text] = strings.TrimSpace(string(line[i+len(spec.bytes):]))
				if len(found) == len(specs) {
					return found, bytesScanned, nil
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return found, bytesScanned, fmt.Errorf("scan response: %w", err)
	}
	if limited.N == 0 {
		return found, bytesScanned, fmt.Errorf("pod log scan exceeded %d bytes", maxPodLogScanBytes)
	}
	return found, bytesScanned, nil
}

// writeCmdlineTempFile materializes the decoded cmdline at a unique per-call
// path under /tmp/katamaran-cmdlines so the existing file-based replay path
// can read it. The directory is a hostPath shared by every katamaran-{source,dest}
// job pod on the node, so a predictable filename combined with the directory's
// shared lifetime would expose us to symlink/TOCTOU pre-creation by a
// coresident pod. os.CreateTemp uses O_EXCL|O_CREATE which fails on existing
// symlinks and gives each invocation its own random name.
func writeCmdlineTempFile(data []byte) (string, error) {
	dir := "/tmp/katamaran-cmdlines"
	// 0o700: keep the directory unreadable to other UIDs sharing /tmp.
	// The decoded cmdline can include hostPath or socket addresses worth
	// hiding from coresident workloads; the file mode is also 0o600.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, "cmdline-from-podlog-*.txt")
	if err != nil {
		return "", fmt.Errorf("create temp cmdline file under %s: %w", dir, err)
	}
	path := f.Name()
	// CreateTemp opens with mode 0o600 on Unix, but be explicit so a future
	// stdlib change can't silently weaken the mode.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("chmod %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close %s: %w", path, err)
	}
	return path, nil
}
