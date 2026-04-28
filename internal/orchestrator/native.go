package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Native is the in-cluster client-go implementation of Orchestrator. It
// renders the source/dest Job manifests in process from embedded templates,
// submits them via clientset, and reports status by polling Job conditions.
//
// What it covers today:
//
//   - Apply / Watch / Stop for both legacy explicit-fields and pod-picker
//     mode requests.
//   - Status updates: PhaseSubmitted on submit, PhaseTransferring once both
//     jobs are scheduled, PhaseSucceeded when the source Job reaches
//     condition=Complete, PhaseFailed when it reaches condition=Failed.
//
// What it does NOT cover yet (Script orchestrator is still the canonical
// path for these):
//
//   - Granular RAM-transfer progress (no log scraping yet).
//   - Per-pod log streaming for the dashboard log pane.
//
// ReplayCmdline support: when the request has ReplayCmdline=true, Native
// submits the source Job, tails the source pod's logs for the
// `KATAMARAN_CMDLINE_AT=` marker, SPDY-streams the cmdline file off the
// source pod, creates a one-shot stager pod on the destination node to
// land the file via hostPath, and only then creates the dest Job. This
// requires a rest.Config (SPDY upgrade), so NewFromClient (test
// constructor without a rest.Config) still returns
// ErrReplayCmdlineNotSupported.
//
// Use New for the in-cluster path and NewFromClient for tests.
type native struct {
	client    kubernetes.Interface
	config    *rest.Config // optional; required only for ReplayCmdline mode (SPDY exec)
	namespace string

	mu       sync.Mutex
	inflight map[MigrationID]*nativeRun
}

type nativeRun struct {
	srcJob    string
	destJob   string
	updates   chan StatusUpdate
	cancel    context.CancelFunc
	finished  chan struct{}
	closeOnce sync.Once // guards close(updates) so Stop + poll exit can race safely

	// resultMu guards the fields below. tailProgress writes them when it
	// scrapes a KATAMARAN_RESULT marker; poll reads them when emitting
	// PhaseSucceeded so the final StatusUpdate carries actual downtime
	// and final RAM totals.
	resultMu       sync.Mutex
	resultCaptured bool
	resultDowntime int64
	resultRAMXfer  int64
	resultRAMTotal int64
}

// ErrReplayCmdlineNotSupported is returned by Native.Apply when the request
// has ReplayCmdline=true but the Native orchestrator was constructed without
// a rest.Config (e.g. via NewFromClient in tests). Use New for
// in-cluster ReplayCmdline support.
var ErrReplayCmdlineNotSupported = errors.New("ReplayCmdline requires a rest.Config (use orchestrator.New, not NewFromClient)")

// New builds an Orchestrator using the in-cluster service account.
// Job manifests are submitted into kube-system (matching the existing
// migrate.sh layout). The returned implementation supports
// ReplayCmdline because it carries a rest.Config for SPDY remote-
// command calls.
func New() (Orchestrator, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	return newFromRestConfig(cfg)
}

// NewFromKubeconfig builds an Orchestrator from a kubeconfig file.
// Intended for out-of-cluster usage (dashboard run on a developer
// laptop, integration tests, etc). Pass an empty path to use the default
// loading rules (KUBECONFIG env / ~/.kube/config).
func NewFromKubeconfig(path, contextName string) (Orchestrator, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path != "" {
		rules.ExplicitPath = path
	}
	overrides := &clientcmd.ConfigOverrides{}
	if contextName != "" {
		overrides.CurrentContext = contextName
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	return newFromRestConfig(cfg)
}

func newFromRestConfig(cfg *rest.Config) (*native, error) {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("clientset: %w", err)
	}
	n := newFromClient(cs)
	n.config = cfg
	return n, nil
}

// NewFromClient is the test-friendly constructor. The returned
// Orchestrator does NOT support ReplayCmdline (no rest.Config for
// SPDY) — production code paths should use New or NewFromKubeconfig.
func NewFromClient(c kubernetes.Interface) Orchestrator {
	return newFromClient(c)
}

func newFromClient(c kubernetes.Interface) *native {
	return &native{
		client:    c,
		namespace: "kube-system",
		inflight:  map[MigrationID]*nativeRun{},
	}
}

// Apply renders both Job manifests, submits them, and returns a fresh ID.
// Status polling starts immediately in a goroutine.
//
// In ReplayCmdline mode the dest Job is held back until the source has
// emitted the QEMU cmdline marker, so the cmdline file is staged on the
// destination node before katamaran-dest starts.
func (n *native) Apply(ctx context.Context, req Request) (MigrationID, error) {
	if err := Validate(req); err != nil {
		return "", err
	}
	if req.ReplayCmdline && n.config == nil {
		return "", ErrReplayCmdlineNotSupported
	}

	id := newID()
	cmdlinePath := fmt.Sprintf("/tmp/katamaran-cmdlines/cmdline-%s.txt", id)
	srcExtra := buildExtraArgs(req)
	destExtra := srcExtra
	if req.ReplayCmdline {
		srcExtra = strings.TrimSpace(srcExtra + " --emit-cmdline-to " + cmdlinePath)
		destExtra = strings.TrimSpace(destExtra + " --replay-cmdline " + cmdlinePath)
	}
	srcJob, err := renderSourceJob(req, id, srcExtra)
	if err != nil {
		return "", fmt.Errorf("render source job: %w", err)
	}
	destJob, err := renderDestJob(req, id, destExtra)
	if err != nil {
		return "", fmt.Errorf("render dest job: %w", err)
	}

	if req.ReplayCmdline {
		// Source first: it has to capture and emit the cmdline before the
		// dest job can spawn QEMU with --replay-cmdline.
		if _, err := n.client.BatchV1().Jobs(n.namespace).Create(ctx, srcJob, metav1.CreateOptions{}); err != nil {
			return "", fmt.Errorf("create source job: %w", err)
		}
	} else {
		// Dest first so the migrate-incoming listener is up before source connects.
		if _, err := n.client.BatchV1().Jobs(n.namespace).Create(ctx, destJob, metav1.CreateOptions{}); err != nil {
			return "", fmt.Errorf("create dest job: %w", err)
		}
		if _, err := n.client.BatchV1().Jobs(n.namespace).Create(ctx, srcJob, metav1.CreateOptions{}); err != nil {
			_ = n.client.BatchV1().Jobs(n.namespace).Delete(ctx, destJob.Name, metav1.DeleteOptions{})
			return "", fmt.Errorf("create source job: %w", err)
		}
	}

	runCtx, cancel := context.WithCancel(context.Background())
	run := &nativeRun{
		srcJob:   srcJob.Name,
		destJob:  destJob.Name,
		updates:  make(chan StatusUpdate, 8),
		cancel:   cancel,
		finished: make(chan struct{}),
	}
	n.mu.Lock()
	n.inflight[id] = run
	n.mu.Unlock()

	run.updates <- StatusUpdate{ID: id, Phase: PhaseSubmitted, When: time.Now()}

	if req.ReplayCmdline {
		// Stage cmdline + create dest job in a goroutine so Apply returns
		// promptly. Status updates flow through the same channel.
		go n.stageThenStartDest(runCtx, id, run, req, destJob)
	}
	go n.poll(runCtx, id, run)
	go n.tailProgress(runCtx, id, run)
	return id, nil
}

// tailProgress watches the source pod's logs for KATAMARAN_PROGRESS and
// KATAMARAN_RESULT markers emitted by the source binary. PROGRESS markers
// are re-emitted as PhaseTransferring StatusUpdates with RAMTransferred /
// RAMTotal populated. The RESULT marker (one-shot, post-completion) is
// stashed on run for the reconciler to attach to PhaseSucceeded.
//
// Exit condition: a RESULT marker, a failed/cancelled progress status,
// or ctx cancel. Plain `status=completed` is NOT terminal here — the
// RESULT line lands a few ms after — so we keep polling until RESULT
// arrives or the run is torn down.
func (n *native) tailProgress(ctx context.Context, id MigrationID, run *nativeRun) {
	srcPod, err := n.firstSourcePod(ctx, run.srcJob)
	if err != nil {
		return // source pod never appeared; poll will surface the failure
	}
	const (
		progressMarker = "KATAMARAN_PROGRESS "
		resultMarker   = "KATAMARAN_RESULT "
	)
	seen := map[string]bool{} // dedupe identical marker lines
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	send := func(u StatusUpdate) bool {
		select {
		case <-ctx.Done():
			return false
		case <-run.finished:
			return false
		default:
		}
		run.send(u)
		return true
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-run.finished:
			return
		case <-ticker.C:
		}
		req := n.client.CoreV1().Pods(n.namespace).GetLogs(srcPod, &corev1.PodLogOptions{Container: "katamaran"})
		stream, err := req.Stream(ctx)
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(stream)
		_ = stream.Close()
		for _, line := range strings.Split(string(data), "\n") {
			if seen[line] {
				continue
			}
			if i := strings.Index(line, resultMarker); i >= 0 {
				seen[line] = true
				fields := parseProgressFields(line[i+len(resultMarker):])
				run.resultMu.Lock()
				run.resultDowntime = parseInt64(fields["downtime_ms"])
				run.resultRAMXfer = parseInt64(fields["ram_transferred"])
				run.resultRAMTotal = parseInt64(fields["ram_total"])
				run.resultCaptured = true
				run.resultMu.Unlock()
				return
			}
			i := strings.Index(line, progressMarker)
			if i < 0 {
				continue
			}
			seen[line] = true
			fields := parseProgressFields(line[i+len(progressMarker):])
			if !send(StatusUpdate{
				ID:             id,
				Phase:          PhaseTransferring,
				When:           time.Now(),
				Message:        "status=" + fields["status"],
				RAMTransferred: parseInt64(fields["ram_transferred"]),
				RAMTotal:       parseInt64(fields["ram_total"]),
			}) {
				return
			}
			if fields["status"] == "failed" || fields["status"] == "cancelled" {
				return
			}
		}
	}
}

// parseProgressFields parses key=value pairs separated by spaces.
func parseProgressFields(s string) map[string]string {
	out := map[string]string{}
	for _, kv := range strings.Fields(s) {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		out[kv[:eq]] = kv[eq+1:]
	}
	return out
}

// succeededUpdate builds the final PhaseSucceeded StatusUpdate, attaching
// captured downtime / RAM totals from tailProgress when available.
func (n *native) succeededUpdate(id MigrationID, run *nativeRun) StatusUpdate {
	u := StatusUpdate{ID: id, Phase: PhaseSucceeded, When: time.Now()}
	run.resultMu.Lock()
	if run.resultCaptured {
		u.DowntimeMS = run.resultDowntime
		u.RAMTransferred = run.resultRAMXfer
		u.RAMTotal = run.resultRAMTotal
	}
	run.resultMu.Unlock()
	return u
}

func parseInt64(s string) int64 {
	if s == "" {
		return 0
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// stageThenStartDest runs the cmdline-staging flow for ReplayCmdline mode:
// finds the source pod, copies the cmdline off it, stages on the dest node,
// then submits the dest Job. Failures abort the run with PhaseFailed.
func (n *native) stageThenStartDest(ctx context.Context, id MigrationID, run *nativeRun, req Request, destJob *batchv1.Job) {
	srcPod, err := n.firstSourcePod(ctx, run.srcJob)
	if err != nil {
		run.send(StatusUpdate{ID: id, Phase: PhaseFailed, When: time.Now(), Error: fmt.Errorf("locate source pod: %w", err)})
		run.cancel()
		return
	}
	if _, err := n.stageCmdline(ctx, id, srcPod, n.namespace, req.DestNode); err != nil {
		run.send(StatusUpdate{ID: id, Phase: PhaseFailed, When: time.Now(), Error: fmt.Errorf("stage cmdline: %w", err)})
		run.cancel()
		return
	}
	if _, err := n.client.BatchV1().Jobs(n.namespace).Create(ctx, destJob, metav1.CreateOptions{}); err != nil {
		run.send(StatusUpdate{ID: id, Phase: PhaseFailed, When: time.Now(), Error: fmt.Errorf("create dest job: %w", err)})
		run.cancel()
		return
	}
}

// send pushes u onto run.updates if the run is still live. If poll has
// already closed updates (e.g. after Stop) the send is dropped silently.
// Callers that need to know whether the send succeeded should select on
// run.finished themselves; this helper exists to make late sends safe.
func (run *nativeRun) send(u StatusUpdate) {
	select {
	case <-run.finished:
		// Channel is being / has been closed by poll. Drop.
	default:
	}
	defer func() { recover() }() // closeOnce + finished signal can race with another send
	select {
	case <-run.finished:
	case run.updates <- u:
	}
}

// firstSourcePod waits up to 60s for the source Job's pod to be created
// and returns its name. We need the pod (not the Job) to read logs from.
func (n *native) firstSourcePod(ctx context.Context, jobName string) (string, error) {
	deadline, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	for {
		pods, err := n.client.CoreV1().Pods(n.namespace).List(deadline, metav1.ListOptions{
			LabelSelector: "batch.kubernetes.io/job-name=" + jobName,
		})
		if err == nil && len(pods.Items) > 0 {
			return pods.Items[0].Name, nil
		}
		select {
		case <-deadline.Done():
			return "", deadline.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// Watch returns the channel of status updates for id. ErrUnknownID if the
// migration completed and was reaped before Watch was called.
func (n *native) Watch(_ context.Context, id MigrationID) (<-chan StatusUpdate, error) {
	n.mu.Lock()
	run, ok := n.inflight[id]
	n.mu.Unlock()
	if !ok {
		return nil, ErrUnknownID
	}
	return run.updates, nil
}

// Stop deletes both Jobs (background propagation). The watcher will emit a
// terminal update when the source Job's controller reports Failed.
func (n *native) Stop(ctx context.Context, id MigrationID) error {
	n.mu.Lock()
	run, ok := n.inflight[id]
	n.mu.Unlock()
	if !ok {
		return ErrUnknownID
	}
	prop := metav1.DeletePropagationBackground
	delOpts := metav1.DeleteOptions{PropagationPolicy: &prop}
	_ = n.client.BatchV1().Jobs(n.namespace).Delete(ctx, run.srcJob, delOpts)
	_ = n.client.BatchV1().Jobs(n.namespace).Delete(ctx, run.destJob, delOpts)
	run.cancel()
	return nil
}

// poll watches BOTH the source and destination Job statuses and emits
// StatusUpdate events. The migration is reported successful when the dest
// Job reaches Complete — the dest Job receives RAM, fires QEMU's RESUME
// event, and exits 0 only on a complete handover. The source Job's exit
// code is incidental: kata-shim frequently kills the source QEMU after
// migration completes (so the source binary's QMP polling errors out and
// the container exits non-zero) even though the migration itself succeeded.
//
// Outcome matrix:
//
//	dest=Complete          → PhaseSucceeded (regardless of source)
//	dest=Failed            → PhaseFailed
//	source=Failed && dest pending → keep waiting (dest may still complete)
//	source=Failed && dest never starts → PhaseFailed
func (n *native) poll(ctx context.Context, id MigrationID, run *nativeRun) {
	defer func() {
		// Signal finished BEFORE closing updates so concurrent senders
		// (tailProgress, stageThenStartDest) can break out via the
		// select-on-finished pattern instead of panicking on a closed
		// channel send.
		close(run.finished)
		run.closeOnce.Do(func() { close(run.updates) })
		n.mu.Lock()
		delete(n.inflight, id)
		n.mu.Unlock()
	}()
	const interval = 2 * time.Second
	const sourceFailGrace = 90 * time.Second // how long to wait for dest after source dies
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	announcedTransferring := false
	var sourceFailedAt time.Time
	for {
		select {
		case <-ctx.Done():
			run.updates <- StatusUpdate{ID: id, Phase: PhaseFailed, When: time.Now(), Error: ctx.Err()}
			return
		case <-ticker.C:
			srcJob, srcErr := n.client.BatchV1().Jobs(n.namespace).Get(ctx, run.srcJob, metav1.GetOptions{})
			destJob, destErr := n.client.BatchV1().Jobs(n.namespace).Get(ctx, run.destJob, metav1.GetOptions{})

			// Dest=Complete → success, even if source failed.
			if destErr == nil {
				if cond := jobCondition(destJob); cond == batchv1.JobComplete {
					run.updates <- n.succeededUpdate(id, run)
					return
				} else if cond == batchv1.JobFailed {
					run.updates <- StatusUpdate{ID: id, Phase: PhaseFailed, When: time.Now(), Error: errors.New("dest job failed")}
					return
				}
			}

			// Source job state.
			if srcErr != nil {
				if apierrors.IsNotFound(srcErr) {
					run.updates <- StatusUpdate{ID: id, Phase: PhaseFailed, When: time.Now(), Error: errors.New("source job disappeared")}
					return
				}
				continue
			}
			srcCond := jobCondition(srcJob)
			if srcCond == batchv1.JobFailed {
				if sourceFailedAt.IsZero() {
					sourceFailedAt = time.Now()
				}
				if time.Since(sourceFailedAt) > sourceFailGrace {
					run.updates <- StatusUpdate{ID: id, Phase: PhaseFailed, When: time.Now(), Error: errors.New("source job failed and dest did not complete within grace window")}
					return
				}
				continue // give dest time to land RESUME and exit 0
			}
			if !announcedTransferring && (srcJob.Status.Active > 0 || srcJob.Status.Ready != nil && *srcJob.Status.Ready > 0) {
				run.updates <- StatusUpdate{ID: id, Phase: PhaseTransferring, When: time.Now()}
				announcedTransferring = true
			}
		}
	}
}

func jobCondition(job *batchv1.Job) batchv1.JobConditionType {
	for _, c := range job.Status.Conditions {
		if c.Status != "True" {
			continue
		}
		if c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed {
			return c.Type
		}
	}
	return ""
}

// buildExtraArgs assembles the EXTRA_ARGS string the source/dest containers
// receive. Mirrors the bash logic in deploy/migrate.sh's SRC_EXTRA_ARGS /
// DEST_EXTRA_ARGS construction (minus the shipped-cmdline flags, which are
// only applicable in ReplayCmdline mode).
func buildExtraArgs(req Request) string {
	var args []string
	if req.SharedStorage {
		args = append(args, "--shared-storage")
	}
	if req.SourcePod != nil {
		args = append(args, "--pod-name", req.SourcePod.Name, "--pod-namespace", req.SourcePod.Namespace)
	}
	if req.DestPod != nil {
		args = append(args, "--dest-pod-name", req.DestPod.Name, "--dest-pod-namespace", req.DestPod.Namespace)
	}
	if req.TapIface != "" {
		args = append(args, "--tap", req.TapIface)
	}
	if req.TapNetns != "" {
		args = append(args, "--tap-netns", req.TapNetns)
	}
	if req.TunnelMode != "" {
		args = append(args, "--tunnel-mode", req.TunnelMode)
	}
	if req.DowntimeMS > 0 {
		args = append(args, "--downtime", strconv.Itoa(req.DowntimeMS))
	}
	if req.AutoDowntime {
		args = append(args, "--auto-downtime")
	}
	// Always pass --multifd-channels (including 0) so the source binary
	// does not fall back to its own non-zero default and create a multifd
	// mismatch with the dest (which sets multifd from this same value).
	args = append(args, "--multifd-channels", strconv.Itoa(req.MultifdChannels))
	if req.LogLevel != "" {
		args = append(args, "--log-level", req.LogLevel)
	}
	if req.LogFormat != "" {
		args = append(args, "--log-format", req.LogFormat)
	}
	return strings.Join(args, " ")
}
