package cli

import (
	"context"
	"os"

	"github.com/raulsh/slk/internal/errdefs"
	"github.com/raulsh/slk/internal/output"
	"github.com/raulsh/slk/internal/resolve"
	"github.com/raulsh/slk/internal/slack"
	"github.com/raulsh/slk/internal/tui"
	"github.com/spf13/cobra"
)

// browseData adapts a session to what the TUI needs, so the TUI never imports
// the transport and can be driven by fixtures.
type browseData struct {
	g   *globals
	s   *session
	dir *resolve.Directory
}

// Conversations is the fast phase: the cached list plus read state, and
// nothing that costs a lookup per row.
func (b browseData) Conversations(ctx context.Context) ([]output.Conversation, error) {
	// Everything downstream needs the identity and the resolved API base.
	if _, err := b.s.client.AuthTest(ctx); err != nil {
		return nil, err
	}
	if err := b.dir.EnsureFresh(ctx, fetcher{b.s.client}); err != nil {
		return nil, err
	}

	convs := b.dir.Conversations
	if state, _, err := b.s.client.Counts(ctx); err == nil {
		convs = applyReadState(convs, state, false)
	} else {
		b.g.log.Debug("could not fetch read state", "error", err)
	}

	out := make([]output.Conversation, 0, len(convs))
	for _, c := range convs {
		if c.IsArchived {
			continue
		}
		// Whatever is already known: a channel has its name inline, and a DM
		// falls back to its peer id until ResolveNames catches up.
		c.Name = b.dir.ConversationName(c)
		out = append(out, c)
	}
	sortForBrowse(out)
	return out, nil
}

// ResolveNames is the slow phase, run after the list is already on screen.
//
// A DM's name is its peer's handle, which is one users.info call per
// conversation - a few dozen on a real account. Doing that before the first
// paint made the browser sit on a spinner for several seconds for no reason.
func (b browseData) ResolveNames(ctx context.Context, cs []output.Conversation) ([]output.Conversation, error) {
	var ids []string
	for _, c := range cs {
		if c.Type == "dm" && c.User != "" {
			ids = append(ids, c.User)
		}
	}
	if len(ids) == 0 {
		return cs, nil
	}

	err := b.dir.EnsureUsers(ctx, fetcher{b.s.client}, ids)
	_ = b.dir.Save()

	out := make([]output.Conversation, len(cs))
	copy(out, cs)
	for i := range out {
		if out[i].Type == "dm" {
			out[i].Name = b.dir.ConversationName(out[i])
		}
	}
	sortForBrowse(out)
	return out, err
}

func (b browseData) Messages(ctx context.Context, channelID string, limit int) ([]output.Message, error) {
	ms, _, err := b.s.client.History(ctx, slack.HistoryOptions{
		Channel: channelID,
		Limit:   limit,
	})
	if err != nil {
		return nil, err
	}

	var kept []output.Message
	for _, m := range ms {
		if noiseSubtypes[m.Subtype] {
			continue
		}
		kept = append(kept, m)
	}

	b.g.finish(ctx, b.s, b.dir, kept)
	output.SortMessages(kept, true)
	return kept, nil
}

func (b browseData) IsVIP(userID string) bool { return b.dir.IsVIP(userID) }

// sortForBrowse puts what needs attention at the top: mentions, then unread,
// then recency.
func sortForBrowse(cs []output.Conversation) {
	for i := 1; i < len(cs); i++ {
		for j := i; j > 0; j-- {
			a, b := cs[j-1], cs[j]
			if !browseBefore(b, a) {
				break
			}
			cs[j-1], cs[j] = b, a
		}
	}
}

func browseBefore(x, y output.Conversation) bool {
	if x.MentionCount != y.MentionCount {
		return x.MentionCount > y.MentionCount
	}
	if x.HasUnreads != y.HasUnreads {
		return x.HasUnreads
	}
	return x.LatestTS > y.LatestTS
}

func newBrowseCmd(g *globals) *cobra.Command {
	var limit int

	cmd := &cobra.Command{
		Use:     "browse",
		Aliases: []string{"ui"},
		Short:   "Browse your Slack interactively",
		Long: `Open an interactive browser.

Conversations needing attention sort to the top: mentions first, then unread,
then recency. Enter opens one, esc goes back, / filters, u and v narrow to
unread and to VIPs.

This is the one command that is not for an agent - it needs a terminal and
never terminates on its own. Everything it shows is available non-interactively
through ` + "`slk unread`, `slk messages` and `slk conversations list`" + `.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Check before doing any work: an automated caller with no
			// terminal would otherwise sit in an event loop waiting for a
			// keypress that can never arrive.
			if !g.stdoutIsTTY() {
				return errdefs.WithHint(
					errdefs.Usagef("browse needs a terminal"),
					"For a script or an agent, the same information is available as data:\n\n"+
						"    slk unread --json\n"+
						"    slk messages --all --unread --jsonl\n"+
						"    slk conversations list --json")
			}

			s, err := g.newSession()
			if err != nil {
				return err
			}
			// No deadline: a browser is meant to stay open.
			ctx := cmd.Context()

			// auth.test deliberately happens inside the load phase rather
			// than here: doing it first meant a network round trip before the
			// first paint, so the terminal sat blank instead of showing a
			// spinner. Errors surface in the view.
			dir := resolve.Open(s.name, s.cfg.Cache.ConversationsTTL.Std(), g.log)
			g.out.SetResolver(dir.Name)

			return tui.Run(ctx, browseData{g: g, s: s, dir: dir}, limit)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 100, "messages to load per conversation")
	return cmd
}

// stdoutIsTTY reports whether the data stream is a terminal.
func (g *globals) stdoutIsTTY() bool {
	f, ok := g.env.Stdout.(*os.File)
	return ok && g.env.IsTTY(f)
}
