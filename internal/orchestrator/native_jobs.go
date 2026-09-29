package orchestrator

import (
	"cmp"
	_ "embed"
	"fmt"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/maci0/katamaran/internal/migration"
)

// Embedded copies of the runtime templates so the Native orchestrator does
// not depend on a writable filesystem. The bash orchestrator (deploy/migrate.sh)
// continues to read these same files via envsubst.
//
//go:embed templates/job-source.yaml
var sourceJobTemplate []byte

//go:embed templates/job-dest.yaml
var destJobTemplate []byte

// renderSourceJob and renderDestJob substitute ${VAR} placeholders in the
// embedded templates and decode the result into a typed *batchv1.Job ready
// for Create. The substitution intentionally mirrors `envsubst $VAR` from
// migrate.sh: simple shell-style variable expansion with no defaults or
// nested expressions.
func renderSourceJob(req Request, id MigrationID, extraArgs string) (*batchv1.Job, error) {
	// --vm-ip and (legacy) --qmp reach the source binary through EXTRA_ARGS
	// (see sourceExtraArgs); the source template does not interpolate them.
	job, err := renderJob(sourceJobTemplate, map[string]string{
		"NODE_NAME":              req.SourceNode,
		"IMAGE":                  req.Image,
		"DEST_IP":                req.DestIP,
		"EXTRA_ARGS":             extraArgs,
		"KATAMARAN_MIGRATION_ID": string(id),
		"JOB_SUFFIX":             string(id),
	})
	if err != nil {
		return nil, err
	}
	stampSourcePod(job, req)
	return job, nil
}

func renderDestJob(req Request, id MigrationID, extraArgs string) (*batchv1.Job, error) {
	job, err := renderJob(destJobTemplate, map[string]string{
		"NODE_NAME": req.DestNode,
		"IMAGE":     req.Image,
		// migration.DestDefaultQMPSocket must stay the placeholder
		// RunDestination overrides with the real sandbox-derived path.
		"QMP_SOCKET":             cmp.Or(req.DestQMP, migration.DestDefaultQMPSocket),
		"EXTRA_ARGS":             extraArgs,
		"KATAMARAN_MIGRATION_ID": string(id),
		"JOB_SUFFIX":             string(id),
	})
	if err != nil {
		return nil, err
	}
	stampSourcePod(job, req)

	// Auto-select mode: DestNode is empty, so let Kubernetes schedule the
	// dest pod using the source pod's constraints plus an anti-affinity
	// that excludes the source node.
	if req.DestNode == "" {
		job.Spec.Template.Spec.NodeName = ""
		job.Spec.Template.Spec.NodeSelector = req.DestNodeSelector
		job.Spec.Template.Spec.Tolerations = req.DestTolerations
		job.Spec.Template.Spec.Affinity = &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{{
							Key:      "kubernetes.io/hostname",
							Operator: corev1.NodeSelectorOpNotIn,
							Values:   []string{req.SourceNode},
						}},
					}},
				},
			},
		}
	}

	return job, nil
}

func renderJob(tmpl []byte, vars map[string]string) (*batchv1.Job, error) {
	expanded := expandShellVars(string(tmpl), vars)
	var job batchv1.Job
	if err := yaml.NewYAMLOrJSONDecoder(strings.NewReader(expanded), 4096).Decode(&job); err != nil {
		return nil, fmt.Errorf("decode rendered job: %w", err)
	}
	return &job, nil
}

// expandShellVars substitutes ${VAR} references in s with vars[VAR]. Unknown
// references are replaced with the empty string (matching `envsubst`).
//
// Only supports the ${NAME} form: no $NAME, no ${NAME:-default}. Mirrors
// migrate.sh's `envsubst '$NODE_NAME $QMP_SOCKET ...'` invocation.
func expandShellVars(s string, vars map[string]string) string {
	var out strings.Builder
	out.Grow(len(s))
	for i := 0; i < len(s); {
		if i+1 < len(s) && s[i] == '$' && s[i+1] == '{' {
			end := strings.IndexByte(s[i+2:], '}')
			if end >= 0 {
				name := s[i+2 : i+2+end]
				out.WriteString(vars[name])
				i += 2 + end + 1
				continue
			}
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
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
