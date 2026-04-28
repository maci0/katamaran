package migration

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// procFS abstracts the host-process / netns probing so tests can stub it.
// PIDForSandbox locates the QEMU PID for a given sandbox UUID. NetnsHasIP
// reports whether the network namespace of the given PID contains the given
// IP address on any interface.
type procFS interface {
	PIDForSandbox(uuid string) (int, error)
	NetnsHasIP(pid int, ip string) (bool, error)
}

// Resolved is the output of sandbox resolution: the matched sandbox UUID and
// the QEMU PID running inside it.
type Resolved struct {
	Sandbox string
	PID     int
}

// resolveSandbox scans root (typically /run/vc/vm) for sandbox directories,
// asks p for each sandbox's QEMU PID, and returns the unique sandbox whose
// network namespace contains podIP. It returns an error on zero matches or
// on multiple matches (it refuses to guess).
//
// PIDForSandbox / NetnsHasIP errors for individual entries are tolerated and
// logged at debug level so that a single transient failure (e.g. a sandbox
// that has just been torn down) does not abort the whole scan.
func resolveSandbox(root string, p procFS, podIP string) (Resolved, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return Resolved{}, fmt.Errorf("read %s: %w", root, err)
	}
	var matches []Resolved
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := p.PIDForSandbox(e.Name())
		if err != nil {
			slog.Warn("PIDForSandbox failed; skipping", "sandbox", e.Name(), "error", err)
			continue
		}
		ok, err := p.NetnsHasIP(pid, podIP)
		if err != nil {
			slog.Warn("NetnsHasIP failed; skipping", "sandbox", e.Name(), "pid", pid, "error", err)
			continue
		}
		if ok {
			matches = append(matches, Resolved{Sandbox: e.Name(), PID: pid})
		}
	}
	switch len(matches) {
	case 0:
		return Resolved{}, fmt.Errorf("no sandbox under %s contains pod IP %s", root, podIP)
	case 1:
		return matches[0], nil
	default:
		return Resolved{}, fmt.Errorf("ambiguous: %d sandboxes match pod IP %s: %+v", len(matches), podIP, matches)
	}
}

// procExecTimeout caps the wall-clock time of pgrep / nsenter invocations
// performed by realProc. Two seconds is comfortably above typical observed
// latencies (single-digit ms) while keeping a stuck process from hanging the
// whole resolve loop.
const procExecTimeout = 2 * time.Second

// realProc is the production implementation of procFS. It shells out to
// pgrep and nsenter; both invocations are bounded by procExecTimeout.
type realProc struct{}

// PIDForSandbox locates the QEMU PID associated with the given sandbox UUID
// by running `pgrep -f "sandbox-<uuid>"`. If pgrep returns multiple PIDs
// (e.g. helper processes whose cmdline mentions the sandbox path), the first
// one is returned and the rest are logged at debug level.
func (realProc) PIDForSandbox(uuid string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), procExecTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "pgrep", "-f", "sandbox-"+uuid).Output()
	if err != nil {
		return 0, fmt.Errorf("pgrep sandbox-%s: %w", uuid, err)
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) == 0 {
		return 0, fmt.Errorf("pgrep returned no PIDs for sandbox-%s", uuid)
	}
	pid, err := strconv.Atoi(lines[0])
	if err != nil {
		return 0, fmt.Errorf("parse PID %q: %w", lines[0], err)
	}
	if len(lines) > 1 {
		slog.Warn("pgrep returned multiple PIDs; using first", "sandbox", uuid, "pids", lines)
	}
	return pid, nil
}

// NetnsHasIP returns true if the network namespace of pid has an interface
// configured with ip. It runs `nsenter --net=/proc/<pid>/ns/net -- ip -o addr
// show` and checks each line for an exact-token match on ip.
func (realProc) NetnsHasIP(pid int, ip string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), procExecTimeout)
	defer cancel()
	nsArg := fmt.Sprintf("--net=/proc/%d/ns/net", pid)
	out, err := exec.CommandContext(ctx, "nsenter", nsArg, "--", "ip", "-o", "addr", "show").Output()
	if err != nil {
		return false, fmt.Errorf("nsenter ip addr in pid %d: %w", pid, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		// `ip -o addr show` lines look like:
		//   2: eth0    inet 10.0.0.5/24 brd ... scope global eth0
		// Match the bare IP (without prefix) as an exact whitespace-or-slash
		// delimited token to avoid 10.0.0.5 matching inside 10.0.0.50/24.
		for _, tok := range splitAddrTokens(line) {
			if tok == ip {
				return true, nil
			}
		}
	}
	return false, nil
}

// splitAddrTokens returns the candidate IP tokens from a line of `ip -o addr`
// output. It strips the CIDR suffix on each whitespace-separated field so the
// caller can compare against a bare IP literal.
func splitAddrTokens(line string) []string {
	fields := strings.Fields(line)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if i := strings.IndexByte(f, '/'); i >= 0 {
			out = append(out, f[:i])
		} else {
			out = append(out, f)
		}
	}
	return out
}

// In-cluster apiserver lookup paths and endpoint. These are package-level
// vars (not consts) so tests can redirect them at httptest servers and at
// temp-dir credentials. Production callers leave them at their defaults.
var (
	tokenPath     = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	caPath        = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	apiserverHost = ""
	apiserverPort = ""

	// Backoff durations used by LookupPodIP between successive attempts.
	// Var-not-const so tests can collapse them to ~ms.
	lookupBackoff1 = 1 * time.Second
	lookupBackoff2 = 2 * time.Second
	lookupBackoff3 = 4 * time.Second
)

// resolveAPIServerHostPort returns the apiserver host:port. It prefers the
// package-level overrides (set by tests) and otherwise falls back to the
// standard $KUBERNETES_SERVICE_HOST / $KUBERNETES_SERVICE_PORT env vars
// injected by the kubelet into every in-cluster pod.
func resolveAPIServerHostPort() (string, string, error) {
	host := apiserverHost
	if host == "" {
		host = os.Getenv("KUBERNETES_SERVICE_HOST")
	}
	port := apiserverPort
	if port == "" {
		port = os.Getenv("KUBERNETES_SERVICE_PORT")
	}
	if host == "" || port == "" {
		return "", "", fmt.Errorf("KUBERNETES_SERVICE_HOST/PORT not set; not running in-cluster?")
	}
	return host, port, nil
}

// podStatusResp is the minimal shape we decode from the apiserver Pod GET.
// We only care about status.podIP; everything else is intentionally ignored.
type podStatusResp struct {
	Status struct {
		PodIP string `json:"podIP"`
	} `json:"status"`
}

// LookupPodIP queries the in-cluster Kubernetes API for the given pod and
// returns its status.podIP. It uses the standard service-account token + CA
// bundle mounted at /var/run/secrets/kubernetes.io/serviceaccount and the
// apiserver endpoint from $KUBERNETES_SERVICE_HOST / $KUBERNETES_SERVICE_PORT.
//
// If the pod's status.podIP is empty (e.g. the pod is still being scheduled),
// LookupPodIP retries up to three times with backoffs of 1s, 2s, 4s. After
// the third empty response it returns a clear error.
//
// Any non-2xx response or transport error during a single attempt is treated
// as a transient failure and aborts the call immediately (no retry) — the
// retry budget is reserved for the "Pod exists but has no IP yet" race.
func LookupPodIP(ctx context.Context, ns, name string) (string, error) {
	host, port, err := resolveAPIServerHostPort()
	if err != nil {
		return "", err
	}

	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil {
		return "", fmt.Errorf("read service account token: %w", err)
	}
	token := strings.TrimSpace(string(tokenBytes))

	caBytes, err := os.ReadFile(caPath)
	if err != nil {
		return "", fmt.Errorf("read service account CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBytes) {
		return "", fmt.Errorf("CA file %s did not contain any PEM certificates", caPath)
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				MinVersion: tls.VersionTLS12,
			},
		},
	}

	// Escape ns/name as single path segments: callers may pass values with
	// '/' (the orchestrator validation allowlist permits it for legitimate
	// path-like arg values), but here they must address one Pod resource —
	// not subresources like /log or /exec.
	endpoint := fmt.Sprintf("https://%s/api/v1/namespaces/%s/pods/%s",
		net.JoinHostPort(host, port), url.PathEscape(ns), url.PathEscape(name))

	backoffs := []time.Duration{lookupBackoff1, lookupBackoff2, lookupBackoff3}
	const attempts = 3
	for i := 0; i < attempts; i++ {
		ip, err := lookupPodIPOnce(ctx, client, endpoint, token)
		if err != nil {
			return "", err
		}
		if ip != "" {
			return ip, nil
		}
		// Empty IP — sleep before next attempt unless this was the last.
		if i < attempts-1 {
			slog.Debug("Pod has no IP yet; will retry", "pod", ns+"/"+name, "attempt", i+1, "backoff", backoffs[i])
			timer := time.NewTimer(backoffs[i])
			select {
			case <-ctx.Done():
				timer.Stop()
				return "", fmt.Errorf("looking up pod %s/%s IP: %w", ns, name, ctx.Err())
			case <-timer.C:
			}
		}
	}
	return "", fmt.Errorf("pod %s/%s has no IP after retries", ns, name)
}

// lookupPodIPOnce performs a single GET against the apiserver and returns the
// pod's status.podIP (which may be empty if the pod is not yet scheduled).
func lookupPodIPOnce(ctx context.Context, client *http.Client, endpoint, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("apiserver GET %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		// Read a little of the body to surface a useful error.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("apiserver returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var ps podStatusResp
	if err := json.NewDecoder(resp.Body).Decode(&ps); err != nil {
		return "", fmt.Errorf("decode pod response: %w", err)
	}
	return ps.Status.PodIP, nil
}
