package orchestrator

import (
	"errors"
	"fmt"
	"strings"
)

// Validate checks a Request for required fields and mode consistency. Exposed
// so callers (e.g. the dashboard's HTTP handler) can pre-validate before
// calling Apply or BuildArgs.
func Validate(req Request) error {
	if req.SourceNode == "" || req.DestNode == "" {
		return errors.New("SourceNode and DestNode are required")
	}
	if req.SourceNode == req.DestNode {
		return errors.New("SourceNode and DestNode must differ")
	}
	if req.DestIP == "" {
		return errors.New("DestIP is required")
	}
	if req.Image == "" {
		return errors.New("Image is required")
	}
	if req.SourcePod == nil {
		// Legacy mode: SourceQMP + VMIP required.
		if req.SourceQMP == "" || req.VMIP == "" {
			return errors.New("either SourcePod or (SourceQMP + VMIP) is required")
		}
	} else if req.SourcePod.Name == "" || req.SourcePod.Namespace == "" {
		return errors.New("SourcePod requires both Name and Namespace")
	}
	if req.DestPod != nil && (req.DestPod.Name == "" || req.DestPod.Namespace == "") {
		return errors.New("DestPod requires both Name and Namespace")
	}
	tunnelMode := strings.ToLower(req.TunnelMode)
	if tunnelMode != "" && tunnelMode != "ipip" && tunnelMode != "gre" && tunnelMode != "none" {
		return fmt.Errorf("TunnelMode must be one of ipip, gre, or none, got %q", req.TunnelMode)
	}
	if req.DowntimeMS < 0 || req.DowntimeMS > 60000 {
		return fmt.Errorf("DowntimeMS must be between 1 and 60000 when set, got %d", req.DowntimeMS)
	}
	if req.MultifdChannels < 0 {
		return fmt.Errorf("MultifdChannels must be non-negative, got %d", req.MultifdChannels)
	}
	logLevel := strings.ToLower(req.LogLevel)
	if logLevel != "" && logLevel != "debug" && logLevel != "info" && logLevel != "warn" && logLevel != "error" {
		return fmt.Errorf("LogLevel must be one of debug, info, warn, or error, got %q", req.LogLevel)
	}
	logFormat := strings.ToLower(req.LogFormat)
	if logFormat != "" && logFormat != "text" && logFormat != "json" {
		return fmt.Errorf("LogFormat must be one of text or json, got %q", req.LogFormat)
	}
	if err := validateRequestArgValues(req); err != nil {
		return err
	}
	return nil
}

const maxSafeArgValueLen = 512

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
		{"KubectlContext", req.KubectlContext},
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
		if err := validateSafeArgValue(f.name, f.value); err != nil {
			return err
		}
	}
	return nil
}

func validateSafeArgValue(field, value string) error {
	if value == "" {
		return nil
	}
	if len(value) > maxSafeArgValueLen {
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
