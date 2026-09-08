package cli

import (
	"strings"

	"github.com/raulsh/slk/internal/output"
	"github.com/raulsh/slk/internal/slack"
	"github.com/spf13/cobra"
)

func newUnreadCmd(g *globals) *cobra.Command {
	var (
		types        []string
		minMentions  int
		limit        int
		includeMuted bool
	)

	cmd := &cobra.Command{
		Use:   "unread",
		Short: "Show where there is something to read",
		Long: `Summarise unread conversations.

This answers "where should I look" in one screen. For the messages themselves,
use ` + "`slk messages --all --unread`" + ` - they are different questions and both are
worth asking.

There is no per-conversation unread count in Slack's client API: it reports
whether a conversation has unreads and how many mentions, and nothing more. So
the count column reads "yes" rather than inventing a number. Thread counts,
where present, are real.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
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

			state, totals, err := s.client.Counts(ctx)
			if err != nil {
				return err
			}

			byID := make(map[string]output.Conversation, len(d.Conversations))
			for _, c := range d.Conversations {
				byID[c.ID] = c
			}

			var rows []output.Unread
			for _, st := range state {
				if !st.Unread() {
					continue
				}
				c, known := byID[st.ID]
				if known && c.IsArchived {
					continue
				}
				if len(types) > 0 && !matchesType(st.Kind, types) {
					continue
				}
				if st.MentionCount < minMentions {
					continue
				}

				name := st.ID
				if known {
					name = d.ConversationName(c)
				}
				readState := "ok"
				if st.NeverRead() {
					readState = "never_read"
				}

				rows = append(rows, output.Unread{
					Channel:        st.ID,
					ChannelName:    name,
					Type:           st.Kind,
					MentionCount:   st.MentionCount,
					ThreadUnread:   st.ThreadUnread,
					ThreadMentions: st.ThreadMentions,
					LastReadTS:     st.LastRead.String(),
					LatestTS:       st.Latest.String(),
					LatestTime:     timeOf(st),
					ReadState:      readState,
				})
			}

			sortUnread(rows)
			if limit > 0 && len(rows) > limit {
				rows = rows[:limit]
				g.out.Truncate("more unread conversations; raise --limit")
			}

			// Resolve the DM peers that appear, in one batch.
			var ids []string
			for _, r := range rows {
				if c, ok := byID[r.Channel]; ok && c.Type == "dm" {
					ids = append(ids, c.User)
				}
			}
			if len(ids) > 0 {
				if err := d.EnsureUsers(ctx, fetcher{s.client}, ids); err == nil {
					for i := range rows {
						if c, ok := byID[rows[i].Channel]; ok && c.Type == "dm" {
							rows[i].ChannelName = d.ConversationName(c)
						}
					}
					_ = d.Save()
				}
			}

			if totals.ThreadUnread > 0 {
				g.out.Warn(strings.TrimSpace(
					"there are unread thread replies; `slk messages --all --unread --include-threads` fetches them"))
			}
			return g.out.Unreads(rows)
		},
	}

	f := cmd.Flags()
	f.StringSliceVar(&types, "type", nil, "channel, private, mpim or dm (repeatable)")
	f.IntVar(&minMentions, "min-mentions", 0, "only conversations with at least this many mentions")
	f.IntVar(&limit, "limit", 0, "maximum conversations to list; 0 for no limit")
	f.BoolVar(&includeMuted, "include-muted", false, "include muted conversations")
	return cmd
}

func timeOf(st slack.ReadState) string {
	if st.Latest.IsZero() {
		return ""
	}
	return st.Latest.Time().Format("2006-01-02T15:04:05Z07:00")
}

// sortUnread puts mentions first, then the most recent - so a truncated list
// keeps what matters most.
func sortUnread(rows []output.Unread) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0; j-- {
			a, b := rows[j-1], rows[j]
			better := b.MentionCount > a.MentionCount ||
				(b.MentionCount == a.MentionCount && b.LatestTS > a.LatestTS)
			if !better {
				break
			}
			rows[j-1], rows[j] = b, a
		}
	}
}
