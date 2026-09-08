package output

import (
	"encoding/json"
	"fmt"
)

// Conversation is a channel, group DM or DM.
type Conversation struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Type is channel, private, mpim or dm - flattened from Slack's
	// overlapping is_* booleans, because a caller wants one answer.
	Type       string `json:"type"`
	Topic      string `json:"topic,omitempty"`
	Purpose    string `json:"purpose,omitempty"`
	User       string `json:"user,omitempty"`
	UserName   string `json:"user_name,omitempty"`
	Members    int    `json:"members,omitempty"`
	IsArchived bool   `json:"is_archived,omitempty"`
	IsShared   bool   `json:"is_shared,omitempty"`
	// HasUnreads is what client.counts reports. There is deliberately no
	// unread count: client.counts does not provide one, and inventing a number
	// would be worse than admitting we have a boolean.
	HasUnreads   bool            `json:"has_unreads,omitempty"`
	MentionCount int             `json:"mention_count,omitempty"`
	LastRead     string          `json:"last_read,omitempty"`
	LatestTS     string          `json:"latest_ts,omitempty"`
	LatestTime   string          `json:"latest_time,omitempty"`
	Raw          json.RawMessage `json:"raw,omitempty"`
}

// Conversations renders a conversation list.
func (w *Writer) Conversations(cs []Conversation) error {
	switch w.opts.Mode {
	case ModeJSON:
		return w.encode(w.envelope(true, cs, len(cs), nil))
	case ModeJSONL:
		for _, c := range cs {
			if err := w.encode(c); err != nil {
				return err
			}
		}
		return nil
	default:
		if len(cs) == 0 {
			w.note("no conversations")
			return nil
		}
		t := w.table("TYPE", "NAME", "UNREAD", "@", "LAST", "ID").rightAlign(3)
		for _, c := range cs {
			unread := ""
			if c.HasUnreads {
				unread = w.colorize(yellow, "yes")
			}
			mentions := ""
			if c.MentionCount > 0 {
				mentions = w.colorize(yellow, fmt.Sprintf("%d", c.MentionCount))
			}
			last := ""
			if c.LatestTime != "" {
				last = w.stamp(c.LatestTime)
			}
			t.row(c.Type, c.Name, unread, mentions, last, c.ID)
		}
		return t.render()
	}
}

// User is a workspace member.
type User struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	RealName string `json:"real_name,omitempty"`
	Display  string `json:"display_name,omitempty"`
	Email    string `json:"email,omitempty"`
	Title    string `json:"title,omitempty"`
	TZ       string `json:"tz,omitempty"`
	Bot      bool   `json:"bot,omitempty"`
	Deleted  bool   `json:"deleted,omitempty"`
	// VIP reflects the people you have marked VIP in the Slack client. It is
	// your own classification, so it is the most useful triage signal slk can
	// carry: "did anyone I care about say something".
	VIP bool            `json:"vip,omitempty"`
	Raw json.RawMessage `json:"raw,omitempty"`
}

// Users renders a user list.
func (w *Writer) Users(us []User) error {
	switch w.opts.Mode {
	case ModeJSON:
		return w.encode(w.envelope(true, us, len(us), nil))
	case ModeJSONL:
		for _, u := range us {
			if err := w.encode(u); err != nil {
				return err
			}
		}
		return nil
	default:
		if len(us) == 0 {
			w.note("no users")
			return nil
		}
		// The star gets its own column rather than a name prefix, so it
		// aligns by construction instead of by arithmetic.
		t := w.table("", "HANDLE", "NAME", "TITLE", "ID")
		for _, u := range us {
			flags := ""
			if u.Bot {
				flags = " [bot]"
			}
			if u.Deleted {
				flags += " [deactivated]"
			}
			vip := ""
			if u.VIP {
				vip = w.colorize(yellow, "★")
			}
			t.row(vip, "@"+u.Name, u.RealName+flags, u.Title, u.ID)
		}
		return t.render()
	}
}

// Unread summarises where there is something to read.
//
// Count is a *fetched* count, not a figure Slack supplied: client.counts
// reports only has_unreads and mention_count, so a number here means messages
// were actually retrieved and counted. Zero with HasUnreads true means "there
// is something, but this run did not fetch it".
type Unread struct {
	Channel        string `json:"channel"`
	ChannelName    string `json:"channel_name"`
	Type           string `json:"type"`
	Count          int    `json:"count"`
	Counted        bool   `json:"counted"`
	MentionCount   int    `json:"mention_count"`
	ThreadUnread   int    `json:"thread_unread,omitempty"`
	ThreadMentions int    `json:"thread_mentions,omitempty"`
	LastReadTS     string `json:"last_read_ts,omitempty"`
	LatestTS       string `json:"latest_ts,omitempty"`
	LatestTime     string `json:"latest_time,omitempty"`
	// VIP marks a conversation with activity from someone you flagged VIP.
	VIP bool `json:"vip,omitempty"`
	// ReadState is ok, never_read or unavailable, so a consumer can tell a
	// quiet channel from one whose read state could not be determined.
	ReadState string `json:"read_state"`
}

// Unreads renders an unread summary.
func (w *Writer) Unreads(us []Unread) error {
	switch w.opts.Mode {
	case ModeJSON:
		return w.encode(w.envelope(true, us, len(us), nil))
	case ModeJSONL:
		for _, u := range us {
			if err := w.encode(u); err != nil {
				return err
			}
		}
		return nil
	default:
		if len(us) == 0 {
			w.note("nothing unread")
			return nil
		}
		t := w.table("", "NAME", "TYPE", "UNREAD", "@", "THREADS", "LATEST").rightAlign(4, 5)
		totalMentions, totalThreads := 0, 0
		for _, u := range us {
			count := "yes"
			if u.Counted {
				count = fmt.Sprintf("%d", u.Count)
			}
			if u.ReadState == "never_read" {
				count = "never read"
			}
			mentions := ""
			if u.MentionCount > 0 {
				mentions = w.colorize(yellow, fmt.Sprintf("%d", u.MentionCount))
				totalMentions += u.MentionCount
			}
			threads := ""
			if u.ThreadUnread > 0 || u.ThreadMentions > 0 {
				threads = fmt.Sprintf("%d", u.ThreadUnread)
				if u.ThreadMentions > 0 {
					threads += fmt.Sprintf(" (%d @)", u.ThreadMentions)
				}
				totalThreads += u.ThreadUnread
			}
			vip := ""
			if u.VIP {
				vip = w.colorize(yellow, "★")
			}
			t.row(vip, u.ChannelName, u.Type, count, mentions, threads, w.stamp(u.LatestTime))
		}
		if err := t.render(); err != nil {
			return err
		}
		w.note(fmt.Sprintf("%d conversations unread, %d mentions, %d thread messages",
			len(us), totalMentions, totalThreads))
		return nil
	}
}
