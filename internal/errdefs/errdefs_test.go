package errdefs

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want Code
		exit int
	}{
		{"nil", nil, "", ExitOK},
		{"bare error", errors.New("boom"), CodeInternal, ExitInternal},
		{"usage", Usagef("no target given"), CodeUsage, ExitUsage},
		{"auth", ErrAuth, CodeAuth, ExitAuth},
		{"wrapped auth", fmt.Errorf("auth.test: %w", ErrAuth), CodeAuth, ExitAuth},
		{"rate limited", ErrRateLimited, CodeRateLimited, ExitRateLimited},
		{"not found", ErrNotFound, CodeNotFound, ExitNotFound},
		{"not allowed maps to not found", ErrNotAllowed, CodeNotFound, ExitNotFound},
		{"config", ErrConfig, CodeConfig, ExitConfig},
		{"write refused", ErrWriteRefused, CodeWriteRefused, ExitWriteRefused},
		{"partial", ErrPartial, CodePartial, ExitPartial},
		{"transport", ErrTransport, CodeNetwork, ExitNetwork},
		{"deadline", context.DeadlineExceeded, CodeNetwork, ExitNetwork},
		{"canceled", context.Canceled, CodeInterrupted, ExitInterrupted},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.err)
			if got.Code != tc.want {
				t.Errorf("Classify(%v).Code = %q, want %q", tc.err, got.Code, tc.want)
			}
			if tc.err != nil && got.Exit() != tc.exit {
				t.Errorf("Classify(%v).Exit() = %d, want %d", tc.err, got.Exit(), tc.exit)
			}
		})
	}
}

// TestCancellationWinsOverTransport documents the ordering choice in Classify:
// a cancelled request usually surfaces as a network failure too, and reporting
// "could not reach slack" when the user pressed Ctrl-C would be a lie.
func TestCancellationWinsOverTransport(t *testing.T) {
	err := fmt.Errorf("%w: %w", ErrTransport, context.Canceled)
	if got := Classify(err).Code; got != CodeInterrupted {
		t.Errorf("Classify(transport+canceled) = %q, want %q", got, CodeInterrupted)
	}
}

func TestWithHintPreservesIdentity(t *testing.T) {
	err := WithHint(fmt.Errorf("auth.test: %w", ErrAuth), "run: slk configure")

	if !errors.Is(err, ErrAuth) {
		t.Error("errors.Is(WithHint(ErrAuth), ErrAuth) = false, want true")
	}
	f := Classify(err)
	if f.Code != CodeAuth {
		t.Errorf("Code = %q, want %q", f.Code, CodeAuth)
	}
	if f.Hint != "run: slk configure" {
		t.Errorf("Hint = %q, want %q", f.Hint, "run: slk configure")
	}
}

func TestWithDetailCarriesSlackErrorAndBackoff(t *testing.T) {
	err := WithDetail(fmt.Errorf("conversations.history: %w", ErrRateLimited), "ratelimited", 30*time.Second)

	f := Classify(err)
	if f.Code != CodeRateLimited {
		t.Errorf("Code = %q, want %q", f.Code, CodeRateLimited)
	}
	if f.SlackError != "ratelimited" {
		t.Errorf("SlackError = %q, want %q", f.SlackError, "ratelimited")
	}
	if f.RetryAfter != 30*time.Second {
		t.Errorf("RetryAfter = %v, want %v", f.RetryAfter, 30*time.Second)
	}
}

// TestHintAndDetailCompose is worth its own case because the two wrappers are
// applied independently by different layers - the transport attaches the Slack
// error, the command attaches the remediation - and Classify must find both
// however they are nested.
func TestHintAndDetailCompose(t *testing.T) {
	inner := WithDetail(fmt.Errorf("auth.test: %w", ErrAuth), "invalid_auth", 0)
	err := WithHint(inner, "run: slk configure")

	f := Classify(err)
	if f.Hint == "" || f.SlackError != "invalid_auth" || f.Code != CodeAuth {
		t.Errorf("Classify = %+v, want code=%q hint set slack_error=%q",
			f, CodeAuth, "invalid_auth")
	}
}

// TestWithHintNilStaysNil keeps the wrappers safe to apply unconditionally at
// a call site that may or may not have an error.
func TestWithHintNilStaysNil(t *testing.T) {
	if got := WithHint(nil, "x"); got != nil {
		t.Errorf("WithHint(nil) = %v, want nil", got)
	}
	if got := WithDetail(nil, "x", 0); got != nil {
		t.Errorf("WithDetail(nil) = %v, want nil", got)
	}
}
