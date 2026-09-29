package orchestrator

import (
	"testing"
	"time"

	"github.com/maci0/katamaran/internal/migration"
)

// The rendered Jobs' activeDeadlineSeconds and the controller's StatusTimeout
// share migration.JobActiveDeadline. Kubernetes kills a Job the instant the
// deadline elapses, so a YAML value below the constant silently truncates
// migrations the constant says are allowed to run; the YAML is rendered by
// deploy/migrate.sh too, which cannot import Go, so nothing else keeps the two
// in step.
func TestJobDeadlineAgreesWithTemplates(t *testing.T) {
	t.Parallel()
	want := int64(migration.JobActiveDeadline / time.Second)

	src, err := renderSourceJob(validRequest(), MigrationID("test"), "")
	if err != nil {
		t.Fatalf("renderSourceJob: %v", err)
	}
	dest, err := renderDestJob(validRequest(), MigrationID("test"), "")
	if err != nil {
		t.Fatalf("renderDestJob: %v", err)
	}
	if got := src.Spec.ActiveDeadlineSeconds; got == nil {
		t.Error("source Job has no activeDeadlineSeconds; it would run unbounded")
	} else if *got != want {
		t.Errorf("source activeDeadlineSeconds = %d, want %d (migration.JobActiveDeadline)", *got, want)
	}
	if got := dest.Spec.ActiveDeadlineSeconds; got == nil {
		t.Error("dest Job has no activeDeadlineSeconds; it would run unbounded")
	} else if *got != want {
		t.Errorf("dest activeDeadlineSeconds = %d, want %d (migration.JobActiveDeadline)", *got, want)
	}
}
