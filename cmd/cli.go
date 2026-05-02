// Package cmd wires the calendar-sync subcommands. The kong CLI struct
// lives here; one file per subcommand contains the flag fields and Run
// method. main.go in cmd/calendar-sync/ is a one-liner that calls Run.
//
// The package is intentionally importable from tests so each subcommand
// can be exercised end-to-end without spawning a subprocess.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/alecthomas/kong"

	"github.com/tammersaleh/calendar-sync/internal/output"
)

// Globals carries the SPEC §"Global Flags" values to every subcommand. kong
// embeds this into CLI; the per-subcommand Run method reads the values via
// the *runtime injected ctx.
type Globals struct {
	Config    string `name:"config" placeholder:"<path>" help:"Path to config.toml. Overrides $CALENDAR_SYNC_CONFIG and the default location." env:"CALENDAR_SYNC_CONFIG"`
	LogLevel  string `name:"log-level" placeholder:"<level>" help:"One of debug, info, warn, error. Overrides settings.log_level." env:"CALENDAR_SYNC_LOG_LEVEL"`
	LogFormat string `name:"log-format" placeholder:"<fmt>" help:"One of json, text. Overrides settings.log_format." env:"CALENDAR_SYNC_LOG_FORMAT"`
	Quiet     bool   `short:"q" name:"quiet" help:"Suppress stdout (logs still go to stderr)."`
	NoColor   bool   `name:"no-color" help:"Disable ANSI colors in text-format logs."`
}

// CLI is the kong root struct. Each cmd:"" field is one subcommand; the
// Globals struct provides every command with the same set of process-wide
// flags.
type CLI struct {
	Globals

	Watch     WatchCmd     `cmd:"" help:"Run the long-running mirror daemon."`
	Run       RunCmd       `cmd:"" help:"Run a one-shot reconcile."`
	Init      InitCmd      `cmd:"" help:"Generate a starter config.toml."`
	Config    ConfigCmd    `cmd:"" help:"Inspect or validate the configuration."`
	Pair      PairCmd      `cmd:"" help:"Inspect or test individual pairs."`
	Mirror    MirrorCmd    `cmd:"" help:"List or prune mirror events."`
	Status    StatusCmd    `cmd:"" help:"Report daemon reachability and per-pdir state."`
	Install   InstallCmd   `cmd:"" help:"Install the launchd agent that runs calendar-sync watch."`
	Uninstall UninstallCmd `cmd:"" help:"Uninstall the launchd agent."`
	Skill     SkillCmd     `cmd:"" help:"Print the Claude Code skill file (SKILL.md)."`
	Version   VersionCmd   `cmd:"" help:"Print version + build metadata."`
}

// Runtime is the dependency-injection bag every subcommand reads from.
// Stdout / Stderr are captured here so tests can swap in bytes.Buffers; the
// other fields are populated by the cmd's Run wrapper before dispatch.
type Runtime struct {
	Stdout  io.Writer
	Stderr  io.Writer
	Globals Globals
	Ctx     context.Context

	// Gws is a hook tests use to inject a stub gws subprocess wrapper.
	// Production-mode gws.New(...) is constructed lazily inside the
	// subcommand methods that need it; tests assign Gws to override.
	Gws GwsClient

	// Logger is the structured-log sink wired from --log-level / --log-format
	// (or the matching settings.toml values). nil is valid: every log call
	// short-circuits before formatting. Subcommand Run methods read this
	// when they want to emit per-step diagnostics.
	Logger *output.Logger
}

// Run is the package's main entry point. main.go calls Run with
// os.Args[1:], os.Stdout, os.Stderr; tests construct a CLI struct directly
// and call its subcommand Run methods so the kong-parsing path is exercised
// minimally.
//
// Returns the SPEC's exit code. kong-side parse errors emit a usage line on
// stderr and surface as exit 64 (SPEC line 398).
func Run(args []string, stdout, stderr io.Writer) int {
	cli := &CLI{}

	parser, err := kong.New(cli,
		kong.Name("calendar-sync"),
		kong.Description("Google Calendar event mirroring tool."),
		kong.Writers(stdout, stderr),
		kong.UsageOnError(),
		kong.Exit(func(int) {}),
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 64
	}

	kctx, err := parser.Parse(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		// kong's parse errors are always usage failures.
		return 64
	}

	rt := &Runtime{
		Stdout:  stdout,
		Stderr:  stderr,
		Globals: cli.Globals,
		Ctx:     signalContext(),
		Logger:  output.NewLogger(stderr, cli.Globals.LogFormat, cli.Globals.LogLevel),
	}
	if cli.Globals.Quiet {
		rt.Stdout = nil
	}

	// kong's Selected().Run(deps...) injects rt as a discovered argument
	// per its dependency-injection contract; each subcommand's Run signature
	// is `Run(rt *Runtime) error`.
	if err := kctx.Run(rt); err != nil {
		return handleErr(stderr, err)
	}
	return 0
}

// signalContext returns a context that is canceled on SIGINT / SIGTERM.
// The daemon (`watch`) uses signal.NotifyContext internally; for
// short-lived commands we still want Ctrl-C to interrupt long-running
// gws-API calls cleanly.
func signalContext() context.Context {
	ctx, _ := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	return ctx
}

// handleErr maps a subcommand's returned error to SPEC's stderr envelope
// and returns the exit code to bubble up to the OS. The returned int is
// always non-zero on a non-nil err.
func handleErr(stderr io.Writer, err error) int {
	// kong-injected usage errors carry their own code; surface as 64.
	var parseErr *kong.ParseError
	if errors.As(err, &parseErr) {
		fmt.Fprintln(stderr, err)
		return 64
	}
	code, detail, hint := MapError(err)
	output.EmitError(stderr, output.ErrorEnvelope{
		Error:  code,
		Detail: detail,
		Hint:   hint,
	})
	return output.ExitCodeFor(code)
}

// printer constructs the per-command JSONL Printer using the runtime's
// stdout (already nil-ed when --quiet was set).
func (rt *Runtime) printer() *output.Printer {
	return &output.Printer{Stdout: rt.Stdout}
}
