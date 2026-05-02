// Package gws wraps the `gws` CLI as a subprocess. Every Calendar API
// operation calendar-sync needs flows through here; nothing else in the
// codebase knows that gws (or any external binary) exists. The single test
// boundary for the project lives at this layer - see internal/testhelpers
// for the fake-gws harness.
package gws

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
)

// Logger is the minimal slice of *output.Logger the gws client consumes,
// re-declared here to avoid an import cycle (output depends on nothing in
// this layer; gws is the lowest layer). Production code passes
// *output.Logger which satisfies this interface naturally.
//
// A nil Logger is valid: every log call short-circuits before formatting,
// so callers (tests in particular) can leave it unset without ceremony.
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// Client invokes gws as a subprocess. Construct with New; methods correspond
// one-to-one to the Calendar API calls SPEC.md uses.
type Client struct {
	binPath string
	log     Logger
}

// Option configures a Client.
type Option func(*Client)

// WithBinary overrides the path to the gws binary. Default is "gws", which
// relies on PATH; tests use this with a fully-qualified path to the fake.
func WithBinary(path string) Option {
	return func(c *Client) { c.binPath = path }
}

// WithLogger wires a structured logger for per-call diagnostics. Every
// Events* method emits one debug line at entry with the params shape; nil
// (the default) silences all log output. SPEC §"Output and Logging" defines
// the level + format vocabulary the layer-7 daemon configures upstream.
func WithLogger(l Logger) Option {
	return func(c *Client) { c.log = l }
}

// New returns a Client. Without options it invokes gws by name, picking up
// whatever is first on PATH.
func New(opts ...Option) *Client {
	c := &Client{binPath: "gws"}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// debug is a nil-safe wrapper around Logger.Debug. Centralizing the nil
// check keeps the per-method log call sites uniform.
func (c *Client) debug(msg string, args ...any) {
	if c.log != nil {
		c.log.Debug(msg, args...)
	}
}

// execute runs `<binPath> <args...>` and returns stdout, stderr, exit code,
// and a wrapping error reserved for non-exit failures: binary missing,
// signal-killed by the context, or any other launch-time failure. A non-zero
// exit code is NOT a Go error here - callers must inspect exitCode and
// stderr to map to the SPEC's error taxonomy. That mapping lives in
// errors.go (added in a later commit); for now the per-method wrappers do
// their own minimal checks.
//
// When ctx is canceled or its deadline exceeded, the returned err wraps
// ctx.Err() so callers can errors.Is(err, context.Canceled) or
// context.DeadlineExceeded. The exit code in that case is reported as -1.
func (c *Client) execute(ctx context.Context, args []string) (stdout, stderr []byte, exitCode int, err error) {
	cmd := exec.CommandContext(ctx, c.binPath, args...)

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := cmd.Run()
	stdout = outBuf.Bytes()
	stderr = errBuf.Bytes()

	if runErr == nil {
		return stdout, stderr, 0, nil
	}

	// Context errors take precedence over ExitError: when the context kills
	// the subprocess, os/exec surfaces the kill as an ExitError with a
	// signal-derived exit code, but the user-meaningful error is "your
	// context fired". Without this check, errors.Is(err, context.Canceled)
	// is always false for a context that fired after the subprocess started.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return stdout, stderr, -1, fmt.Errorf("gws subprocess: %w", ctxErr)
	}

	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return stdout, stderr, exitErr.ExitCode(), nil
	}

	if errors.Is(runErr, fs.ErrNotExist) || isExecNotFound(runErr) {
		// Wrap with the typed sentinel so the cmd layer's MapError routes
		// this to gws_not_found (SPEC line 518) instead of falling through
		// to api_invalid_request. The underlying os/exec error stays
		// reachable via errors.Unwrap for log enrichment.
		return stdout, stderr, -1, &Error{
			Code:     CodeGWSNotFound,
			ExitCode: 1,
			Op:       "gws subprocess launch",
			Cause:    fmt.Sprintf("gws binary not found at %q: %s", c.binPath, runErr),
		}
	}

	return stdout, stderr, -1, fmt.Errorf("gws subprocess failed: %w", runErr)
}

// isExecNotFound recognizes the "executable file not found in $PATH" error
// returned by os/exec when the binary lookup fails. exec.Error wraps
// fs.ErrNotExist via Unwrap, but only on Go versions where the wrapping was
// added; this guard catches both.
func isExecNotFound(err error) bool {
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		return errors.Is(execErr.Err, exec.ErrNotFound) || errors.Is(execErr, fs.ErrNotExist)
	}
	return false
}
