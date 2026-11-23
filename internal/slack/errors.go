package slack

import (
	"errors"
	"fmt"
	"time"

	"github.com/raulsh/slk/internal/errdefs"
)

var (
	// ErrCredentials means the credential pair is incomplete before we even
	// tried it.
	ErrCredentials = errors.New("credentials are incomplete")
	// ErrLoginPage means Slack served the sign-in page instead of JSON, which
	// is what a dead cookie looks like.
	ErrLoginPage = errors.New("slack returned a login page instead of JSON")
	// errNoChange means a write found the message already in the state it
	// asked for. It never leaves this package: React turns it into success.
	errNoChange = errors.New("already in the requested state")
)

// slackErrorSentinel maps Slack's error strings onto slk's sentinels.
//
// This table is the only place error strings are compared. Every caller uses
// errors.Is against a sentinel, so a Slack rename or an unfamiliar code
// degrades to "internal" rather than being silently mishandled somewhere.
var slackErrorSentinel = map[string]error{
	// Dead or wrong credentials. This is the routine failure: an xoxc token
	// expires when the browser session rotates.
	"invalid_auth":       errdefs.ErrAuth,
	"not_authed":         errdefs.ErrAuth,
	"token_expired":      errdefs.ErrAuth,
	"token_revoked":      errdefs.ErrAuth,
	"account_inactive":   errdefs.ErrAuth,
	"missing_auth":       errdefs.ErrAuth,
	"org_login_required": errdefs.ErrAuth,

	"channel_not_found": errdefs.ErrNotFound,
	"user_not_found":    errdefs.ErrNotFound,
	"thread_not_found":  errdefs.ErrNotFound,
	"message_not_found": errdefs.ErrNotFound,
	"invalid_thread_ts": errdefs.ErrNotFound,

	"not_in_channel":          errdefs.ErrNotAllowed,
	"is_archived":             errdefs.ErrNotAllowed,
	"missing_scope":           errdefs.ErrNotAllowed,
	"restricted_action":       errdefs.ErrNotAllowed,
	"team_access_not_granted": errdefs.ErrNotAllowed,

	// A post refused because of where it was aimed. The remedy is the same as
	// for a conversation the user cannot see - another target - so it shares
	// that classification rather than inventing a new exit code.
	"cannot_reply_to_message":                  errdefs.ErrNotAllowed,
	"posting_to_general_channel_denied":        errdefs.ErrNotAllowed,
	"restricted_action_read_only_channel":      errdefs.ErrNotAllowed,
	"restricted_action_thread_only_channel":    errdefs.ErrNotAllowed,
	"restricted_action_non_threadable_channel": errdefs.ErrNotAllowed,
	"restricted_action_thread_locked":          errdefs.ErrNotAllowed,
	"messages_tab_disabled":                    errdefs.ErrNotAllowed,
	"cannot_dm_bot":                            errdefs.ErrNotAllowed,
	"user_disabled":                            errdefs.ErrNotAllowed,
	"thread_locked":                            errdefs.ErrNotAllowed,
	"not_reactable":                            errdefs.ErrNotAllowed,
	"too_many_reactions":                       errdefs.ErrNotAllowed,
	"too_many_emoji":                           errdefs.ErrNotAllowed,

	// The caller's own mistake in what it asked to send: fix the invocation.
	"msg_too_long":  errdefs.ErrUsage,
	"no_text":       errdefs.ErrUsage,
	"invalid_name":  errdefs.ErrUsage,
	"bad_timestamp": errdefs.ErrUsage,

	// The reaction is already there, or already gone: the state asked for.
	"already_reacted": errNoChange,
	"no_reaction":     errNoChange,

	"ratelimited":  errdefs.ErrRateLimited,
	"rate_limited": errdefs.ErrRateLimited,
	// A workspace's message quota rather than a per-method rate, but the
	// remedy - wait and try later - is the same.
	"message_limit_exceeded": errdefs.ErrRateLimited,
}

// APIError is a non-ok Slack response.
type APIError struct {
	// Method is the API method, e.g. "auth.test".
	Method string
	// Code is Slack's own error string, e.g. "invalid_auth".
	Code string
	// HTTPStatus is the transport status, which is almost always 200: Slack
	// reports application failures in the body.
	HTTPStatus int
	RetryAfter time.Duration
	// Body holds the first bytes of a non-JSON response, so an HTML login page
	// is diagnosable without re-running with --log-level debug.
	Body string
}

func (e *APIError) Error() string {
	switch {
	case e.Code != "":
		return fmt.Sprintf("%s: slack returned %s", e.Method, e.Code)
	case e.Body != "":
		return fmt.Sprintf("%s: unexpected non-JSON response (%d): %s", e.Method, e.HTTPStatus, e.Body)
	default:
		return fmt.Sprintf("%s: http %d", e.Method, e.HTTPStatus)
	}
}

// Unwrap maps this failure onto a sentinel so callers never compare strings.
func (e *APIError) Unwrap() error {
	if s, ok := slackErrorSentinel[e.Code]; ok {
		return s
	}
	switch e.HTTPStatus {
	case 401, 403:
		return errdefs.ErrAuth
	case 429:
		return errdefs.ErrRateLimited
	case 404:
		return errdefs.ErrNotFound
	}
	if e.HTTPStatus >= 500 {
		return errdefs.ErrTransport
	}
	return nil
}

// authHint is the most important string in the tool.
//
// Credential expiry is routine rather than exceptional, so the error has to do
// the whole job: say what expired, why, and exactly what to re-copy.
func (c *Client) authHint(slackError string) string {
	return fmt.Sprintf(`The xoxc token and d cookie are browser session credentials. They expire when
you sign out, change your password, or after a few weeks. Get a fresh pair:

    slk configure --profile %s --from-curl -

To find them: open %s.slack.com, then DevTools (F12) >
Network > filter on /api/ > right-click a request > Copy > Copy as cURL,
and paste that in. Slack reported: %s`, c.creds.Workspace, c.creds.Workspace, slackError)
}

// annotate attaches the Slack error string, any requested backoff, and - for
// an expired credential - the remediation text, so the command layer renders a
// useful failure without knowing anything about Slack.
func (c *Client) annotate(err error) error {
	var ae *APIError
	if !errors.As(err, &ae) {
		return err
	}
	err = errdefs.WithDetail(err, ae.Code, ae.RetryAfter)
	if errors.Is(err, errdefs.ErrAuth) {
		err = errdefs.WithHint(err, c.authHint(ae.Code))
	}
	return err
}
