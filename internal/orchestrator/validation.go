package orchestrator

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/maci0/katamaran/internal/migration"
)

// maxPodWaitTimeoutSeconds caps spec.podWaitTimeoutSeconds at 24 hours. See
// the comment on the PodWaitTimeoutSeconds check in Validate for why an upper
// bound is required.
const maxPodWaitTimeoutSeconds = 86400

// Validate checks a Request for required fields and mode consistency. Exposed
// so callers (e.g. the dashboard's HTTP handler) can pre-validate before
// calling Apply.
func Validate(req Request) error {
	if req.SourceNode == "" {
		return errors.New("sourceNode is required")
	}
	// DestNode may be empty when SourcePod is set (auto-selection mode).
	// In that case DestIP is also deferred until the dest Job is scheduled.
	if req.DestNode == "" && req.SourcePod == nil {
		return errors.New("destNode is required when sourcePod is not set")
	}
	if req.DestNode != "" && req.SourceNode == req.DestNode {
		return errors.New("sourceNode and destNode must differ")
	}
	if req.DestNode != "" && req.DestIP == "" {
		return errors.New("destIP is required")
	}
	if req.Image == "" {
		return errors.New("image is required")
	}
	if req.SourcePod == nil {
		// Legacy mode: SourceQMP + VMIP required.
		if req.SourceQMP == "" || req.VMIP == "" {
			return errors.New("either SourcePod or (SourceQMP + VMIP) is required")
		}
	} else if req.SourcePod.Name == "" || req.SourcePod.Namespace == "" {
		return errors.New("sourcePod requires both Name and Namespace")
	}
	// ReplayCmdline and auto-select (empty DestNode) are mutually exclusive.
	// Replay requires the source Job to start first so it can emit the QEMU
	// cmdline marker; auto-select requires the dest Job to start first so its
	// scheduled node can be resolved into DestIP before the source Job is
	// rendered. Apply cannot satisfy both orderings, so the replay branch
	// would render the source Job with an empty DestIP and the migration would
	// never connect. Reject the combination instead of silently building a
	// broken source Job.
	if req.ReplayCmdline && req.DestNode == "" {
		return errors.New("replayCmdline requires destNode to be set (it is incompatible with destination auto-selection)")
	}
	if req.DestPod != nil && (req.DestPod.Name == "" || req.DestPod.Namespace == "") {
		return errors.New("destPod requires both Name and Namespace")
	}
	// Enum checks are case-sensitive and accept only the canonical
	// lowercase values rendered into Job args. The katamaran CLI normalizes
	// enum flag case before validating, so this is deliberately stricter
	// than the runtime requires: case variants are rejected at request time
	// instead of being silently accepted in a running migration Job.
	if req.TunnelMode != "" && req.TunnelMode != "ipip" && req.TunnelMode != "gre" && req.TunnelMode != "none" {
		return fmt.Errorf("tunnelMode must be one of ipip, gre, or none, got %q", req.TunnelMode)
	}
	if req.DowntimeMS < 0 || req.DowntimeMS > migration.MaxDowntimeMS {
		return fmt.Errorf("downtimeMS must be between 0 and %d, got %d", migration.MaxDowntimeMS, req.DowntimeMS)
	}
	if req.MultifdChannels < 0 {
		return fmt.Errorf("multifdChannels must be non-negative, got %d", req.MultifdChannels)
	}
	// AutoDowntimeFloorMS is the lower bound for the auto-calculated downtime
	// and becomes the programmed downtime directly when the RTT is ~0, so it is
	// the same unit and budget as DowntimeMS and shares its upper bound. A floor
	// above MaxDowntimeMS would pause the guest longer than the fixed-downtime
	// path ever allows, defeating the tool's purpose.
	if req.AutoDowntimeFloorMS < 0 || req.AutoDowntimeFloorMS > migration.MaxDowntimeMS {
		return fmt.Errorf("autoDowntimeFloorMS must be between 0 and %d, got %d", migration.MaxDowntimeMS, req.AutoDowntimeFloorMS)
	}
	if req.CNIConvergenceDelaySeconds < 0 {
		return fmt.Errorf("cniConvergenceDelaySeconds must be non-negative, got %d", req.CNIConvergenceDelaySeconds)
	}
	if req.LogLevel != "" && req.LogLevel != "debug" && req.LogLevel != "info" && req.LogLevel != "warn" && req.LogLevel != "error" {
		return fmt.Errorf("logLevel must be one of debug, info, warn, or error, got %q", req.LogLevel)
	}
	if req.LogFormat != "" && req.LogFormat != "text" && req.LogFormat != "json" {
		return fmt.Errorf("logFormat must be one of text or json, got %q", req.LogFormat)
	}
	// PodWaitTimeoutSeconds is multiplied by time.Second when building the
	// pod-wait deadline (waitForJobPod). That product overflows int64
	// nanoseconds above ~9.2e9 seconds and wraps negative, making the wait
	// context expire instantly instead of honoring the requested timeout.
	// 24h is already far beyond any legitimate pod start (the orchestrator
	// default is 60s) and keeps the multiplication far from overflow, so any
	// larger value is treated as a typo rather than an intent to wait days.
	if req.PodWaitTimeoutSeconds < 0 || req.PodWaitTimeoutSeconds > maxPodWaitTimeoutSeconds {
		return fmt.Errorf("podWaitTimeoutSeconds must be between 0 and %d, got %d", maxPodWaitTimeoutSeconds, req.PodWaitTimeoutSeconds)
	}
	if req.SourceCleanup != "" && req.SourceCleanup != "none" && req.SourceCleanup != "delete" && req.SourceCleanup != "orphan" {
		return fmt.Errorf("sourceCleanup must be one of none, delete, or orphan, got %q", req.SourceCleanup)
	}
	if err := validateRequestArgValues(req); err != nil {
		return err
	}
	// Defense-in-depth: DestIP and VMIP are interpolated into the rendered
	// source/dest Job's shell command. Reject anything that isn't a parseable
	// IP at the orchestrator boundary so a future change to ValidateSafeArgValue
	// (or a non-dashboard caller) can't pass a value that looks IP-shaped but
	// hides shell metacharacters or hostname-style content.
	if req.DestIP != "" {
		if _, err := netip.ParseAddr(req.DestIP); err != nil {
			return fmt.Errorf("destIP %q is not a valid IP address: %w", req.DestIP, err)
		}
	}
	if req.VMIP != "" {
		if _, err := netip.ParseAddr(req.VMIP); err != nil {
			return fmt.Errorf("vmIP %q is not a valid IP address: %w", req.VMIP, err)
		}
	}
	return nil
}

// MaxSafeArgValueLen caps argument length to prevent buffer bloat.
const MaxSafeArgValueLen = 512

func validateRequestArgValues(req Request) error {
	type requestArgValue struct {
		name  string
		value string
	}
	fields := []requestArgValue{
		{"SourceNode", req.SourceNode},
		{"DestNode", req.DestNode},
		{"SourceQMP", req.SourceQMP},
		{"VMIP", req.VMIP},
		{"DestQMP", req.DestQMP},
		{"DestIP", req.DestIP},
		{"Image", req.Image},
		{"TunnelMode", req.TunnelMode},
		{"TapIface", req.TapIface},
		{"TapNetns", req.TapNetns},
		{"LogLevel", req.LogLevel},
		{"LogFormat", req.LogFormat},
	}
	if req.SourcePod != nil {
		fields = append(fields,
			requestArgValue{"SourcePod.Namespace", req.SourcePod.Namespace},
			requestArgValue{"SourcePod.Name", req.SourcePod.Name},
		)
	}
	if req.DestPod != nil {
		fields = append(fields,
			requestArgValue{"DestPod.Namespace", req.DestPod.Namespace},
			requestArgValue{"DestPod.Name", req.DestPod.Name},
		)
	}
	for _, f := range fields {
		if err := ValidateSafeArgValue(f.name, f.value); err != nil {
			return err
		}
	}
	return nil
}

// ValidateSafeArgValue checks that a string is safe to be passed as an argument
// to the katamaran CLI. It rejects overly long strings, path traversal, and
// characters with shell meaning. Exposed so the dashboard can validate raw
// form inputs before building a Request.
func ValidateSafeArgValue(field, value string) error {
	if value == "" {
		return nil
	}
	if len(value) > MaxSafeArgValueLen {
		return fmt.Errorf("%s is too long", field)
	}
	if strings.Contains(value, "..") {
		return fmt.Errorf("%s contains invalid path traversal", field)
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '.' || r == '/' || r == ':' || r == '@' || r == '=' || r == '-':
		default:
			return fmt.Errorf("%s contains invalid characters", field)
		}
	}
	return nil
}
