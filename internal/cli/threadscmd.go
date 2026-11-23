package cli

import (
	"context"
	"fmt"

	"github.com/raulsh/slk/internal/errdefs"
	"github.com/raulsh/slk/internal/output"
	"github.com/raulsh/slk/internal/slack"
	"github.com/raulsh/slk/internal/slacktime"
	"github.com/spf13/cobra"
)

func newThreadsCmd(g *globals) *cobra.Command {
	var (
		unread     bool
		limit      int
		maxThreads int
	)

	cmd := &cobra.Command{
		Use:   "threads",
		Short: "Read the threads you follow, newest activity first",
		Long: `Read the threads you follow - the ones you started, replied to, or chose to
follow - newest activity first, each as its first message and then its replies.

With --unread, only threads with replies you have not read, and all of those
replies. This is how to find a new reply on an old thread: slk messages never
sees one, because Slack does not return replies with a conversation's history.

Without --unread, each thread comes with the few newest replies Slack's view
keeps with it, not all of them; slk thread <permalink> reads one in full.

Every message carries its permalink, so answering is one more command:
slk send --thread <permalink> --yes "...". Reading never marks anything read.`,
		Example: `  slk threads --unread
  slk threads --unread --json
  slk threads --max-threads 5`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit < 1 {
				return errdefs.Usagef("--limit must be at least 1")
			}
			if maxThreads < 0 {
				return errdefs.Usagef("--max-threads cannot be negative")
			}

			s, err := g.newSession()
			if err != nil {
				return err
			}
			ctx, cancel := g.deadline(cmd.Context())
			defer cancel()

			d, err := g.directory(ctx, s)
			if err != nil {
				return err
			}

			threads, more, err := s.client.FollowedThreads(ctx, slack.FollowedThreadsOptions{
				UnreadOnly: unread, Max: maxThreads,
			})
			if err != nil {
				if len(threads) == 0 {
					return err
				}
				// The pages already read are worth returning.
				g.out.Warn("stopped reading the threads view early: " + err.Error())
			}

			var (
				ms      []output.Message
				clipped bool
			)
			for _, t := range threads {
				name, ok := d.Name(t.Channel)
				if !ok {
					name = t.Channel
				}

				replies := t.Latest
				if unread {
					var cut bool
					replies, cut, err = unreadReplies(ctx, s.client, t, limit)
					if err != nil {
						// One unreadable thread must not lose the rest.
						g.out.Warn(fmt.Sprintf("skipped a thread in %s: %s", name, err))
						continue
					}
					clipped = clipped || cut
				}

				root := t.Root
				root.ChannelName = name
				root.HasNewReplies = t.Unread()
				ms = append(ms, root)
				for _, r := range replies {
					r.ChannelName = name
					if ts, perr := slacktime.Parse(r.TS); perr == nil && ts.After(t.LastRead) {
						r.Unread = true
					}
					ms = append(ms, r)
				}
			}

			g.finish(ctx, s, d, ms)

			if unread && len(threads) == 0 {
				g.out.Info("no unread replies in the threads you follow")
			}
			if more {
				g.out.Truncate("more followed threads are available; raise --max-threads")
			}
			if clipped {
				g.out.Truncate("a thread has more unread replies than --limit; raise it or read the thread with slk thread")
			}
			// Deliberately not sorted: the threads keep the view's order,
			// newest activity first, and each reads root first, then replies.
			return g.out.Messages(ms)
		},
	}

	f := cmd.Flags()
	f.BoolVar(&unread, "unread", false, "only threads with replies you have not read, with all of those replies")
	f.IntVar(&limit, "limit", 50, "maximum unread replies per thread")
	f.IntVar(&maxThreads, "max-threads", 20, "maximum threads; 0 for no limit")
	return cmd
}

// unreadReplies fetches the replies in a thread that are newer than the user's
// read marker in it, oldest first, and reports whether --limit cut them short.
//
// It asks conversations.replies from the read marker rather than trusting the
// view for the content: that endpoint is the one `slk thread` already uses, and
// the view keeps only the newest couple of replies.
func unreadReplies(ctx context.Context, c *slack.Client, t slack.FollowedThread, limit int) ([]output.Message, bool, error) {
	oldest := ""
	if !t.LastRead.IsZero() {
		oldest = t.LastRead.String()
	}
	// Room for the root and the last read reply, which the inclusive bound
	// returns too, plus one to tell "exactly at the limit" from "over it".
	ms, err := c.Replies(ctx, slack.RepliesOptions{
		Channel: t.Channel, ThreadTS: t.Root.TS, Oldest: oldest, Limit: limit + 3,
	})
	if err != nil {
		return nil, false, err
	}

	var out []output.Message
	for _, m := range ms {
		ts, err := slacktime.Parse(m.TS)
		if err != nil || m.TS == t.Root.TS || !ts.After(t.LastRead) {
			continue
		}
		out = append(out, m)
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}
