package main

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/umputun/tg-spam/app/config"
)

// yamlOverlay is the partial schema accepted from the --config YAML file.
// Only fields that are not exposed through CLI/env tags are read here; CLI
// remains the source of truth for everything else. Currently this covers
// Telegram.Groups (multi-chat target list) and Admin.SuperUsersCrossChat
// (cross-chat super-user resolution opt-in).
type yamlOverlay struct {
	Telegram struct {
		Groups []config.ConfiguredChat `yaml:"groups"`
	} `yaml:"telegram"`
	Admin struct {
		SuperUsersCrossChat bool `yaml:"superusers_cross_chat"`
	} `yaml:"admin"`
}

// applyYAMLOverlay reads the YAML file at path and merges its contents onto
// the resolved settings. Fields covered by the overlay schema overwrite any
// values set via CLI/env; fields not covered are left untouched. The overlay
// is strict — unknown top-level keys produce an error so typos do not silently
// vanish.
func applyYAMLOverlay(path string, settings *config.Settings) error {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // path comes from operator-supplied CLI flag
	if err != nil {
		return fmt.Errorf("read config file %q: %w", path, err)
	}
	var overlay yamlOverlay
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&overlay); err != nil {
		return fmt.Errorf("parse config file %q: %w", path, err)
	}
	if len(overlay.Telegram.Groups) > 0 {
		settings.Telegram.Groups = overlay.Telegram.Groups
	}
	settings.Admin.SuperUsersCrossChat = overlay.Admin.SuperUsersCrossChat
	return nil
}
