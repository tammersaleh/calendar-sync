package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// Env-var name SPEC.md "Location" calls out for the override.
const envConfigPath = "CALENDAR_SYNC_CONFIG"

// FindPath returns the config file path to use, applying the SPEC.md
// "Location" precedence rules:
//
//  1. The flag value, if non-empty.
//  2. $CALENDAR_SYNC_CONFIG, if set.
//  3. $XDG_CONFIG_HOME/calendar-sync/config.toml, defaulting XDG_CONFIG_HOME
//     to ~/.config when unset.
//
// Returns the path even if the file does not exist; callers handle the
// not-found case (commands that need a config emit config_not_found).
func FindPath(flag string) string {
	if flag != "" {
		return flag
	}
	if env := os.Getenv(envConfigPath); env != "" {
		return env
	}
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			// Fall back to a relative path so the error surfaces as
			// "file not found" rather than panicking. The caller's
			// open() will fail informatively.
			return filepath.Join(".config", "calendar-sync", "config.toml")
		}
		xdg = filepath.Join(home, ".config")
	}
	return filepath.Join(xdg, "calendar-sync", "config.toml")
}

// Load reads and parses a config file from disk, applying defaults for
// any unset Settings field. It does NOT run Validate or Canonicalize;
// callers compose those steps explicitly.
//
// Returns os.ErrNotExist (wrapped) when the file is missing so callers
// can errors.Is(err, os.ErrNotExist) to surface SPEC's config_not_found.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %q: %w", path, err)
	}
	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse %q: %w", path, err)
	}
	cfg.applyDefaults()
	return &cfg, nil
}

// applyDefaults fills any unset Settings field with the SPEC.md default.
// Pair-level defaults (Enabled defaulting to true) are handled at the
// IsEnabled accessor; we don't mutate the Pair struct so a round-trip
// emit can show the absence cleanly.
func (c *Config) applyDefaults() {
	if c.Settings.PollInterval == 0 {
		c.Settings.PollInterval = Duration(60 * time.Second)
	}
	if c.Settings.Horizon == 0 {
		c.Settings.Horizon = Duration(365 * 24 * time.Hour)
	}
	if c.Settings.FullSyncInterval == 0 {
		c.Settings.FullSyncInterval = Duration(24 * time.Hour)
	}
	if c.Settings.LogLevel == "" {
		c.Settings.LogLevel = LogLevelInfo
	}
	if c.Settings.LogFormat == "" {
		c.Settings.LogFormat = LogFormatJSON
	}
	// DryRun defaults to false (Go zero value); no override needed.
}
