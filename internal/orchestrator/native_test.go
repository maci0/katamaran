package orchestrator

import (
	"context"
	"maps"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func validRequest() Request {
	return Request{
		SourceNode: "n1",
		DestNode:   "n2",
		DestIP:     "10.0.0.20",
		Image:      "katamaran:dev",
		SourcePod:  &PodRef{Namespace: "default", Name: "vm-a"},
	}
}

// Legacy mode (SourceQMP + VMIP, no SourcePod) must render a source command
// carrying --vm-ip, otherwise the source binary exits 2 ("source mode requires
// either (--vm-ip ...) or (--pod-name ...)"). A SourceQMP override is forwarded
// as --qmp. Neither flag may leak into the dest command (the dest CLI takes its
// socket from --qmp built into the template and rejects an unexpected --vm-ip).
func TestNative_Apply_LegacyModeSourceCarriesVMIP(t *testing.T) {
	t.Parallel()
	cs := fake.NewSimpleClientset()
	n := newFromClient(cs)
	req := Request{
		SourceNode: "n1",
		DestNode:   "n2",
		DestIP:     "10.0.0.20",
		Image:      "katamaran:dev",
		SourceQMP:  "/run/vc/vm/custom/extra-monitor.sock",
		VMIP:       "10.244.1.5",
	}
	if _, err := n.Apply(context.Background(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	jobs, err := cs.BatchV1().Jobs("kube-system").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	var src, dest batchv1.Job
	for _, j := range jobs.Items {
		switch j.Labels["app.kubernetes.io/component"] {
		case "source":
			src = j
		case "dest":
			dest = j
		}
	}
	sourceCmd := jobCommand(t, src)
	for _, want := range []string{"--mode source", "--vm-ip 10.244.1.5", "--qmp /run/vc/vm/custom/extra-monitor.sock"} {
		if !strings.Contains(sourceCmd, want) {
			t.Fatalf("source command missing %q: %s", want, sourceCmd)
		}
	}
	destCmd := jobCommand(t, dest)
	if strings.Contains(destCmd, "--vm-ip") {
		t.Fatalf("dest command must not carry --vm-ip: %s", destCmd)
	}
}

func TestInjectReplayFromPod_AppendsFlag(t *testing.T) {
	t.Parallel()
	job := &batchv1.Job{
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    "katamaran",
						Command: []string{"/bin/sh", "-c", "/usr/local/bin/katamaran --mode dest --qmp \"/run/vc/vm/x/qmp.sock\""},
					}},
				},
			},
		},
	}
	patched, err := injectReplayFromPod(job, "default", "vm-a-pod-xyz")
	if err != nil {
		t.Fatalf("injectReplayFromPod: %v", err)
	}
	got := patched.Spec.Template.Spec.Containers[0].Command
	if !strings.Contains(got[len(got)-1], "--replay-cmdline-from-pod default/vm-a-pod-xyz") {
		t.Fatalf("flag not appended: %q", got[len(got)-1])
	}
	if &patched.Spec.Template.Spec.Containers[0] == &job.Spec.Template.Spec.Containers[0] {
		t.Fatal("expected DeepCopy, got same container address")
	}
}

func TestInjectReplayFromPod_NoKatamaranContainer(t *testing.T) {
	t.Parallel()
	job := &batchv1.Job{
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "other", Command: []string{"x"}}},
				},
			},
		},
	}
	if _, err := injectReplayFromPod(job, "default", "vm-a-pod"); err == nil {
		t.Fatal("expected error when no katamaran container exists")
	}
}

// The ref is interpolated into a /bin/sh -c command string, so both halves
// must pass DNS-1123 validation before any command text is touched.
func TestInjectReplayFromPod_RejectsInvalidRef(t *testing.T) {
	t.Parallel()
	newJob := func() *batchv1.Job {
		return &batchv1.Job{
			Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:    "katamaran",
							Command: []string{"/bin/sh", "-c", "/usr/local/bin/katamaran --mode dest"},
						}},
					},
				},
			},
		}
	}
	tests := []struct {
		name    string
		ns      string
		pod     string
		wantErr string
	}{
		{"NamespaceNotDNSLabel", "default/vm;id", "vm-a-pod", "invalid source pod namespace"},
		{"NamespaceEmpty", "", "vm-a-pod", "invalid source pod namespace"},
		{"PodNameWithMetachars", "default", "vm-a$(reboot)", "invalid source pod name"},
		{"PodNameNotSubdomain", "default", "VM-A-POD", "invalid source pod name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			job := newJob()
			got, err := injectReplayFromPod(job, tt.ns, tt.pod)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got: %v", tt.wantErr, err)
			}
			if got != nil {
				t.Fatal("job must not be returned on validation failure")
			}
			cmd := job.Spec.Template.Spec.Containers[0].Command
			if len(cmd) < 3 || strings.Contains(cmd[len(cmd)-1], "--replay-cmdline-from-pod") {
				t.Fatalf("command must be untouched on validation failure: %q", cmd)
			}
		})
	}
}

func TestNative_Apply_CreatesBothJobs(t *testing.T) {
	t.Parallel()
	cs := fake.NewSimpleClientset()
	n := newFromClient(cs)
	id, err := n.Apply(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if id == "" {
		t.Fatal("expected migration id")
	}
	jobs, err := cs.BatchV1().Jobs("kube-system").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 2 {
		t.Fatalf("expected exactly 2 jobs, got %d: %+v", len(jobs.Items), jobs.Items)
	}
	byComponent := map[string]batchv1.Job{}
	for _, j := range jobs.Items {
		component := j.Labels["app.kubernetes.io/component"]
		byComponent[component] = j
		if j.Labels[MigrationIDLabel] != string(id) {
			t.Fatalf("job %s migration label = %q, want %q", j.Name, j.Labels[MigrationIDLabel], id)
		}
	}
	src, hasSrc := byComponent["source"]
	dest, hasDest := byComponent["dest"]
	if !hasSrc || !hasDest {
		t.Fatalf("expected source and dest jobs, got components %v", byComponent)
	}
	if !strings.HasPrefix(src.Name, "katamaran-source-") || !strings.HasPrefix(dest.Name, "katamaran-dest-") {
		t.Fatalf("unexpected job names: source=%q dest=%q", src.Name, dest.Name)
	}
	if src.Spec.Template.Spec.NodeName != "n1" || dest.Spec.Template.Spec.NodeName != "n2" {
		t.Fatalf("jobs scheduled to wrong nodes: source=%q dest=%q", src.Spec.Template.Spec.NodeName, dest.Spec.Template.Spec.NodeName)
	}

	sourceCmd := jobCommand(t, src)
	for _, want := range []string{
		"--mode source",
		"--dest-ip \"10.0.0.20\"",
		"--pod-name vm-a",
		"--pod-namespace default",
		"--multifd-channels 0",
	} {
		if !strings.Contains(sourceCmd, want) {
			t.Fatalf("source command missing %q: %s", want, sourceCmd)
		}
	}
	destCmd := jobCommand(t, dest)
	for _, want := range []string{"--mode dest", "--multifd-channels 0"} {
		if !strings.Contains(destCmd, want) {
			t.Fatalf("dest command missing %q: %s", want, destCmd)
		}
	}
}

func TestNative_Apply_ReplayCmdlineStagesDestAfterSourcePodAppears(t *testing.T) {
	t.Parallel()
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		jobName, ok := jobNameFromPodListAction(action)
		if !ok || !strings.HasPrefix(jobName, "katamaran-source-") {
			return false, nil, nil
		}
		return true, &corev1.PodList{Items: []corev1.Pod{{
			ObjectMeta: metav1.ObjectMeta{
				Name:      jobName + "-pod",
				Namespace: DefaultJobNamespace,
				Labels:    map[string]string{"batch.kubernetes.io/job-name": jobName},
			},
		}}}, nil
	})
	n := newFromClient(cs)
	req := validRequest()
	req.ReplayCmdline = true

	id, err := n.Apply(context.Background(), req)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	t.Cleanup(func() { _ = n.Stop(context.Background(), id) })

	src, err := cs.BatchV1().Jobs(DefaultJobNamespace).Get(context.Background(), SourceJobName(id), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("source job not created: %v", err)
	}
	srcCmd := jobCommand(t, *src)
	if !strings.Contains(srcCmd, "--emit-cmdline-to "+cmdlinePathFor(id)) {
		t.Fatalf("source command missing replay capture flag: %s", srcCmd)
	}

	dest := waitForJob(t, cs, DestJobName(id))
	destCmd := jobCommand(t, *dest)
	wantReplayFlag := "--replay-cmdline-from-pod " + DefaultJobNamespace + "/" + SourceJobName(id) + "-pod"
	if !strings.Contains(destCmd, wantReplayFlag) {
		t.Fatalf("dest command missing %q: %s", wantReplayFlag, destCmd)
	}
}

func TestNative_Apply_AutoSelectDestNodeCreatesSourceWithResolvedDestIP(t *testing.T) {
	t.Parallel()
	cs := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-b"},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{
			Type:    corev1.NodeInternalIP,
			Address: "10.0.0.30",
		}}},
	})
	cs.PrependReactor("list", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		jobName, ok := jobNameFromPodListAction(action)
		if !ok || !strings.HasPrefix(jobName, "katamaran-dest-") {
			return false, nil, nil
		}
		return true, &corev1.PodList{Items: []corev1.Pod{{
			ObjectMeta: metav1.ObjectMeta{
				Name:      jobName + "-pod",
				Namespace: DefaultJobNamespace,
				Labels:    map[string]string{"batch.kubernetes.io/job-name": jobName},
			},
			Spec: corev1.PodSpec{NodeName: "worker-b"},
		}}}, nil
	})
	n := newFromClient(cs)
	req := validRequest()
	req.SourceNode = "worker-a"
	req.DestNode = ""
	req.DestIP = ""
	req.DestNodeSelector = map[string]string{"katamaran.io/enabled": "true"}
	req.DestTolerations = []corev1.Toleration{{
		Key:      "katamaran",
		Operator: corev1.TolerationOpExists,
		Effect:   corev1.TaintEffectNoSchedule,
	}}

	id, err := n.Apply(context.Background(), req)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	t.Cleanup(func() { _ = n.Stop(context.Background(), id) })

	dest, err := cs.BatchV1().Jobs(DefaultJobNamespace).Get(context.Background(), DestJobName(id), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("dest job not created: %v", err)
	}
	if dest.Spec.Template.Spec.NodeName != "" {
		t.Fatalf("auto-selected dest job NodeName = %q, want empty", dest.Spec.Template.Spec.NodeName)
	}
	if got := dest.Spec.Template.Spec.NodeSelector["katamaran.io/enabled"]; got != "true" {
		t.Fatalf("dest node selector = %v, want katamaran.io/enabled=true", dest.Spec.Template.Spec.NodeSelector)
	}
	if len(dest.Spec.Template.Spec.Tolerations) != 1 || dest.Spec.Template.Spec.Tolerations[0].Key != "katamaran" {
		t.Fatalf("dest tolerations = %+v, want copied request toleration", dest.Spec.Template.Spec.Tolerations)
	}
	if dest.Spec.Template.Spec.Affinity == nil || dest.Spec.Template.Spec.Affinity.NodeAffinity == nil {
		t.Fatalf("dest job missing source-node anti-affinity")
	}

	src, err := cs.BatchV1().Jobs(DefaultJobNamespace).Get(context.Background(), SourceJobName(id), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("source job not created after dest scheduling: %v", err)
	}
	if src.Spec.Template.Spec.NodeName != "worker-a" {
		t.Fatalf("source job NodeName = %q, want worker-a", src.Spec.Template.Spec.NodeName)
	}
	srcCmd := jobCommand(t, *src)
	if !strings.Contains(srcCmd, "--dest-ip \"10.0.0.30\"") {
		t.Fatalf("source command missing resolved dest IP: %s", srcCmd)
	}
}

// TestNative_Apply_AutoSelectSameNodeRejectedCleansUpDestJob locks the
// auto-select safety guard: if Kubernetes schedules the dest pod onto the
// source node, Apply must fail loudly (migrating a VM to its own node would
// corrupt it) AND delete the already-created dest Job via cleanupDestJob,
// leaving no orphan Jobs behind.
func TestNative_Apply_AutoSelectSameNodeRejectedCleansUpDestJob(t *testing.T) {
	t.Parallel()
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		jobName, ok := jobNameFromPodListAction(action)
		if !ok || !strings.HasPrefix(jobName, "katamaran-dest-") {
			return false, nil, nil
		}
		return true, &corev1.PodList{Items: []corev1.Pod{{
			ObjectMeta: metav1.ObjectMeta{
				Name:      jobName + "-pod",
				Namespace: DefaultJobNamespace,
				Labels:    map[string]string{"batch.kubernetes.io/job-name": jobName},
			},
			Spec: corev1.PodSpec{NodeName: "worker-a"}, // same as req.SourceNode
		}}}, nil
	})
	n := newFromClient(cs)
	req := validRequest()
	req.SourceNode = "worker-a"
	req.DestNode = ""
	req.DestIP = ""

	id, err := n.Apply(context.Background(), req)
	if err == nil {
		t.Fatalf("Apply with dest pod on source node must fail; got id %q", id)
	}
	if !strings.Contains(err.Error(), "same node") {
		t.Fatalf("error should mention same-node scheduling, got: %v", err)
	}
	jobs, err := cs.BatchV1().Jobs(DefaultJobNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("rejected migration left %d Job(s) behind: %+v", len(jobs.Items), jobs.Items)
	}
}

// TestNative_Apply_CancelDuringSchedulingWaitCleansUpDestJob locks the
// compensation contract behind cleanupDestJob: a Migration CR deleted while
// Apply is waiting for dest scheduling has no status.migrationID yet, so the
// controller's Stop path cannot reach its Jobs. The compensating dest-Job
// delete must therefore survive the very cancellation that aborted Apply —
// issued on a cancellation-proof context (see cleanupContext) — instead of
// failing instantly and orphaning the Job in the cluster until its
// activeDeadlineSeconds expires.
func TestNative_Apply_CancelDuringSchedulingWaitCleansUpDestJob(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs := fake.NewSimpleClientset()
	// Cancellation lands exactly while Apply is inside the scheduling wait;
	// no dest pod ever appears, so waitForJobPod exits via ctx.Done().
	cs.PrependReactor("list", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		cancel()
		return true, &corev1.PodList{}, nil
	})
	n := newFromClient(cs)
	req := validRequest()
	req.SourceNode = "worker-a"
	req.DestNode = ""
	req.DestIP = ""

	if _, err := n.Apply(ctx, req); err == nil {
		t.Fatal("Apply must fail once the context is cancelled during the scheduling wait")
	}
	jobs, err := cs.BatchV1().Jobs(DefaultJobNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("cancelled migration left %d Job(s) behind: %+v", len(jobs.Items), jobs.Items)
	}
}

// TestCleanupContext_IgnoresParentCancellation pins the property the dest-Job
// compensation relies on: the derived context keeps working (fresh deadline,
// no inherited cancellation) even when the caller's context is already dead.
func TestCleanupContext_IgnoresParentCancellation(t *testing.T) {
	t.Parallel()
	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()
	cctx, cancel := cleanupContext(parent)
	defer cancel()
	if err := cctx.Err(); err != nil {
		t.Fatalf("cleanup context must ignore caller cancellation, got %v", err)
	}
	deadline, ok := cctx.Deadline()
	if !ok {
		t.Fatal("cleanup context must carry a bounded deadline")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > cleanupJobTimeout {
		t.Fatalf("cleanup context remaining budget = %v, want (0,%s]", remaining, cleanupJobTimeout)
	}
}

// TestNative_Apply_EmitsStartingPhases_LegacyMode: legacy mode creates the
// dest Job first (migrate-incoming listener before source connects), then the
// source Job. The update stream must surface that staging order as
// submitted → dest-starting → src-starting so CR / dashboard consumers see
// the lifecycle the type system and CRD schema document.
func TestNative_Apply_EmitsStartingPhases_LegacyMode(t *testing.T) {
	t.Parallel()
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("get", "jobs", func(action clienttesting.Action) (bool, runtime.Object, error) {
		ga, ok := action.(clienttesting.GetAction)
		if !ok {
			return false, nil, nil
		}
		name := ga.GetName()
		if !strings.HasPrefix(name, "katamaran-source-") && !strings.HasPrefix(name, "katamaran-dest-") {
			return false, nil, nil
		}
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system"},
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: "True"}},
			},
		}, nil
	})

	n := newFromClient(cs)
	id, err := n.Apply(context.Background(), validRequest())
	if err != nil {
		t.Fatal(err)
	}
	// Pre-create the source pod so succeededUpdate's synchronous
	// KATAMARAN_RESULT scrape finds it instantly instead of burning its
	// 5s bounded-scrape window (mirrors TestNative_Watch_TerminalSucceeded).
	srcJob := "katamaran-source-" + string(id)
	if _, err := cs.CoreV1().Pods(DefaultJobNamespace).Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      srcJob + "-pod",
			Namespace: DefaultJobNamespace,
			Labels:    map[string]string{"batch.kubernetes.io/job-name": srcJob},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create source pod for log scrape fallback: %v", err)
	}
	updates, err := n.Watch(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	got := drainUpdates(updates, 10*time.Second)
	wantPrefix := []StatusPhase{PhaseSubmitted, PhaseDestStarting, PhaseSrcStarting}
	if len(got) < len(wantPrefix)+1 {
		t.Fatalf("want >=%d updates, got %d: %+v", len(wantPrefix)+1, len(got), got)
	}
	for i, want := range wantPrefix {
		if got[i].Phase != want {
			t.Fatalf("update[%d].phase = %s, want %s (all: %+v)", i, got[i].Phase, want, got)
		}
	}
	if last := got[len(got)-1]; last.Phase != PhaseSucceeded {
		t.Fatalf("last phase = %s, want %s", last.Phase, PhaseSucceeded)
	}
}

// TestStageThenStartDest_EmitsDestStarting drives the ReplayCmdline staging
// goroutine body synchronously and asserts it emits exactly one
// PhaseDestStarting once the dest Job is created.
func TestStageThenStartDest_EmitsDestStarting(t *testing.T) {
	t.Parallel()
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		jobName, ok := jobNameFromPodListAction(action)
		if !ok || !strings.HasPrefix(jobName, "katamaran-source-") {
			return false, nil, nil
		}
		return true, &corev1.PodList{Items: []corev1.Pod{{
			ObjectMeta: metav1.ObjectMeta{
				Name:      jobName + "-pod",
				Namespace: DefaultJobNamespace,
				Labels:    map[string]string{"batch.kubernetes.io/job-name": jobName},
			},
		}}}, nil
	})
	n := newFromClient(cs)
	id := MigrationID("replayid")
	req := validRequest()
	req.ReplayCmdline = true
	destJob, err := renderDestJob(req, id, buildExtraArgs(req))
	if err != nil {
		t.Fatalf("renderDestJob: %v", err)
	}
	run := &nativeRun{
		srcJob:   SourceJobName(id),
		destJob:  DestJobName(id),
		updates:  make(chan StatusUpdate, 4),
		cancel:   func() {},
		finished: make(chan struct{}),
	}
	n.stageThenStartDest(context.Background(), id, run, destJob)
	select {
	case u := <-run.updates:
		if u.Phase != PhaseDestStarting {
			t.Fatalf("phase = %s, want %s", u.Phase, PhaseDestStarting)
		}
	default:
		t.Fatal("stageThenStartDest emitted no status update")
	}
	if _, err := cs.BatchV1().Jobs(DefaultJobNamespace).Get(context.Background(), DestJobName(id), metav1.GetOptions{}); err != nil {
		t.Fatalf("dest job not created: %v", err)
	}
}

// TestPhaseFromMarker covers KATAMARAN_PHASE validation. Only non-terminal
// lifecycle phases may be surfaced from a log marker; terminal outcomes are
// owned by the Job-condition poll's outcome matrix.
func TestPhaseFromMarker(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in    string
		want  StatusPhase
		wantK bool
	}{
		{in: "cutover", want: PhaseCutover, wantK: true},
		{in: "transferring", want: PhaseTransferring, wantK: true},
		{in: "succeeded", wantK: false},
		{in: "failed", wantK: false},
		{in: "submitted", wantK: false},
		{in: "", wantK: false},
		{in: "bogus", wantK: false},
	}
	for _, tc := range cases {
		gotPhase, ok := phaseFromMarker(tc.in)
		if ok != tc.wantK || gotPhase != tc.want {
			t.Errorf("phaseFromMarker(%q) = (%q, %v), want (%q, %v)", tc.in, gotPhase, ok, tc.want, tc.wantK)
		}
	}
}

func TestNative_Watch_TerminalSucceeded(t *testing.T) {
	t.Parallel()
	cs := fake.NewSimpleClientset()
	// React to Get on either katamaran-{source,dest} by returning a Job
	// whose conditions include JobComplete=True. fake.Clientset doesn't
	// auto-update Job status from controller activity — we have to
	// fabricate it. The Native.poll loop reports PhaseSucceeded as soon as
	// the DEST Job is Complete (regardless of source's exit status).
	cs.PrependReactor("get", "jobs", func(action clienttesting.Action) (bool, runtime.Object, error) {
		ga, ok := action.(clienttesting.GetAction)
		if !ok {
			return false, nil, nil
		}
		name := ga.GetName()
		if !strings.HasPrefix(name, "katamaran-source-") && !strings.HasPrefix(name, "katamaran-dest-") {
			return false, nil, nil
		}
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system"},
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobComplete, Status: "True"},
				},
			},
		}, nil
	})

	n := newFromClient(cs)
	id, err := n.Apply(context.Background(), validRequest())
	if err != nil {
		t.Fatal(err)
	}
	srcJob := "katamaran-source-" + string(id)
	if _, err := cs.CoreV1().Pods(DefaultJobNamespace).Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      srcJob + "-pod",
			Namespace: DefaultJobNamespace,
			Labels:    map[string]string{"batch.kubernetes.io/job-name": srcJob},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create source pod for log scrape fallback: %v", err)
	}
	updates, err := n.Watch(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	got := drainUpdates(updates, 10*time.Second)
	if len(got) < 2 {
		t.Fatalf("want >=2 updates, got %d: %+v", len(got), got)
	}
	if got[0].Phase != PhaseSubmitted {
		t.Errorf("first phase = %s, want %s", got[0].Phase, PhaseSubmitted)
	}
	if got[len(got)-1].Phase != PhaseSucceeded {
		t.Errorf("last phase = %s, want %s", got[len(got)-1].Phase, PhaseSucceeded)
	}
}

func TestNative_Watch_DestFailedIncludesConditionDetails(t *testing.T) {
	t.Parallel()
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("get", "jobs", func(action clienttesting.Action) (bool, runtime.Object, error) {
		ga, ok := action.(clienttesting.GetAction)
		if !ok {
			return false, nil, nil
		}
		name := ga.GetName()
		switch {
		case strings.HasPrefix(name, "katamaran-dest-"):
			return true, &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system"},
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{{
						Type:    batchv1.JobFailed,
						Status:  corev1.ConditionTrue,
						Reason:  "BackoffLimitExceeded",
						Message: "pod crashed before opening migration listener",
					}},
				},
			}, nil
		case strings.HasPrefix(name, "katamaran-source-"):
			return true, &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system"},
				Status:     batchv1.JobStatus{Active: 1},
			}, nil
		default:
			return false, nil, nil
		}
	})

	n := newFromClient(cs)
	id, err := n.Apply(context.Background(), validRequest())
	if err != nil {
		t.Fatal(err)
	}
	updates, err := n.Watch(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	got := drainUpdates(updates, 10*time.Second)
	if len(got) < 2 {
		t.Fatalf("want >=2 updates, got %d: %+v", len(got), got)
	}
	last := got[len(got)-1]
	if last.Phase != PhaseFailed {
		t.Fatalf("last phase = %s, want %s", last.Phase, PhaseFailed)
	}
	if last.Error == nil {
		t.Fatal("failed update missing error")
	}
	errText := last.Error.Error()
	for _, want := range []string{"BackoffLimitExceeded", "pod crashed before opening migration listener"} {
		if !strings.Contains(errText, want) {
			t.Fatalf("failed update error %q missing %q", errText, want)
		}
	}
}

func TestNative_Stop_DeletesJobs(t *testing.T) {
	t.Parallel()
	cs := fake.NewSimpleClientset()
	n := newFromClient(cs)
	id, err := n.Apply(context.Background(), validRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Stop(context.Background(), id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	jobs, err := cs.BatchV1().Jobs("kube-system").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("expected jobs deleted, got %v", jobs.Items)
	}
}

func TestExpandShellVars(t *testing.T) {
	t.Parallel()
	got := expandShellVars("hello ${A} ${B}!", map[string]string{"A": "world", "B": "x"})
	if got != "hello world x!" {
		t.Fatalf("got %q", got)
	}
	if expandShellVars("${unknown}", nil) != "" {
		t.Fatal("expected unknown var to expand to empty")
	}
}

// TestExpandShellVars_NoBareNameExpansion pins the documented boundary
// (see native_jobs.go): only ${NAME} is expanded. A bare $NAME must pass
// through literally, and an unclosed ${ must too — rendered commands are
// shell-evaluated, so silent mangling of either would change every job.
func TestExpandShellVars_NoBareNameExpansion(t *testing.T) {
	t.Parallel()
	vars := map[string]string{"NODE_NAME": "worker-a"}
	tests := []struct {
		in   string
		want string
	}{
		{"$NODE_NAME", "$NODE_NAME"},
		{"node=${NODE_NAME}", "node=worker-a"},
		{"unclosed ${A", "unclosed ${A"},
		{"${A", "${A"},
		{"$", "$"},
	}
	for _, tt := range tests {
		if got := expandShellVars(tt.in, vars); got != tt.want {
			t.Errorf("expandShellVars(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func jobCommand(t *testing.T, job batchv1.Job) string {
	t.Helper()
	if len(job.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("job %s containers = %d, want 1", job.Name, len(job.Spec.Template.Spec.Containers))
	}
	return strings.Join(job.Spec.Template.Spec.Containers[0].Command, " ")
}

func jobNameFromPodListAction(action clienttesting.Action) (string, bool) {
	la, ok := action.(clienttesting.ListAction)
	if !ok {
		return "", false
	}
	return strings.CutPrefix(la.GetListRestrictions().Labels.String(), "batch.kubernetes.io/job-name=")
}

func waitForJob(t *testing.T, cs *fake.Clientset, name string) *batchv1.Job {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		job, err := cs.BatchV1().Jobs(DefaultJobNamespace).Get(context.Background(), name, metav1.GetOptions{})
		if err == nil {
			return job
		}
		lastErr = err
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s was not created within timeout: %v", name, lastErr)
	return nil
}

// TestSucceededUpdate_RecoversFromTailRace covers the race the
// scrapeResultMarker helper was added to fix: tailProgress polls every
// 2s but poll fires PhaseSucceeded as soon as the dest Job reaches
// Complete. If those land between tailProgress ticks, the
// KATAMARAN_RESULT marker is in the source pod log but never copied
// onto run.* — the terminal StatusUpdate would otherwise carry zeros.
func TestSucceededUpdate_RecoversFromTailRace(t *testing.T) {
	t.Parallel()
	cs := fake.NewSimpleClientset(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "katamaran-source-id1-pod",
				Namespace: "kube-system",
				Labels:    map[string]string{"batch.kubernetes.io/job-name": "katamaran-source-id1"},
			},
			Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
		},
	)
	// fake.Clientset doesn't implement GetLogs; ReactionChain returning
	// a *rest.Request is more involved than this test needs. Instead,
	// stub n.scrapeResultMarker by exercising it through a custom
	// fake-pod-log helper. Bypass the real GetLogs by setting up
	// resultCaptured pre-emptively, then assert succeededUpdate copies
	// the values onto the StatusUpdate.
	n := newFromClient(cs)
	run := &nativeRun{
		srcJob:               "katamaran-source-id1",
		destJob:              "katamaran-dest-id1",
		updates:              make(chan StatusUpdate, 4),
		finished:             make(chan struct{}),
		resultCaptured:       true,
		resultDowntime:       42,
		resultRAMTransferred: 111,
		resultRAMTotal:       222,
		downtimeCaptured:     true,
		appliedDowntime:      25,
		rttMS:                3,
		autoDowntime:         true,
	}
	u := n.succeededUpdate(context.Background(), MigrationID("id1"), run)
	if u.Phase != PhaseSucceeded {
		t.Fatalf("phase = %s, want %s", u.Phase, PhaseSucceeded)
	}
	if u.DowntimeMS != 42 || u.RAMTransferred != 111 || u.RAMTotal != 222 {
		t.Errorf("captured result not threaded: %+v", u)
	}
	if u.AppliedDowntimeMS != 25 || u.RTTMS != 3 || !u.AutoDowntime {
		t.Errorf("captured downtime-limit not threaded: %+v", u)
	}
}

// TestSucceededUpdate_NoCaptureFallsBackCleanly: when neither
// tailProgress nor the synchronous scrape filled run.result*, the
// terminal update should still ship as PhaseSucceeded with zero values
// rather than blocking or panicking.
func TestSucceededUpdate_NoCaptureFallsBackCleanly(t *testing.T) {
	t.Parallel()
	cs := fake.NewSimpleClientset()
	n := newFromClient(cs)
	run := &nativeRun{
		srcJob:   "katamaran-source-empty",
		destJob:  "katamaran-dest-empty",
		updates:  make(chan StatusUpdate, 4),
		finished: make(chan struct{}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	u := n.succeededUpdate(ctx, MigrationID("empty"), run)
	if u.Phase != PhaseSucceeded {
		t.Fatalf("phase = %s, want %s", u.Phase, PhaseSucceeded)
	}
	if u.DowntimeMS != 0 || u.RAMTransferred != 0 || u.RAMTotal != 0 {
		t.Errorf("expected zero result fields when no capture; got %+v", u)
	}
}

// TestParseProgressFields covers the marker-parsing helper that backs
// every KATAMARAN_* scrape. Reordering or extra whitespace must not
// shift values.
func TestParseProgressFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want map[string]string
	}{
		{
			in:   "status=completed ram_transferred=100 ram_total=200 ram_remaining=0",
			want: map[string]string{"status": "completed", "ram_transferred": "100", "ram_total": "200", "ram_remaining": "0"},
		},
		{
			in:   "downtime_ms=18 total_time_ms=1234 ram_transferred=900 ram_total=2000",
			want: map[string]string{"downtime_ms": "18", "total_time_ms": "1234", "ram_transferred": "900", "ram_total": "2000"},
		},
		{
			in:   "applied_ms=25 rtt_ms=0 auto=true",
			want: map[string]string{"applied_ms": "25", "rtt_ms": "0", "auto": "true"},
		},
		{
			in:   "  multiple   spaces=ok ",
			want: map[string]string{"spaces": "ok"},
		},
		{
			in:   "message=value=with=equals status=running",
			want: map[string]string{"message": "value=with=equals", "status": "running"},
		},
	}
	for _, tc := range cases {
		got := parseProgressFields(tc.in)
		if !maps.Equal(got, tc.want) {
			t.Errorf("parseProgressFields(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestParseInt64_RejectsMalformedAndOutOfRange pins that marker integer
// fields decode to 0 unless they are exact int64 values. strconv.ParseInt
// clamps out-of-range input to MaxInt64/MinInt64 alongside its error; that
// clamp must never reach consumers (persisted CR status, progress math),
// where e.g. ram_transferred=99999999999999999999 previously surfaced as
// math.MaxInt64 bytes.
func TestParseInt64_RejectsMalformedAndOutOfRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"0", 0},
		{"42", 42},
		{"-7", -7},
		{"abc", 0},
		{"12abc", 0},
		{" 12", 0},
		{"9223372036854775807", 9223372036854775807},
		{"9223372036854775808", 0},  // MaxInt64+1: ParseInt would clamp to MaxInt64
		{"-9223372036854775809", 0}, // MinInt64-1: ParseInt would clamp to MinInt64
		{"99999999999999999999", 0},
	}
	for _, tc := range cases {
		if got := parseInt64(tc.in); got != tc.want {
			t.Errorf("parseInt64(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestNativeRunSend_AfterClose: poll's defer can close run.updates
// before tailProgress tries to send. The run.send helper must absorb
// that race without panicking.
func TestNativeRunSend_AfterClose(t *testing.T) {
	t.Parallel()
	run := &nativeRun{
		updates:  make(chan StatusUpdate, 1),
		finished: make(chan struct{}),
	}
	// Mimic poll's defer order: signal finished, then close updates.
	close(run.finished)
	run.closeOnce.Do(func() { close(run.updates) })

	// Sending after the close must not panic.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("run.send panicked after close: %v", r)
		}
	}()
	run.send(StatusUpdate{Phase: PhaseTransferring})
}

// drainUpdates collects every value from c until it closes or deadline
// fires. Used by tests that exercise the full Apply -> Watch -> close
// lifecycle without caring about per-event timing.
func drainUpdates(c <-chan StatusUpdate, timeout time.Duration) []StatusUpdate {
	var out []StatusUpdate
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case u, ok := <-c:
			if !ok {
				return out
			}
			out = append(out, u)
		case <-deadline.C:
			return out
		}
	}
}

// TestNative_Resume_NoOpWhenNotReplay confirms Resume only does work in
// ReplayCmdline mode.
func TestNative_Resume_NoOpWhenNotReplay(t *testing.T) {
	t.Parallel()
	cs := fake.NewSimpleClientset()
	n := newFromClient(cs)
	created, err := n.Resume(context.Background(), MigrationID("abc"), validRequest())
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if created {
		t.Fatal("Resume must report created=false in non-replay mode")
	}
	jobs, err := cs.BatchV1().Jobs("kube-system").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("Resume must not create jobs in non-replay mode; got %d", len(jobs.Items))
	}
}

// TestNative_Resume_IdempotentWhenDestExists locks the contract that
// Resume is a no-op once the dest Job is already created — letting the
// recovery path call it on every reconcile tick without bumping counters.
func TestNative_Resume_IdempotentWhenDestExists(t *testing.T) {
	t.Parallel()
	id := MigrationID("idem-1")
	cs := fake.NewSimpleClientset(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: DestJobName(id), Namespace: "kube-system"},
	})
	n := newFromClient(cs)
	req := validRequest()
	req.ReplayCmdline = true
	created, err := n.Resume(context.Background(), id, req)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if created {
		t.Fatal("Resume must report created=false when dest already exists")
	}
	jobs, err := cs.BatchV1().Jobs("kube-system").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected exactly 1 job (the existing dest), got %d", len(jobs.Items))
	}
}

// TestNative_Resume_ErrorsWhenSourceMissing covers the "source job
// disappeared" case — Resume cannot proceed and surfaces the missing
// source so the reconciler can mark the CR Failed instead of looping.
func TestNative_Resume_ErrorsWhenSourceMissing(t *testing.T) {
	t.Parallel()
	cs := fake.NewSimpleClientset()
	n := newFromClient(cs)
	req := validRequest()
	req.ReplayCmdline = true
	created, err := n.Resume(context.Background(), MigrationID("ghost"), req)
	if err == nil {
		t.Fatal("expected error when source job is missing")
	}
	if created {
		t.Fatal("Resume must report created=false when it errored")
	}
	if !strings.Contains(err.Error(), "get source job") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestNative_Resume_CreatesDestWithReplayFromPod is the happy-path
// equivalent of stageThenStartDest: source job + pod exist, Resume
// resolves the pod and submits a dest job whose command carries the
// `--replay-cmdline-from-pod <ns>/<srcPod>` flag.
func TestNative_Resume_CreatesDestWithReplayFromPod(t *testing.T) {
	t.Parallel()
	id := MigrationID("happy-1")
	srcName := SourceJobName(id)
	cs := fake.NewSimpleClientset(
		&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: srcName, Namespace: "kube-system"},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      srcName + "-abc12",
				Namespace: "kube-system",
				Labels:    map[string]string{"batch.kubernetes.io/job-name": srcName},
			},
		},
	)
	n := newFromClient(cs)
	req := validRequest()
	req.ReplayCmdline = true
	created, err := n.Resume(context.Background(), id, req)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !created {
		t.Fatal("Resume must report created=true on happy path")
	}
	dest, err := cs.BatchV1().Jobs("kube-system").Get(context.Background(), DestJobName(id), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("dest job not created: %v", err)
	}
	cmd := jobCommand(t, *dest)
	if !strings.Contains(cmd, "--replay-cmdline-from-pod kube-system/"+srcName+"-abc12") {
		t.Fatalf("dest cmd missing --replay-cmdline-from-pod flag: %s", cmd)
	}
}
