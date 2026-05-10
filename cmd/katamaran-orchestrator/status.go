package main

import "github.com/maci0/katamaran/internal/orchestrator"

type statusOutput struct {
	ID                orchestrator.MigrationID `json:"id"`
	Phase             orchestrator.StatusPhase `json:"phase"`
	Time              string                   `json:"time"`
	Msg               string                   `json:"msg,omitempty"`
	Err               string                   `json:"err,omitempty"`
	RAMTransferred    int64                    `json:"ram_transferred,omitempty"`
	RAMTotal          int64                    `json:"ram_total,omitempty"`
	DowntimeMS        int64                    `json:"downtime_ms,omitempty"`
	AppliedDowntimeMS int64                    `json:"applied_downtime_ms,omitempty"`
	RTTMS             int64                    `json:"rtt_ms,omitempty"`
	AutoDowntime      bool                     `json:"auto_downtime,omitempty"`
}

func newStatusOutput(u orchestrator.StatusUpdate) statusOutput {
	out := statusOutput{
		ID:                u.ID,
		Phase:             u.Phase,
		Time:              u.When.UTC().Format("2006-01-02T15:04:05.000Z"),
		Msg:               u.Message,
		RAMTransferred:    u.RAMTransferred,
		RAMTotal:          u.RAMTotal,
		DowntimeMS:        u.DowntimeMS,
		AppliedDowntimeMS: u.AppliedDowntimeMS,
		RTTMS:             u.RTTMS,
		AutoDowntime:      u.AutoDowntime,
	}
	if u.Error != nil {
		out.Err = u.Error.Error()
	}
	return out
}
