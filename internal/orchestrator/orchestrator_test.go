package orchestrator

import (
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestTerminalJobCondition(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		conditions []batchv1.JobCondition
		want       batchv1.JobConditionType
	}{
		{name: "none"},
		{
			name: "ignores false terminal condition",
			conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionFalse},
			},
		},
		{
			name: "returns single true terminal condition",
			conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
			},
			want: batchv1.JobFailed,
		},
		{
			name: "returns most recent terminal condition by transition time",
			conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(base)},
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(base.Add(time.Second))},
			},
			want: batchv1.JobComplete,
		},
		{
			name: "equal timestamps use later condition",
			conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
			},
			want: batchv1.JobFailed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			job := &batchv1.Job{Status: batchv1.JobStatus{Conditions: tt.conditions}}
			if got := TerminalJobCondition(job); got != tt.want {
				t.Fatalf("TerminalJobCondition() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStatusPhaseIsTerminal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		phase StatusPhase
		want  bool
	}{
		{PhaseSubmitted, false},
		{PhaseDestStarting, false},
		{PhaseSrcStarting, false},
		{PhaseTransferring, false},
		{PhaseCutover, false},
		{PhaseSucceeded, true},
		{PhaseFailed, true},
		{StatusPhase("unknown"), false},
	}
	for _, tt := range tests {
		if got := tt.phase.IsTerminal(); got != tt.want {
			t.Fatalf("%s.IsTerminal() = %v, want %v", tt.phase, got, tt.want)
		}
	}
}
