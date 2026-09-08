package output

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Message is the compact wire form of a Slack message.
//
// Compact by default because the primary consumer pays per token: blocks are
// typically five to twenty times the size of text and near-duplicative, and
// full attachments likewise, so both are available only under --raw.
type Message struct {
	Channel     string `json:"channel"`
	ChannelName string `json:"channel_name,omitempty"`
	// TS is the message's identity as well as its time - `slk thread` needs
	// it - which is why both this and Time are kept.
	TS   string `json:"ts"`
	Time string `json:"time"`

	User     string `json:"user,omitempty"`
	UserName string `json:"user_name,omitempty"`

	Text string `json:"text"`
	// TextRendered carries the same text with entity references resolved. It
	// is separate, and present only when it differs, because a consumer often
	// needs the ids to act on and the names to reason about; destroying either
	// forces a second call.
	TextRendered string `json:"text_rendered,omitempty"`

	ThreadTS   string `json:"thread_ts,omitempty"`
	ReplyCount int    `json:"reply_count,omitempty"`
	// HasNewReplies is set when the thread's latest reply postdates the
	// channel's read marker, so an unread run can flag a thread it did not
	// descend into rather than silently omitting it.
	HasNewReplies bool `json:"has_new_replies,omitempty"`

	Edited  bool   `json:"edited,omitempty"`
	Subtype string `json:"subtype,omitempty"`
	Bot     bool   `json:"bot,omitempty"`
	// VIP marks a message from someone you flagged VIP in Slack. It is your
	// own classification, which makes it the best triage signal available:
	// "did anyone I care about say something".
	VIP bool `json:"vip,omitempty"`
	// BotID is kept when a bot post carries no resolvable name, so the sender
	// is still identifiable rather than blank.
	BotID  string `json:"bot_id,omitempty"`
	Unread bool   `json:"unread,omitempty"`

	Files     []File     `json:"files,omitempty"`
	Reactions []Reaction `json:"reactions,omitempty"`
	// AttachmentCount tells a consumer there is more to see under --raw
	// without paying for it now.
	AttachmentCount int `json:"attachment_count,omitempty"`

	Permalink string          `json:"permalink,omitempty"`
	Raw       json.RawMessage `json:"raw,omitempty"`
}

type File struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	Type  string `json:"type,omitempty"`
	Size  int64  `json:"size,omitempty"`
	Title string `json:"title,omitempty"`
}

// Reaction keeps the name and count only. The users array is large, rarely
// read, and available under --raw.
type Reaction struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// Messages renders a message list.
func (w *Writer) Messages(ms []Message) error {
	switch w.opts.Mode {
	case ModeJSON:
		return w.encode(w.envelope(true, ms, len(ms), nil))
	case ModeJSONL:
		for _, m := range ms {
			if err := w.encode(m); err != nil {
				return err
			}
		}
		return nil
	default:
		return w.messagesText(ms)
	}
}

// messagesText groups by conversation and adapts the timestamp to distance,
// which is both a real width saving and how a person reads a log.
func (w *Writer) messagesText(ms []Message) error {
	if len(ms) == 0 {
		w.note("no messages")
		return nil
	}

	// A channel header only earns its line when the result spans more than one
	// conversation.
	multi := false
	for _, m := range ms {
		if m.Channel != ms[0].Channel {
			multi = true
			break
		}
	}

	current := ""
	t := w.table().borderless()
	flush := func() error {
		if t.empty() {
			return nil
		}
		err := t.render()
		t = w.table().borderless()
		return err
	}

	for _, m := range ms {
		if multi && m.Channel != current {
			if err := flush(); err != nil {
				return err
			}
			current = m.Channel
			if _, err := fmt.Fprintf(w.out, "%s\n",
				w.colorize(bold, displayName(m.ChannelName, m.Channel))); err != nil {
				return err
			}
		}

		text := m.TextRendered
		if text == "" {
			text = m.Text
		}
		if strings.TrimSpace(text) == "" {
			// A file share or image-only post has no text. Saying so beats a
			// blank row the reader has to interpret.
			text = w.colorize(dim, emptyBody(m))
		}
		// One line per message: a log that wraps is unreadable and
		// ungreppable, so newlines become a visible marker instead.
		text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", " ⏎ "), "\n", " ⏎ ")
		text = w.truncate(text)

		marks := ""
		if m.ReplyCount > 0 {
			marks += fmt.Sprintf(" ↩%d", m.ReplyCount)
		}
		switch {
		case m.Bot || m.Subtype == "bot_message":
			marks += " [bot]"
		case m.Subtype != "":
			marks += " [" + m.Subtype + "]"
		}

		// The star is its own cell, so it aligns by construction rather than
		// by padding arithmetic that ANSI codes would defeat anyway.
		vip := ""
		if m.VIP {
			vip = w.colorize(yellow, "★")
		}
		t.row(vip, w.stamp(m.Time), w.colorize(cyan, sender(m)), text+w.colorize(dim, marks))
	}
	return flush()
}

// textWidth bounds a rendered message in text mode.
//
// A bot post with blocks can run to several thousand characters, and one such
// message destroys the readability of a whole screen. JSON output is never
// truncated, and --full turns this off, so nothing is actually lost.
const textWidth = 220

func (w *Writer) truncate(s string) string {
	if w.opts.Full || len([]rune(s)) <= textWidth {
		return s
	}
	r := []rune(s)
	return string(r[:textWidth]) + w.colorize(dim, " …(--full for the rest)")
}

// stamp renders a timestamp at a width proportional to how far away it is.
func (w *Writer) stamp(rfc string) string {
	t, err := time.Parse(time.RFC3339, rfc)
	if err != nil {
		return rfc
	}
	t = t.In(w.location())
	n := time.Now().In(w.location())
	switch {
	case sameDay(t, n):
		return t.Format("15:04")
	case t.Year() == n.Year():
		return t.Format("Jan 02 15:04")
	default:
		return t.Format("2006-01-02 15:04")
	}
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

func (w *Writer) location() *time.Location {
	if w.loc != nil {
		return w.loc
	}
	return time.Local
}

// displayName prefers a resolved name and falls back to the raw id, so an
// unresolvable reference is still actionable rather than blank.
func displayName(name, id string) string {
	if name != "" {
		return name
	}
	return id
}

// SortMessages orders chronologically. Slack returns newest-first, but a
// conversation reads oldest-first, so selection order and print order differ
// deliberately.
func SortMessages(ms []Message, oldestFirst bool) {
	sort.SliceStable(ms, func(i, j int) bool {
		if ms[i].Channel != ms[j].Channel {
			return ms[i].Channel < ms[j].Channel
		}
		if oldestFirst {
			return ms[i].TS < ms[j].TS
		}
		return ms[i].TS > ms[j].TS
	})
}

// emptyBody describes a message that rendered to nothing.
func emptyBody(m Message) string {
	switch {
	case len(m.Files) > 0:
		names := make([]string, 0, len(m.Files))
		for _, f := range m.Files {
			n := f.Name
			if n == "" {
				n = f.Title
			}
			if n == "" {
				n = f.Type
			}
			names = append(names, n)
		}
		return "[file: " + strings.Join(names, ", ") + "]"
	case m.AttachmentCount > 0:
		return fmt.Sprintf("[%d attachment(s), --raw to see them]", m.AttachmentCount)
	default:
		return "[no text]"
	}
}

// sender names whoever posted, trying every place an identity can hide before
// giving up: an app post often carries no user and no username, only a bot id.
func sender(m Message) string {
	switch {
	case m.UserName != "":
		return m.UserName
	case m.User != "":
		return m.User
	case m.BotID != "":
		return m.BotID
	default:
		return "?"
	}
}
