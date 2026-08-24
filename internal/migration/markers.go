package migration

// Structured stdout markers printed by the source binary on its pod log and
// scraped from that log by consumers in this package (cmdlinefetch.go) and
// by the orchestrator's log tailer (internal/orchestrator/native.go). These
// strings are a wire protocol between three binaries; every producer and
// consumer references these constants so the exact spelling cannot drift.
const (
	// CmdlineAtMarker prefixes the captured-cmdline host path consumed by
	// deploy/migrate.sh's kubectl cp staging step.
	CmdlineAtMarker = "KATAMARAN_CMDLINE_AT="

	// CmdlineB64Marker prefixes the base64 QEMU cmdline payload that
	// replay-mode dest jobs decode to spawn their own QEMU.
	CmdlineB64Marker = "KATAMARAN_CMDLINE_B64="

	// VMConfigB64Marker / AgentConfigB64Marker prefix the base64 Kata
	// persist.json payloads scraped for factory VM adoption.
	VMConfigB64Marker    = "KATAMARAN_VMCONFIG_B64="
	AgentConfigB64Marker = "KATAMARAN_AGENTCONFIG_B64="

	// ProgressMarker, ResultMarker, DowntimeLimitMarker, and PhaseMarker are
	// space-terminated because their payloads are key=value fields appended
	// after one space; scrapers match these exact prefixes.
	ProgressMarker      = "KATAMARAN_PROGRESS "
	ResultMarker        = "KATAMARAN_RESULT "
	DowntimeLimitMarker = "KATAMARAN_DOWNTIME_LIMIT "
	PhaseMarker         = "KATAMARAN_PHASE "
)
