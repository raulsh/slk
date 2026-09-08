package slack

import (
	"context"
	"fmt"
	"net/url"

	"github.com/raulsh/slk/internal/errdefs"
	"github.com/raulsh/slk/internal/slacktime"
)

// countsSnapshot is one conversation's read state as client.counts reports it.
//
// Verified against a real workspace: updated and history_invalid arrive as
// ts-format *strings*, not integers, and history_invalid really does show up
// as "0000000000.000000" - which is why every one of these is parsed rather
// than compared as text.
type countsSnapshot struct {
	ID             string `json:"id"`
	LastRead       string `json:"last_read"`
	Latest         string `json:"latest"`
	Updated        string `json:"updated"`
	HistoryInvalid string `json:"history_invalid"`
	MentionCount   int    `json:"mention_count"`
	HasUnreads     bool   `json:"has_unreads"`
}

type countsResponse struct {
	Channels []countsSnapshot `json:"channels"`
	MPIMs    []countsSnapshot `json:"mpims"`
	IMs      []countsSnapshot `json:"ims"`
	Threads  struct {
		HasUnreads       bool           `json:"has_unreads"`
		MentionCount     int            `json:"mention_count"`
		MentionByChannel map[string]int `json:"mention_count_by_channel"`
		UnreadByChannel  map[string]int `json:"unread_count_by_channel"`
	} `json:"threads"`
	ChannelBadges struct {
		Channels      int `json:"channels"`
		DMs           int `json:"dms"`
		AppDMs        int `json:"app_dms"`
		ThreadMention int `json:"thread_mentions"`
		ThreadUnreads int `json:"thread_unreads"`
	} `json:"channel_badges"`
}

// ReadState is per-conversation read state.
type ReadState struct {
	ID             string
	Kind           string // channel, mpim or dm
	LastRead       slacktime.TS
	Latest         slacktime.TS
	MentionCount   int
	HasUnreads     bool
	ThreadUnread   int
	ThreadMentions int
}

// NeverRead reports whether the user has never read this conversation, which
// needs its own answer: paging a five-year channel from the beginning would
// let one dormant conversation dominate an entire run.
func (r ReadState) NeverRead() bool { return r.LastRead.IsZero() }

// Unread reports whether there is genuinely something newer than the read
// marker.
//
// Testing latest > last_read as well as has_unreads guards a stale flag and,
// more usefully, skips the whole fetch for a conversation where nothing can
// possibly be new.
func (r ReadState) Unread() bool {
	if r.MentionCount > 0 || r.ThreadUnread > 0 {
		return true
	}
	return r.HasUnreads && r.Latest.After(r.LastRead)
}

// Counts fetches read state for every conversation in one call.
//
// This endpoint is undocumented, and it is load-bearing rather than an
// optimisation: conversations.info returns unread_count and last_read for DM
// conversations *only*, so there is no documented way to get a channel's read
// state at all. Verified against a real workspace: last_read is present for
// every channel, not just DMs.
func (c *Client) Counts(ctx context.Context) ([]ReadState, ThreadTotals, error) {
	form := url.Values{}
	form.Set("thread_counts_by_channel", "true")
	form.Set("org_wide_aware", "true")
	form.Set("include_file_channels", "true")

	var resp countsResponse
	if err := c.call(ctx, "client.counts", form, &resp); err != nil {
		return nil, ThreadTotals{}, err
	}

	// A schema change that still answers ok:true with no arrays must fall
	// through to the fallback, not report "nothing unread" - which would be a
	// silently wrong answer rather than a visible failure.
	if resp.Channels == nil && resp.IMs == nil && resp.MPIMs == nil {
		return nil, ThreadTotals{}, fmt.Errorf(
			"client.counts: %w: response carried no channels, ims or mpims",
			errdefs.ErrUnsupported)
	}

	var out []ReadState
	add := func(snaps []countsSnapshot, kind string) {
		for _, s := range snaps {
			lr, _ := slacktime.Parse(s.LastRead)
			lt, _ := slacktime.Parse(s.Latest)
			out = append(out, ReadState{
				ID:             s.ID,
				Kind:           kind,
				LastRead:       lr,
				Latest:         lt,
				MentionCount:   s.MentionCount,
				HasUnreads:     s.HasUnreads,
				ThreadUnread:   resp.Threads.UnreadByChannel[s.ID],
				ThreadMentions: resp.Threads.MentionByChannel[s.ID],
			})
		}
	}
	add(resp.Channels, "channel")
	add(resp.MPIMs, "mpim")
	add(resp.IMs, "dm")

	totals := ThreadTotals{
		HasUnreads:   resp.Threads.HasUnreads,
		MentionCount: resp.Threads.MentionCount,
		Channels:     resp.ChannelBadges.Channels,
		DMs:          resp.ChannelBadges.DMs,
		ThreadUnread: resp.ChannelBadges.ThreadUnreads,
	}
	return out, totals, nil
}

// ThreadTotals are the workspace-wide badge numbers client.counts supplies
// alongside the per-conversation state.
type ThreadTotals struct {
	HasUnreads   bool
	MentionCount int
	Channels     int
	DMs          int
	ThreadUnread int
}
