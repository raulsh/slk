package output

import (
	"strings"

	"github.com/raulsh/slk/internal/errdefs"
)

// envelope is the single object ModeJSON emits.
//
// It is self-describing on purpose: an agent that receives one should never
// need a second call to interpret it, which is why the id->name map and the
// resolved time window travel with the results.
//
// It is unexported, with Results as any, because the public API is the typed
// Writer methods - callers never build one, so the JSON shape is defined in
// exactly one place.
type envelope struct {
	OK        bool     `json:"ok"`
	Schema    int      `json:"schema"`
	Command   string   `json:"command"`
	Profile   string   `json:"profile,omitempty"`
	Workspace string   `json:"workspace,omitempty"`
	Count     int      `json:"count"`
	Results   any      `json:"results"`
	Names     *Names   `json:"names,omitempty"`
	Page      *Page    `json:"page,omitempty"`
	Window    *Window  `json:"window,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
	Notes     []string `json:"notes,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
	Error     *Error   `json:"error,omitempty"`
}

// Names resolves the ids that appear in Results, and only those, so it stays
// small: an agent needs no follow-up call to turn U0123ABCD into a name.
type Names struct {
	Users    map[string]string `json:"users,omitempty"`
	Channels map[string]string `json:"channels,omitempty"`
}

// Page reports what more there is to fetch.
type Page struct {
	NextCursor string `json:"next_cursor,omitempty"`
	Page       int    `json:"page,omitempty"`
	Pages      int    `json:"pages,omitempty"`
	Total      int    `json:"total,omitempty"`
	HasMore    bool   `json:"has_more"`
}

// Window echoes the resolved --since/--until in both representations, so a
// caller never has to know Slack's epoch encoding to say what it looked at.
type Window struct {
	Since   string `json:"since,omitempty"`    // RFC3339
	Until   string `json:"until,omitempty"`    // RFC3339
	SinceTS string `json:"since_ts,omitempty"` // Slack ts
	UntilTS string `json:"until_ts,omitempty"`
	TZ      string `json:"tz"`
}

// Error is the failure shape an agent parses.
type Error struct {
	Code       errdefs.Code `json:"code"`
	Message    string       `json:"message"`
	Hint       string       `json:"hint,omitempty"`
	RetryAfter int          `json:"retry_after,omitempty"` // seconds
	SlackError string       `json:"slack_error,omitempty"`
}

func faultToError(f errdefs.Fault) *Error {
	e := &Error{
		Code:       f.Code,
		Message:    f.Message,
		Hint:       f.Hint,
		SlackError: f.SlackError,
	}
	if f.RetryAfter > 0 {
		e.RetryAfter = int(f.RetryAfter.Seconds())
	}
	return e
}

// envelope assembles the wrapper around a result set. results is passed
// through as-is so each Writer method keeps its concrete slice type.
func (w *Writer) envelope(ok bool, results any, count int, failure *Error) envelope {
	env := envelope{
		OK:        ok,
		Schema:    Schema,
		Command:   w.opts.Command,
		Profile:   w.opts.Profile,
		Workspace: w.opts.Workspace,
		Count:     count,
		Results:   results,
		Page:      w.page,
		Window:    w.window,
		Truncated: w.truncated,
		Notes:     w.notes,
		Warnings:  w.warnings,
		Error:     failure,
	}
	// An absent results array would force every consumer into a nil check, so
	// a failure envelope still carries an empty one.
	if env.Results == nil {
		env.Results = []any{}
	}
	if n := w.names(); n != nil {
		env.Names = n
	}
	return env
}

// names reports the ids actually substituted during rendering, split by kind.
// Slack ids are prefixed by type, which is what makes the split free.
func (w *Writer) names() *Names {
	if len(w.seen) == 0 {
		return nil
	}
	n := &Names{}
	for id, name := range w.seen {
		switch {
		case strings.HasPrefix(id, "U"), strings.HasPrefix(id, "W"), strings.HasPrefix(id, "B"):
			if n.Users == nil {
				n.Users = map[string]string{}
			}
			n.Users[id] = name
		case strings.HasPrefix(id, "C"), strings.HasPrefix(id, "D"), strings.HasPrefix(id, "G"):
			if n.Channels == nil {
				n.Channels = map[string]string{}
			}
			n.Channels[id] = name
		}
	}
	if n.Users == nil && n.Channels == nil {
		return nil
	}
	return n
}

// indent prefixes every line, so a multi-line remediation hint stays visually
// attached to the error above it.
func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "\n")
}
