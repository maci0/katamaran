package orchestrator

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

// cmdlineHostDir is the hostPath mount used to ship the captured source
// QEMU cmdline file from the orchestrator (via a stager pod) onto the
// destination node, where the dest Job picks it up. Matches the hostPath
// in templates/job-{source,dest}.yaml.
const cmdlineHostDir = "/tmp/katamaran-cmdlines"

// cmdlinePathFor returns the per-migration cmdline file path inside
// cmdlineHostDir. Used identically by Apply (to build --emit-cmdline-to /
// --replay-cmdline args) and stageCmdline (to write the file).
func cmdlinePathFor(id MigrationID) string {
	return fmt.Sprintf("%s/cmdline-%s.txt", cmdlineHostDir, id)
}

// stageCmdline performs the source-to-dest cmdline transfer that
// deploy/migrate.sh does in bash:
//
//  1. Wait for the source pod to log `KATAMARAN_CMDLINE_AT=<path>`,
//     signalling that the source binary has captured the QEMU cmdline.
//  2. SPDY-stream into the source pod to read the file via cat.
//  3. Create a stager pod on the dest node with hostPath mount of
//     cmdlineHostDir.
//  4. SPDY-stream into the stager to write the file via tee.
//  5. Delete the stager pod (best-effort).
//
// The dest job's hostPath cmdlineHostDir mount then sees the file.
//
// Returns the path the file is now visible at inside the dest job's
// filesystem (typically cmdlineHostDir/cmdline-<id>.txt). The dest job's
// EXTRA_ARGS gets `--replay-cmdline <path>` appended by the caller.
func (n *native) stageCmdline(ctx context.Context, id MigrationID, srcPodName, srcPodNamespace, destNode, image string) (string, error) {
	if n.config == nil {
		return "", fmt.Errorf("native orchestrator missing rest.Config; cannot stream into pods")
	}
	srcCmdlinePath, err := n.waitForCmdlineMarker(ctx, srcPodNamespace, srcPodName)
	if err != nil {
		return "", fmt.Errorf("waiting for KATAMARAN_CMDLINE_AT in source logs: %w", err)
	}
	cmdline, err := n.podCat(ctx, srcPodNamespace, srcPodName, srcCmdlinePath)
	if err != nil {
		return "", fmt.Errorf("reading cmdline from source pod: %w", err)
	}
	stagerName := fmt.Sprintf("katamaran-cmdline-stager-%s", id)
	stagedPath := cmdlinePathFor(id)
	if err := n.createStagerPod(ctx, stagerName, destNode, image); err != nil {
		return "", fmt.Errorf("create stager pod: %w", err)
	}
	defer func() {
		if err := n.client.CoreV1().Pods(n.namespace).Delete(context.Background(), stagerName, metav1.DeleteOptions{
			GracePeriodSeconds: int64ptr(0),
		}); err != nil && !apierrors.IsNotFound(err) {
			slog.Warn("stageCmdline: stager pod cleanup failed; pod may leak", "pod", stagerName, "namespace", n.namespace, "migration_id", id, "error", err)
		}
	}()
	if err := n.waitPodReady(ctx, n.namespace, stagerName); err != nil {
		return "", fmt.Errorf("stager pod not Ready: %w", err)
	}
	if err := n.podWrite(ctx, n.namespace, stagerName, stagedPath, cmdline); err != nil {
		return "", fmt.Errorf("write cmdline to stager: %w", err)
	}
	return stagedPath, nil
}

// waitForCmdlineMarker tails the source pod's logs until it sees the
// KATAMARAN_CMDLINE_AT=<path> marker. Returns the captured path. Times out
// after 5 minutes.
func (n *native) waitForCmdlineMarker(ctx context.Context, namespace, name string) (string, error) {
	deadline, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	const marker = "KATAMARAN_CMDLINE_AT="
	scanBuf := make([]byte, 0, 64*1024)
	for {
		req := n.client.CoreV1().Pods(namespace).GetLogs(name, &corev1.PodLogOptions{Container: "katamaran"})
		stream, err := req.Stream(deadline)
		if err != nil {
			if deadline.Err() == nil {
				slog.Debug("waitForCmdlineMarker: opening pod log stream failed, will retry", "pod", name, "namespace", namespace, "error", err)
			}
		} else {
			scanner := bufio.NewScanner(stream)
			scanner.Buffer(scanBuf, 1024*1024)
			for scanner.Scan() {
				line := scanner.Text()
				if i := strings.Index(line, marker); i >= 0 {
					_ = stream.Close()
					return strings.TrimSpace(line[i+len(marker):]), nil
				}
			}
			readErr := scanner.Err()
			_ = stream.Close()
			if readErr != nil && deadline.Err() == nil {
				slog.Debug("waitForCmdlineMarker: reading pod log stream failed, will retry", "pod", name, "namespace", namespace, "error", readErr)
			}
		}
		select {
		case <-deadline.Done():
			return "", deadline.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// maxPodCatBytes bounds the bytes podCat will buffer from the source pod's
// stdout. The captured QEMU cmdline is well under 64 KiB in practice; cap
// at 1 MiB so a misbehaving or compromised source pod cannot exhaust dest
// memory by streaming an unbounded `cat` payload through SPDY exec.
const maxPodCatBytes = 1 << 20

// podCat runs `cat <path>` in the named pod and returns stdout.
func (n *native) podCat(ctx context.Context, namespace, name, path string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	limited := &limitWriter{w: &stdout, n: maxPodCatBytes}
	if err := n.podStream(ctx, namespace, name, []string{"cat", path}, nil, limited, &stderr); err != nil {
		return nil, fmt.Errorf("cat %s: %w (stderr=%s)", path, err, strings.TrimSpace(stderr.String()))
	}
	if limited.exceeded {
		return nil, fmt.Errorf("cat %s: output exceeded %d bytes", path, maxPodCatBytes)
	}
	return stdout.Bytes(), nil
}

// limitWriter wraps an io.Writer with an absolute byte budget. Writes that
// would exceed the budget are silently dropped past the limit and the
// exceeded flag is set so the caller can fail the operation.
type limitWriter struct {
	w        io.Writer
	n        int64
	exceeded bool
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		l.exceeded = true
		return len(p), nil
	}
	if int64(len(p)) > l.n {
		l.exceeded = true
		_, err := l.w.Write(p[:l.n])
		l.n = 0
		if err != nil {
			return 0, err
		}
		return len(p), nil
	}
	l.n -= int64(len(p))
	return l.w.Write(p)
}

// podWrite writes content to path inside pod by piping to `tee`. The path
// is fully controlled by the orchestrator (composed from migration ID), so
// shell injection is not a concern.
func (n *native) podWrite(ctx context.Context, namespace, name, path string, content []byte) error {
	cmd := []string{"sh", "-c", fmt.Sprintf("umask 077 && mkdir -p %q && tee %q > /dev/null", dirOf(path), path)}
	var stderr bytes.Buffer
	if err := n.podStream(ctx, namespace, name, cmd, bytes.NewReader(content), io.Discard, &stderr); err != nil {
		return fmt.Errorf("tee %s: %w (stderr=%s)", path, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// podStream runs cmd in pod via SPDY remotecommand. In-process equivalent
// of `kubectl exec`.
func (n *native) podStream(ctx context.Context, namespace, name string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) error {
	opts := &corev1.PodExecOptions{
		Command: cmd,
		Stdin:   stdin != nil,
		Stdout:  stdout != nil,
		Stderr:  stderr != nil,
	}
	req := n.client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(name).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(opts, scheme.ParameterCodec)
	streamer, err := remotecommand.NewSPDYExecutor(n.config, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("new SPDY streamer: %w", err)
	}
	return streamer.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
	})
}

// createStagerPod creates a small staging pod on destNode whose only purpose
// is to give us a writable hostPath into cmdlineHostDir on that node. We
// immediately stream into it to write the cmdline file.
func (n *native) createStagerPod(ctx context.Context, podName, destNode, image string) error {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: n.namespace},
		Spec: corev1.PodSpec{
			NodeName:                     destNode,
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: boolptr(false),
			Containers: []corev1.Container{{
				Name: "stager",
				// Reuse the trusted migration image instead of pulling an
				// extra utility image just to get sh, tee, and sleep.
				Image:           image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"sleep", "300"},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: boolptr(false),
					Capabilities: &corev1.Capabilities{
						Drop: []corev1.Capability{"ALL"},
					},
					SeccompProfile: &corev1.SeccompProfile{
						Type: corev1.SeccompProfileTypeRuntimeDefault,
					},
				},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "cmdline-dir",
					MountPath: cmdlineHostDir,
				}},
			}},
			Volumes: []corev1.Volume{{
				Name: "cmdline-dir",
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{
						Path: cmdlineHostDir,
						Type: hostPathType(corev1.HostPathDirectoryOrCreate),
					},
				},
			}},
		},
	}
	_, err := n.client.CoreV1().Pods(n.namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// waitPodReady polls until the named pod is Running with all containers ready.
func (n *native) waitPodReady(ctx context.Context, namespace, name string) error {
	deadline, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		p, err := n.client.CoreV1().Pods(namespace).Get(deadline, name, metav1.GetOptions{})
		if err == nil && p.Status.Phase == corev1.PodRunning {
			ready := true
			for _, c := range p.Status.ContainerStatuses {
				if !c.Ready {
					ready = false
					break
				}
			}
			if ready {
				return nil
			}
		}
		select {
		case <-deadline.Done():
			return deadline.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func dirOf(path string) string {
	if i := strings.LastIndexByte(path, '/'); i > 0 {
		return path[:i]
	}
	return "."
}

func hostPathType(t corev1.HostPathType) *corev1.HostPathType { return &t }
func int64ptr(v int64) *int64                                 { return &v }
func boolptr(v bool) *bool                                    { return &v }
