// Command slk reads Slack from the command line as you, so an agent can shell
// out to it.
package main

import (
	"context"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/raulsh/slk/internal/cli"
)

// Build metadata, set via -ldflags.
var (
	version = "dev"
	commit  = "none"
)

func main() {
	// Signals and exiting are process concerns, so they live here and nowhere
	// else: cli.Execute takes a context and returns a status.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	v, c := buildInfo()
	os.Exit(cli.Execute(ctx, cli.Env{
		Args:    os.Args[1:],
		Version: v,
		Commit:  c,
	}))
}

// buildInfo prefers the ldflags values and falls back to the module's own
// recorded version.
//
// The fallback exists because a CLI is very often `go install`ed rather than
// built through the Makefile, and reporting "dev" for a tagged install is a
// bug in a tool whose output an agent may record for traceability.
func buildInfo() (string, string) {
	v, c := version, commit
	if v != "dev" {
		return v, c
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return v, c
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		v = info.Main.Version
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			c = s.Value
			if len(c) > 7 {
				c = c[:7]
			}
		}
	}
	return v, c
}
