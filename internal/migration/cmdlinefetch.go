package migration

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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
	ns, pod, err := parsePodRef(ref)
	if err != nil {
		return "", err
	}
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
	endpoint := fmt.Sprintf("https://%s/api/v1/namespaces/%s/pods/%s/log?container=katamaran",
		net.JoinHostPort(host, port), url.PathEscape(ns), url.PathEscape(pod))

	deadline, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	const marker = "KATAMARAN_CMDLINE_B64="
	for {
		select {
		case <-deadline.Done():
			return "", fmt.Errorf("source pod %s/%s did not emit %s within timeout", ns, pod, marker)
		default:
		}
		body, err := getPodLog(deadline, client, endpoint, token)
		if err == nil {
			if b64 := scanMarker(body, marker); b64 != "" {
				decoded, derr := base64.StdEncoding.DecodeString(b64)
				if derr != nil {
					return "", fmt.Errorf("decode KATAMARAN_CMDLINE_B64: %w", derr)
				}
				return writeCmdlineTempFile(decoded)
			}
		}
		// Retry every 2s. Either the pod isn't up yet (404), the
		// cmdline marker hasn't been emitted (apiserver returns the
		// log so far), or a transient apiserver error.
		select {
		case <-deadline.Done():
			return "", fmt.Errorf("source pod %s/%s did not emit %s within timeout", ns, pod, marker)
		case <-time.After(2 * time.Second):
		}
	}
}

// parsePodRef splits "<ns>/<name>" into its components.
func parsePodRef(ref string) (string, string, error) {
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid pod ref %q (expected <namespace>/<name>)", ref)
	}
	return parts[0], parts[1], nil
}

// getPodLog fetches the pod's log via the apiserver and returns the body.
// Non-2xx responses are surfaced as errors so callers can retry.
func getPodLog(ctx context.Context, client *http.Client, endpoint, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/plain")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("apiserver returned %d for %s", resp.StatusCode, endpoint)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return body, nil
}

// scanMarker returns the value after the first `<marker>` line in body.
// Returns "" when no such line exists.
func scanMarker(body []byte, marker string) string {
	for _, line := range strings.Split(string(body), "\n") {
		i := strings.Index(line, marker)
		if i < 0 {
			continue
		}
		return strings.TrimSpace(line[i+len(marker):])
	}
	return ""
}

// writeCmdlineTempFile materializes the decoded cmdline at a stable
// per-process path so the existing file-based replay path can read it.
// Reuses the same /tmp/katamaran-cmdlines directory the source uses,
// which the dest job already mounts as a hostPath.
func writeCmdlineTempFile(data []byte) (string, error) {
	dir := "/tmp/katamaran-cmdlines"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, fmt.Sprintf("cmdline-from-podlog-%d.txt", os.Getpid()))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}
