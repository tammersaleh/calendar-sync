package cmd

import (
	"github.com/tammersaleh/calendar-sync/internal/config"
)

// ConfigCmd is the kong group for `config show` / `config validate`.
type ConfigCmd struct {
	Show     ConfigShowCmd     `cmd:"" help:"Print the resolved configuration as a single JSON object."`
	Validate ConfigValidateCmd `cmd:"" help:"Validate the configuration."`
}

// ConfigShowCmd implements `calendar-sync config show`. SPEC §"calendar-sync
// config show" lines 574-598.
type ConfigShowCmd struct {
	IncludeDefaults bool `name:"include-defaults" help:"Include fields that fall through to built-in defaults."`
	Canonicalize    bool `name:"canonicalize" help:"Resolve aliased calendar IDs (e.g. \"primary\") to their canonical IDs in the output. Requires gws."`
}

// Run loads + (optionally) canonicalizes config, then emits a single JSON
// line per SPEC line 588.
func (c *ConfigShowCmd) Run(rt *Runtime) error {
	cfg, err := loadConfig(rt)
	if err != nil {
		return err
	}

	body := configShowPayload{
		Settings: settingsPayload{
			PollInterval:         cfg.Settings.PollInterval.Compact(),
			Horizon:              cfg.Settings.Horizon.Compact(),
			FullSyncInterval:     cfg.Settings.FullSyncInterval.Compact(),
			LogLevel:             cfg.Settings.LogLevel,
			LogFormat:            cfg.Settings.LogFormat,
			DryRun:               cfg.Settings.DryRun,
			PropagateTargetEdits: cfg.Settings.PropagateTargetEdits,
		},
	}

	if c.Canonicalize {
		canonical, err := cfg.Canonicalize(rt.Ctx, rt.gwsClient())
		if err != nil {
			return err
		}
		for _, p := range cfg.Pairs {
			body.Pairs = append(body.Pairs, pairPayloadFromCanonical(p, canonical))
		}
	} else {
		for _, p := range cfg.Pairs {
			row := pairPayload{
				Name:    p.Name,
				Source:  p.Source,
				Target:  p.Target,
				Enabled: p.IsEnabled(),
			}
			if p.Horizon != nil {
				row.Horizon = p.Horizon.Compact()
			}
			body.Pairs = append(body.Pairs, row)
		}
	}

	p := rt.printer()
	p.Emit(body)
	p.Meta(metaCount{Count: 1})
	return nil
}

// pairPayloadFromCanonical returns a pairPayload populated from canonical
// resolution: source/target are the canonical IDs (post-primary-expansion).
// Every pdir is a_to_b in v2.0.0+, so the pdir's SourceCalendar and
// TargetCalendar map directly to the pair's source and target.
func pairPayloadFromCanonical(p config.Pair, canonical *config.Canonical) pairPayload {
	source := p.Source
	target := p.Target
	for _, pd := range canonical.PDirs {
		if pd.PairName != p.Name {
			continue
		}
		source = pd.SourceCalendar
		target = pd.TargetCalendar
		break
	}
	row := pairPayload{
		Name:    p.Name,
		Source:  source,
		Target:  target,
		Enabled: p.IsEnabled(),
	}
	if p.Horizon != nil {
		row.Horizon = p.Horizon.Compact()
	}
	return row
}

// ConfigValidateCmd implements `calendar-sync config validate`. SPEC
// §"calendar-sync config validate" lines 599-620.
type ConfigValidateCmd struct{}

// Run loads config, validates it, and emits the SPEC's one-line success
// shape. Validation failures bubble out as config-typed errors that the
// outer Run loop maps to config_invalid + exit 1.
func (c *ConfigValidateCmd) Run(rt *Runtime) error {
	cfg, err := loadConfig(rt)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	canonical, err := cfg.Canonicalize(rt.Ctx, rt.gwsClient())
	if err != nil {
		return err
	}

	pairCount := 0
	for _, p := range cfg.Pairs {
		if p.IsEnabled() {
			pairCount++
		}
	}

	p := rt.printer()
	p.Emit(configValidatePayload{
		Status: "ok",
		Pairs:  pairCount,
		PDirs:  len(canonical.PDirs),
	})
	p.Meta(metaCount{Count: 1})
	return nil
}

// configShowPayload is SPEC line 588's wire shape for the JSON output.
// Settings is a single object; Pairs is the list verbatim from config.
type configShowPayload struct {
	Settings settingsPayload `json:"settings"`
	Pairs    []pairPayload   `json:"pairs"`
}

// settingsPayload mirrors SPEC line 588's settings sub-object. Durations
// are rendered via config.Duration.Compact ("60s", "24h" - whole hours go
// out as "<N>h", everything else as "<N>s") to match SPEC line 588's
// example exactly. Compact is shared with the daemon's IPC status response
// (SPEC line 725) so both wire shapes agree.
type settingsPayload struct {
	PollInterval         string `json:"poll_interval"`
	Horizon              string `json:"horizon"`
	FullSyncInterval     string `json:"full_sync_interval"`
	LogLevel             string `json:"log_level"`
	LogFormat            string `json:"log_format"`
	DryRun               bool   `json:"dry_run"`
	PropagateTargetEdits bool   `json:"propagate_target_edits"`
}

// pairPayload is the wire shape for one pair entry in `config show` and
// `pair list`. v2.0.0 dropped the `direction` field; every pair is
// implicitly source-to-target. The PDir-level direction (always "a_to_b")
// still surfaces in `pair test` output via pairTestPayload.
//
// Horizon surfaces only when the pair set its own override; absence (the
// settings-fallback case) drops the field via omitempty so the wire shape
// matches the user's config exactly.
type pairPayload struct {
	Name    string `json:"name"`
	Source  string `json:"source"`
	Target  string `json:"target"`
	Enabled bool   `json:"enabled"`
	Horizon string `json:"horizon,omitempty"`
}

// configValidatePayload is SPEC line 609's wire shape:
// `{"status":"ok","pairs":2,"pdirs":3}`.
type configValidatePayload struct {
	Status string `json:"status"`
	Pairs  int    `json:"pairs"`
	PDirs  int    `json:"pdirs"`
}

// loadConfig is the shared "find + load + return" path used by every
// subcommand that needs config. Errors map to config_not_found /
// config_invalid via MapError on the way out.
func loadConfig(rt *Runtime) (*config.Config, error) {
	path := config.FindPath(rt.Globals.Config)
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}
