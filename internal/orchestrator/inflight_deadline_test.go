package orchestrator

import (
	"context"
	"math"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// An activeDeadlineSeconds read back from the apiserver is an unvalidated
// int64. Multiplying one past math.MaxInt64/1e9 by time.Second wraps to a
// negative Duration, and every caller of inflightForSourcePod compares an
// elapsed time against that bound, so the Job reads as expired the moment it
// is created. The guard then hands out a second migration ID for a pod whose
// first migration is still running, which is exactly the duplicate it exists
// to prevent.
func TestInflightForSourcePod_HugeDeadlineIsNotTreatedAsExpired(t *testing.T) {
	t.Parallel()
	req := validRequest()
	overflown := int64(10000000000) // * time.Second wraps to a negative Duration
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "migration-job",
			Namespace: DefaultJobNamespace,
			Labels: map[string]string{
				MigrationIDLabel:        "abc123def456",
				SourcePodLabel:          req.SourcePod.Name,
				SourcePodNamespaceLabel: req.SourcePod.Namespace,
			},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Minute)),
		},
		Spec: batchv1.JobSpec{ActiveDeadlineSeconds: &overflown},
	}
	cs := fake.NewSimpleClientset(job)
	n := newFromClient(cs)

	id, inflight, err := n.inflightForSourcePod(context.Background(), req)
	if err != nil {
		t.Fatalf("inflightForSourcePod: %v", err)
	}
	if !inflight {
		t.Fatal("a Job with a 10^10s deadline was treated as already expired; the guard would start a second migration for the same pod")
	}
	if id != MigrationID("abc123def456") {
		t.Errorf("migration ID = %q, want %q", id, "abc123def456")
	}
}

// The saturating conversion must keep ordinary deadlines exact and must not
// turn a non-positive deadline into a positive bound.
func TestSecondsAsDuration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		seconds int64
		want    time.Duration
	}{
		{"zero", 0, 0},
		{"negative", -5, 0},
		{"typical", 14400, 4 * time.Hour},
		{"at the limit", maxDurationSeconds, time.Duration(maxDurationSeconds) * time.Second},
		{"past the limit", maxDurationSeconds + 1, time.Duration(math.MaxInt64)},
		{"far past the limit", int64(10000000000), time.Duration(math.MaxInt64)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := secondsAsDuration(tt.seconds); got != tt.want {
				t.Errorf("secondsAsDuration(%d) = %v, want %v", tt.seconds, got, tt.want)
			}
		})
	}
}
