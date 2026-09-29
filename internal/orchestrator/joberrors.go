package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	batchv1 "k8s.io/api/batch/v1"

	"github.com/maci0/katamaran/internal/logging"
)

// ConditionFailureDetails extracts a terminal Job condition's reason and
// message into both a compact "reason=… message=…" summary string and slog
// attrs. Exported so the Migration CRD controller's recovery path formats
// job failures identically to the orchestrator's outcome matrix instead of
// keeping its own copy of the extraction.
func ConditionFailureDetails(cond batchv1.JobCondition) (detail string, attrs []any) {
	details := make([]string, 0, 2)
	attrs = make([]any, 0, 4)
	if cond.Reason != "" {
		details = append(details, "reason="+cond.Reason)
		attrs = append(attrs, "reason", cond.Reason)
	}
	if cond.Message != "" {
		details = append(details, "message="+cond.Message)
		attrs = append(attrs, "message", cond.Message)
	}
	return strings.Join(details, " "), attrs
}

func jobConditionAttrs(cond batchv1.JobCondition) []any {
	_, attrs := ConditionFailureDetails(cond)
	return attrs
}

func jobFailedError(base string, cond batchv1.JobCondition) error {
	detail, _ := ConditionFailureDetails(cond)
	if detail == "" {
		return errors.New(base)
	}
	return fmt.Errorf("%s: %s", base, detail)
}

func logTransientJobStatusError(message string, id MigrationID, jobName, namespace string, err error, consecutive int) {
	attrs := []any{"migration_id", id, "job", jobName, "namespace", namespace, "error", err, "consecutive_errors", consecutive}
	slog.Log(context.Background(), logging.TransientLevel(consecutive), message, attrs...)
}
