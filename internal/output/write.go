package output

import "fmt"

// Sent is what `send` reports: where the message went, and the ids that find
// it again.
//
// It is not a Message. The caller already knows who wrote it and when; what
// it needs next is the address. channel, ts and thread_ts are exactly what
// `send --thread`, `react` and `thread` take back, and the permalink is what a
// person clicks.
type Sent struct {
	// DryRun is always present, so a rehearsal and a post can be told apart
	// without knowing that ts is absent from one of them.
	DryRun bool `json:"dry_run"`
	// Channel is empty only under --dry-run, for a DM that a real run would
	// first have to open.
	Channel     string `json:"channel"`
	ChannelName string `json:"channel_name,omitempty"`
	TS          string `json:"ts,omitempty"`
	// ThreadTS is the thread root the message went under, which is not
	// necessarily the ts the caller gave: a reply's ts is resolved to its
	// root first.
	ThreadTS  string `json:"thread_ts,omitempty"`
	Permalink string `json:"permalink,omitempty"`
	// Text is exactly what was sent, after escaping. It can differ from what
	// was typed, and that difference is the thing worth checking in a dry run.
	Text string `json:"text"`
}

// Sent renders the outcome of a post.
func (w *Writer) Sent(s Sent) error {
	switch w.opts.Mode {
	case ModeJSON:
		return w.encode(w.envelope(true, []Sent{s}, 1, nil))
	case ModeJSONL:
		return w.encode(s)
	default:
		where := displayName(s.ChannelName, s.Channel)
		if s.ThreadTS != "" {
			where += " in thread " + s.ThreadTS
		}
		if s.DryRun {
			// The whole body, untruncated and unwrapped: reading it is the
			// point of a rehearsal.
			_, err := fmt.Fprintf(w.out, "would send to %s:\n%s\n", where, indent(s.Text, "  "))
			return err
		}
		_, err := fmt.Fprintf(w.out, "sent to %s  %s\n", where, w.colorize(dim, s.Permalink))
		return err
	}
}

// Reacted is what `react` reports.
type Reacted struct {
	DryRun bool `json:"dry_run"`
	// Action is "add" or "remove".
	Action      string `json:"action"`
	Channel     string `json:"channel"`
	ChannelName string `json:"channel_name,omitempty"`
	TS          string `json:"ts"`
	// Name is the emoji's Slack name without colons, the same field a
	// message's reactions carry.
	Name string `json:"name"`
	// Changed is false when the message was already in the requested state -
	// the reaction was already there, or already gone. That is still a
	// success, because the outcome asked for holds, and it is what makes a
	// retry safe. In a dry run nothing changes, so it is false there too.
	Changed bool `json:"changed"`
}

// Reacted renders the outcome of a reaction.
func (w *Writer) Reacted(r Reacted) error {
	switch w.opts.Mode {
	case ModeJSON:
		return w.encode(w.envelope(true, []Reacted{r}, 1, nil))
	case ModeJSONL:
		return w.encode(r)
	default:
		emoji := ":" + r.Name + ":"
		msg := displayName(r.ChannelName, r.Channel) + " " + r.TS
		var line string
		switch {
		case r.DryRun && r.Action == "remove":
			line = "would remove " + emoji + " from " + msg
		case r.DryRun:
			line = "would add " + emoji + " to " + msg
		case r.Action == "remove" && r.Changed:
			line = "removed " + emoji + " from " + msg
		case r.Action == "remove":
			line = emoji + " was not on " + msg
		case r.Changed:
			line = "added " + emoji + " to " + msg
		default:
			line = emoji + " was already on " + msg
		}
		_, err := fmt.Fprintln(w.out, line)
		return err
	}
}
