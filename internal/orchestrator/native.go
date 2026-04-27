package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
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
//   - Cmdline-replay shipment between source and dest pods (the cmdline
//     stager pod + kubectl-cp dance from migrate.sh). When ReplayCmdline
//     is true, Native returns ErrReplayCmdlineNotSupported.
//   - Granular RAM-transfer progress (no log scraping yet).
//   - Per-pod log streaming for the dashboard log pane.
//
// Use NewNative for the in-cluster path and NewNativeFromClient for tests.
type Native struct {
	client    kubernetes.Interface
	config    *rest.Config // optional; required only for ReplayCmdline mode (SPDY exec)
	namespace string

	mu       sync.Mutex
	inflight map[MigrationID]*nativeRun
}

type nativeRun struct {
	srcJob   string
	destJob  string
	updates  chan StatusUpdate
	cancel   context.CancelFunc
	finished chan struct{}
}

// ErrReplayCmdlineNotSupported is returned by Native.Apply when the request
// has ReplayCmdline=true but the Native orchestrator was constructed without
// a rest.Config (e.g. via NewNativeFromClient in tests). Use NewNative for
// in-cluster ReplayCmdline support.
var ErrReplayCmdlineNotSupported = errors.New("Native ReplayCmdline requires a rest.Config (use NewNative, not NewNativeFromClient)")

// NewNative builds a Native orchestrator using the in-cluster service
// account. Job manifests are submitted into kube-system (matching the
// existing migrate.sh layout). The returned Native supports ReplayCmdline
// because it has a rest.Config for SPDY remote-command calls.
func NewNative() (*Native, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("clientset: %w", err)
	}
	n := NewNativeFromClient(cs)
	n.config = cfg
	return n, nil
}

// NewNativeFromClient is the test-friendly constructor. The returned Native
// does NOT support ReplayCmdline (no rest.Config for SPDY) — set .config
// manually if needed.
func NewNativeFromClient(c kubernetes.Interface) *Native {
	return &Native{
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
func (n *Native) Apply(ctx context.Context, req Request) (MigrationID, error) {
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
	return id, nil
}

// stageThenStartDest runs the cmdline-staging flow for ReplayCmdline mode:
// finds the source pod, copies the cmdline off it, stages on the dest node,
// then submits the dest Job. Failures abort the run with PhaseFailed.
func (n *Native) stageThenStartDest(ctx context.Context, id MigrationID, run *nativeRun, req Request, destJob *batchv1.Job) {
	srcPod, err := n.firstSourcePod(ctx, run.srcJob)
	if err != nil {
		run.updates <- StatusUpdate{ID: id, Phase: PhaseFailed, When: time.Now(), Error: fmt.Errorf("locate source pod: %w", err)}
		run.cancel()
		return
	}
	if _, err := n.stageCmdline(ctx, id, srcPod, n.namespace, req.DestNode); err != nil {
		run.updates <- StatusUpdate{ID: id, Phase: PhaseFailed, When: time.Now(), Error: fmt.Errorf("stage cmdline: %w", err)}
		run.cancel()
		return
	}
	if _, err := n.client.BatchV1().Jobs(n.namespace).Create(ctx, destJob, metav1.CreateOptions{}); err != nil {
		run.updates <- StatusUpdate{ID: id, Phase: PhaseFailed, When: time.Now(), Error: fmt.Errorf("create dest job: %w", err)}
		run.cancel()
		return
	}
}

// firstSourcePod waits up to 60s for the source Job's pod to be created
// and returns its name. We need the pod (not the Job) to read logs from.
func (n *Native) firstSourcePod(ctx context.Context, jobName string) (string, error) {
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
func (n *Native) Watch(_ context.Context, id MigrationID) (<-chan StatusUpdate, error) {
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
func (n *Native) Stop(ctx context.Context, id MigrationID) error {
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

// poll watches the source Job's status until it terminates, emitting
// StatusUpdate events. The dest Job's lifecycle is incidental — we only
// care about source completion since that signals migration success.
func (n *Native) poll(ctx context.Context, id MigrationID, run *nativeRun) {
	defer func() {
		close(run.updates)
		close(run.finished)
		n.mu.Lock()
		delete(n.inflight, id)
		n.mu.Unlock()
	}()
	const interval = 2 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	announcedTransferring := false
	for {
		select {
		case <-ctx.Done():
			run.updates <- StatusUpdate{ID: id, Phase: PhaseFailed, When: time.Now(), Error: ctx.Err()}
			return
		case <-ticker.C:
			job, err := n.client.BatchV1().Jobs(n.namespace).Get(ctx, run.srcJob, metav1.GetOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) {
					run.updates <- StatusUpdate{ID: id, Phase: PhaseFailed, When: time.Now(), Error: errors.New("source job disappeared")}
					return
				}
				continue // transient — retry on next tick
			}
			cond := jobCondition(job)
			switch cond {
			case batchv1.JobComplete:
				run.updates <- StatusUpdate{ID: id, Phase: PhaseSucceeded, When: time.Now()}
				return
			case batchv1.JobFailed:
				run.updates <- StatusUpdate{ID: id, Phase: PhaseFailed, When: time.Now(), Error: errors.New("source job failed")}
				return
			}
			if !announcedTransferring && (job.Status.Active > 0 || job.Status.Ready != nil && *job.Status.Ready > 0) {
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
	if req.MultifdChannels > 0 {
		args = append(args, "--multifd-channels", strconv.Itoa(req.MultifdChannels))
	}
	if req.LogLevel != "" {
		args = append(args, "--log-level", req.LogLevel)
	}
	if req.LogFormat != "" {
		args = append(args, "--log-format", req.LogFormat)
	}
	return strings.Join(args, " ")
}
