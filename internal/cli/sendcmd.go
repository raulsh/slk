package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/raulsh/slk/internal/errdefs"
	"github.com/raulsh/slk/internal/output"
	"github.com/raulsh/slk/internal/resolve"
	"github.com/raulsh/slk/internal/slack"
	"github.com/raulsh/slk/internal/slacktime"
	"github.com/spf13/cobra"
)

type sendOpts struct {
	channel        string
	dm             string
	thread         string
	markup         bool
	allowBroadcast bool
}

func newSendCmd(g *globals) *cobra.Command {
	var o sendOpts

	cmd := &cobra.Command{
		Use:   "send (--channel <channel> | --dm <user> | --thread <permalink>) <text | ->",
		Short: "Post a message to a channel, a DM or a thread",
		Long: `Post a message as you.

Writes are off until the person who owns the credentials sets allow_writes: true
(or SLK_ALLOW_WRITES=1), and every call needs --yes unless you confirm at a
terminal. --dry-run shows exactly what would be sent and sends nothing.

Pick the destination with --channel or --dm. To reply in a thread, pass --thread
with the permalink of any message in it - the parent or a reply, both land in
the same thread - or --thread <ts> together with --channel or --dm. Every
message slk prints carries its permalink, channel and ts, so whatever an agent
reads it can answer in place.

The text is posted literally: & < and > are escaped, so code and comparisons
arrive as written, while *bold*, lists and links still render. With --markup it
is sent as Slack markup instead, for mentions (<@U0123ABCD>) and labelled links,
or to send a "text" field from slk's own JSON back verbatim. Mentions that
notify a whole conversation (<!here>, <!channel>, <!everyone>, a user group)
also need --allow-broadcast.

Give the text as one quoted argument, or - to read it from stdin. Text that
starts with "-" needs "--" in front of it.

Exit 6 - a network failure or a timeout - does not prove the message was not
posted. Check with slk messages or slk thread before sending it again.`,
		Example: `  slk send --channel eng "deploy finished"
  slk send --dm @alice "lunch?"
  slk send --thread https://acme.slack.com/archives/C0123ABCD/p1725806400123456 "on it"
  slk send --channel eng --thread 1725806400.123456 "on it"
  slk send --channel eng --yes - < note.md
  slk send --channel eng --markup "<@U0123ABCD> can you look?"`,
		// Arguments are checked in RunE rather than by cobra, which validates
		// them before the output writer exists and would answer a --json
		// caller with prose.
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runSend(cmd, args, &o)
		},
	}

	f := cmd.Flags()
	f.StringVar(&o.channel, "channel", "", "channel by exact name or id")
	f.StringVar(&o.dm, "dm", "", "person by @handle or user id; opens a DM if you have none")
	f.StringVar(&o.thread, "thread", "", "reply in a thread: a message permalink, or a ts with --channel or --dm")
	f.BoolVar(&o.markup, "markup", false, "send the text as Slack markup instead of escaping it")
	f.BoolVar(&o.allowBroadcast, "allow-broadcast", false,
		"with --markup, permit <!here>, <!channel>, <!everyone> and user-group mentions")
	return cmd
}

// sendTarget is the destination as typed: validated, not yet resolved.
type sendTarget struct {
	channel  string
	dm       string
	threadTS string
}

// destination is where a post lands. channel is empty only while a DM that a
// real run would open does not exist yet.
type destination struct {
	channel string
	name    string
	userID  string
}

func (g *globals) runSend(cmd *cobra.Command, args []string, o *sendOpts) error {
	if len(args) != 1 {
		return errdefs.Usagef("give the message as exactly one quoted argument, or - to read stdin (got %d)", len(args))
	}
	if o.allowBroadcast && !o.markup {
		return errdefs.Usagef("--allow-broadcast only applies with --markup; escaped text cannot mention anyone")
	}
	t, err := parseSendTarget(o)
	if err != nil {
		return err
	}

	ctx, cancel := g.deadline(cmd.Context())
	defer cancel()

	raw, fromStdin := args[0], args[0] == "-"
	if fromStdin {
		if raw, err = g.readBody(ctx); err != nil {
			return err
		}
	}
	text, err := messageText(raw, o.markup, o.allowBroadcast)
	if err != nil {
		return err
	}

	s, err := g.newSession()
	if err != nil {
		return err
	}
	if err := g.gateWrite(s); err != nil {
		return err
	}
	if err := g.requireConsent(fromStdin); err != nil {
		return err
	}

	d, err := g.directory(ctx, s)
	if err != nil {
		return err
	}
	dest, err := g.resolveDestination(ctx, s, d, t)
	if err != nil {
		return err
	}

	root := ""
	if t.threadTS != "" {
		if dest.channel == "" {
			return fmt.Errorf("%w: there is no DM with %s yet, so it has no thread %s",
				errdefs.ErrNotFound, dest.name, t.threadTS)
		}
		if root, err = s.client.ThreadRoot(ctx, dest.channel, t.threadTS); err != nil {
			return err
		}
	}

	rec := output.Sent{DryRun: g.dryRun, Channel: dest.channel, ChannelName: dest.name, ThreadTS: root, Text: text}
	where := dest.name
	if root != "" {
		where += " in thread " + root
	}

	if g.dryRun {
		if dest.channel == "" {
			g.out.Info(fmt.Sprintf("there is no DM with %s yet; a real run would open one", dest.name))
		}
		g.out.Info("dry run: nothing was sent")
		return g.out.Sent(rec)
	}
	if err := g.confirmWrite(indent(text), fmt.Sprintf("Send to %s?", where)); err != nil {
		return err
	}

	if dest.channel == "" {
		if dest.channel, err = s.client.OpenDM(ctx, dest.userID); err != nil {
			return err
		}
	}
	// A deadline that has already passed would fail inside the request anyway,
	// but here it is still certain that nothing was sent.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w before sending; nothing was sent", err)
	}
	m, err := s.client.PostMessage(ctx, slack.PostOptions{Channel: dest.channel, Text: text, ThreadTS: root})
	if err != nil {
		check := fmt.Sprintf("`slk messages --channel %s --since 10m`", dest.channel)
		if root != "" {
			check = fmt.Sprintf("`slk thread %s %s`", dest.channel, root)
		}
		return uncertain(err, check)
	}

	rec.Channel, rec.TS, rec.ThreadTS, rec.Permalink = m.Channel, m.TS, m.ThreadTS, m.Permalink
	return g.out.Sent(rec)
}

// parseSendTarget validates the destination flags without touching the
// network.
func parseSendTarget(o *sendOpts) (sendTarget, error) {
	var t sendTarget
	if o.thread != "" {
		if ch, ts, _, err := resolve.ParsePermalink(o.thread); err == nil {
			if o.channel != "" || o.dm != "" {
				return t, errdefs.Usagef("--thread is a permalink, which already names its channel; drop --channel and --dm")
			}
			// The message's own ts, even when the link carries its thread's:
			// ThreadRoot resolves either, and proves the message exists.
			return sendTarget{channel: ch, threadTS: ts}, nil
		}
		ts, err := slacktime.ParseStrict(o.thread)
		if err != nil {
			return t, errdefs.Usagef("--thread: %s", err)
		}
		t.threadTS = ts.String()
	}

	switch {
	case o.channel != "" && o.dm != "":
		return t, errdefs.Usagef("give --channel or --dm, not both")
	case o.channel == "" && o.dm == "":
		return t, errdefs.Usagef("give a destination: --channel <name>, --dm <@user>, or --thread <permalink>")
	}
	if o.channel != "" {
		if _, _, _, err := resolve.ParsePermalink(o.channel); err == nil {
			return t, errdefs.Usagef("--channel got a message permalink; to reply in its thread use --thread %s", o.channel)
		}
		if resolve.IsID(o.channel) && (o.channel[0] == 'U' || o.channel[0] == 'W') {
			return t, errdefs.Usagef("--channel %s is a user id; to message a person use --dm %s", o.channel, o.channel)
		}
	}
	t.channel, t.dm = o.channel, o.dm
	return t, nil
}

func (g *globals) resolveDestination(ctx context.Context, s *session, d *resolve.Directory, t sendTarget) (destination, error) {
	if t.dm != "" {
		return g.resolveDM(ctx, s, d, t.dm)
	}
	id, err := d.ChannelExact(t.channel)
	if err != nil {
		return destination{}, err
	}
	name, ok := d.Name(id)
	if !ok {
		name = id
	}
	return destination{channel: id, name: name}, nil
}

// resolveDM finds the DM with a person. When there is none it returns the
// person alone, and only a real run opens one.
func (g *globals) resolveDM(ctx context.Context, s *session, d *resolve.Directory, spec string) (destination, error) {
	spec = strings.TrimSpace(spec)
	if resolve.IsID(spec) && spec[0] == 'D' {
		name, ok := d.Name(spec)
		if !ok {
			name = spec
		}
		return destination{channel: spec, name: name}, nil
	}

	uid, err := d.User(spec)
	if err != nil {
		// The name cache only knows people slk has already shown. A DM
		// partner is the likeliest target, and learning all of them costs
		// the same batched lookup `conversations list` makes.
		g.warmDMPartners(ctx, s, d)
		if uid, err = d.User(spec); err != nil {
			return destination{}, err
		}
	}
	if uid[0] != 'U' && uid[0] != 'W' {
		return destination{}, errdefs.Usagef("--dm %s is not a person; give an @handle or a user id (U…)", spec)
	}

	name := "@" + uid
	if n, ok := d.Name(uid); ok {
		name = "@" + n
	}
	if id, err := d.DM(uid); err == nil {
		return destination{channel: id, name: name, userID: uid}, nil
	}
	return destination{name: name, userID: uid}, nil
}

// warmDMPartners learns the names of everyone the user has a DM with.
func (g *globals) warmDMPartners(ctx context.Context, s *session, d *resolve.Directory) {
	var ids []string
	for _, c := range d.Conversations {
		if c.Type == "dm" && c.User != "" {
			ids = append(ids, c.User)
		}
	}
	if err := d.EnsureUsers(ctx, fetcher{s.client}, ids); err != nil {
		g.log.Debug("could not resolve dm partners", "error", err)
	}
	if err := d.Save(); err != nil {
		g.log.Debug("could not save cache", "error", err)
	}
}

// indent prefixes every line of a preview so it reads as quoted, not as prose.
func indent(s string) string {
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}
