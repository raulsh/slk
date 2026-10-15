// Package cli wires the cobra command tree.
//
// It is an adapter: cobra's flag plumbing and help generation live here and
// nowhere else, so every other package stays free of the CLI framework and is
// testable without building a command.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/raulsh/slk/internal/errdefs"
	"github.com/raulsh/slk/internal/slack"
	"golang.org/x/term"
)

// Env is the process environment a command runs in, injected rather than
// reached for.
//
// Execute takes this and returns an exit status instead of touching os.Stdout
// and calling os.Exit, which is what makes the whole CLI - flag wiring, format
// selection, write gating, exit codes - testable in-process. cmd/slk/main.go
// is the only thing that exits.
type Env struct {
	Args           []string
	Stdout, Stderr io.Writer
	Stdin          io.Reader

	// IsTTY is a field, not a call, because it is otherwise unfakeable and
	// both branches of the write-confirmation and no-echo-prompt paths have to
	// be tested. Nothing is a terminal in a test, which is also an agent's
	// case, so the test default and the production-risk case coincide.
	IsTTY func(f *os.File) bool

	Version string
	Commit  string

	// slackOptions configure the Slack client, which is how a test points a
	// whole command at an httptest server. Unexported on purpose: anything
	// that redirects the API base redirects the credentials with it, so no
	// flag or environment variable may reach this.
	slackOptions []slack.Option
}

func (e Env) withDefaults() Env {
	if e.Stdout == nil {
		e.Stdout = os.Stdout
	}
	if e.Stderr == nil {
		e.Stderr = os.Stderr
	}
	if e.Stdin == nil {
		e.Stdin = os.Stdin
	}
	if e.IsTTY == nil {
		e.IsTTY = func(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }
	}
	if e.Version == "" {
		e.Version = "dev"
	}
	if e.Commit == "" {
		e.Commit = "none"
	}
	return e
}

// Execute runs the command tree and returns the process exit status.
func Execute(ctx context.Context, env Env) int {
	env = env.withDefaults()

	g := &globals{env: env}
	// Commands receive the context through cmd.Context(), which
	// ExecuteContext below propagates; the constructors only build the tree.
	root := newRoot(g) //nolint:contextcheck // context arrives via ExecuteContext
	root.SetArgs(env.Args)
	root.SetOut(env.Stdout)
	root.SetErr(env.Stderr)
	root.SetIn(env.Stdin)

	err := root.ExecuteContext(ctx)
	if err == nil {
		return errdefs.ExitOK
	}

	f := errdefs.Classify(err)

	// Cobra rejects an unresolvable command or bad positional arguments before
	// any of our hooks run, so an unclassified failure with no command
	// recorded is by construction an invocation problem rather than an
	// internal one. Testing for the hook having run keeps this out of the
	// business of matching cobra's error strings.
	if f.Code == errdefs.CodeInternal && g.cmd == nil {
		f.Code = errdefs.CodeUsage
	}

	// The writer exists once flag parsing has succeeded. Before that there is
	// no --json to honour, so stderr is the only channel available.
	if g.out != nil {
		if ferr := g.out.Fail(f); ferr != nil {
			note(env.Stderr, "error: "+ferr.Error())
		}
	} else {
		note(env.Stderr, "error: "+f.Message)
	}

	// Usage text comes last, after the error, because the reason is what the
	// reader needs first and the usage block is long enough to scroll it away.
	if f.Code == errdefs.CodeUsage {
		if c := g.cmd; c != nil {
			note(env.Stderr, "")
			note(env.Stderr, strings.TrimRight(c.UsageString(), "\n"))
		}
	}
	return f.Exit()
}

// note writes one line to the human channel.
//
// A failed write to stderr has no recovery path, so the error is discarded
// deliberately and in one place rather than silently at every call site.
func note(w io.Writer, line string) {
	_, _ = fmt.Fprintln(w, line)
}
