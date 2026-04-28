package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// stageCmdline performs the source-to-dest cmdline transfer that
// deploy/migrate.sh does in bash:
//
//  1. Wait for the source pod to log `KATAMARAN_CMDLINE_AT=<path>`,
//     signalling that the source binary has captured the QEMU cmdline.
//  2. SPDY-stream into the source pod to read the file via cat.
//  3. Create a stager pod on the dest node with hostPath mount of
//     /tmp/katamaran-cmdlines.
//  4. SPDY-stream into the stager to write the file via tee.
//  5. Delete the stager pod (best-effort).
//
// The dest job's hostPath /tmp/katamaran-cmdlines mount then sees the file.
//
// Returns the path the file is now visible at inside the dest job's
// filesystem (typically /tmp/katamaran-cmdlines/cmdline-<id>.txt). The
// dest job's EXTRA_ARGS gets `--replay-cmdline <path>` appended by the
// caller.
func (n *native) stageCmdline(ctx context.Context, id MigrationID, srcPodName, srcPodNamespace, destNode string) (string, error) {
	if n.config == nil {
		return "", fmt.Errorf("Native orchestrator missing rest.Config; cannot stream into pods")
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
	stagedPath := fmt.Sprintf("/tmp/katamaran-cmdlines/cmdline-%s.txt", id)
	if err := n.createStagerPod(ctx, stagerName, destNode); err != nil {
		return "", fmt.Errorf("create stager pod: %w", err)
	}
	defer func() {
		_ = n.client.CoreV1().Pods(n.namespace).Delete(context.Background(), stagerName, metav1.DeleteOptions{
			GracePeriodSeconds: int64ptr(0),
		})
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
	for {
		req := n.client.CoreV1().Pods(namespace).GetLogs(name, &corev1.PodLogOptions{Container: "katamaran"})
		stream, err := req.Stream(deadline)
		if err == nil {
			data, _ := io.ReadAll(stream)
			_ = stream.Close()
			for _, line := range strings.Split(string(data), "\n") {
				if i := strings.Index(line, marker); i >= 0 {
					return strings.TrimSpace(line[i+len(marker):]), nil
				}
			}
		}
		select {
		case <-deadline.Done():
			return "", deadline.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// podCat runs `cat <path>` in the named pod and returns stdout.
func (n *native) podCat(ctx context.Context, namespace, name, path string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	if err := n.podStream(ctx, namespace, name, []string{"cat", path}, nil, &stdout, &stderr); err != nil {
		return nil, fmt.Errorf("cat %s: %w (stderr=%s)", path, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// podWrite writes content to path inside pod by piping to `tee`. The path
// is fully controlled by the orchestrator (composed from migration ID), so
// shell injection is not a concern.
func (n *native) podWrite(ctx context.Context, namespace, name, path string, content []byte) error {
	cmd := []string{"sh", "-c", fmt.Sprintf("mkdir -p %q && tee %q > /dev/null", dirOf(path), path)}
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

// createStagerPod creates a small busybox-like pod on destNode whose only
// purpose is to give us a writable hostPath into /tmp/katamaran-cmdlines on
// that node. We immediately stream into it to write the cmdline file.
func (n *native) createStagerPod(ctx context.Context, podName, destNode string) error {
	priv := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: n.namespace},
		Spec: corev1.PodSpec{
			NodeName:      destNode,
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name: "stager",
				// Need a shell + tee + sleep, so we cannot use the kubernetes
				// pause image (which only ships /pause). busybox is small and
				// has the userland we need.
				Image:   "busybox:1.36",
				Command: []string{"sleep", "300"},
				SecurityContext: &corev1.SecurityContext{
					Privileged: &priv,
				},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "cmdline-dir",
					MountPath: "/tmp/katamaran-cmdlines",
				}},
			}},
			Volumes: []corev1.Volume{{
				Name: "cmdline-dir",
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{
						Path: "/tmp/katamaran-cmdlines",
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

// nativeReplayConfig holds the rest.Config the Native orchestrator needs
// for SPDY remote-command calls. New sets it; tests using fake
// clientsets leave it nil and ReplayCmdline mode returns
// ErrReplayCmdlineNotSupported.
type nativeReplayConfig struct{ cfg *rest.Config }

func dirOf(path string) string {
	if i := strings.LastIndexByte(path, '/'); i > 0 {
		return path[:i]
	}
	return "."
}

func hostPathType(t corev1.HostPathType) *corev1.HostPathType { return &t }
func int64ptr(v int64) *int64                                 { return &v }
