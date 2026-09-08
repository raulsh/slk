// Package output renders results in the three shapes slk supports: aligned
// text for a human, one JSON envelope for an agent, and JSON Lines for a pipe.
//
// The JSON field names here are a public contract. An agent that shells out to
// slk parses them and cannot be redeployed when we rename something, so this
// package exists to make "is this a breaking change?" a question about one
// directory.
//
// Nothing but results reaches stdout. Log lines and human error prose go to
// stderr, unconditionally, so `slk ... --json | jq` is always safe.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/raulsh/slk/internal/errdefs"
)

// Schema is the envelope's contract version. It changes only when a field is
// removed or its meaning changes; adding a field does not bump it.
const Schema = 1

// Mode selects the rendering shape.
type Mode int

const (
	// ModeText is aligned, human-readable output. The default.
	ModeText Mode = iota
	// ModeJSON emits one self-describing envelope covering a complete result
	// set.
	ModeJSON
	// ModeJSONL emits one record per line, for streaming and for capping a
	// large pull with head(1).
	ModeJSONL
)

func (m Mode) String() string {
	switch m {
	case ModeJSON:
		return "json"
	case ModeJSONL:
		return "jsonl"
	default:
		return "text"
	}
}

// Machine reports whether this mode is meant for a program rather than a
// person. It is what --raw resolves against, so that --raw picks the envelope
// for a finite command and JSON Lines for a stream instead of forcing one.
func (m Mode) Machine() bool { return m != ModeText }

// Options configures a Writer. Command, Profile and Workspace are echoed into
// every envelope so an agent can tell which credentials produced a result
// without a second call.
type Options struct {
	Mode      Mode
	Color     bool
	Command   string
	Profile   string
	Workspace string
	Timezone  string
	// Full disables the text-mode truncation below. Machine modes are never
	// truncated - a consumer asked for the data, not a preview.
	Full bool
}

// Writer renders results and failures onto a pair of streams.
//
// It holds io.Writer rather than *os.File so that the whole CLI is testable
// in-process: a command test runs the real renderer into a bytes.Buffer.
type Writer struct {
	out  io.Writer
	errw io.Writer
	opts Options

	warnings  []string
	notes     []string
	truncated bool
	page      *Page
	window    *Window
	resolver  func(id string) (string, bool)
	seen      map[string]string
	loc       *time.Location
}

// New builds a Writer. out receives results, errw receives human prose.
func New(out, errw io.Writer, opts Options) *Writer {
	w := &Writer{out: out, errw: errw, opts: opts, seen: map[string]string{}}
	// An unknown timezone must not be fatal at render time; Validate already
	// rejects one in the config, so anything reaching here is a caller bug and
	// local is the safe answer.
	if opts.Timezone != "" && opts.Timezone != "Local" {
		if loc, err := time.LoadLocation(opts.Timezone); err == nil {
			w.loc = loc
		}
	}
	if w.loc == nil {
		w.loc = time.Local
	}
	return w
}

// Location reports the timezone timestamps are rendered in.
func (w *Writer) Location() *time.Location { return w.loc }

// Mode reports the configured rendering shape.
func (w *Writer) Mode() Mode { return w.opts.Mode }

// SetResolver installs id->name lookup. The Writer resolves entity references
// as it renders and accumulates only the ids it actually substituted, so the
// envelope's name map stays minimal without the caller tracking references.
func (w *Writer) SetResolver(fn func(id string) (string, bool)) { w.resolver = fn }

// SetPage records pagination state for the envelope.
func (w *Writer) SetPage(p *Page) { w.page = p }

// SetWindow records the resolved --since/--until, so an agent that asked for
// "7d" can state the window actually examined rather than guessing.
func (w *Writer) SetWindow(win *Window) { w.window = win }

// Truncate marks the result set as cut short and notes why.
func (w *Writer) Truncate(reason string) {
	w.truncated = true
	w.Warn("results truncated: " + reason)
}

// Warn records a non-fatal problem. Warnings travel inside the envelope in
// machine modes and go to stderr in text mode, so they are never mistaken for
// results.
func (w *Writer) Warn(msg string) {
	w.warnings = append(w.warnings, msg)
	if !w.opts.Mode.Machine() {
		w.note("warning: " + msg)
	}
}

// Info records metadata about the request itself - the query that was
// actually sent, the window that was actually examined.
//
// Separate from Warn because it is not a problem: labelling "query: deploy" as
// a warning trains a reader to ignore warnings.
func (w *Writer) Info(msg string) {
	w.notes = append(w.notes, msg)
	if !w.opts.Mode.Machine() {
		w.note(msg)
	}
}

// Fail renders a classified failure.
//
// In machine modes the error goes to stdout as a parseable envelope *and* to
// stderr as one human line. Duplicating costs nothing, means a --json consumer
// never sees empty stdout on failure, and keeps a human watching the terminal
// informed.
func (w *Writer) Fail(f errdefs.Fault) error {
	w.note("error: " + f.Message)
	if f.Hint != "" {
		w.note("")
		w.note(indent(f.Hint, "  "))
	}

	if !w.opts.Mode.Machine() {
		return nil
	}
	return w.encode(w.envelope(false, nil, 0, faultToError(f)))
}

// note writes one line to the human channel.
//
// A failed write to stderr has no recovery path - there is nowhere left to
// report it - so the error is discarded deliberately, here, rather than
// silently at every call site.
func (w *Writer) note(line string) {
	_, _ = fmt.Fprintln(w.errw, line)
}

func (w *Writer) encode(v any) error {
	enc := json.NewEncoder(w.out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encode output: %w", err)
	}
	return nil
}
