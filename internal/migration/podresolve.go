package migration

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// sandboxUUIDRe restricts sandbox identifiers to characters that cannot have
// special meaning when interpolated into the `pgrep -f` regex. Sandbox dir
// names come from /run/vc/vm/<uuid>; rejecting unexpected characters bounds
// the input even if a bug or attacker creates an oddly named directory.
var sandboxUUIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// procFS abstracts the host-process / netns probing so tests can stub it.
// PIDsForSandboxes locates the QEMU PID for each of the given sandbox UUIDs
// in a single pass over /proc. NetnsHasIP reports whether the network
// namespace of the given PID contains the given IP address on any interface.
type procFS interface {
	PIDsForSandboxes(uuids []string) map[string]int
	NetnsHasIP(pid int, ip string) (bool, error)
}

// Resolved is the output of sandbox resolution: the matched sandbox UUID and
// the QEMU PID running inside it.
type Resolved struct {
	Sandbox string
	PID     int
}

// resolveSandbox scans root (typically /run/vc/vm) for sandbox directories,
// resolves the QEMU PID for every sandbox in a single /proc pass, and returns
// the unique sandbox whose network namespace contains podIP. It returns an
// error on zero matches or on multiple matches (it refuses to guess).
//
// Sandboxes whose PID lookup fails and NetnsHasIP errors for individual
// entries are tolerated and logged so that a single transient failure (e.g.
// a sandbox that has just been torn down) does not abort the whole scan.
func resolveSandbox(root string, p procFS, podIP string) (Resolved, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return Resolved{}, fmt.Errorf("read %s: %w", root, err)
	}
	var uuids []string
	for _, e := range entries {
		if e.IsDir() {
			uuids = append(uuids, e.Name())
		}
	}
	// One /proc scan for all sandboxes: per-sandbox scans would cost
	// O(sandboxes x processes) cmdline reads on a busy node.
	pids := p.PIDsForSandboxes(uuids)
	var matches []Resolved
	for _, uuid := range uuids {
		pid, ok := pids[uuid]
		if !ok {
			slog.Warn("No QEMU process found for sandbox; skipping", "sandbox", uuid)
			continue
		}
		ok, err := p.NetnsHasIP(pid, podIP)
		if err != nil {
			slog.Warn("NetnsHasIP failed; skipping", "sandbox", uuid, "pid", pid, "error", err)
			continue
		}
		if ok {
			matches = append(matches, Resolved{Sandbox: uuid, PID: pid})
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

// resolvePodSandbox resolves a kata pod to its sandbox directory and
// QEMU PID: fetch status.podIP via LookupPodIP, then find the unique
// sandbox whose netns carries that IP. The parsed pod IP is returned
// alongside because RunSource programs it into tunnel/route setup;
// RunDestination ignores it. Shared by both ends so their resolution
// semantics cannot drift.
func resolvePodSandbox(ctx context.Context, namespace, name string) (podIP netip.Addr, res Resolved, err error) {
	ip, err := lookupPodIP(ctx, namespace, name)
	if err != nil {
		return netip.Addr{}, Resolved{}, fmt.Errorf("lookup pod IP: %w", err)
	}
	podIP, err = netip.ParseAddr(ip)
	if err != nil {
		return netip.Addr{}, Resolved{}, fmt.Errorf("parse resolved pod IP %q: %w", ip, err)
	}
	res, err = resolveSandbox(sandboxRoot, procImpl, ip)
	if err != nil {
		return netip.Addr{}, Resolved{}, fmt.Errorf("resolve sandbox: %w", err)
	}
	return podIP, res, nil
}

// sandboxQMPSocket is the per-sandbox extra-monitor socket path Kata
// binds under sandboxRoot/<sandboxID>/.
func sandboxQMPSocket(sandboxID string) string {
	return filepath.Join(sandboxRoot, sandboxID, extraMonitorSocketName)
}

// overrideQMPSocket applies the QMP-socket override rule shared by both
// ends of a migration: an empty socket or the role's well-known
// placeholder means "no explicit override", and the resolved
// sandbox-specific path wins. placeholder is DefaultQMPSocket on the
// source and DestDefaultQMPSocket on the dest; keeping the comparison
// in one function keeps the "empty or placeholder" contract from
// drifting between the two call sites.
func overrideQMPSocket(socket, placeholder, sandboxID string) string {
	if socket == "" || socket == placeholder {
		return sandboxQMPSocket(sandboxID)
	}
	return socket
}

// realProc is the production implementation of procFS.
type realProc struct{}

// PIDsForSandboxes locates the QEMU PID associated with each given sandbox
// UUID by scanning /proc/<pid>/cmdline for the literal substring
// "sandbox-<uuid>". The whole set is resolved in one pass over /proc so N
// sandboxes cost N cmdline reads rather than N scans of every process. This
// is a native scan rather than a `pgrep -f` shellout: no external binary, and
// the substring is matched literally against the raw NUL-separated cmdline so
// a `.` in the UUID cannot act as a regex wildcard and match unrelated
// processes. If several processes match a sandbox (e.g. helpers whose cmdline
// mentions the sandbox path), the lowest PID wins. UUIDs with no matching
// process are absent from the returned map; invalid identifiers are skipped.
func (realProc) PIDsForSandboxes(uuids []string) map[string]int {
	type want struct {
		needle string
		best   int // lowest matching PID so far; 0 = none yet
	}
	wanted := make(map[string]*want, len(uuids))
	for _, uuid := range uuids {
		if !sandboxUUIDRe.MatchString(uuid) {
			slog.Warn("Skipping invalid sandbox identifier", "sandbox", uuid)
			continue
		}
		wanted[uuid] = &want{needle: "sandbox-" + uuid}
	}
	if len(wanted) == 0 {
		return nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		slog.Warn("Cannot read /proc; no sandbox PIDs resolvable", "error", err)
		return nil
	}
	var multi []string
	for _, e := range entries {
		pid, perr := strconv.Atoi(e.Name())
		if perr != nil {
			continue // not a PID directory
		}
		// A read error means the process exited mid-scan or is unreadable; skip.
		raw, rerr := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if rerr != nil {
			continue
		}
		for uuid, w := range wanted {
			if w.best != 0 && pid >= w.best {
				continue // already matched a lower PID for this sandbox
			}
			if bytes.Contains(raw, []byte(w.needle)) {
				if w.best != 0 {
					multi = append(multi, uuid)
				}
				w.best = pid
			}
		}
	}
	out := make(map[string]int, len(wanted))
	for uuid, w := range wanted {
		if w.best != 0 {
			out[uuid] = w.best
		}
	}
	if len(multi) > 0 {
		slices.Sort(multi)
		slog.Warn("multiple processes match sandboxes; using lowest PID", "sandboxes", multi)
	}
	return out
}

// NetnsHasIP returns true if the network namespace of pid has an interface
// configured with ip. Instead of shelling out to `nsenter ... ip addr`, it
// enters the target netns natively via setns(2) and lists addresses with the
// standard library's net.InterfaceAddrs.
func (realProc) NetnsHasIP(pid int, ip string) (bool, error) {
	target := net.ParseIP(ip)
	if target == nil {
		return false, fmt.Errorf("invalid IP %q", ip)
	}
	addrs, err := netnsInterfaceAddrs(pid)
	if err != nil {
		return false, err
	}
	for _, a := range addrs {
		var got net.IP
		switch v := a.(type) {
		case *net.IPNet:
			got = v.IP
		case *net.IPAddr:
			got = v.IP
		}
		if got != nil && got.Equal(target) {
			return true, nil
		}
	}
	return false, nil
}

// netnsInterfaceAddrs returns the interface addresses visible inside the network
// namespace of pid. setns(2) only affects the calling OS thread, and Go freely
// migrates goroutines across threads, so the switch is done on a dedicated
// goroutine pinned with runtime.LockOSThread that is never unlocked: when it
// returns, the runtime terminates the (now netns-tainted) thread rather than
// recycling it. The netlink socket net.InterfaceAddrs opens is created on the
// pinned thread, so it inherits the target namespace.
//
// ponytail: one throwaway OS thread per call. Fine at resolve-time frequency
// (a handful of calls per migration); revisit only if this turns into a hot path.
func netnsInterfaceAddrs(pid int) ([]net.Addr, error) {
	type result struct {
		addrs []net.Addr
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		runtime.LockOSThread() // deliberately never unlocked; see doc comment
		nsPath := fmt.Sprintf("/proc/%d/ns/net", pid)
		f, err := os.Open(nsPath)
		if err != nil {
			ch <- result{nil, fmt.Errorf("open %s: %w", nsPath, err)}
			return
		}
		defer func() { _ = f.Close() }()
		if err := unix.Setns(int(f.Fd()), unix.CLONE_NEWNET); err != nil {
			ch <- result{nil, fmt.Errorf("setns into pid %d netns: %w", pid, err)}
			return
		}
		addrs, err := net.InterfaceAddrs()
		if err != nil {
			ch <- result{nil, fmt.Errorf("list addrs in pid %d netns: %w", pid, err)}
			return
		}
		ch <- result{addrs, nil}
	}()
	r := <-ch
	return r.addrs, r.err
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

// inClusterAPIClient bundles what every direct apiserver caller needs:
// an HTTP client wired with the service-account CA bundle and a strict
// no-redirect policy, plus the bearer token to set per request.
type inClusterAPIClient struct {
	http  *http.Client
	token string
}

// newInClusterAPIClient builds the shared apiserver HTTP client: SA token +
// CA bundle from the service-account mount, TLS 1.2 minimum, and redirects
// refused so a crafted redirect can never divert the bearer token off-cluster.
func newInClusterAPIClient() (*inClusterAPIClient, error) {
	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("read service account token: %w", err)
	}
	caBytes, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read service account CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBytes) {
		return nil, fmt.Errorf("CA file %s did not contain any PEM certificates", caPath)
	}
	return &inClusterAPIClient{
		http: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					RootCAs:    pool,
					MinVersion: tls.VersionTLS12,
				},
			},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		token: strings.TrimSpace(string(tokenBytes)),
	}, nil
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
// as a transient failure and aborts the call immediately (no retry): the
// retry budget is reserved for the "Pod exists but has no IP yet" race.
func LookupPodIP(ctx context.Context, ns, name string) (string, error) {
	host, port, err := resolveAPIServerHostPort()
	if err != nil {
		return "", err
	}
	api, err := newInClusterAPIClient()
	if err != nil {
		return "", err
	}
	defer api.http.CloseIdleConnections()

	// Escape ns/name as single path segments: callers may pass values with
	// '/' (the orchestrator validation allowlist permits it for legitimate
	// path-like arg values), but here they must address one Pod resource,
	// not subresources like /log or /exec.
	endpoint := fmt.Sprintf("https://%s/api/v1/namespaces/%s/pods/%s",
		net.JoinHostPort(host, port), url.PathEscape(ns), url.PathEscape(name))

	backoffs := []time.Duration{lookupBackoff1, lookupBackoff2, lookupBackoff3}
	const attempts = 3
	for i := 0; i < attempts; i++ {
		ip, err := lookupPodIPOnce(ctx, api.http, endpoint, api.token)
		if err != nil {
			return "", err
		}
		if ip != "" {
			return ip, nil
		}
		// Empty IP: sleep before next attempt unless this was the last.
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ps); err != nil {
		return "", fmt.Errorf("decode pod response: %w", err)
	}
	return ps.Status.PodIP, nil
}
