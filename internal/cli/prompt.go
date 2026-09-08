package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/raulsh/slk/internal/errdefs"
	"golang.org/x/term"
)

// ErrNotATerminal is returned when a command needs to prompt but has no
// terminal to prompt on.
var ErrNotATerminal = errors.New("standard input is not a terminal")

// prompter reads answers from the caller.
//
// readSecret is a field so a test can exercise the prompting path without a
// pty: the no-echo read is otherwise unfakeable, and the branch that matters
// most - what happens with no terminal - has to be testable.
type prompter struct {
	env        Env
	in         *bufio.Reader
	readSecret func(fd int) ([]byte, error)
}

func newPrompter(env Env) *prompter {
	return &prompter{
		env:        env,
		in:         bufio.NewReader(env.Stdin),
		readSecret: term.ReadPassword,
	}
}

// requireTerminal fails fast when there is nothing to prompt on.
//
// Checking before the first prompt rather than blocking on a read is the whole
// point: an agent invoking `slk configure` with no terminal would otherwise
// hang forever waiting on stdin, which is the failure mode most likely to
// wedge an automated caller.
func (p *prompter) requireTerminal() error {
	f, ok := p.env.Stdin.(*os.File)
	if ok && p.env.IsTTY(f) {
		return nil
	}
	return errdefs.WithHint(
		fmt.Errorf("%w: %w", errdefs.ErrUsage, ErrNotATerminal),
		"configure needs a terminal to prompt for credentials. For a script or an agent,\n"+
			"pass them instead:\n\n"+
			"    slk configure --from-curl -            # paste a DevTools \"Copy as cURL\"\n"+
			"    slk configure --workspace acme --token xoxc-... --cookie xoxd-...\n\n"+
			"or set SLK_WORKSPACE, SLK_TOKEN and SLK_COOKIE and skip configure entirely.")
}

// line reads one visible answer.
func (p *prompter) line(label, def string) (string, error) {
	if def != "" {
		p.printf("%s [%s]: ", label, def)
	} else {
		p.printf("%s: ", label)
	}
	s, err := p.in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read %s: %w", label, err)
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return def, nil
	}
	return s, nil
}

// secret reads one answer without echoing it.
func (p *prompter) secret(label string, optional bool) (string, error) {
	if optional {
		p.printf("%s (blank to skip): ", label)
	} else {
		p.printf("%s: ", label)
	}

	f, ok := p.env.Stdin.(*os.File)
	if !ok {
		return "", p.requireTerminal()
	}
	b, err := p.readSecret(int(f.Fd()))
	p.printf("\n")
	if err != nil {
		return "", fmt.Errorf("read %s: %w", label, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// confirm asks a yes/no question, defaulting to no.
//
// Defaulting to no, and treating a bare Enter as a decline, is the right bias
// for a question that precedes an irreversible public action.
func (p *prompter) confirm(question string) (bool, error) {
	p.printf("%s [y/N]: ", question)
	s, err := p.in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

// printf writes prompt text to stderr, keeping stdout free for results even
// during an interactive session.
func (p *prompter) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(p.env.Stderr, format, args...)
}
