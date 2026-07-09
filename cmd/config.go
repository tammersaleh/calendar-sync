package cmd

import (
	"os"

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
		// Canonical feeds already dropped disabled entries and resolved the
		// target ref to its canonical ID; RedactedURL keeps the secret out.
		body.Feeds = feedPayloadsFromCanonical(canonical.Feeds)
	} else {
		for _, p := range cfg.Pairs {
			row := pairPayload{
				Name:                 p.Name,
				Source:               p.Source,
				Target:               p.Target,
				Enabled:              p.IsEnabled(),
				PropagateTargetEdits: p.PropagateTargetEdits,
			}
			if p.Horizon != nil {
				row.Horizon = p.Horizon.Compact()
			}
			body.Pairs = append(body.Pairs, row)
		}
		body.Feeds = feedPayloadsFromConfig(cfg.Feeds)
	}

	p := rt.printer()
	p.Emit(body)
	p.Meta(metaCount{Count: 1})
	return nil
}

// pairPayloadFromCanonical returns a pairPayload populated from canonical
// resolution: source/target are the canonical IDs (post-primary or
// post-summary expansion). Every pdir is a_to_b in v2.0.0+, so the pdir's
// SourceCalendar and TargetCalendar map directly to the pair's source and
// target. The wire shape collapses summary-form refs to their canonical ID
// in the canonicalize=true output: that's the entire point of the flag.
func pairPayloadFromCanonical(p config.Pair, canonical *config.Canonical) pairPayload {
	source := p.Source
	target := p.Target
	for _, pd := range canonical.PDirs {
		if pd.PairName != p.Name {
			continue
		}
		source = config.CalendarRef{ID: pd.SourceCalendar}
		target = config.CalendarRef{ID: pd.TargetCalendar}
		break
	}
	row := pairPayload{
		Name:                 p.Name,
		Source:               source,
		Target:               target,
		Enabled:              p.IsEnabled(),
		PropagateTargetEdits: p.PropagateTargetEdits,
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
	Feeds    []feedPayload   `json:"feeds,omitempty"`
}

// feedPayload is the wire shape for one [[feeds]] entry in `config show`.
// URL is ALWAYS the redacted rendering (scheme://host/<redacted>): the feed
// URL is a bearer secret and the raw value must never reach stdout. Target is
// the raw ref (non-canonicalize) or the canonical calendar ID (canonicalize).
type feedPayload struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Target string `json:"target"`
}

// feedPayloadsFromCanonical renders resolved canonical feeds (already
// enabled-filtered, target resolved to a canonical ID). RedactedURL is the
// only URL form that leaves the process.
func feedPayloadsFromCanonical(feeds []config.CanonicalFeed) []feedPayload {
	if len(feeds) == 0 {
		return nil
	}
	out := make([]feedPayload, 0, len(feeds))
	for _, f := range feeds {
		out = append(out, feedPayload{
			Name:   f.Name,
			URL:    f.RedactedURL(),
			Target: f.TargetCalendar,
		})
	}
	return out
}

// feedPayloadsFromConfig renders raw config feeds for the non-canonicalize
// path (no gws call). Disabled feeds are dropped to match the canonical
// output. The URL secret - inline or resolved from url_env - is redacted via
// CanonicalFeed.RedactedURL before it can reach stdout.
func feedPayloadsFromConfig(feeds []config.Feed) []feedPayload {
	var out []feedPayload
	for _, f := range feeds {
		if !f.IsEnabled() {
			continue
		}
		raw := f.URL
		if raw == "" && f.URLEnv != "" {
			raw = os.Getenv(f.URLEnv)
		}
		out = append(out, feedPayload{
			Name:   f.Name,
			URL:    config.CanonicalFeed{URL: raw}.RedactedURL(),
			Target: feedTargetString(f.Target),
		})
	}
	return out
}

// feedTargetString renders a raw target ref as a string for feedPayload.Target
// (which is a plain string, not a CalendarRef). Summary-form refs render as
// their summary; ID-form refs as their ID.
func feedTargetString(r config.CalendarRef) string {
	if r.IsSummaryRef() {
		return r.Summary
	}
	return r.ID
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
// Source and Target use config.CalendarRef so the JSON wire shape mirrors
// the user's TOML: ID-form refs marshal as plain strings (backwards-
// compatible with the SPEC examples and pre-F1 output), summary-form refs
// marshal as objects. See CalendarRef.MarshalJSON.
//
// Horizon surfaces only when the pair set its own override; absence (the
// settings-fallback case) drops the field via omitempty so the wire shape
// matches the user's config exactly.
//
// PropagateTargetEdits is *bool (not bool) so omitempty distinguishes
// "absent" (nil → settings fallback) from "explicit false" (&false → the
// operator turned the gate off for this pair specifically). A plain bool
// with omitempty would also drop `false`, silently demoting a deliberate
// per-pair override back to the fallback.
type pairPayload struct {
	Name                 string             `json:"name"`
	Source               config.CalendarRef `json:"source"`
	Target               config.CalendarRef `json:"target"`
	Enabled              bool               `json:"enabled"`
	Horizon              string             `json:"horizon,omitempty"`
	PropagateTargetEdits *bool              `json:"propagate_target_edits,omitempty"`
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
