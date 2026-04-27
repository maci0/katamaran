package orchestrator

import (
	"bytes"
	_ "embed"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
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
	return renderJob(sourceJobTemplate, map[string]string{
		"NODE_NAME":              req.SourceNode,
		"IMAGE":                  req.Image,
		"QMP_SOCKET":             firstNonEmpty(req.SourceQMP, "/run/vc/vm/extra-monitor.sock"),
		"DEST_IP":                req.DestIP,
		"VM_IP":                  req.VMIP,
		"EXTRA_ARGS":             extraArgs,
		"KATAMARAN_MIGRATION_ID": string(id),
	})
}

func renderDestJob(req Request, id MigrationID, extraArgs string) (*batchv1.Job, error) {
	return renderJob(destJobTemplate, map[string]string{
		"NODE_NAME":              req.DestNode,
		"IMAGE":                  req.Image,
		"QMP_SOCKET":             firstNonEmpty(req.DestQMP, "/run/vc/vm/katamaran-dest/qmp.sock"),
		"EXTRA_ARGS":             extraArgs,
		"KATAMARAN_MIGRATION_ID": string(id),
	})
}

func renderJob(tmpl []byte, vars map[string]string) (*batchv1.Job, error) {
	expanded := expandShellVars(string(tmpl), vars)
	var job batchv1.Job
	if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader([]byte(expanded)), 4096).Decode(&job); err != nil {
		return nil, fmt.Errorf("decode rendered job: %w", err)
	}
	return &job, nil
}

// expandShellVars substitutes ${VAR} references in s with vars[VAR]. Unknown
// references are replaced with the empty string (matching `envsubst`).
//
// Only supports the ${NAME} form — no $NAME, no ${NAME:-default}. Mirrors
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

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
