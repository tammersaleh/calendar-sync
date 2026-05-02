package cmd

import (
	"errors"
	"fmt"

	"github.com/tammersaleh/calendar-sync/internal/config"
	"github.com/tammersaleh/calendar-sync/internal/output"
)

// PairCmd is the kong group for `pair list` / `pair test`.
type PairCmd struct {
	List PairListCmd `cmd:"" help:"List configured pairs."`
	Test PairTestCmd `cmd:"" help:"Test a pair (canonicalize + dry-run reconcile)."`
}

// PairListCmd implements `calendar-sync pair list`. SPEC §"calendar-sync
// pair list" lines 622-635.
type PairListCmd struct {
	EnabledOnly bool `name:"enabled-only" help:"Skip pairs with enabled=false."`
}

// Run loads config and emits one JSON line per pair.
func (c *PairListCmd) Run(rt *Runtime) error {
	cfg, err := loadConfig(rt)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	p := rt.printer()
	count := 0
	for _, pair := range cfg.Pairs {
		if c.EnabledOnly && !pair.IsEnabled() {
			continue
		}
		row := pairPayload{
			Name:                 pair.Name,
			Source:               pair.Source,
			Target:               pair.Target,
			Enabled:              pair.IsEnabled(),
			PropagateTargetEdits: pair.PropagateTargetEdits,
		}
		if pair.Horizon != nil {
			row.Horizon = pair.Horizon.Compact()
		}
		p.Emit(row)
		count++
	}
	p.Meta(metaCount{Count: count})
	return nil
}

// PairTestCmd implements `calendar-sync pair test`. SPEC §"calendar-sync
// pair test" lines 637-651: a wrapper around `run --pair <name> --dry-run`
// with an extra canonicalize-and-print step.
//
// Unlike SPEC's other arguments (which use kong's <name> form), pair test
// takes a positional name argument so the user invocation matches SPEC's
// example: `calendar-sync pair test work-personal`.
type PairTestCmd struct {
	Name      string `arg:"" name:"name" help:"Pair name to test."`
	Direction string `name:"direction" placeholder:"<dir>" help:"Limit to one direction. Only a_to_b is currently meaningful."`
}

// Run validates the pair exists, canonicalizes it, then delegates to the
// run command's dry-run path.
func (c *PairTestCmd) Run(rt *Runtime) error {
	cfg, err := loadConfig(rt)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	var found *config.Pair
	for i := range cfg.Pairs {
		if cfg.Pairs[i].Name == c.Name {
			found = &cfg.Pairs[i]
			break
		}
	}
	if found == nil {
		return newCmdError(output.CodePairNotFound,
			fmt.Sprintf("pair %q not found", c.Name), "", errors.New("no matching pair"))
	}

	canonical, err := cfg.Canonicalize(rt.Ctx, rt.gwsClient())
	if err != nil {
		return err
	}

	// Emit one canonicalized-pair sanity line per pdir under this pair.
	p := rt.printer()
	count := 0
	for _, pd := range canonical.PDirs {
		if pd.PairName != c.Name {
			continue
		}
		if c.Direction != "" && pd.Direction != c.Direction {
			continue
		}
		p.Emit(pairTestPayload{
			Name:           pd.PairName,
			Direction:      pd.Direction,
			SourceCalendar: pd.SourceCalendar,
			TargetCalendar: pd.TargetCalendar,
			SourceWritable: pd.SourceWritable,
		})
		count++
	}

	// Delegate to the run command's dry-run path.
	runCmd := &RunCmd{
		Pair:      []string{c.Name},
		Direction: c.Direction,
		DryRun:    true,
	}
	if err := runCmd.run(rt, canonical, &count); err != nil {
		return err
	}
	p.Meta(metaCount{Count: count})
	return nil
}

// pairTestPayload is the per-pdir sanity line emitted by `pair test`. The
// fields surface the canonical IDs SPEC line 639 calls out for the
// "extra canonicalize-and-print step".
type pairTestPayload struct {
	Name           string `json:"name"`
	Direction      string `json:"direction"`
	SourceCalendar string `json:"source_calendar"`
	TargetCalendar string `json:"target_calendar"`
	SourceWritable bool   `json:"source_writable"`
}
