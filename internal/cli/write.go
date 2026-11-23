package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/raulsh/slk/internal/errdefs"
)

// The checks every write command runs, in the order it runs them:
//
//  1. the invocation, with no I/O at all;
//  2. gateWrite: the credential owner's standing allow_writes;
//  3. requireConsent: the caller's consent for this one call;
//  4. resolution, which only reads;
//  5. confirmWrite, or the --dry-run rehearsal in its place;
//  6. the write, exactly once.
//
// Steps 2 and 3 come before any network call, so a refused agent costs nothing
// and learns why at once. They guard against accidents rather than attackers:
// anything running as the user can read the credentials anyway.

// maxTextRunes is where Slack starts truncating a message's text. Refusing
// past it beats a post that silently loses its tail.
const maxTextRunes = 40000

// maxBodyBytes bounds a stdin read: the longest legal message in the widest
// UTF-8 encoding, plus one byte to tell "exactly at the limit" from "over it".
const maxBodyBytes = maxTextRunes*utf8.UTFMax + 1

// gateWrite enforces the standing consent of the person who owns the
// credentials.
//
// It applies to --dry-run as well, so a rehearsal exits with the code the real
// run would: an agent that rehearses first learns that writes are off before it
// relies on them.
func (g *globals) gateWrite(s *session) error {
	if s.cfg.WritesAllowed(s.profile) {
		return nil
	}
	return errdefs.WithHint(
		fmt.Errorf("%w: writes are disabled for profile %q; nothing was sent", errdefs.ErrWriteRefused, s.name),
		fmt.Sprintf(`slk only writes to Slack once the person who owns these credentials turns writes
on, so an automated caller cannot post as you by accident. To turn them on, set

    allow_writes: true

in %s, at the top level for every profile or under
profiles.%s for this one, or export SLK_ALLOW_WRITES=1 for a single session.`, s.cfg.Path(), s.name))
}

// requireConsent fails fast when nobody can say yes to this particular write.
//
// --yes is the per-call consent. Without it the only other is a person at a
// terminal, and stdin must then not be carrying the message body, or there
// would be nothing left to read the answer from. A rehearsal needs no consent,
// because it sends nothing.
func (g *globals) requireConsent(bodyFromStdin bool) error {
	if g.yes || g.dryRun {
		return nil
	}
	if g.stdinIsTerminal() && !bodyFromStdin {
		return nil
	}
	return errdefs.WithHint(
		fmt.Errorf("%w: no --yes and no terminal to confirm on; nothing was sent", errdefs.ErrWriteRefused),
		"A write needs explicit consent on every call. From a script or an agent, pass --yes;\n"+
			"rehearse first with --dry-run to see exactly what would be sent.")
}

func (g *globals) stdinIsTerminal() bool {
	f, ok := g.env.Stdin.(*os.File)
	return ok && g.env.IsTTY(f)
}

// confirmWrite asks the person at the terminal, defaulting to no. requireConsent
// has already established that there is a terminal whenever this would ask.
func (g *globals) confirmWrite(preview, question string) error {
	if g.yes || g.dryRun {
		return nil
	}
	p := newPrompter(g.env)
	if preview != "" {
		p.printf("%s\n", preview)
	}
	ok, err := p.confirm(question)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: declined at the confirmation prompt; nothing was sent", errdefs.ErrWriteRefused)
	}
	return nil
}

// literal escapes the three characters Slack reads as markup, which is all its
// formatting rules ask a sender to escape.
var literal = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// broadcastMention finds the mentions that notify a whole conversation or a
// user group rather than one person.
var broadcastMention = regexp.MustCompile(`<!(?:here|channel|everyone)[|>]|<!subteam\^[^>]*>`)

// messageText turns what the caller gave into what is sent, applying every
// rule that needs no network.
//
// By default the text is made literal. Text a model writes is full of &, < and
// > - a < b, R&D, a code sample - and none of it is meant as Slack markup,
// which would otherwise turn <T> into a broken link and swallow it. *bold*,
// `code`, fences, lists and bare URLs use none of the three, so they still
// render. With markup the text is sent as Slack wire format, which is what a
// mention needs and what slk's own "text" field already is - but then a
// mention that would notify everyone in a conversation needs its own flag, so
// an agent quoting a message that contained one cannot page the channel again.
func messageText(raw string, markup, allowBroadcast bool) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errdefs.Usagef("the message is empty")
	}
	if !utf8.ValidString(raw) {
		return "", errdefs.Usagef("the message is not valid UTF-8")
	}

	text := raw
	if markup {
		if m := broadcastMention.FindString(raw); m != "" && !allowBroadcast {
			return "", errdefs.WithHint(
				fmt.Errorf("%w: the message contains %s, which notifies everyone it reaches; nothing was sent",
					errdefs.ErrWriteRefused, strings.TrimRight(m, "|>")+">"),
				"Pass --allow-broadcast if notifying the whole conversation or group is intended.")
		}
	} else {
		text = literal.Replace(raw)
	}

	if n := utf8.RuneCountInString(text); n > maxTextRunes {
		return "", errdefs.Usagef("the message is %d characters as sent; Slack truncates past %d", n, maxTextRunes)
	}
	return text, nil
}

// readBody reads the message from stdin, for a caller that passed "-".
//
// It is bounded twice: by size, so a runaway pipe cannot exhaust memory, and
// by the command's deadline, so a pipe that is never closed - an agent harness
// that leaves stdin open is the usual case - fails as a timeout instead of
// hanging. The read is abandoned rather than interrupted on a timeout, which
// is harmless in a process about to exit.
func (g *globals) readBody(ctx context.Context) (string, error) {
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(g.env.Stdin, maxBodyBytes))
		done <- result{data, err}
	}()

	select {
	case <-ctx.Done():
		return "", fmt.Errorf("read the message from stdin: %w", ctx.Err())
	case r := <-done:
		if r.err != nil {
			return "", fmt.Errorf("read the message from stdin: %w", r.err)
		}
		if len(r.data) >= maxBodyBytes {
			return "", errdefs.Usagef("the message on stdin is longer than Slack's %d-character limit", maxTextRunes)
		}
		// A pipe almost always ends in one newline nobody meant to send.
		s := strings.TrimSuffix(string(r.data), "\n")
		return strings.TrimSuffix(s, "\r"), nil
	}
}

// uncertain marks a failed write whose outcome is unknown.
//
// A Slack error response proves nothing was written. A dropped connection, a
// server error or a timeout proves nothing either way: the request may have
// been processed before the failure. Retrying blindly is the usual advice for
// those, and for a post it is exactly wrong, so the error says what to check.
func uncertain(err error, check string) error {
	if errors.Is(err, errdefs.ErrTransport) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled) {
		return errdefs.WithHint(err,
			"The request may have reached Slack before this failed, so the write may have happened.\n"+
				"Check with "+check+" before trying again: a blind retry can post twice.")
	}
	return err
}
