package controller

import (
	"os"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// jobTemplatePath resolves a Job manifest owned by the orchestrator package.
// The templates are the single source of truth for the deadline: the Native
// orchestrator embeds them and deploy/migrate.sh renders the same files, so
// the controller's watch budget has to track them from disk.
func jobTemplatePath(name string) string {
	return "../orchestrator/templates/" + name
}

func readJobActiveDeadline(t *testing.T, name string) int64 {
	t.Helper()
	raw, err := os.ReadFile(jobTemplatePath(name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var job batchv1.Job
	if err := yaml.Unmarshal(raw, &job); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	if job.Spec.ActiveDeadlineSeconds == nil {
		t.Fatalf("%s sets no activeDeadlineSeconds", name)
	}
	return *job.Spec.ActiveDeadlineSeconds
}

// TestJobDeadlineMatchesStatusTimeout pins the budget invariant documented in
// AGENTS.md: the controller's StatusTimeout and both Jobs' activeDeadlineSeconds
// are the same duration, and that duration is above the source binary's
// storage-sync (2h) plus RAM-migration (1h) budgets. If the Jobs expire first,
// the controller's watch dies mid-transfer and a healthy migration is marked
// Failed; if the watch expires first, the same transfer is abandoned without a
// terminal Job condition to reconcile from.
func TestJobDeadlineMatchesStatusTimeout(t *testing.T) {
	t.Parallel()

	rec := NewReconciler(nil, nil, nil, nil)
	statusSeconds := int64(rec.StatusTimeout / time.Second)

	source := readJobActiveDeadline(t, "job-source.yaml")
	dest := readJobActiveDeadline(t, "job-dest.yaml")

	if source != dest {
		t.Errorf("job-source.yaml deadline %ds != job-dest.yaml deadline %ds", source, dest)
	}
	if source != statusSeconds {
		t.Errorf("StatusTimeout %ds != Job activeDeadlineSeconds %ds", statusSeconds, source)
	}
	// Headroom requirement from the source binary: storageSyncTimeout (2h) +
	// migrationTimeout (1h), plus Job startup and CNI convergence.
	const minimum = int64((3 * time.Hour) / time.Second)
	if source < minimum {
		t.Errorf("Job activeDeadlineSeconds %ds is below the 3h source-side budget", source)
	}
}
