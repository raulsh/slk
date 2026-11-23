package cli

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/raulsh/slk/internal/errdefs"
	"github.com/raulsh/slk/internal/output"
	"github.com/raulsh/slk/internal/resolve"
	"github.com/raulsh/slk/internal/slacktime"
	"github.com/spf13/cobra"
)

func newReactCmd(g *globals) *cobra.Command {
	var remove bool

	cmd := &cobra.Command{
		Use:   "react <permalink> <emoji> | <channel> <ts> <emoji>",
		Short: "Add or remove an emoji reaction on a message",
		Long: `Add an emoji reaction to a message as you, or remove one with --remove.

Writes are off until the person who owns the credentials sets allow_writes: true
(or SLK_ALLOW_WRITES=1), and every call needs --yes unless you confirm at a
terminal. --dry-run shows what would change and changes nothing.

The message is a permalink, or a channel and a ts - the same forms slk thread
takes and every message slk prints carries. The emoji is its Slack name, with
or without colons: eyes, :eyes:, +1, thumbsup::skin-tone-2. Write -1 as :-1:,
since a bare -1 reads as a flag.

Reacting is idempotent. A reaction that is already there, or one being removed
that is already gone, succeeds with "changed": false, so retrying after a
failure is safe.`,
		Example: `  slk react https://acme.slack.com/archives/C0123ABCD/p1725806400123456 eyes
  slk react eng 1725806400.123456 :white_check_mark:
  slk react --remove https://acme.slack.com/archives/C0123ABCD/p1725806400123456 eyes`,
		// Checked in RunE, for the same reason as send: cobra validates
		// arguments before the writer exists, and a --json caller would get
		// prose.
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runReact(cmd, args, remove)
		},
	}

	cmd.Flags().BoolVar(&remove, "remove", false, "remove the reaction instead of adding it")
	return cmd
}

// emojiName is a Slack emoji name, with an optional skin tone. Checking it
// here turns a pasted 👍 into a usage error that says what to write instead
// of Slack's bare invalid_name.
var emojiName = regexp.MustCompile(`^[a-z0-9_+'-]{1,100}(::skin-tone-[2-6])?$`)

func (g *globals) runReact(cmd *cobra.Command, args []string, remove bool) error {
	var channelSpec, tsSpec, emoji string
	switch len(args) {
	case 2:
		ch, ts, _, err := resolve.ParsePermalink(args[0])
		if err != nil {
			return errdefs.Usagef("give a permalink and an emoji, or a channel, a ts and an emoji")
		}
		channelSpec, tsSpec, emoji = ch, ts, args[1]
	case 3:
		channelSpec, tsSpec, emoji = args[0], args[1], args[2]
		if _, _, _, err := resolve.ParsePermalink(channelSpec); err == nil {
			return errdefs.Usagef("a permalink already names the message; give it with just the emoji")
		}
		if resolve.IsID(channelSpec) && (channelSpec[0] == 'U' || channelSpec[0] == 'W') {
			return errdefs.Usagef("%s is a user id; a message lives in a channel or DM id (C…, G… or D…)", channelSpec)
		}
	default:
		return errdefs.Usagef("give a permalink and an emoji, or a channel, a ts and an emoji (got %d arguments)", len(args))
	}

	ts, err := slacktime.ParseStrict(tsSpec)
	if err != nil {
		return errdefsUsage(err)
	}
	name := strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(emoji), ":"), ":"))
	if !emojiName.MatchString(name) {
		return errdefs.Usagef("%q is not an emoji name; use the name Slack shows for it, such as eyes or thumbsup", emoji)
	}

	ctx, cancel := g.deadline(cmd.Context())
	defer cancel()

	s, err := g.newSession()
	if err != nil {
		return err
	}
	if err := g.gateWrite(s); err != nil {
		return err
	}
	if err := g.requireConsent(false); err != nil {
		return err
	}

	d, err := g.directory(ctx, s)
	if err != nil {
		return err
	}
	channel, err := d.ChannelExact(channelSpec)
	if err != nil {
		return err
	}
	channelName, ok := d.Name(channel)
	if !ok {
		channelName = channel
	}

	rec := output.Reacted{
		DryRun: g.dryRun, Action: "add",
		Channel: channel, ChannelName: channelName, TS: ts.String(), Name: name,
	}
	question := fmt.Sprintf("Add :%s: to %s %s?", name, channelName, rec.TS)
	if remove {
		rec.Action = "remove"
		question = fmt.Sprintf("Remove :%s: from %s %s?", name, channelName, rec.TS)
	}

	if g.dryRun {
		g.out.Info("dry run: nothing was changed")
		return g.out.Reacted(rec)
	}
	if err := g.confirmWrite("", question); err != nil {
		return err
	}

	// No uncertain() here: a reaction cannot land twice, so after a timeout
	// the plain advice - retry - is the right one.
	changed, err := s.client.React(ctx, channel, rec.TS, name, remove)
	if err != nil {
		return err
	}
	rec.Changed = changed
	return g.out.Reacted(rec)
}
