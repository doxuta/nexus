package config

import (
	"fmt"
	"strconv"
	"time"
)

// Preview-as-role session lifetime (specs/064-preview-as-role-sample-data FR-003).
const (
	defaultPreviewTTLMinutes = 30
	minPreviewTTLMinutes     = 1
	maxPreviewTTLMinutes     = 60
)

// PreviewConfig holds the preview-as-role configuration read from the environment.
type PreviewConfig struct {
	TTL time.Duration // PREVIEW_TTL_MINUTES
}

// LoadPreviewConfig reads PREVIEW_TTL_MINUTES (whole minutes, 1..60, default 30). Unlike the
// degrade-to-default knobs it FAILS FAST on an unparsable or out-of-range value: the TTL is the
// security bound of a token that carries another role, so a typo must stop the boot rather than
// silently hand out 30-minute (or unbounded) tokens.
func LoadPreviewConfig() (*PreviewConfig, error) {
	raw := getEnv("PREVIEW_TTL_MINUTES", "")
	if raw == "" {
		return &PreviewConfig{TTL: defaultPreviewTTLMinutes * time.Minute}, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < minPreviewTTLMinutes || n > maxPreviewTTLMinutes {
		return nil, fmt.Errorf("config.LoadPreviewConfig: PREVIEW_TTL_MINUTES=%q must be a whole number of minutes in %d..%d",
			raw, minPreviewTTLMinutes, maxPreviewTTLMinutes)
	}
	return &PreviewConfig{TTL: time.Duration(n) * time.Minute}, nil
}
