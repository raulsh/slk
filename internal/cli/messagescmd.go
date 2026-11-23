package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/raulsh/slk/internal/errdefs"
	"github.com/raulsh/slk/internal/output"
	"github.com/raulsh/slk/internal/resolve"
	"github.com/raulsh/slk/internal/slack"
	"github.com/raulsh/slk/internal/slacktime"
	"github.com/spf13/cobra"
)

// noiseSubtypes are the join/leave/topic events that make up a large share of
// message volume in a busy channel and carry near-zero value to a reader.
var noiseSubtypes = map[string]bool{
	"channel_join": true, "channel_leave": true,
	"group_join": true, "group_leave": true,
	"channel_topic": true, "channel_purpose": true, "channel_name": true,
	"channel_archive": true, "channel_unarchive": true,
	"channel_convert_to_private": true, "channel_convert_to_public": true,
	"pinned_item": true, "unpinned_item": true,
	"bot_add": true, "bot_remove": true, "reminder_add": true,
}

type messagesOpts struct {
	timeFlags
	all            bool
	channels       []string
	dms            []string
	unread         bool
	limit          int
	maxTotal       int
	oldestFirst    bool
	from           []string
	excludeBots    bool
	includeSystem  bool
	includeThreads bool
	vipOnly        bool
}

func newMessagesCmd(g *globals) *cobra.Command {
	var o messagesOpts

	cmd := &cobra.Command{
		Use:     "messages",
		Aliases: []string{"msgs", "msg"},
		Short:   "Read messages from channels and DMs",
		Long: `Read message history.

Pick a target with --all, --channel or --dm, and narrow it with --since/--until
or --on. --unread reads only what you have not seen.

Two things worth knowing, because they are easy to get backwards:

  --limit is per conversation and --max-total caps the whole run.
  Selection always takes the most recent messages in the range; --oldest-first
  controls print order only.

Reading never marks anything read.`,
		Example: `  slk messages --channel general --since 2d
  slk messages --dm @alice --since 7d
  slk messages --all --unread --max-total 50
  slk messages --channel eng --on 2026-09-01 --json
  slk messages --all --unread --vip`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runMessages(cmd, &o)
		},
	}

	o.register(cmd)
	f := cmd.Flags()
	f.BoolVar(&o.all, "all", false, "every conversation you are a member of")
	f.StringSliceVar(&o.channels, "channel", nil, "channel by name or id (repeatable)")
	f.StringSliceVar(&o.dms, "dm", nil, "DM by @handle or id (repeatable)")
	f.BoolVar(&o.unread, "unread", false, "only messages you have not read")
	f.IntVar(&o.limit, "limit", 50, "maximum messages per conversation")
	f.IntVar(&o.maxTotal, "max-total", 1000, "maximum messages overall; 0 for no limit")
	f.BoolVar(&o.oldestFirst, "oldest-first", true, "print chronologically")
	f.StringSliceVar(&o.from, "from", nil, "only messages from these people (repeatable)")
	f.BoolVar(&o.excludeBots, "exclude-bots", false, "drop app and bot posts")
	f.BoolVar(&o.includeSystem, "include-system", false, "include join, leave and topic notices")
	f.BoolVar(&o.includeThreads, "include-threads", false, "also fetch replies to threads in range")
	f.BoolVar(&o.vipOnly, "vip", false, "only messages from people you have marked VIP in Slack")
	return cmd
}

func (g *globals) runMessages(cmd *cobra.Command, o *messagesOpts) error {
	if !o.all && len(o.channels) == 0 && len(o.dms) == 0 {
		return errdefs.Usagef("give a target: --all, --channel <name> or --dm <@user>")
	}
	if o.all && (len(o.channels) > 0 || len(o.dms) > 0) {
		return errdefs.Usagef("--all cannot be combined with --channel or --dm")
	}

	rng, err := g.timeRange(o.since, o.until, o.on)
	if err != nil {
		return err
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

	targets, err := g.resolveTargets(ctx, s, d, o)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		g.out.Warn("no conversations matched")
		return g.out.Messages(nil)
	}

	var (
		all       []output.Message
		truncated bool
	)
	for _, t := range targets {
		if ctx.Err() != nil {
			truncated = true
			break
		}

		limit := o.limit
		if o.maxTotal > 0 {
			remaining := o.maxTotal - len(all)
			if remaining <= 0 {
				truncated = true
				break
			}
			limit = min(limit, remaining)
		}

		hopts := slack.HistoryOptions{Channel: t.id, Range: rng, Limit: limit}
		if o.unread {
			// oldest=last_read with Slack's default inclusive=false yields
			// exactly ts > last_read: the last message you read is excluded,
			// with no arithmetic.
			if t.state.NeverRead() {
				// Never paging a dormant five-year channel from the
				// beginning: one such conversation would otherwise dominate
				// the entire run.
				hopts.Oldest = ""
				if rng.IsZero() {
					hopts.Range = clampNeverRead(g)
				}
			} else {
				hopts.Oldest = t.state.LastRead.String()
			}
		}

		ms, more, err := s.client.History(ctx, hopts)
		if err != nil {
			// One unreadable conversation must not lose the rest of the run.
			g.log.Warn("could not read conversation", "channel", t.name, "error", err)
			g.out.Warn(fmt.Sprintf("skipped %s: %s", t.name, err))
			continue
		}
		if more {
			truncated = true
		}

		for i := range ms {
			ms[i].ChannelName = t.name
			if o.unread {
				ms[i].Unread = true
			}
			if lr := t.state.LastRead; !lr.IsZero() {
				if latest, err := slacktime.Parse(ms[i].TS); err == nil && ms[i].ReplyCount > 0 {
					_ = latest
					ms[i].HasNewReplies = hasNewReplies(ms[i], lr)
				}
			}
		}
		all = append(all, ms...)

		if o.includeThreads {
			all = append(all, g.fetchThreads(ctx, s, t, ms, o)...)
		}
	}

	kept, dropped := filterMessages(all, o, s.client)
	if len(o.from) > 0 {
		before := len(kept)
		kept = g.keepFrom(ctx, s, d, kept, o.from)
		dropped.from = before - len(kept)
	}
	if dropped.any() {
		g.log.Debug("filtered messages", "system", dropped.system, "bots", dropped.bots, "from", dropped.from)
	}

	g.finish(ctx, s, d, kept)

	// After finish, because VIP is stamped there: the filter needs the mark,
	// and the mark needs the resolved identities.
	if o.vipOnly {
		var vips []output.Message
		for _, m := range kept {
			if m.VIP {
				vips = append(vips, m)
			}
		}
		if len(d.VIPs) == 0 {
			g.out.Warn("no VIPs are marked in Slack, so --vip matched nothing")
		}
		kept = vips
	}

	output.SortMessages(kept, o.oldestFirst)

	g.out.SetWindow(window(rng, g.out.Location().String()))
	if truncated {
		g.out.Truncate("more messages are available; raise --limit or --max-total")
	}
	return g.out.Messages(kept)
}

// clampNeverRead bounds an unread read of a never-read conversation.
func clampNeverRead(g *globals) slacktime.Range {
	r, err := slacktime.ParseRange("30d", "", "", g.out.Location())
	if err != nil {
		return slacktime.Range{}
	}
	return r
}

func hasNewReplies(m output.Message, lastRead slacktime.TS) bool {
	if m.ReplyCount == 0 {
		return false
	}
	ts, err := slacktime.Parse(m.TS)
	if err != nil {
		return false
	}
	return ts.After(lastRead)
}

// target is one conversation to read, with its read state attached.
type target struct {
	id    string
	name  string
	kind  string
	state slack.ReadState
}

// resolveTargets turns the selector flags into conversations, attaching read
// state when it is needed.
func (g *globals) resolveTargets(ctx context.Context, s *session, d *resolve.Directory, o *messagesOpts) ([]target, error) {
	var state map[string]slack.ReadState
	if o.unread {
		rs, _, err := s.client.Counts(ctx)
		if err != nil {
			return nil, err
		}
		state = make(map[string]slack.ReadState, len(rs))
		for _, r := range rs {
			state[r.ID] = r
		}
	}

	var out []target
	add := func(id string) {
		name, _ := d.Name(id)
		t := target{id: id, name: name}
		if st, ok := state[id]; ok {
			t.state = st
			t.kind = st.Kind
		}
		out = append(out, t)
	}

	switch {
	case o.all:
		for _, c := range d.Conversations {
			if c.IsArchived {
				continue
			}
			if o.unread {
				st, ok := state[c.ID]
				if !ok || !st.Unread() {
					continue
				}
			}
			out = append(out, target{
				id:    c.ID,
				name:  d.ConversationName(c),
				kind:  c.Type,
				state: state[c.ID],
			})
		}
		// Mentions first, so a truncated run keeps what matters most.
		sortTargets(out)

	default:
		for _, spec := range o.channels {
			id, err := d.Channel(spec)
			if err != nil {
				return nil, err
			}
			add(id)
		}
		for _, spec := range o.dms {
			id, err := d.DM(spec)
			if err != nil {
				return nil, err
			}
			add(id)
		}
	}
	return out, nil
}

func sortTargets(ts []target) {
	// Simple insertion by mention count then recency; the list is short.
	for i := 1; i < len(ts); i++ {
		for j := i; j > 0; j-- {
			a, b := ts[j-1], ts[j]
			better := b.state.MentionCount > a.state.MentionCount ||
				(b.state.MentionCount == a.state.MentionCount && b.state.Latest.After(a.state.Latest))
			if !better {
				break
			}
			ts[j-1], ts[j] = b, a
		}
	}
}

// fetchThreads descends into threads whose latest reply is newer than the read
// marker.
//
// Stated limitation: this catches new replies on parents inside the fetched
// window, and misses replies on older parents. Full coverage needs a separate
// undocumented endpoint that v1 does not call.
func (g *globals) fetchThreads(ctx context.Context, s *session, t target, parents []output.Message, o *messagesOpts) []output.Message {
	var out []output.Message
	for _, p := range parents {
		if p.ReplyCount == 0 || p.ThreadTS != "" && p.ThreadTS != p.TS {
			continue
		}
		oldest := ""
		if o.unread && !t.state.LastRead.IsZero() {
			oldest = t.state.LastRead.String()
		}
		replies, err := s.client.Replies(ctx, slack.RepliesOptions{
			Channel: t.id, ThreadTS: p.TS, Oldest: oldest, Limit: o.limit,
		})
		if err != nil {
			g.log.Debug("could not read thread", "channel", t.id, "ts", p.TS, "error", err)
			continue
		}
		for _, r := range replies {
			// conversations.replies includes the parent; skip it.
			if r.TS == p.TS {
				continue
			}
			r.ChannelName = t.name
			out = append(out, r)
		}
	}
	return out
}

type dropCounts struct{ system, bots, from int }

func (d dropCounts) any() bool { return d.system+d.bots+d.from > 0 }

// filterMessages applies the content filters and reports what it removed, so a
// caller can tell a quiet channel from an aggressive filter.
func filterMessages(ms []output.Message, o *messagesOpts, c *slack.Client) ([]output.Message, dropCounts) {
	var (
		kept    []output.Message
		dropped dropCounts
	)
	self := ""
	if id := c.Identity(); id != nil {
		self = id.UserID
	}

	for _, m := range ms {
		if !o.includeSystem && noiseSubtypes[m.Subtype] {
			dropped.system++
			continue
		}
		if o.excludeBots && m.Bot {
			dropped.bots++
			continue
		}
		_ = self
		kept = append(kept, m)
	}
	return kept, dropped
}

// keepFrom applies --from. It takes every spelling --dm does - a handle,
// display name, email or user id - plus the name an app posts under, so the
// two flags never disagree about who someone is.
//
// It resolves the authors first: until then a person's message carries
// nothing but a user id, so a handle could never match. finish then finds the
// kept authors cached, and still renders only the messages that survive, which
// keeps the envelope's names map to the ids that appear in the results.
func (g *globals) keepFrom(ctx context.Context, s *session, d *resolve.Directory, ms []output.Message, from []string) []output.Message {
	ids := make([]string, 0, len(ms))
	for _, m := range ms {
		ids = append(ids, m.User)
	}
	slices.Sort(ids)
	if err := d.EnsureUsers(ctx, fetcher{s.client}, slices.Compact(ids)); err != nil {
		g.log.Debug("could not resolve some authors", "error", err)
	}

	want := make(map[string]bool, len(from))
	for _, f := range from {
		want[strings.ToLower(strings.TrimPrefix(strings.TrimSpace(f), "@"))] = true
	}
	matches := func(name string) bool { return name != "" && want[strings.ToLower(name)] }

	var kept []output.Message
	for _, m := range ms {
		names := []string{m.User, m.UserName}
		if u, ok := d.Users[m.User]; ok {
			names = append(names, u.Name, u.Display, u.Email)
		}
		if slices.ContainsFunc(names, matches) {
			kept = append(kept, m)
		}
	}
	return kept
}
