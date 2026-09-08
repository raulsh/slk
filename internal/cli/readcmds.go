package cli

import (
	"context"
	"strings"

	"github.com/raulsh/slk/internal/errdefs"
	"github.com/raulsh/slk/internal/output"
	"github.com/raulsh/slk/internal/resolve"
	"github.com/raulsh/slk/internal/slack"
	"github.com/raulsh/slk/internal/slacktime"
	"github.com/spf13/cobra"
)

// timeFlags are the range selectors every read command shares. They mean the
// same thing everywhere, which is the whole reason they are defined once.
type timeFlags struct {
	since string
	until string
	on    string
}

func (t *timeFlags) register(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&t.since, "since", "", "start of the range: 7d, 2h, today, yesterday, mon, 2026-09-01")
	f.StringVar(&t.until, "until", "", "end of the range; a bare date means the end of that day")
	f.StringVar(&t.on, "on", "", "a single day; shorthand for --since D --until D")
}

// ---------------------------------------------------------------- conversations

func newConversationsCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "conversations",
		Aliases: []string{"convs", "conv"},
		Short:   "List the channels and DMs you belong to",
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newConversationsListCmd(g))
	return cmd
}

func newConversationsListCmd(g *globals) *cobra.Command {
	var (
		types      []string
		name       string
		archived   bool
		unreadOnly bool
		limit      int
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the conversations you are a member of",
		Long: `List the channels, group DMs and DMs you belong to.

Only conversations you are in are listed. The whole workspace is deliberately
not enumerated: it would return thousands of channels you are not in, and bulk
enumeration is the traffic pattern that gets browser-session credentials
invalidated.`,
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

			// Read state always, not only under --unread-only: it is one
			// extra request and it is what makes the unread column mean
			// something rather than always being blank.
			convs := d.Conversations
			state, _, err := s.client.Counts(ctx)
			if err != nil {
				g.log.Debug("could not fetch read state", "error", err)
				g.out.Warn("read state unavailable: " + err.Error())
			} else {
				convs = applyReadState(convs, state, unreadOnly)
			}

			var rows []output.Conversation
			for _, c := range convs {
				if !archived && c.IsArchived {
					continue
				}
				if len(types) > 0 && !matchesType(c.Type, types) {
					continue
				}
				if name != "" && !strings.Contains(strings.ToLower(c.Name), strings.ToLower(name)) {
					continue
				}
				c.Name = d.ConversationName(c)
				rows = append(rows, c)
				if limit > 0 && len(rows) >= limit {
					break
				}
			}

			resolveDMNames(ctx, g, s, d, rows)
			return g.out.Conversations(rows)
		},
	}

	f := cmd.Flags()
	f.StringSliceVar(&types, "type", nil, "channel, private, mpim or dm (repeatable)")
	f.StringVar(&name, "name", "", "case-insensitive substring match on the name")
	f.BoolVar(&archived, "archived", false, "include archived conversations")
	f.BoolVar(&unreadOnly, "unread-only", false, "only conversations with something unread")
	f.IntVar(&limit, "limit", 200, "maximum conversations to return; 0 for no limit")
	return cmd
}

func matchesType(kind string, want []string) bool {
	for _, w := range want {
		if strings.EqualFold(kind, w) {
			return true
		}
	}
	return false
}

// applyReadState folds client.counts into the conversation list.
func applyReadState(cs []output.Conversation, state []slack.ReadState, onlyUnread bool) []output.Conversation {
	byID := make(map[string]slack.ReadState, len(state))
	for _, s := range state {
		byID[s.ID] = s
	}

	out := make([]output.Conversation, 0, len(cs))
	for _, c := range cs {
		if st, ok := byID[c.ID]; ok {
			c.HasUnreads = st.Unread()
			c.MentionCount = st.MentionCount
			c.LastRead = st.LastRead.String()
			c.LatestTS = st.Latest.String()
			if !st.Latest.IsZero() {
				c.LatestTime = st.Latest.Time().Format("2006-01-02T15:04:05Z07:00")
			}
		}
		if onlyUnread && !c.HasUnreads {
			continue
		}
		out = append(out, c)
	}
	return out
}

// resolveDMNames fills in the peer handles a DM row needs, in one batch.
func resolveDMNames(ctx context.Context, g *globals, s *session, d *resolve.Directory, rows []output.Conversation) {
	var ids []string
	for _, c := range rows {
		if c.Type == "dm" && c.User != "" {
			ids = append(ids, c.User)
		}
	}
	if len(ids) == 0 {
		return
	}
	if err := d.EnsureUsers(ctx, fetcher{s.client}, ids); err != nil {
		g.log.Debug("could not resolve dm peers", "error", err)
	}
	for i := range rows {
		if rows[i].Type == "dm" {
			rows[i].Name = d.ConversationName(rows[i])
			if u, ok := d.Users[rows[i].User]; ok {
				rows[i].UserName = u.Name
			}
		}
	}
	_ = d.Save()
}

// ---------------------------------------------------------------------- users

func newUsersCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "users",
		Short: "Look up workspace members",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newUsersListCmd(g), newUsersInfoCmd(g))
	return cmd
}

func newUsersListCmd(g *globals) *cobra.Command {
	var (
		limit          int
		includeBots    bool
		includeDeleted bool
		name           string
		vipOnly        bool
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List workspace members",
		Long: `List workspace members.

This enumerates the member directory, which is the one traffic pattern reported
to get browser-session credentials invalidated for "data scraping". It is
therefore never used to warm a cache automatically - name resolution fetches
only the ids it actually sees. Use it deliberately, with --limit.`,
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

			var us []output.User

			if vipOnly {
				// The VIP set is a handful of known ids, so resolving exactly
				// those costs a few tier-4 calls and avoids enumerating the
				// whole directory entirely. That is the whole point of having
				// this path.
				if len(d.VIPs) == 0 {
					g.out.Warn("no VIPs are marked in Slack; mark someone VIP from their profile")
					return g.out.Users(nil)
				}
				if err := d.EnsureUsers(ctx, fetcher{s.client}, d.VIPs); err != nil {
					return err
				}
				_ = d.Save()
				for _, id := range d.VIPs {
					if u, ok := d.Users[id]; ok {
						us = append(us, u)
					} else {
						// Say so rather than dropping them: a VIP who cannot
						// be resolved is more interesting than a silent gap.
						us = append(us, output.User{ID: id, Name: id})
					}
				}
			} else {
				us, err = s.client.UsersList(ctx, slack.UsersListOptions{
					Limit:          limit,
					IncludeBots:    includeBots,
					IncludeDeleted: includeDeleted,
				})
				if err != nil {
					return err
				}
			}

			if name != "" {
				want := strings.ToLower(name)
				var kept []output.User
				for _, u := range us {
					if strings.Contains(strings.ToLower(u.Name), want) ||
						strings.Contains(strings.ToLower(u.RealName), want) {
						kept = append(kept, u)
					}
				}
				us = kept
			}

			d.MarkVIPs(us)
			return g.out.Users(us)
		},
	}

	f := cmd.Flags()
	f.IntVar(&limit, "limit", 200, "maximum members to return")
	f.BoolVar(&includeBots, "include-bots", false, "include apps and bots")
	f.BoolVar(&includeDeleted, "include-deleted", false, "include deactivated accounts")
	f.StringVar(&name, "name", "", "case-insensitive substring match on handle or real name")
	f.BoolVar(&vipOnly, "vip", false, "only the people you have marked VIP in Slack (no directory enumeration)")
	return cmd
}

func newUsersInfoCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "info <user>",
		Short: "Show one member, by handle or id",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := g.newSession()
			if err != nil {
				return err
			}
			ctx, cancel := g.deadline(cmd.Context())
			defer cancel()

			target := args[0]
			if !resolve.IsID(target) {
				d, err := g.directory(ctx, s)
				if err != nil {
					return err
				}
				id, err := d.User(target)
				if err != nil {
					return err
				}
				target = id
			}

			u, err := s.client.UserInfo(ctx, target)
			if err != nil {
				return err
			}
			d, derr := g.directory(ctx, s)
			if derr == nil {
				u.VIP = d.IsVIP(u.ID)
			}
			return g.out.Users([]output.User{u})
		},
	}
}

// --------------------------------------------------------------------- thread

func newThreadCmd(g *globals) *cobra.Command {
	var limit int

	cmd := &cobra.Command{
		Use:   "thread <channel> <ts> | <permalink>",
		Short: "Show one thread",
		Long: `Show a thread's replies.

The timestamp is accepted in every form you might be holding: the wire form
1725806400.123456, the permalink form p1725806400123456, or a whole Slack
archive URL - from which the channel is taken too, so it can be the only
argument.`,
		Args: cobra.RangeArgs(1, 2),
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

			var channel, ts string
			if ch, mts, _, err := resolve.ParsePermalink(args[0]); err == nil {
				channel, ts = ch, mts
			} else {
				if len(args) != 2 {
					return errdefs.Usagef("give a channel and a timestamp, or a single permalink")
				}
				channel, err = d.Channel(args[0])
				if err != nil {
					return err
				}
				parsed, perr := slacktime.Parse(args[1])
				if perr != nil || parsed.IsZero() {
					return errdefs.Usagef("%q is not a message timestamp", args[1])
				}
				ts = parsed.String()
			}

			ms, err := s.client.Replies(ctx, slack.RepliesOptions{
				Channel: channel, ThreadTS: ts, Limit: limit,
			})
			if err != nil {
				return err
			}

			name, _ := d.Name(channel)
			for i := range ms {
				ms[i].ChannelName = name
			}
			g.finish(ctx, s, d, ms)
			output.SortMessages(ms, true)
			return g.out.Messages(ms)
		},
	}

	cmd.Flags().IntVar(&limit, "limit", 200, "maximum replies to return")
	return cmd
}
