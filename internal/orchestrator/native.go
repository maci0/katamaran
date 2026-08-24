package orchestrator

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
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

	"github.com/maci0/katamaran/internal/logging"
	"github.com/maci0/katamaran/internal/migration"
)

// native is the client-go implementation of Orchestrator. It renders the
// source/dest Job manifests in process from embedded templates, submits
// them via clientset, and reports status by polling Job conditions.
//
// What it covers today:
//
//   - Apply / Watch / Stop for both legacy explicit-fields and pod-picker
//     mode requests.
//   - Status updates: PhaseSubmitted on submit, PhaseDestStarting /
//     PhaseSrcStarting as each side's Job is created, PhaseTransferring once
//     the source Job becomes Active/Ready (and from source
//     KATAMARAN_PROGRESS log markers when available), PhaseCutover from the
//     source KATAMARAN_PHASE marker (VM paused, downtime window open),
//     PhaseSucceeded when the destination Job reaches condition=Complete,
//     and PhaseFailed when the destination fails or the source fails
//     without a successful handover.
//
// Limitations: only structured KATAMARAN_PROGRESS / KATAMARAN_RESULT /
// KATAMARAN_DOWNTIME_LIMIT marker lines are tailed from the source pod. Full
// per-pod log streaming for the dashboard log pane is not implemented.
//
// ReplayCmdline support: when the request has ReplayCmdline=true, the
// orchestrator submits the source Job first, waits for its pod to be
// scheduled, then submits the dest Job with `--replay-cmdline-from-pod
// <ns>/<srcPod>` appended. The dest binary fetches the source's QEMU
// cmdline by reading the source pod's log via the in-cluster apiserver
// (KATAMARAN_CMDLINE_B64 marker).
//
// Use New for the in-cluster path; tests construct newFromClient directly.
type native struct {
	client         kubernetes.Interface
	namespace      string
	podWaitTimeout time.Duration // default for firstSourcePod; overridden by Request.PodWaitTimeoutSeconds

	mu       sync.Mutex
	inflight map[MigrationID]*nativeRun
}

type nativeRun struct {
	srcJob                string
	destJob               string
	podWaitTimeoutSeconds int // per-request override; 0 = use orchestrator default
	updates               chan StatusUpdate
	cancel                context.CancelFunc
	finished              chan struct{}
	closeOnce             sync.Once // guards close(updates) so Stop + poll exit can race safely

	// resultMu guards the fields below. tailProgress writes them when it
	// scrapes a KATAMARAN_RESULT marker; poll reads them when emitting
	// PhaseSucceeded so the final StatusUpdate carries actual downtime
	// and final RAM totals.
	resultMu             sync.Mutex
	resultCaptured       bool
	resultDowntime       int64
	resultRAMTransferred int64
	resultRAMTotal       int64

	// Downtime-limit marker captured from the source pod log before the
	// cutover. Populated by tailProgress when it sees
	// KATAMARAN_DOWNTIME_LIMIT, surfaced by succeededUpdate too.
	downtimeCaptured bool
	appliedDowntime  int64
	rttMS            int64
	autoDowntime     bool
}

// New builds an Orchestrator. It uses the in-cluster service account when
// running inside a pod, falling back to a kubeconfig (the given path, or the
// default KUBECONFIG/~/.kube/config rules when empty) for out-of-cluster use.
func New(kubeconfig string) (Orchestrator, error) {
	cfg, err := LoadRESTConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	return newFromRestConfig(cfg)
}

// LoadRESTConfig resolves a *rest.Config: in-cluster service account first,
// then a kubeconfig file (explicit path, or the default loading rules when
// empty). Shared by the orchestrator/discoverer constructors and katamaran-mgr.
func LoadRESTConfig(kubeconfig string) (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	return cfg, nil
}

func newFromRestConfig(cfg *rest.Config) (*native, error) {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("clientset: %w", err)
	}
	return newFromClient(cs), nil
}

const defaultPodWaitTimeout = 60 * time.Second

func newFromClient(c kubernetes.Interface) *native {
	return &native{
		client:         c,
		namespace:      DefaultJobNamespace,
		podWaitTimeout: defaultPodWaitTimeout,
		inflight:       map[MigrationID]*nativeRun{},
	}
}

// SetPodWaitTimeout overrides the default timeout for waiting for migration
// Job pods to appear. Used by the controller flag / env var path.
func SetPodWaitTimeout(o Orchestrator, d time.Duration) {
	if n, ok := o.(*native); ok && d > 0 {
		n.podWaitTimeout = d
	}
}

// cleanupJobTimeout bounds each compensating Job delete issued by
// cleanupDestJob. Matches the per-cleanup budget migration.cleanupCtx uses.
const cleanupJobTimeout = 10 * time.Second

// cleanupDestJob best-effort deletes the dest Job after a setup error and
// logs a warning if the delete itself fails. Consolidates the cleanup
// branches scattered through the auto-select and dest-first code paths.
//
// The delete deliberately runs on a context that ignores the caller's
// cancellation (values preserved, deadline fresh): cancellation is itself one
// of the failures this compensation exists for — a Migration CR deleted while
// Apply is waiting for dest scheduling has no status.migrationID yet, so the
// controller's Stop path cannot reach these Jobs, and issuing the delete with
// the already-cancelled ctx would fail instantly and orphan the Job in the
// cluster until its activeDeadlineSeconds expires. Same pattern as
// migration.cleanupCtx.
func (n *native) cleanupDestJob(ctx context.Context, destJobName, reason string) {
	cctx, cancel := cleanupContext(ctx)
	defer cancel()
	if err := n.client.BatchV1().Jobs(n.namespace).Delete(cctx, destJobName, metav1.DeleteOptions{}); err != nil {
		slog.Warn("failed to clean up dest job", "reason", reason, "dest_job", destJobName, "namespace", n.namespace, "error", err)
	}
}

// cleanupContext derives a fresh, bounded deadline that ignores the parent's
// cancellation state but inherits its values, for compensating actions that
// must run after the operation they compensate was itself aborted by that
// cancellation. Same pattern as migration.cleanupCtx.
func cleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), cleanupJobTimeout)
}

// Apply renders both Job manifests, submits them, and returns a fresh ID.
// Status polling starts immediately in a goroutine.
//
// In ReplayCmdline mode the dest Job is held back until the source pod is
// up; the dest binary then scrapes the KATAMARAN_CMDLINE_B64 marker from
// the source pod log via the apiserver.
func (n *native) Apply(ctx context.Context, req Request) (MigrationID, error) {
	if err := Validate(req); err != nil {
		return "", err
	}

	id := newID()
	cmdlinePath := cmdlinePathFor(id)
	srcExtra := sourceExtraArgs(req)
	destExtra := buildExtraArgs(req)
	// staged collects the starting-phase updates emitted while the Jobs are
	// created below. They are flushed onto run.updates only after run is
	// registered, so an Apply error path still leaves no inflight entry and
	// no goroutines behind (the phase sequence stays submitted-first).
	var staged []StatusUpdate
	emitStarting := func(p StatusPhase) {
		staged = append(staged, StatusUpdate{ID: id, Phase: p, When: time.Now()})
	}
	if req.ReplayCmdline {
		// Source captures /proc/<qemu>/cmdline locally so it can compute
		// the KATAMARAN_CMDLINE_B64 marker on the way out. The dest then
		// scrapes that marker from the source pod log via apiserver —
		// stageThenStartDest patches `--replay-cmdline-from-pod
		// <ns>/<podname>` onto the dest job's command once the source pod
		// is up. No --replay-cmdline file flag here on the dest extra.
		srcExtra = strings.TrimSpace(srcExtra + " --emit-cmdline-to " + cmdlinePath)
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
		emitStarting(PhaseSrcStarting)
		slog.Info("Migration source job created; destination waits for cmdline replay", "migration_id", id, "source_job", srcJob.Name, "dest_job", destJob.Name, "namespace", n.namespace)
	} else if req.DestNode == "" {
		// Auto-select mode: create dest Job first (it has no nodeName and
		// will be scheduled by Kubernetes), wait for the pod to land on a
		// node, resolve DestIP from that node, then create the source Job
		// with the now-known DestIP.
		if _, err := n.client.BatchV1().Jobs(n.namespace).Create(ctx, destJob, metav1.CreateOptions{}); err != nil {
			return "", fmt.Errorf("create dest job: %w", err)
		}
		emitStarting(PhaseDestStarting)
		slog.Info("Auto-select: dest job created, waiting for scheduling", "migration_id", id, "dest_job", destJob.Name, "namespace", n.namespace)

		destNodeName, err := n.waitForDestNodeName(ctx, destJob.Name, req.PodWaitTimeoutSeconds)
		if err != nil {
			n.cleanupDestJob(ctx, destJob.Name, "scheduling wait failed")
			return "", fmt.Errorf("wait for dest pod scheduling: %w", err)
		}
		if destNodeName == req.SourceNode {
			n.cleanupDestJob(ctx, destJob.Name, "same-node scheduling")
			return "", fmt.Errorf("dest pod scheduled on source node %s; cannot migrate to same node", destNodeName)
		}
		disc := &nativeDiscoverer{client: n.client}
		destIP, err := disc.LookupNodeInternalIP(ctx, destNodeName)
		if err != nil {
			n.cleanupDestJob(ctx, destJob.Name, "IP lookup failed")
			return "", fmt.Errorf("resolve dest node IP: %w", err)
		}
		req.DestNode = destNodeName
		req.DestIP = destIP
		slog.Info("Auto-select: dest pod scheduled", "migration_id", id, "dest_node", destNodeName, "dest_ip", destIP)

		// Re-render the source job now that we know DestIP. ReplayCmdline
		// takes the earlier branch, so no --emit-cmdline-to is needed here.
		srcJob, err = renderSourceJob(req, id, sourceExtraArgs(req))
		if err != nil {
			n.cleanupDestJob(ctx, destJob.Name, "source job re-render failed")
			return "", fmt.Errorf("re-render source job: %w", err)
		}
		if _, err := n.client.BatchV1().Jobs(n.namespace).Create(ctx, srcJob, metav1.CreateOptions{}); err != nil {
			n.cleanupDestJob(ctx, destJob.Name, "source create failed; manual cleanup may be required")
			return "", fmt.Errorf("create source job: %w", err)
		}
		emitStarting(PhaseSrcStarting)
		slog.Info("Auto-select: migration jobs created", "migration_id", id, "source_job", srcJob.Name, "dest_job", destJob.Name, "source_node", req.SourceNode, "dest_node", req.DestNode, "namespace", n.namespace)
	} else {
		// Dest first so the migrate-incoming listener is up before source connects.
		if _, err := n.client.BatchV1().Jobs(n.namespace).Create(ctx, destJob, metav1.CreateOptions{}); err != nil {
			return "", fmt.Errorf("create dest job: %w", err)
		}
		emitStarting(PhaseDestStarting)
		if _, err := n.client.BatchV1().Jobs(n.namespace).Create(ctx, srcJob, metav1.CreateOptions{}); err != nil {
			n.cleanupDestJob(ctx, destJob.Name, "source create failed; manual cleanup may be required")
			return "", fmt.Errorf("create source job: %w", err)
		}
		emitStarting(PhaseSrcStarting)
		slog.Info("Migration jobs created", "migration_id", id, "source_job", srcJob.Name, "dest_job", destJob.Name, "namespace", n.namespace)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	run := &nativeRun{
		srcJob:                srcJob.Name,
		destJob:               destJob.Name,
		podWaitTimeoutSeconds: req.PodWaitTimeoutSeconds,
		updates:               make(chan StatusUpdate, 8),
		cancel:                cancel,
		finished:              make(chan struct{}),
	}
	n.mu.Lock()
	n.inflight[id] = run
	n.mu.Unlock()

	run.updates <- StatusUpdate{ID: id, Phase: PhaseSubmitted, When: time.Now()}
	for _, u := range staged {
		run.updates <- u
	}

	if req.ReplayCmdline {
		// Stage cmdline + create dest job in a goroutine so Apply returns
		// promptly. Status updates flow through the same channel.
		go n.stageThenStartDest(runCtx, id, run, destJob)
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
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("tailProgress panic", "migration_id", id, "panic", rec, "stack", string(debug.Stack()))
		}
	}()
	srcPod, err := n.firstSourcePod(ctx, run.srcJob, run.podWaitTimeoutSeconds)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("Progress tail unavailable: source pod was not found", "migration_id", id, "source_job", run.srcJob, "namespace", n.namespace, "error", err)
		}
		return // source pod never appeared; poll will surface the failure
	}
	const (
		progressMarker      = "KATAMARAN_PROGRESS "
		resultMarker        = "KATAMARAN_RESULT "
		downtimeLimitMarker = "KATAMARAN_DOWNTIME_LIMIT "
		phaseMarker         = "KATAMARAN_PHASE "
		// The log is consumed as ONE followed stream instead of re-fetching
		// a rolling SinceSeconds window every tick: polling a 30s window at
		// a 2s cadence re-transfers every log byte ~15x through the
		// apiserver-to-kubelet proxy and sets up a fresh HTTP stream per
		// tick. With Follow=true each marker is delivered exactly once as
		// the source emits it.
		//
		// SinceSeconds bounds the initial backfill on (re)connect;
		// LimitBytes bounds the total stream size. Both also cap what a
		// reconnect re-downloads after an error, so worst-case behavior
		// degrades to today's polling cost rather than exceeding it.
		logFetchOverlapSec int64 = 30
		logFetchLimitBytes int64 = 4 * 1024 * 1024
		// logStreamTTL bounds how long one followed stream is trusted. A
		// kubelet/apiserver hiccup can leave a stream silently stalled;
		// closing and redialing periodically guarantees tailProgress keeps
		// up with the source and cannot block forever on a dead stream.
		logStreamTTL = time.Minute
	)
	seen := map[string]bool{} // dedupe identical marker lines across reconnects
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	// Reused scanner buffer: avoids allocating 64KB per (re)connect over multi-hour migrations.
	scanBuf := make([]byte, 0, 64*1024)
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
	overlap := logFetchOverlapSec
	limitBytes := logFetchLimitBytes
	// Hoisted outside the loop: same value every connect, no need to re-allocate.
	logOpts := &corev1.PodLogOptions{Container: "katamaran", Follow: true, SinceSeconds: &overlap, LimitBytes: &limitBytes}
	var consecStreamErrors int
	for {
		select {
		case <-ctx.Done():
			return
		case <-run.finished:
			return
		case <-ticker.C:
		}
		// Cap the dedup map: only markers from within the reconnect
		// backfill window can recur, so anything beyond it is dead weight.
		// Resetting periodically keeps memory bounded on multi-hour migrations.
		if len(seen) > 1024 {
			clear(seen)
		}
		req := n.client.CoreV1().Pods(n.namespace).GetLogs(srcPod, logOpts)
		streamCtx, streamCancel := context.WithTimeout(ctx, logStreamTTL)
		stream, err := req.Stream(streamCtx)
		if err != nil {
			streamCancel()
			if ctx.Err() == nil {
				consecStreamErrors++
				attrs := []any{"migration_id", id, "pod", srcPod, "error", err, "consecutive_errors", consecStreamErrors}
				// Persistent failures (apiserver flapping, RBAC drop) leave the
				// caller without progress markers indefinitely; escalate so the
				// blind window is visible without raising the global log level.
				slog.Log(ctx, logging.TransientLevel(consecStreamErrors),
					"tailProgress: opening source pod log stream failing repeatedly", attrs...)
			}
			continue
		}
		consecStreamErrors = 0
		// Stream line-by-line instead of materializing the log window as a
		// single string + slice; the backfill can be hundreds of KB on
		// chatty migrations.
		scanner := bufio.NewScanner(stream)
		scanner.Buffer(scanBuf, 1024*1024)
		done := false
		for scanner.Scan() {
			line := scanner.Text()
			// Fast path: chatty pods produce many non-marker lines per tick
			// (a 4MB / 30s window can be tens of thousands of lines). Skip
			// them with a single substring scan before the dedup map lookup
			// and three per-marker Index calls below.
			if !strings.Contains(line, "KATAMARAN_") {
				continue
			}
			if seen[line] {
				continue
			}
			if i := strings.Index(line, resultMarker); i >= 0 {
				fields := parseProgressFields(line[i+len(resultMarker):])
				run.resultMu.Lock()
				run.resultDowntime, run.resultRAMTransferred, run.resultRAMTotal = parseResultFields(fields)
				run.resultCaptured = true
				run.resultMu.Unlock()
				done = true
				break
			}
			if i := strings.Index(line, downtimeLimitMarker); i >= 0 {
				seen[line] = true
				fields := parseProgressFields(line[i+len(downtimeLimitMarker):])
				applied := parseInt64(fields["applied_ms"])
				rttMS := parseInt64(fields["rtt_ms"])
				autoFlag := fields["auto"] == "true"
				run.resultMu.Lock()
				run.appliedDowntime = applied
				run.rttMS = rttMS
				run.autoDowntime = autoFlag
				run.downtimeCaptured = true
				run.resultMu.Unlock()
				msg := fmt.Sprintf("downtime limit applied: %dms", applied)
				if autoFlag {
					msg += fmt.Sprintf(" (auto from %dms RTT)", rttMS)
				}
				if !send(StatusUpdate{
					ID:                id,
					Phase:             PhaseTransferring,
					When:              time.Now(),
					Message:           msg,
					AppliedDowntimeMS: applied,
					RTTMS:             rttMS,
					AutoDowntime:      autoFlag,
				}) {
					done = true
					break
				}
				continue
			}
			if i := strings.Index(line, phaseMarker); i >= 0 {
				fields := parseProgressFields(line[i+len(phaseMarker):])
				ph, ok := phaseFromMarker(fields["phase"])
				if !ok {
					continue
				}
				seen[line] = true
				if !send(StatusUpdate{
					ID:      id,
					Phase:   ph,
					When:    time.Now(),
					Message: "source reported " + string(ph),
				}) {
					done = true
					break
				}
				continue
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
				done = true
				break
			}
			if fields["status"] == "failed" || fields["status"] == "cancelled" {
				done = true
				break
			}
		}
		if scanErr := scanner.Err(); scanErr != nil && ctx.Err() == nil && streamCtx.Err() == nil {
			slog.Debug("tailProgress: reading source pod log stream failed", "migration_id", id, "pod", srcPod, "error", scanErr)
		}
		streamCancel()
		_ = stream.Close()
		if done {
			return
		}
	}
}

// parseProgressFields parses key=value pairs separated by spaces.
func parseProgressFields(s string) map[string]string {
	out := make(map[string]string, 8)
	for _, kv := range strings.Fields(s) {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		out[kv[:eq]] = kv[eq+1:]
	}
	return out
}

// phaseFromMarker validates the phase= value of a KATAMARAN_PHASE marker
// emitted by the source/dest binaries. Only non-terminal lifecycle phases
// may be fabricated from log markers: terminal outcomes are owned by the
// Job-condition poll (its outcome matrix), never by a scraped line.
func phaseFromMarker(s string) (StatusPhase, bool) {
	switch StatusPhase(s) {
	case PhaseTransferring, PhaseCutover:
		return StatusPhase(s), true
	}
	return "", false
}

// succeededUpdate builds the final PhaseSucceeded StatusUpdate, attaching
// captured downtime / RAM totals from tailProgress when available.
//
// poll fires PhaseSucceeded as soon as it sees the dest Job reach
// Complete; that can race with tailProgress's 2s ticker, leaving the
// KATAMARAN_RESULT marker unscraped even though it's already in the
// source pod's log. To close that gap we do one synchronous final scrape
// here when the result hasn't been captured yet.
func (n *native) succeededUpdate(ctx context.Context, id MigrationID, run *nativeRun) StatusUpdate {
	u := StatusUpdate{ID: id, Phase: PhaseSucceeded, When: time.Now()}
	run.resultMu.Lock()
	captured := run.resultCaptured
	if captured {
		u.DowntimeMS = run.resultDowntime
		u.RAMTransferred = run.resultRAMTransferred
		u.RAMTotal = run.resultRAMTotal
	}
	if run.downtimeCaptured {
		u.AppliedDowntimeMS = run.appliedDowntime
		u.RTTMS = run.rttMS
		u.AutoDowntime = run.autoDowntime
	}
	run.resultMu.Unlock()
	if captured {
		return u
	}
	// Final synchronous scrape, bounded so a wedged apiserver never holds
	// up the terminal status update.
	scrapeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if down, transferred, total, ok := n.scrapeResultMarker(scrapeCtx, run.srcJob); ok {
		run.resultMu.Lock()
		run.resultCaptured = true
		run.resultDowntime = down
		run.resultRAMTransferred = transferred
		run.resultRAMTotal = total
		run.resultMu.Unlock()
		u.DowntimeMS = down
		u.RAMTransferred = transferred
		u.RAMTotal = total
	}
	return u
}

// scrapeResultMarker does a one-shot bounded fetch of the source pod's recent
// log tail and returns the latest KATAMARAN_RESULT marker's downtime /
// transferred / total fields. Returns ok=false if no source pod exists, the
// log stream fails, or no marker is present in the captured log window.
func (n *native) scrapeResultMarker(ctx context.Context, srcJob string) (downtimeMS, ramTransferred, ramTotal int64, ok bool) {
	pod, err := n.firstSourcePod(ctx, srcJob, 0)
	if err != nil {
		return 0, 0, 0, false
	}
	tailLines := int64(200)
	limitBytes := int64(1024 * 1024)
	stream, err := n.client.CoreV1().Pods(n.namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container:  "katamaran",
		TailLines:  &tailLines,
		LimitBytes: &limitBytes,
	}).Stream(ctx)
	if err != nil {
		return 0, 0, 0, false
	}
	defer func() { _ = stream.Close() }()
	const marker = "KATAMARAN_RESULT "
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		i := strings.Index(line, marker)
		if i < 0 {
			continue
		}
		downtimeMS, ramTransferred, ramTotal = parseResultFields(parseProgressFields(line[i+len(marker):]))
		ok = true
		// Don't break — take the LAST marker, which is what tailProgress
		// would have picked up too.
	}
	return downtimeMS, ramTransferred, ramTotal, ok
}

// parseInt64 decodes an integer field from a scraped KATAMARAN_* marker.
// Missing fields already yield 0 via the map lookup; parse failures and
// out-of-range values must yield 0 too. strconv.ParseInt clamps out-of-range
// input to MaxInt64/MinInt64 alongside its error; letting that clamp through
// would persist bogus byte/downtime counts into Migration CR status and
// overflow downstream math like (ramTransferred * 100).
func parseInt64(s string) int64 {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// parseResultFields decodes the three KATAMARAN_RESULT marker counters.
// Shared by tailProgress (live capture) and scrapeResultMarker (final
// synchronous scrape) so the field names cannot drift between the two paths.
func parseResultFields(fields map[string]string) (downtimeMS, ramTransferred, ramTotal int64) {
	return parseInt64(fields["downtime_ms"]),
		parseInt64(fields["ram_transferred"]),
		parseInt64(fields["ram_total"])
}

// ConditionFailureDetails extracts a terminal Job condition's reason and
// message into both a compact "reason=… message=…" summary string and slog
// attrs. Exported so the Migration CRD controller's recovery path formats
// job failures identically to the orchestrator's outcome matrix instead of
// keeping its own copy of the extraction.
func ConditionFailureDetails(cond batchv1.JobCondition) (detail string, attrs []any) {
	details := make([]string, 0, 2)
	attrs = make([]any, 0, 4)
	if cond.Reason != "" {
		details = append(details, "reason="+cond.Reason)
		attrs = append(attrs, "reason", cond.Reason)
	}
	if cond.Message != "" {
		details = append(details, "message="+cond.Message)
		attrs = append(attrs, "message", cond.Message)
	}
	return strings.Join(details, " "), attrs
}

func jobConditionAttrs(cond batchv1.JobCondition) []any {
	_, attrs := ConditionFailureDetails(cond)
	return attrs
}

func jobFailedError(base string, cond batchv1.JobCondition) error {
	detail, _ := ConditionFailureDetails(cond)
	if detail == "" {
		return errors.New(base)
	}
	return fmt.Errorf("%s: %s", base, detail)
}

func logTransientJobStatusError(message string, id MigrationID, jobName, namespace string, err error, consecutive int) {
	attrs := []any{"migration_id", id, "job", jobName, "namespace", namespace, "error", err, "consecutive_errors", consecutive}
	slog.Log(context.Background(), logging.TransientLevel(consecutive), message, attrs...)
}

// stageThenStartDest resolves the source pod's name (so the dest binary
// can fetch the source's cmdline directly from its pod log) and submits
// the dest Job with --replay-cmdline-from-pod=<ns>/<podname> appended
// to its EXTRA_ARGS. The only synchronisation between source-job
// creation and dest-job creation is "wait for the source pod to exist".
func (n *native) stageThenStartDest(ctx context.Context, id MigrationID, run *nativeRun, destJob *batchv1.Job) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("stageThenStartDest panic", "migration_id", id, "panic", rec, "stack", string(debug.Stack()))
			run.send(StatusUpdate{ID: id, Phase: PhaseFailed, When: time.Now(), Error: fmt.Errorf("dest staging panic: %v", rec)})
		}
	}()
	srcPod, err := n.firstSourcePod(ctx, run.srcJob, run.podWaitTimeoutSeconds)
	if err != nil {
		slog.Error("Cmdline replay failed: source pod not found", "migration_id", id, "source_job", run.srcJob, "namespace", n.namespace, "error", err)
		run.send(StatusUpdate{ID: id, Phase: PhaseFailed, When: time.Now(), Error: fmt.Errorf("locate source pod: %w", err)})
		run.cancel()
		return
	}
	patched, err := injectReplayFromPod(destJob, n.namespace, srcPod)
	if err != nil {
		slog.Error("Cmdline replay failed: patching dest job command", "migration_id", id, "dest_job", destJob.Name, "error", err)
		run.send(StatusUpdate{ID: id, Phase: PhaseFailed, When: time.Now(), Error: fmt.Errorf("inject --replay-cmdline-from-pod: %w", err)})
		run.cancel()
		return
	}
	if _, err := n.client.BatchV1().Jobs(n.namespace).Create(ctx, patched, metav1.CreateOptions{}); err != nil {
		slog.Error("Cmdline replay destination job create failed", "migration_id", id, "dest_job", destJob.Name, "namespace", n.namespace, "error", err)
		run.send(StatusUpdate{ID: id, Phase: PhaseFailed, When: time.Now(), Error: fmt.Errorf("create dest job: %w", err)})
		run.cancel()
		return
	}
	run.send(StatusUpdate{ID: id, Phase: PhaseDestStarting, When: time.Now()})
	slog.Info("Replay-from-pod wired; destination job created", "migration_id", id, "source_pod", srcPod, "dest_job", destJob.Name, "namespace", n.namespace)
}

// injectReplayFromPod returns a copy of destJob with
// `--replay-cmdline-from-pod <ns>/<pod>` appended to the dest container's
// command. The render path doesn't know the source pod name (it's only
// resolved after the source Job creates its pod), so this final argv
// patch happens here.
func injectReplayFromPod(destJob *batchv1.Job, ns, srcPod string) (*batchv1.Job, error) {
	if destJob == nil || len(destJob.Spec.Template.Spec.Containers) == 0 {
		return nil, fmt.Errorf("dest job has no containers")
	}
	// Defense-in-depth: the ref is interpolated into a /bin/sh -c command
	// string, so both halves must pass the same DNS-1123 validation the
	// dest side applies when it later parses and URL-escapes the value.
	if !migration.ValidateDNSLabel(ns) {
		return nil, fmt.Errorf("invalid source pod namespace %q: must be a DNS-1123 label", ns)
	}
	if !migration.ValidateDNSSubdomain(srcPod) {
		return nil, fmt.Errorf("invalid source pod name %q: must be a DNS-1123 subdomain", srcPod)
	}
	out := destJob.DeepCopy()
	cs := out.Spec.Template.Spec.Containers
	for i := range cs {
		if cs[i].Name != "katamaran" {
			continue
		}
		// The dest job template invokes /bin/sh -c "<full command>"; the
		// last element of cs[i].Command is that command string. We append
		// the new flag to it so the dest binary's flag.Parse picks it
		// up alongside the existing --mode dest --qmp ... etc.
		if len(cs[i].Command) == 0 {
			return nil, fmt.Errorf("katamaran container has empty command")
		}
		last := len(cs[i].Command) - 1
		cs[i].Command[last] += fmt.Sprintf(" --replay-cmdline-from-pod %s/%s", ns, srcPod)
		return out, nil
	}
	return nil, fmt.Errorf("no katamaran container in dest job")
}

// send pushes u onto run.updates if the run is still live. If poll has
// already closed updates (e.g. after Stop) the send is dropped silently.
// Callers that need to know whether the send succeeded should select on
// run.finished themselves; this helper exists to make late sends safe.
//
// Go channels have no non-panicking "send unless closed" primitive: the
// finished signal can fire between the select branches and poll's
// close(run.updates) here, so the send below races with that close.
// The deferred recover absorbs that specific panic; any other runtime
// panic in this goroutine still propagates.
func (run *nativeRun) send(u StatusUpdate) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("send to closed run.updates absorbed", "panic", r)
		}
	}()
	select {
	case <-run.finished:
	case run.updates <- u:
	}
}

// waitForJobPod polls until pick returns a non-empty value for any pod under
// jobName, then returns it. Shared backbone for firstSourcePod (returns the
// first pod name as soon as any pod appears) and waitForDestNodeName (returns
// the assigned node name once the dest pod is scheduled). desc shapes the
// timeout error and the retry-log message.
func (n *native) waitForJobPod(ctx context.Context, jobName, desc string, reqTimeout int, pick func(corev1.Pod) string) (string, error) {
	timeout := n.podWaitTimeout
	if reqTimeout > 0 {
		timeout = time.Duration(reqTimeout) * time.Second
	}
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastListErr error
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		// Watch-cache read: this loop re-lists every 2s while waiting for a
		// pod to appear/schedule. A watch-cache lag of one tick is absorbed
		// by the retry, so quorum reads would only sustain pointless etcd
		// load (same rationale as poll's cacheRead).
		pods, err := n.client.CoreV1().Pods(n.namespace).List(deadline, metav1.ListOptions{
			LabelSelector:   "batch.kubernetes.io/job-name=" + jobName,
			ResourceVersion: "0",
		})
		if err == nil {
			for _, p := range pods.Items {
				if v := pick(p); v != "" {
					return v, nil
				}
			}
		} else if deadline.Err() == nil {
			lastListErr = err
			slog.Debug("waitForJobPod: list pods failed, will retry", "desc", desc, "job", jobName, "namespace", n.namespace, "error", err)
		}
		select {
		case <-deadline.Done():
			if lastListErr != nil {
				return "", fmt.Errorf("waiting for %s of job %s: %w (last list error: %v)", desc, jobName, deadline.Err(), lastListErr)
			}
			return "", fmt.Errorf("waiting for %s of job %s: %w", desc, jobName, deadline.Err())
		case <-ticker.C:
		}
	}
}

// waitForDestNodeName waits for the destination Job's pod to be scheduled
// (i.e. have a non-empty spec.nodeName) and returns the assigned node name.
// Used in auto-select mode to discover which node Kubernetes chose.
func (n *native) waitForDestNodeName(ctx context.Context, jobName string, reqTimeout int) (string, error) {
	return n.waitForJobPod(ctx, jobName, "dest pod scheduling", reqTimeout, func(p corev1.Pod) string {
		return p.Spec.NodeName
	})
}

// firstSourcePod waits for the source Job's pod to be created and returns
// its name. The timeout is determined by the per-request override
// (PodWaitTimeoutSeconds), falling back to the orchestrator-level default.
func (n *native) firstSourcePod(ctx context.Context, jobName string, reqTimeout int) (string, error) {
	return n.waitForJobPod(ctx, jobName, "source pod", reqTimeout, func(p corev1.Pod) string {
		return p.Name
	})
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

// Resume re-runs the source-pod-resolution + dest-Job-creation step
// for an in-flight migration whose original goroutine was lost (e.g.
// the controller pod restarted between source-Job submission and the
// dest-Job submission). Returns (true, nil) when this call actually
// created the dest Job, (false, nil) when the dest Job already existed
// (idempotent: the recovery path can call Resume on every reconcile
// tick without inflating counters), or (false, err) when the source
// Job is missing or has no reachable pod.
//
// Used by the Migration CRD controller's recovery path: when reconcile
// finds a non-terminal CR with a source Job but no dest Job, it calls
// Resume to drive the staging forward instead of marking the migration
// failed.
func (n *native) Resume(ctx context.Context, id MigrationID, req Request) (bool, error) {
	if !req.ReplayCmdline {
		// Non-replay mode submits both Jobs in Apply itself; there's
		// nothing to resume. The recovery path only invokes Resume when
		// the dest Job is missing AND the source Job is present, which
		// only happens in ReplayCmdline mode.
		return false, nil
	}
	destName := DestJobName(id)
	if _, err := n.client.BatchV1().Jobs(n.namespace).Get(ctx, destName, metav1.GetOptions{}); err == nil {
		return false, nil // already created
	} else if !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("get dest job %s: %w", destName, err)
	}
	srcName := SourceJobName(id)
	if _, err := n.client.BatchV1().Jobs(n.namespace).Get(ctx, srcName, metav1.GetOptions{}); err != nil {
		return false, fmt.Errorf("get source job %s: %w", srcName, err)
	}
	srcPod, err := n.firstSourcePod(ctx, srcName, req.PodWaitTimeoutSeconds)
	if err != nil {
		return false, fmt.Errorf("locate source pod: %w", err)
	}
	destJob, err := renderDestJob(req, id, buildExtraArgs(req))
	if err != nil {
		return false, fmt.Errorf("render dest job: %w", err)
	}
	patched, err := injectReplayFromPod(destJob, n.namespace, srcPod)
	if err != nil {
		return false, fmt.Errorf("inject replay flag: %w", err)
	}
	if _, err := n.client.BatchV1().Jobs(n.namespace).Create(ctx, patched, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return false, nil
		}
		return false, fmt.Errorf("create dest job: %w", err)
	}
	slog.Info("Resume: destination job created", "migration_id", id, "source_pod", srcPod, "dest_job", destJob.Name, "namespace", n.namespace)
	return true, nil
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
	if err := n.client.BatchV1().Jobs(n.namespace).Delete(ctx, run.srcJob, delOpts); err != nil && !apierrors.IsNotFound(err) {
		slog.Warn("Stop: source job delete failed; job may leak", "migration_id", id, "job", run.srcJob, "namespace", n.namespace, "error", err)
	}
	if err := n.client.BatchV1().Jobs(n.namespace).Delete(ctx, run.destJob, delOpts); err != nil && !apierrors.IsNotFound(err) {
		slog.Warn("Stop: dest job delete failed; job may leak", "migration_id", id, "job", run.destJob, "namespace", n.namespace, "error", err)
	}
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
//	source=Failed && dest pending → wait up to sourceFailGrace (90s) for dest
//	source=Failed && dest still pending after grace → PhaseFailed
func (n *native) poll(ctx context.Context, id MigrationID, run *nativeRun) {
	defer run.cancel()
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
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("poll panic", "migration_id", id, "panic", rec, "stack", string(debug.Stack()))
		}
	}()
	const interval = 2 * time.Second
	const sourceFailGrace = 90 * time.Second // how long to wait for dest after source dies
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// Serve both Job GETs from the apiserver watch cache: unset
	// ResourceVersion makes every Get a quorum read against etcd, which at
	// 2 GETs per tick per migration sustains hours of pointless etcd load
	// for data that changes twice per migration. A tick of staleness is
	// irrelevant on a 2s polling loop with a 90s failure grace window.
	cacheRead := metav1.GetOptions{ResourceVersion: "0"}
	announcedTransferring := false
	var sourceFailedAt time.Time
	var destStatusErrors, srcStatusErrors int
	for {
		select {
		case <-ctx.Done():
			slog.Warn("Migration poll canceled", "migration_id", id, "source_job", run.srcJob, "dest_job", run.destJob, "error", ctx.Err())
			run.updates <- StatusUpdate{ID: id, Phase: PhaseFailed, When: time.Now(), Error: ctx.Err()}
			return
		case <-ticker.C:
			srcJob, srcErr := n.client.BatchV1().Jobs(n.namespace).Get(ctx, run.srcJob, cacheRead)
			destJob, destErr := n.client.BatchV1().Jobs(n.namespace).Get(ctx, run.destJob, cacheRead)

			// Dest=Complete → success, even if source failed.
			if destErr == nil {
				destStatusErrors = 0
				if cond, ok := LatestTerminalJobCondition(destJob); ok && cond.Type == batchv1.JobComplete {
					slog.Info("Migration destination job completed", "migration_id", id, "source_job", run.srcJob, "dest_job", run.destJob)
					run.updates <- n.succeededUpdate(ctx, id, run)
					return
				} else if ok && cond.Type == batchv1.JobFailed {
					attrs := []any{"migration_id", id, "source_job", run.srcJob, "dest_job", run.destJob}
					attrs = append(attrs, jobConditionAttrs(cond)...)
					slog.Error("Migration destination job failed", attrs...)
					run.updates <- StatusUpdate{ID: id, Phase: PhaseFailed, When: time.Now(), Error: jobFailedError("dest job failed", cond)}
					return
				}
			} else if !apierrors.IsNotFound(destErr) {
				destStatusErrors++
				logTransientJobStatusError("Migration destination job status unavailable", id, run.destJob, n.namespace, destErr, destStatusErrors)
			} else {
				destStatusErrors = 0
			}

			// Source job state.
			if srcErr != nil {
				if apierrors.IsNotFound(srcErr) {
					slog.Error("Migration source job disappeared", "migration_id", id, "source_job", run.srcJob, "dest_job", run.destJob)
					run.updates <- StatusUpdate{ID: id, Phase: PhaseFailed, When: time.Now(), Error: errors.New("source job disappeared")}
					return
				}
				srcStatusErrors++
				logTransientJobStatusError("Migration source job status unavailable", id, run.srcJob, n.namespace, srcErr, srcStatusErrors)
				continue
			}
			srcStatusErrors = 0
			if srcCond, ok := LatestTerminalJobCondition(srcJob); ok && srcCond.Type == batchv1.JobFailed {
				if sourceFailedAt.IsZero() {
					sourceFailedAt = time.Now()
					attrs := []any{"migration_id", id, "source_job", run.srcJob, "dest_job", run.destJob, "grace", sourceFailGrace}
					attrs = append(attrs, jobConditionAttrs(srcCond)...)
					slog.Warn("Migration source job failed; waiting for destination grace window", attrs...)
				}
				if time.Since(sourceFailedAt) > sourceFailGrace {
					attrs := []any{"migration_id", id, "source_job", run.srcJob, "dest_job", run.destJob, "grace", sourceFailGrace}
					attrs = append(attrs, jobConditionAttrs(srcCond)...)
					slog.Error("Migration source job failed and destination did not complete", attrs...)
					run.updates <- StatusUpdate{ID: id, Phase: PhaseFailed, When: time.Now(), Error: jobFailedError("source job failed and dest did not complete within grace window", srcCond)}
					return
				}
				continue // give dest time to land RESUME and exit 0
			}
			if !announcedTransferring && (srcJob.Status.Active > 0 || (srcJob.Status.Ready != nil && *srcJob.Status.Ready > 0)) {
				run.updates <- StatusUpdate{ID: id, Phase: PhaseTransferring, When: time.Now()}
				announcedTransferring = true
			}
		}
	}
}

// sourceExtraArgs returns the EXTRA_ARGS string for the source Job. It
// augments the shared buildExtraArgs with source-only flags that the source
// CLI requires but that must not leak into the dest command. In legacy mode
// (no SourcePod) the source binary needs an explicit --vm-ip, otherwise it
// exits 2 with "source mode requires either (--vm-ip ...) or (--pod-name ...)";
// a SourceQMP override is forwarded too. In pod mode the source CLI rejects
// --qmp/--vm-ip alongside --pod-name, so nothing extra is added and the
// caller-supplied override (if any) is intentionally ignored.
func sourceExtraArgs(req Request) string {
	base := buildExtraArgs(req)
	if req.SourcePod != nil {
		return base
	}
	extra := []string{base, "--vm-ip", req.VMIP}
	if req.SourceQMP != "" {
		extra = append(extra, "--qmp", req.SourceQMP)
	}
	return strings.TrimSpace(strings.Join(extra, " "))
}

// buildExtraArgs assembles the EXTRA_ARGS string appended to both rendered
// source and dest container commands. Mode-specific flags may appear in this
// shared string; the katamaran CLI warns and ignores flags that do not apply
// to the current mode. Replay cmdline delivery flags are appended separately.
// Source-only flags (--vm-ip, legacy --qmp) are added by sourceExtraArgs.
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
		if req.AutoDowntimeFloorMS > 0 {
			args = append(args, "--auto-downtime-floor-ms", strconv.Itoa(req.AutoDowntimeFloorMS))
		}
	}
	if req.CNIConvergenceDelaySeconds > 0 {
		args = append(args, "--cni-convergence-delay", fmt.Sprintf("%ds", req.CNIConvergenceDelaySeconds))
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
