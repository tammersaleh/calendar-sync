package launchd

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
)

// Runner invokes a `launchctl` command and returns its stdout, stderr, and
// exit code. The interface is the only test seam for install/uninstall:
// production wires ExecRunner, tests pass a hand-rolled stub that records
// invocations and returns canned exit codes.
//
// A non-zero exit code is reported via the exitCode return, NOT the err
// return. err is reserved for "couldn't launch the subprocess at all"
// failures (binary missing, fork failed, context canceled). This matches
// the convention used by internal/gws.
type Runner interface {
	Run(ctx context.Context, args ...string) (stdout, stderr []byte, exitCode int, err error)
}

// ExecRunner is the production Runner: shells out to `launchctl <args...>`.
type ExecRunner struct {
	// Path overrides the launchctl binary location. Empty defaults to
	// "launchctl" (resolved against PATH).
	Path string
}

// Run executes launchctl with args. Per the Runner contract, a non-zero
// launchctl exit returns (stdout, stderr, exitCode, nil). Only launch-time
// failures or context cancellation populate err.
func (r ExecRunner) Run(ctx context.Context, args ...string) (stdout, stderr []byte, exitCode int, err error) {
	bin := r.Path
	if bin == "" {
		bin = "launchctl"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	stdout = outBuf.Bytes()
	stderr = errBuf.Bytes()
	if runErr == nil {
		return stdout, stderr, 0, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return stdout, stderr, -1, ctxErr
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return stdout, stderr, exitErr.ExitCode(), nil
	}
	return stdout, stderr, -1, runErr
}
