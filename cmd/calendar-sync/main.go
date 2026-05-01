// Package main is the entry point for the calendar-sync CLI. The actual
// CLI surface lives in the cmd package next door so the subcommands can be
// unit-tested without spawning a subprocess.
package main

import (
	"os"

	"github.com/tammersaleh/calendar-sync/cmd"
)

func main() {
	os.Exit(cmd.Run(os.Args[1:], os.Stdout, os.Stderr))
}
