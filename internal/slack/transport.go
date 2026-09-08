package slack

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/raulsh/slk/internal/errdefs"
)

// chromeMajor is the Chrome version the request envelope claims to be.
//
// The user agent and the sec-ch-ua client hint must agree: real Chrome never
// emits a mismatched pair, so an inconsistent one is a worse signature than
// sending neither. Both are derived from this constant for that reason - never
// bump one alone.
const chromeMajor = 150

var (
	defaultUserAgent = fmt.Sprintf(
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.0.0 Safari/537.36",
		chromeMajor)
	secChUA = fmt.Sprintf(`"Not;A=Brand";v="8", "Chromium";v="%d", "Google Chrome";v="%d"`,
		chromeMajor, chromeMajor)
)

// maxErrorBody bounds how much of a non-JSON response we keep for diagnosis.
const maxErrorBody = 200

// response is the envelope every Slack method shares.
type response struct {
	OK       bool     `json:"ok"`
	Error    string   `json:"error"`
	Warning  string   `json:"warning"`
	Warnings []string `json:"warnings"`
}

// call posts a form-encoded request and decodes the result into out.
//
// POST with the token in a form field is the maximally-attested shape: it is
// what the web client sends and what slack-go sends, so the documented and
// undocumented endpoints produce identical wire traffic and share one code
// path here.
func (c *Client) call(ctx context.Context, method string, form url.Values, out any) error {
	if form == nil {
		form = url.Values{}
	}
	form.Set("token", c.creds.Token)
	c.addEnvelope(method, form)

	endpoint := c.base + method + "?" + c.queryEnvelope()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("%s: build request: %w", method, err)
	}
	c.setHeaders(req)

	c.log.Debug("slack call", "method", method, "url", c.base+method)

	resp, err := c.http.Do(req)
	if err != nil {
		// A dial, DNS or TLS failure, or the caller's deadline. Either way it
		// is not Slack rejecting us, and must not be reported as such.
		if ctx.Err() != nil {
			return fmt.Errorf("%s: %w", method, ctx.Err())
		}
		return fmt.Errorf("%s: %w: %w", method, errdefs.ErrTransport, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s: read response: %w", method, errdefs.ErrTransport)
	}

	if err := c.checkNotLoginPage(method, resp, body); err != nil {
		return c.annotate(err)
	}

	var env response
	if err := json.Unmarshal(body, &env); err != nil {
		return c.annotate(&APIError{
			Method:     method,
			HTTPStatus: resp.StatusCode,
			Body:       snippet(body),
		})
	}
	if !env.OK {
		return c.annotate(&APIError{
			Method:     method,
			Code:       env.Error,
			HTTPStatus: resp.StatusCode,
			RetryAfter: retryAfter(resp),
		})
	}
	if env.Warning != "" {
		c.log.Debug("slack warning", "method", method, "warning", env.Warning)
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: decode response: %w", method, err)
	}
	return nil
}

// checkNotLoginPage distinguishes a dead cookie from a real API failure.
//
// When the d cookie expires, Slack stops answering with JSON and serves the
// sign-in page - or redirects to it - with a 200. Sniffing for that here means
// the user is told their cookie expired, instead of being shown a JSON parse
// error about an unexpected '<'.
func (c *Client) checkNotLoginPage(method string, resp *http.Response, body []byte) error {
	if loc := resp.Header.Get("Location"); resp.StatusCode >= 300 && resp.StatusCode < 400 {
		if strings.Contains(loc, "signin") || strings.Contains(loc, "login") {
			return fmt.Errorf("%s: %w (redirected to %s)", method, ErrLoginPage, loc)
		}
	}

	ct := resp.Header.Get("Content-Type")
	trimmed := strings.TrimLeft(string(body), " \t\r\n")
	looksJSON := strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")

	if strings.Contains(ct, "application/json") && looksJSON {
		return nil
	}
	if looksJSON {
		// Some internal endpoints answer with text/plain. The body is what
		// matters, not the label.
		return nil
	}
	return fmt.Errorf("%s: %w: %w (content-type %q, body %s)",
		method, errdefs.ErrAuth, ErrLoginPage, ct, snippet(body))
}

// setHeaders makes the request look like the one the web client sends.
//
// Deliberately no Referer: across hundreds of captured real requests to
// /api/, not one carried one, so sending it would be a fingerprint rather
// than camouflage.
func (c *Client) setHeaders(req *http.Request) {
	h := req.Header
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	h.Set("User-Agent", c.ua)
	h.Set("Accept", "*/*")
	h.Set("Accept-Language", "en-US,en;q=0.9")
	h.Set("Origin", "https://app.slack.com")
	h.Set("Sec-Fetch-Site", "same-site")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Ch-Ua", secChUA)
	h.Set("Sec-Ch-Ua-Mobile", "?0")
	h.Set("Sec-Ch-Ua-Platform", `"Linux"`)
	h.Set("Cache-Control", "no-cache")
	h.Set("Pragma", "no-cache")
	h.Set("Priority", "u=1, i")
}

// xReason names the UI action a call would have come from, per method.
//
// Slack does not appear to validate these - different clients send different
// values for the same method - but a request carrying no _x_* parameters at
// all is a single-predicate separator that matches essentially none of the
// real traffic, so sending plausible ones is cheap camouflage.
var xReason = map[string]string{
	"auth.test":             "auth-test",
	"client.counts":         "fetchClientCountsOnConnect",
	"client.userBoot":       "initial-data",
	"conversations.history": "message-pane/requestHistory",
	"conversations.replies": "message-pane/requestThreadHistory",
	"conversations.mark":    "viewed",
	"users.conversations":   "channel-list",
	"search.messages":       "search-results",
}

// addEnvelope adds the body half of the client's telemetry parameters.
func (c *Client) addEnvelope(method string, form url.Values) {
	reason := xReason[method]
	if reason == "" {
		reason = "api-call"
	}
	form.Set("_x_reason", reason)
	form.Set("_x_mode", "online")
	form.Set("_x_sonic", "true")
	form.Set("_x_app_name", "client")
}

// queryEnvelope adds the query half.
func (c *Client) queryEnvelope() string {
	q := url.Values{}
	q.Set("_x_id", newXID())
	if c.identity != nil && c.identity.TeamID != "" {
		q.Set("slack_route", c.identity.TeamID)
	}
	return q.Encode()
}

// newXID mimics the web client's per-request correlation id: a short random
// prefix and a millisecond timestamp.
func newXID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	now := time.Now()
	return fmt.Sprintf("%s-%d.%03d", hex.EncodeToString(b), now.Unix(), now.Nanosecond()/1e6)
}

func retryAfter(resp *http.Response) time.Duration {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs < 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

func snippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > maxErrorBody {
		s = s[:maxErrorBody] + "…"
	}
	if s == "" {
		return "(empty)"
	}
	return strconv.Quote(s)
}

// CallRaw posts an arbitrary method and returns the whole response body.
//
// It exists for probing the undocumented endpoints this package depends on.
// Their shapes are attested by prior art rather than documentation, so being
// able to ask a real workspace what a method actually returns - before writing
// a struct for it - is the difference between a verified design and a guess.
func (c *Client) CallRaw(ctx context.Context, method string, form url.Values) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.call(ctx, method, form, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}
