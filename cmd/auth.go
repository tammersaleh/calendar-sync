package cmd

import (
	"context"
	"fmt"
	"os/exec"
)

// runGwsAuthStatus shells out to `gws auth status`. Returns nil on exit 0,
// non-nil error otherwise. The daemon and run subcommands wire this as
// AuthChecker; tests assign a stub to the package-level AuthChecker var.
func runGwsAuthStatus(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "gws", "auth", "status")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gws auth status: %w (output: %s)", err, string(out))
	}
	return nil
}
