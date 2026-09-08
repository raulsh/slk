// Package errdefs classifies every failure slk can produce into a sentinel, a
// machine-readable code, and a process exit status.
//
// It is its own package because both ends of the failure path need the
// classification and neither can own it: the producers (config, slack) raise
// the sentinels, and output renders the code into JSON that an agent parses.
// Keeping the table here lets both import it without a cycle, and puts the
// whole exit-code contract in one reviewable file.
package errdefs

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Code is the machine-readable failure code that appears in --json output.
//
// These strings are part of the agent-facing contract: an agent branches on
// them, so they change only alongside the envelope schema version.
type Code string

const (
	CodeInternal     Code = "internal"
	CodeUsage        Code = "usage"
	CodeAuth         Code = "auth"
	CodeRateLimited  Code = "rate_limited"
	CodeNotFound     Code = "not_found"
	CodeNetwork      Code = "network"
	CodeConfig       Code = "config"
	CodeWriteRefused Code = "write_refused"
	CodePartial      Code = "partial"
	CodeInterrupted  Code = "interrupted"
)

// Process exit statuses.
//
// An agent has to branch on failure without parsing prose, so the causes get
// distinct codes rather than collapsing into 1. ExitAuth and ExitConfig are
// both "a human must act", but the remedies differ - a stale credential versus
// nothing configured at all - and that is the distinction an agent most needs.
// These deliberately avoid the shell-reserved 126, 127 and 128+N range.
const (
	ExitOK           = 0
	ExitInternal     = 1
	ExitUsage        = 2
	ExitAuth         = 3
	ExitRateLimited  = 4
	ExitNotFound     = 5
	ExitNetwork      = 6
	ExitConfig       = 7
	ExitWriteRefused = 8
	ExitPartial      = 9
	ExitInterrupted  = 130
)

// Sentinels. Producers wrap these with %w; nothing anywhere compares error
// text, so wording stays free to change.
var (
	ErrAuth         = errors.New("slack rejected the credentials")
	ErrRateLimited  = errors.New("slack rate limited the request")
	ErrNotFound     = errors.New("no such channel, user or message")
	ErrNotAllowed   = errors.New("not permitted to read that conversation")
	ErrTransport    = errors.New("could not reach slack")
	ErrConfig       = errors.New("configuration problem")
	ErrWriteRefused = errors.New("write refused")
	// ErrUnsupported means an endpoint slk relies on is unavailable or has
	// changed shape. It is separate from a transport failure because the
	// remedy differs: this one means fall through to a documented fallback.
	ErrUnsupported = errors.New("endpoint unavailable on this workspace")
	ErrPartial     = errors.New("run ended before completion")
	// ErrUsage marks a failure as the caller's fault rather than Slack's.
	// The root command's FlagErrorFunc wraps every pflag failure in one.
	ErrUsage = errors.New("invalid usage")
)

// Fault is a classified failure, ready to render. It is the single shape the
// output layer turns into an error envelope and the cli layer turns into an
// exit status.
type Fault struct {
	Code       Code
	Message    string
	Hint       string
	RetryAfter time.Duration
	SlackError string
}

// Exit is the process status this fault should exit with.
func (f Fault) Exit() int { return ExitFor(f.Code) }

// ExitFor maps a code onto its process exit status.
func ExitFor(c Code) int {
	switch c {
	case CodeUsage:
		return ExitUsage
	case CodeAuth:
		return ExitAuth
	case CodeRateLimited:
		return ExitRateLimited
	case CodeNotFound:
		return ExitNotFound
	case CodeNetwork:
		return ExitNetwork
	case CodeConfig:
		return ExitConfig
	case CodeWriteRefused:
		return ExitWriteRefused
	case CodePartial:
		return ExitPartial
	case CodeInterrupted:
		return ExitInterrupted
	default:
		return ExitInternal
	}
}

// Classify turns any error into a Fault by inspecting sentinels only.
//
// Order matters: context cancellation is checked before the transport
// sentinels because a cancelled request usually surfaces as a network error
// too, and reporting "could not reach slack" when the user pressed Ctrl-C
// would be a lie.
func Classify(err error) Fault {
	if err == nil {
		return Fault{}
	}

	f := Fault{Code: CodeInternal, Message: err.Error()}

	var h *hinted
	if errors.As(err, &h) {
		f.Hint = h.hint
	}
	var d *detailed
	if errors.As(err, &d) {
		f.SlackError = d.slackError
		f.RetryAfter = d.retryAfter
	}

	switch {
	case errors.Is(err, ErrUsage):
		f.Code = CodeUsage
	case errors.Is(err, context.Canceled):
		f.Code = CodeInterrupted
	case errors.Is(err, ErrAuth):
		f.Code = CodeAuth
	case errors.Is(err, ErrRateLimited):
		f.Code = CodeRateLimited
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrNotAllowed):
		f.Code = CodeNotFound
	case errors.Is(err, ErrConfig):
		f.Code = CodeConfig
	case errors.Is(err, ErrWriteRefused):
		f.Code = CodeWriteRefused
	case errors.Is(err, ErrPartial):
		f.Code = CodePartial
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, ErrTransport):
		f.Code = CodeNetwork
	}
	return f
}

// WithHint attaches remediation text to an error without changing its
// identity, so errors.Is still matches the sentinel underneath.
//
// The hint is what makes a routine failure actionable: an expired xoxc token
// is the most common thing that will ever go wrong, and the difference between
// a usable tool and an infuriating one is whether the error tells you to run
// `slk configure` or just says "invalid_auth".
func WithHint(err error, hint string) error {
	if err == nil {
		return nil
	}
	return &hinted{err: err, hint: hint}
}

type hinted struct {
	err  error
	hint string
}

func (h *hinted) Error() string { return h.err.Error() }
func (h *hinted) Unwrap() error { return h.err }

// WithDetail attaches Slack's own error string and any requested backoff, so
// they can be surfaced in the JSON an agent parses without any caller having
// to re-derive them.
func WithDetail(err error, slackError string, retryAfter time.Duration) error {
	if err == nil {
		return nil
	}
	return &detailed{err: err, slackError: slackError, retryAfter: retryAfter}
}

type detailed struct {
	err        error
	slackError string
	retryAfter time.Duration
}

func (d *detailed) Error() string { return d.err.Error() }
func (d *detailed) Unwrap() error { return d.err }

// Usagef builds a usage error, which is always the caller's mistake and always
// exits 2.
//
// It carries its own message rather than wrapping ErrUsage with %w so that the
// sentinel's text does not surface: the reader wants "unknown flag: --nope",
// not "invalid usage: unknown flag: --nope". Unwrap keeps errors.Is working.
func Usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

type usageError struct{ msg string }

func (u *usageError) Error() string { return u.msg }
func (u *usageError) Unwrap() error { return ErrUsage }
