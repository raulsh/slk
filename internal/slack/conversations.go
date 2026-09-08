package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/raulsh/slk/internal/output"
	"github.com/raulsh/slk/internal/slacktime"
)

// maxPages bounds pagination so a pathological or repeating cursor terminates
// instead of looping forever.
const maxPages = 50

// pageLimit is the per-request page size. Slack allows up to 999 on the
// methods we page, but a smaller page returns sooner and wastes less when a
// limit cuts the run short.
const pageLimit = 200

type rawConversation struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	IsChannel   bool            `json:"is_channel"`
	IsGroup     bool            `json:"is_group"`
	IsIM        bool            `json:"is_im"`
	IsMPIM      bool            `json:"is_mpim"`
	IsPrivate   bool            `json:"is_private"`
	IsArchived  bool            `json:"is_archived"`
	IsShared    bool            `json:"is_shared"`
	IsExtShared bool            `json:"is_ext_shared"`
	User        string          `json:"user"`
	NumMembers  int             `json:"num_members"`
	Topic       topicPurpose    `json:"topic"`
	Purpose     topicPurpose    `json:"purpose"`
	Raw         json.RawMessage `json:"-"`
}

type topicPurpose struct {
	Value string `json:"value"`
}

// ConversationsOptions selects which conversations to list.
type ConversationsOptions struct {
	// Types are Slack's type names. Empty means all four.
	Types           []string
	ExcludeArchived bool
	Limit           int
	WantRaw         bool
}

// Conversations lists the conversations the user belongs to.
//
// It uses users.conversations, never conversations.list: the latter enumerates
// every public channel in the workspace - thousands the user is not in - and
// bulk enumeration is the traffic pattern reported to get xoxc sessions
// invalidated for "data scraping".
func (c *Client) Conversations(ctx context.Context, o ConversationsOptions) ([]output.Conversation, error) {
	types := o.Types
	if len(types) == 0 {
		types = []string{"public_channel", "private_channel", "mpim", "im"}
	}

	var out []output.Conversation
	cursor := ""
	for page := 0; page < maxPages; page++ {
		form := url.Values{}
		form.Set("types", strings.Join(types, ","))
		form.Set("exclude_archived", strconv.FormatBool(o.ExcludeArchived))
		form.Set("limit", strconv.Itoa(pageLimit))
		if cursor != "" {
			form.Set("cursor", cursor)
		}

		var resp struct {
			Channels         []rawConversation `json:"channels"`
			ResponseMetadata struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := c.call(ctx, "users.conversations", form, &resp); err != nil {
			return out, err
		}

		for _, rc := range resp.Channels {
			out = append(out, rc.toOutput())
			if o.Limit > 0 && len(out) >= o.Limit {
				return out, nil
			}
		}

		next := resp.ResponseMetadata.NextCursor
		// Both conditions, not either: has_more can be set with an empty
		// cursor, and a repeated cursor means the server is not advancing.
		if next == "" || next == cursor {
			break
		}
		cursor = next
	}
	return out, nil
}

func (rc rawConversation) toOutput() output.Conversation {
	c := output.Conversation{
		ID:         rc.ID,
		Name:       rc.Name,
		Type:       rc.kind(),
		Topic:      rc.Topic.Value,
		Purpose:    rc.Purpose.Value,
		User:       rc.User,
		Members:    rc.NumMembers,
		IsArchived: rc.IsArchived,
		IsShared:   rc.IsShared || rc.IsExtShared,
	}
	if c.Type == "mpim" {
		c.Name = prettyMPIM(rc.Name)
	}
	return c
}

// kind flattens Slack's overlapping is_* booleans into one answer.
//
// Order matters: an MPIM also reports is_group, and a DM reports neither a
// name nor a channel flag, so the most specific test has to come first.
func (rc rawConversation) kind() string {
	switch {
	case rc.IsIM:
		return "dm"
	case rc.IsMPIM:
		return "mpim"
	case rc.IsPrivate:
		return "private"
	default:
		return "channel"
	}
}

// prettyMPIM turns Slack's internal group-DM name into something readable.
//
// The wire form is mpdm-alice--bob--carol-1, which is an implementation detail
// nobody should have to see.
func prettyMPIM(name string) string {
	s := strings.TrimPrefix(name, "mpdm-")
	if i := strings.LastIndex(s, "-"); i > 0 {
		s = s[:i] // drop the trailing -1
	}
	parts := strings.Split(s, "--")
	for i, p := range parts {
		parts[i] = "@" + p
	}
	return strings.Join(parts, ", ")
}

// HistoryOptions selects messages from one conversation.
type HistoryOptions struct {
	Channel   string
	Range     slacktime.Range
	Oldest    string // overrides Range, used by the unread path with last_read
	Limit     int
	WantRaw   bool
	Inclusive bool
}

type rawMessage struct {
	Type        string `json:"type"`
	Subtype     string `json:"subtype"`
	TS          string `json:"ts"`
	ThreadTS    string `json:"thread_ts"`
	User        string `json:"user"`
	BotID       string `json:"bot_id"`
	Username    string `json:"username"`
	Text        string `json:"text"`
	ReplyCount  int    `json:"reply_count"`
	LatestReply string `json:"latest_reply"`
	Edited      *struct {
		User string `json:"user"`
		TS   string `json:"ts"`
	} `json:"edited"`
	Files []struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Filetype string `json:"filetype"`
		Size     int64  `json:"size"`
		Title    string `json:"title"`
	} `json:"files"`
	Reactions []struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	} `json:"reactions"`
	Attachments []rawAttachment `json:"attachments"`
	Blocks      json.RawMessage `json:"blocks"`
	BotProfile  *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"bot_profile"`
}

// rawAttachment is the legacy attachment shape. Apps still use it heavily, and
// a great many put the whole message body in here with an empty top-level
// text, so it cannot be treated as decoration.
type rawAttachment struct {
	Text     string          `json:"text"`
	Fallback string          `json:"fallback"`
	Title    string          `json:"title"`
	Pretext  string          `json:"pretext"`
	Blocks   json.RawMessage `json:"blocks"`
}

// History fetches messages from one conversation, paging as needed.
func (c *Client) History(ctx context.Context, o HistoryOptions) ([]output.Message, bool, error) {
	limit := o.Limit
	if limit <= 0 {
		limit = 100
	}

	var out []output.Message
	cursor := ""
	more := false

	for page := 0; page < maxPages; page++ {
		form := url.Values{}
		form.Set("channel", o.Channel)
		form.Set("limit", strconv.Itoa(min(limit-len(out), pageLimit)))
		form.Set("inclusive", strconv.FormatBool(o.Inclusive))

		switch {
		case o.Oldest != "":
			form.Set("oldest", o.Oldest)
		case o.Range.Oldest() != "":
			form.Set("oldest", o.Range.Oldest())
		}
		if l := o.Range.Latest(); l != "" {
			form.Set("latest", l)
		}
		if cursor != "" {
			form.Set("cursor", cursor)
		}

		var resp struct {
			Messages         []rawMessage `json:"messages"`
			HasMore          bool         `json:"has_more"`
			ResponseMetadata struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := c.call(ctx, "conversations.history", form, &resp); err != nil {
			return out, false, err
		}

		// The throttled non-Marketplace tier has an unmistakable signature:
		// ask for more than 15 and receive exactly 15 with more available.
		// Detecting it turns being wrong about the tier from broken to slow.
		if limit > 15 && len(resp.Messages) == 15 && resp.HasMore && page == 0 {
			c.log.Warn("conversations.history appears capped at 15 per request; " +
				"this workspace may be on the throttled tier")
		}

		for _, rm := range resp.Messages {
			out = append(out, rm.toOutput(o.Channel, c.workspaceHost()))
			if len(out) >= limit {
				return out, resp.HasMore || resp.ResponseMetadata.NextCursor != "", nil
			}
		}

		more = resp.HasMore
		next := resp.ResponseMetadata.NextCursor
		if !resp.HasMore || next == "" || next == cursor {
			break
		}
		cursor = next
	}
	return out, more, nil
}

// RepliesOptions selects the replies in one thread.
type RepliesOptions struct {
	Channel  string
	ThreadTS string
	Oldest   string
	Limit    int
}

// Replies fetches a thread.
//
// It is a separate call because conversations.history does not return threaded
// replies at all - which also means an unread reply on an old thread can never
// appear in a history result, however far back `oldest` reaches.
func (c *Client) Replies(ctx context.Context, o RepliesOptions) ([]output.Message, error) {
	limit := o.Limit
	if limit <= 0 {
		limit = 200
	}

	form := url.Values{}
	form.Set("channel", o.Channel)
	form.Set("ts", o.ThreadTS)
	form.Set("limit", strconv.Itoa(limit))
	form.Set("inclusive", "true")
	if o.Oldest != "" {
		form.Set("oldest", o.Oldest)
	}

	var resp struct {
		Messages []rawMessage `json:"messages"`
	}
	if err := c.call(ctx, "conversations.replies", form, &resp); err != nil {
		return nil, err
	}

	out := make([]output.Message, 0, len(resp.Messages))
	for _, rm := range resp.Messages {
		out = append(out, rm.toOutput(o.Channel, c.workspaceHost()))
	}
	return out, nil
}

func (rm rawMessage) toOutput(channel, host string) output.Message {
	ts, _ := slacktime.Parse(rm.TS)

	m := output.Message{
		Channel:    channel,
		TS:         rm.TS,
		User:       rm.User,
		UserName:   rm.Username,
		Text:       bestText(rm.Text, rm.Blocks, rm.Attachments),
		ThreadTS:   rm.ThreadTS,
		ReplyCount: rm.ReplyCount,
		Subtype:    rm.Subtype,
		Bot:        rm.BotID != "" || rm.Subtype == "bot_message",
		Edited:     rm.Edited != nil,
	}
	if !ts.IsZero() {
		m.Time = ts.Time().Format("2006-01-02T15:04:05Z07:00")
	}
	if host != "" && !ts.IsZero() {
		m.Permalink = fmt.Sprintf("https://%s/archives/%s/%s", host, channel, ts.Permalink())
	}

	for _, f := range rm.Files {
		m.Files = append(m.Files, output.File{
			ID: f.ID, Name: f.Name, Type: f.Filetype, Size: f.Size, Title: f.Title,
		})
	}
	for _, r := range rm.Reactions {
		m.Reactions = append(m.Reactions, output.Reaction{Name: r.Name, Count: r.Count})
	}
	m.AttachmentCount = len(rm.Attachments)

	// A bot post carries its name in any of three places, or none - in which
	// case the bot id is at least actionable, unlike a blank column.
	if m.Bot && m.UserName == "" {
		switch {
		case rm.BotProfile != nil && rm.BotProfile.Name != "":
			m.UserName = rm.BotProfile.Name
		case rm.BotID != "":
			m.BotID = rm.BotID
		}
	}
	return m
}

// workspaceHost is the host permalinks are built against.
func (c *Client) workspaceHost() string {
	base := strings.TrimPrefix(c.base, "https://")
	host, _, _ := strings.Cut(base, "/")
	return host
}
