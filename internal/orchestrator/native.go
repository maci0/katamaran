package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"runtime/debug"
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
	ids            idSource      // migration ID minting; newID unless a test injects one

	mu       sync.Mutex
	inflight map[MigrationID]*nativeRun
	// dedupMu serializes the duplicate-source-pod check against the first
	// Job submit of an Apply (see createUnlessInFlight). Held only across
	// that one List and one Create, never across a pod-scheduling wait.
	dedupMu sync.Mutex
}

type nativeRun struct {
	srcJob                string
	destJob               string
	podWaitTimeoutSeconds int // per-request override; 0 = use orchestrator default
	updates               chan StatusUpdate
	cancel                context.CancelFunc
	finished              chan struct{}
	closeOnce             sync.Once
	sendMu                sync.RWMutex

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
		if kubeconfig != "" {
			// In-cluster wins, so an operator who passed --kubeconfig
			// inside a pod would otherwise never learn which credentials
			// were actually used.
			slog.Warn("In-cluster service account in use; ignoring --kubeconfig", "kubeconfig", kubeconfig, "host", cfg.Host)
		}
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

// jobPollInterval is the cadence of every controller-side poll loop over Job
// and Pod state: the source log tail, the dest pod wait, and poll's own Job
// inspection. One cadence keeps poll's sourceFailGrace reasoning (a tick of
// staleness must stay well inside the grace window) true for all three.
const jobPollInterval = 2 * time.Second

// logScannerInitBuf is the initial capacity of the reused log-line scanner
// buffer, and logScannerMaxBuf its hard cap.
const (
	logScannerInitBuf = 64 * 1024
	logScannerMaxBuf  = 1024 * 1024
)

func newFromClient(c kubernetes.Interface) *native {
	return &native{
		client:         c,
		namespace:      DefaultJobNamespace,
		podWaitTimeout: defaultPodWaitTimeout,
		ids:            newID,
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

// maxDurationSeconds is the largest second count that fits in a time.Duration
// without overflowing int64 nanoseconds (math.MaxInt64 / 1e9, rounded down).
const maxDurationSeconds = int64(math.MaxInt64) / int64(time.Second)

// secondsAsDuration converts a Job's spec.activeDeadlineSeconds into a
// Duration, saturating instead of overflowing. The field is an int64 read back
// from the apiserver, so a Job carrying a deadline above maxDurationSeconds
// makes the bare multiplication wrap to a negative Duration. Every caller here
// compares an elapsed time against the result, and a negative bound makes that
// comparison true for any age, i.e. the Job looks expired the instant it is
// created. That is fail-open on a deadline check: a Job with a huge deadline
// would be treated as already reaped, so the in-flight guard would hand out a
// second migration ID for a pod whose first migration is still running.
// Saturating keeps a hostile value meaning "no realistic expiry", which is
// what an unbounded deadline means.
func secondsAsDuration(seconds int64) time.Duration {
	if seconds <= 0 {
		return 0
	}
	if seconds > maxDurationSeconds {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(seconds) * time.Second
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
// Apply is idempotent on the source pod: a second call while the first
// migration for that pod is still running returns the in-flight migration's
// ID and creates nothing. Both Jobs drive a full storage sync and a QEMU
// handover against one live VM sharing one node-global destination sandbox,
// so a second set of Jobs for the same pod is a second destructive copy, not
// a redundant no-op.
//
// In ReplayCmdline mode the dest Job is held back until the source pod is
// up; the dest binary then scrapes the KATAMARAN_CMDLINE_B64 marker from
// the source pod log via the apiserver.
func (n *native) Apply(ctx context.Context, req Request) (MigrationID, error) {
	if err := Validate(req); err != nil {
		return "", err
	}

	id := n.ids()
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
		// scrapes that marker from the source pod log via apiserver:
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
		joined, err := n.createUnlessInFlight(ctx, req, srcJob)
		if err != nil {
			return "", fmt.Errorf("create source job: %w", err)
		}
		if joined != "" {
			return n.joinInFlight(joined, req), nil
		}
		emitStarting(PhaseSrcStarting)
		slog.Info("Migration source job created; destination waits for cmdline replay", "migration_id", id, "source_job", srcJob.Name, "dest_job", destJob.Name, "namespace", n.namespace)
	} else if req.DestNode == "" {
		// Auto-select mode: create dest Job first (it has no nodeName and
		// will be scheduled by Kubernetes), wait for the pod to land on a
		// node, resolve DestIP from that node, then create the source Job
		// with the now-known DestIP.
		if joined, err := n.createUnlessInFlight(ctx, req, destJob); err != nil {
			return "", fmt.Errorf("create dest job: %w", err)
		} else if joined != "" {
			return n.joinInFlight(joined, req), nil
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
		if joined, err := n.createUnlessInFlight(ctx, req, destJob); err != nil {
			return "", fmt.Errorf("create dest job: %w", err)
		} else if joined != "" {
			return n.joinInFlight(joined, req), nil
		}
		emitStarting(PhaseDestStarting)
		if _, err := n.client.BatchV1().Jobs(n.namespace).Create(ctx, srcJob, metav1.CreateOptions{}); err != nil {
			n.cleanupDestJob(ctx, destJob.Name, "source create failed; manual cleanup may be required")
			return "", fmt.Errorf("create source job: %w", err)
		}
		emitStarting(PhaseSrcStarting)
		slog.Info("Migration jobs created", "migration_id", id, "source_job", srcJob.Name, "dest_job", destJob.Name, "namespace", n.namespace)
	}

	n.startRun(id, srcJob.Name, destJob.Name, req, staged, destJob, true)
	return id, nil
}

// startRun registers a run and launches its status watchers. A run already
// registered under id is left alone, so a duplicate caller neither resets
// the live run nor starts a second set of watchers on its update channel.
// stageDestJob is nil on the join path: the staging goroutine belongs to
// whoever submitted the source Job, and a duplicate must not run it again.
func (n *native) startRun(id MigrationID, srcName, destName string, req Request, staged []StatusUpdate, stageDestJob *batchv1.Job, seedSubmitted bool) {
	// Claim the id before building the run: a duplicate call would otherwise
	// allocate a cancel context and an updates channel it then drops, and the
	// dropped cancel is a context nothing will ever release.
	n.mu.Lock()
	if _, exists := n.inflight[id]; exists {
		n.mu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(context.Background())
	run := &nativeRun{
		srcJob:                srcName,
		destJob:               destName,
		podWaitTimeoutSeconds: req.PodWaitTimeoutSeconds,
		updates:               make(chan StatusUpdate, 8),
		cancel:                cancel,
		finished:              make(chan struct{}),
	}
	n.inflight[id] = run
	n.mu.Unlock()

	if seedSubmitted {
		run.updates <- StatusUpdate{ID: id, Phase: PhaseSubmitted, When: time.Now()}
	}
	for _, u := range staged {
		run.updates <- u
	}

	if stageDestJob != nil && req.ReplayCmdline {
		// Stage cmdline + create dest job in a goroutine so Apply returns
		// promptly. Status updates flow through the same channel.
		go n.stageThenStartDest(runCtx, id, run, stageDestJob)
	}
	go n.poll(runCtx, id, run)
	go n.tailProgress(runCtx, id, run)
}

// stageThenStartDest resolves the source pod's name (so the dest binary
// can fetch the source's cmdline directly from its pod log) and submits
// the dest Job with --replay-cmdline-from-pod=<ns>/<podname> appended
// to its EXTRA_ARGS. The only synchronisation between source-job
// creation and dest-job creation is "wait for the source pod to exist".
func (n *native) stageThenStartDest(ctx context.Context, id MigrationID, run *nativeRun, destJob *batchv1.Job) {
	fail := func(err error) {
		defer run.cancel()
		cctx, cancel := cleanupContext(ctx)
		defer cancel()
		propagation := metav1.DeletePropagationBackground
		if cleanupErr := n.client.BatchV1().Jobs(n.namespace).Delete(cctx, run.srcJob, metav1.DeleteOptions{PropagationPolicy: &propagation}); cleanupErr != nil && !apierrors.IsNotFound(cleanupErr) {
			slog.Error("Failed to clean up source job after dest staging failure", "migration_id", id, "source_job", run.srcJob, "namespace", n.namespace, "error", cleanupErr)
			err = errors.Join(err, fmt.Errorf("clean up source job %s: %w", run.srcJob, cleanupErr))
		}
		run.send(StatusUpdate{ID: id, Phase: PhaseFailed, When: time.Now(), Error: err})
	}
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("stageThenStartDest panic", "migration_id", id, "panic", rec, "stack", string(debug.Stack()))
			fail(fmt.Errorf("dest staging panic: %v", rec))
		}
	}()
	srcPod, err := n.firstSourcePod(ctx, run.srcJob, run.podWaitTimeoutSeconds)
	if err != nil {
		slog.Error("Cmdline replay failed: source pod not found", "migration_id", id, "source_job", run.srcJob, "namespace", n.namespace, "error", err)
		fail(fmt.Errorf("locate source pod: %w", err))
		return
	}
	patched, err := injectReplayFromPod(destJob, PodRef{Namespace: n.namespace, Name: srcPod})
	if err != nil {
		slog.Error("Cmdline replay failed: patching dest job command", "migration_id", id, "dest_job", destJob.Name, "error", err)
		fail(fmt.Errorf("inject --replay-cmdline-from-pod: %w", err))
		return
	}
	created, err := n.createDestJobIfAbsent(ctx, patched)
	if err != nil {
		slog.Error("Cmdline replay destination job create failed", "migration_id", id, "dest_job", destJob.Name, "namespace", n.namespace, "error", err)
		fail(fmt.Errorf("create dest job: %w", err))
		return
	}
	if !created {
		// The dest Job was already there: this staging pass is a repeat of
		// work already done, so the migration continues against the running
		// Job rather than failing. Reporting failure here would delete the
		// source Job out from under a live migration.
		slog.Info("Destination job already present, resuming against it", "migration_id", id, "source_pod", srcPod, "dest_job", destJob.Name, "namespace", n.namespace)
	}
	run.send(StatusUpdate{ID: id, Phase: PhaseDestStarting, When: time.Now()})
	slog.Info("Replay-from-pod wired; destination job created", "migration_id", id, "source_pod", srcPod, "dest_job", destJob.Name, "namespace", n.namespace)
}

// injectReplayFromPod returns a copy of destJob with
// `--replay-cmdline-from-pod <ns>/<pod>` appended to the dest container's
// command. The render path doesn't know the source pod name (it's only
// resolved after the source Job creates its pod), so this final argv
// patch happens here.
func injectReplayFromPod(destJob *batchv1.Job, srcPod PodRef) (*batchv1.Job, error) {
	if destJob == nil || len(destJob.Spec.Template.Spec.Containers) == 0 {
		return nil, fmt.Errorf("dest job has no containers")
	}
	// Defense-in-depth: the ref is interpolated into a /bin/sh -c command
	// string, so both halves must pass the same DNS-1123 validation the
	// dest side applies when it later parses and URL-escapes the value.
	if !migration.ValidateDNSLabel(srcPod.Namespace) {
		return nil, fmt.Errorf("invalid source pod namespace %q: must be a DNS-1123 label", srcPod.Namespace)
	}
	if !migration.ValidateDNSSubdomain(srcPod.Name) {
		return nil, fmt.Errorf("invalid source pod name %q: must be a DNS-1123 subdomain", srcPod.Name)
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
		cs[i].Command[last] += fmt.Sprintf(" --replay-cmdline-from-pod %s/%s", srcPod.Namespace, srcPod.Name)
		return out, nil
	}
	return nil, fmt.Errorf("no katamaran container in dest job")
}

// send is the only supported way to publish a StatusUpdate. It takes
// sendMu for reading so closeUpdates cannot close run.updates underneath an
// in-flight send, and it aborts as soon as run.finished closes. Every
// producer (Apply, poll, tailProgress, stageThenStartDest) must go through
// it: a bare `run.updates <-` is not covered by the sendMu protocol, so a
// producer that blocked on a full buffer could keep poll from ever reaching
// its closeUpdates defer, pinning both goroutines and the inflight entry.
func (run *nativeRun) send(u StatusUpdate) {
	run.sendMu.RLock()
	defer run.sendMu.RUnlock()
	select {
	case <-run.finished:
		return
	default:
	}
	select {
	case <-run.finished:
	case run.updates <- u:
	}
}

func (run *nativeRun) closeUpdates() {
	run.closeOnce.Do(func() {
		close(run.finished)
		run.sendMu.Lock()
		defer run.sendMu.Unlock()
		close(run.updates)
	})
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
	ticker := time.NewTicker(jobPollInterval)
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
				// Join so the operator can inspect both causes; lastListErr is
				// the only evidence of why the pod list kept failing.
				return "", fmt.Errorf("waiting for %s of job %s: %w", desc, jobName, errors.Join(deadline.Err(), lastListErr))
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
	patched, err := injectReplayFromPod(destJob, PodRef{Namespace: n.namespace, Name: srcPod})
	if err != nil {
		return false, fmt.Errorf("inject replay flag: %w", err)
	}
	if created, err := n.createDestJobIfAbsent(ctx, patched); err != nil {
		return false, err
	} else if !created {
		return false, nil
	}
	slog.Info("Resume: destination job created", "migration_id", id, "source_pod", srcPod, "dest_job", destJob.Name, "namespace", n.namespace)
	return true, nil
}

func (n *native) Stop(ctx context.Context, id MigrationID) error {
	if id == "" {
		return ErrUnknownID
	}
	n.mu.Lock()
	run := n.inflight[id]
	n.mu.Unlock()
	if run != nil {
		defer run.cancel()
	}
	prop := metav1.DeletePropagationBackground
	delOpts := metav1.DeleteOptions{PropagationPolicy: &prop}
	var errs []error
	for _, name := range []string{SourceJobName(id), DestJobName(id)} {
		if err := n.client.BatchV1().Jobs(n.namespace).Delete(ctx, name, delOpts); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete job %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// poll watches BOTH the source and destination Job statuses and emits
// StatusUpdate events. The migration is reported successful when the dest
// Job reaches Complete: the dest Job receives RAM, fires QEMU's RESUME
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
		run.closeUpdates()
		n.mu.Lock()
		delete(n.inflight, id)
		n.mu.Unlock()
	}()
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("poll panic", "migration_id", id, "panic", rec, "stack", string(debug.Stack()))
		}
	}()
	const sourceFailGrace = 90 * time.Second // how long to wait for dest after source dies
	ticker := time.NewTicker(jobPollInterval)
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
			run.send(StatusUpdate{ID: id, Phase: PhaseFailed, When: time.Now(), Error: ctx.Err()})
			return
		case <-ticker.C:
			srcJob, srcErr := n.client.BatchV1().Jobs(n.namespace).Get(ctx, run.srcJob, cacheRead)
			destJob, destErr := n.client.BatchV1().Jobs(n.namespace).Get(ctx, run.destJob, cacheRead)

			// Dest=Complete → success, even if source failed.
			if destErr == nil {
				destStatusErrors = 0
				if cond, ok := LatestTerminalJobCondition(destJob); ok && cond.Type == batchv1.JobComplete {
					slog.Info("Migration destination job completed", "migration_id", id, "source_job", run.srcJob, "dest_job", run.destJob)
					run.send(n.succeededUpdate(ctx, id, run))
					return
				} else if ok && cond.Type == batchv1.JobFailed {
					attrs := []any{"migration_id", id, "source_job", run.srcJob, "dest_job", run.destJob}
					attrs = append(attrs, jobConditionAttrs(cond)...)
					slog.Error("Migration destination job failed", attrs...)
					run.send(StatusUpdate{ID: id, Phase: PhaseFailed, When: time.Now(), Error: jobFailedError("dest job failed", cond)})
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
					run.send(StatusUpdate{ID: id, Phase: PhaseFailed, When: time.Now(), Error: errors.New("source job disappeared")})
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
					run.send(StatusUpdate{ID: id, Phase: PhaseFailed, When: time.Now(), Error: jobFailedError("source job failed and dest did not complete within grace window", srcCond)})
					return
				}
				continue // give dest time to land RESUME and exit 0
			}
			if !announcedTransferring && (srcJob.Status.Active > 0 || (srcJob.Status.Ready != nil && *srcJob.Status.Ready > 0)) {
				run.send(StatusUpdate{ID: id, Phase: PhaseTransferring, When: time.Now()})
				announcedTransferring = true
			}
		}
	}
}
