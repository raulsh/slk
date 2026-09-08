package slack

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/raulsh/slk/internal/errdefs"
)

func testCreds() Credentials {
	return Credentials{
		Workspace: "acme",
		Token:     "xoxc-1-2-3-abcdef0123456789",
		Cookie:    "xoxd-AbC%2Fd%3D",
	}
}

// newTestClient points a client at a fake Slack.
func newTestClient(t *testing.T, h http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	c, err := New(testCreds(), slog.New(slog.DiscardHandler), WithBaseURL(srv.URL+"/api/"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, srv
}

func jsonHandler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
}

func TestAuthTest(t *testing.T) {
	c, _ := newTestClient(t, jsonHandler(`{
		"ok": true,
		"url": "https://acme.slack.com/",
		"team": "Acme",
		"user": "raul",
		"team_id": "T012",
		"user_id": "U345"
	}`))

	id, err := c.AuthTest(context.Background())
	if err != nil {
		t.Fatalf("AuthTest: %v", err)
	}

	cases := []struct{ name, got, want string }{
		{"UserID", id.UserID, "U345"},
		{"UserName", id.UserName, "raul"},
		{"TeamID", id.TeamID, "T012"},
		{"TeamName", id.TeamName, "Acme"},
		{"TeamDomain", id.TeamDomain, "acme"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
	if id.IsEnterprise {
		t.Error("IsEnterprise = true, want false")
	}
}

// TestAuthTestIsCached matters because auth.test is the bootstrap for every
// other call: re-running it per command would double the request count of a
// tool an agent invokes dozens of times.
func TestAuthTestIsCached(t *testing.T) {
	var calls int
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"url":"https://acme.slack.com/","user_id":"U1"}`))
	}))

	for range 3 {
		if _, err := c.AuthTest(context.Background()); err != nil {
			t.Fatalf("AuthTest: %v", err)
		}
	}
	if calls != 1 {
		t.Errorf("auth.test called %d times, want 1", calls)
	}
}

// TestRequestShape pins the wire format: the token travels as a form field,
// the cookie is sent, the telemetry parameters are present, and no Referer is
// set - real traffic to /api/ never carries one, so sending it would be a
// fingerprint rather than camouflage.
func TestRequestShape(t *testing.T) {
	var (
		gotForm    url.Values
		gotHeaders http.Header
		gotCookies []*http.Cookie
		gotQuery   url.Values
		gotMethod  string
	)
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		gotHeaders = r.Header
		gotCookies = r.Cookies()
		gotQuery = r.URL.Query()
		gotMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"url":"https://acme.slack.com/"}`))
	}))

	if _, err := c.AuthTest(context.Background()); err != nil {
		t.Fatalf("AuthTest: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if got := gotForm.Get("token"); got != testCreds().Token {
		t.Errorf("form token = %q, want the xoxc token", got)
	}
	if got := gotForm.Get("_x_reason"); got == "" {
		t.Error("form _x_reason is empty, want a plausible UI reason")
	}
	if got := gotForm.Get("_x_mode"); got != "online" {
		t.Errorf("form _x_mode = %q, want online", got)
	}
	if got := gotQuery.Get("_x_id"); got == "" {
		t.Error("query _x_id is empty, want a correlation id")
	}
	if got := gotHeaders.Get("Referer"); got != "" {
		t.Errorf("Referer = %q, want none; real /api/ traffic carries no Referer", got)
	}
	if got := gotHeaders.Get("Origin"); got != "https://app.slack.com" {
		t.Errorf("Origin = %q, want https://app.slack.com", got)
	}
	if got := gotHeaders.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q, want form encoding", got)
	}

	// Cookie delivery is deliberately NOT asserted here. The credential
	// cookies are Secure and scoped to .slack.com, so they are correctly
	// withheld from a plaintext localhost test server - which is the jar
	// behaving properly, not a bug. TestJarHoldsCredentialCookies covers it.
	_ = gotCookies
}

// TestJarHoldsCredentialCookies checks cookie delivery where it actually
// happens: against a real Slack host over TLS.
func TestJarHoldsCredentialCookies(t *testing.T) {
	creds := testCreds()
	creds.CookieDS = "1757354645"

	jar, err := newJar(creds)
	if err != nil {
		t.Fatalf("newJar: %v", err)
	}

	// One .slack.com entry has to cover the workspace host, the enterprise
	// host, the edge API and the RTM socket.
	for _, host := range []string{
		"https://acme.slack.com/api/auth.test",
		"https://acme.enterprise.slack.com/api/client.counts",
		"https://edgeapi.slack.com/cache/T1/users/info",
		"https://wss-primary.slack.com/websocket",
	} {
		u, err := url.Parse(host)
		if err != nil {
			t.Fatalf("parse %s: %v", host, err)
		}
		got := map[string]string{}
		for _, ck := range jar.Cookies(u) {
			got[ck.Name] = ck.Value
		}
		if got["d"] != creds.Cookie {
			t.Errorf("%s: d = %q, want %q sent verbatim", host, got["d"], creds.Cookie)
		}
		if got["d-s"] != creds.CookieDS {
			t.Errorf("%s: d-s = %q, want %q", host, got["d-s"], creds.CookieDS)
		}
	}

	// Never over plaintext: these are session credentials.
	u, _ := url.Parse("http://acme.slack.com/api/auth.test")
	if ck := jar.Cookies(u); len(ck) != 0 {
		t.Errorf("cookies sent over http: %v", ck)
	}
}

// TestSynthesisedCookieDS documents the guess: some enterprise grids want a
// d-s alongside d, it is undocumented, and prior art synthesises a unix
// timestamp slightly in the past. Costs nothing where the workspace ignores it.
func TestSynthesisedCookieDS(t *testing.T) {
	creds := testCreds()
	creds.CookieDS = ""

	jar, err := newJar(creds)
	if err != nil {
		t.Fatalf("newJar: %v", err)
	}
	u, _ := url.Parse("https://acme.slack.com/api/auth.test")
	var ds string
	for _, ck := range jar.Cookies(u) {
		if ck.Name == "d-s" {
			ds = ck.Value
		}
	}
	if ds == "" {
		t.Fatal("no d-s cookie synthesised for an xoxc token")
	}
	if _, err := strconv.ParseInt(ds, 10, 64); err != nil {
		t.Errorf("d-s = %q, want unix seconds", ds)
	}
}

// TestUserAgentMatchesClientHint guards a specific mistake: bumping the user
// agent without the sec-ch-ua hint produces a pair real Chrome never emits,
// which is a worse signature than sending neither.
func TestUserAgentMatchesClientHint(t *testing.T) {
	major := "150"
	if !strings.Contains(defaultUserAgent, "Chrome/"+major+".") {
		t.Errorf("user agent %q does not carry Chrome/%s", defaultUserAgent, major)
	}
	if !strings.Contains(secChUA, `"Chromium";v="`+major+`"`) {
		t.Errorf("sec-ch-ua %q disagrees with the user agent", secChUA)
	}
}

func TestSlackErrorsMapToSentinels(t *testing.T) {
	cases := []struct {
		slackError string
		want       error
	}{
		{"invalid_auth", errdefs.ErrAuth},
		{"not_authed", errdefs.ErrAuth},
		{"token_expired", errdefs.ErrAuth},
		{"token_revoked", errdefs.ErrAuth},
		{"account_inactive", errdefs.ErrAuth},
		{"org_login_required", errdefs.ErrAuth},
		{"channel_not_found", errdefs.ErrNotFound},
		{"user_not_found", errdefs.ErrNotFound},
		{"not_in_channel", errdefs.ErrNotAllowed},
		{"is_archived", errdefs.ErrNotAllowed},
		{"ratelimited", errdefs.ErrRateLimited},
	}

	for _, tc := range cases {
		t.Run(tc.slackError, func(t *testing.T) {
			c, _ := newTestClient(t, jsonHandler(`{"ok":false,"error":"`+tc.slackError+`"}`))
			_, err := c.AuthTest(context.Background())
			if !errors.Is(err, tc.want) {
				t.Errorf("AuthTest error = %v, want errors.Is %v", err, tc.want)
			}
		})
	}
}

// TestExpiredCredentialCarriesRemediation is the failure an agent will hit
// most often, so the hint is part of the contract rather than a nicety.
func TestExpiredCredentialCarriesRemediation(t *testing.T) {
	c, _ := newTestClient(t, jsonHandler(`{"ok":false,"error":"invalid_auth"}`))

	_, err := c.AuthTest(context.Background())
	f := errdefs.Classify(err)

	if f.Code != errdefs.CodeAuth {
		t.Errorf("code = %q, want %q", f.Code, errdefs.CodeAuth)
	}
	if f.SlackError != "invalid_auth" {
		t.Errorf("slack_error = %q, want invalid_auth", f.SlackError)
	}
	if !strings.Contains(f.Hint, "slk configure") {
		t.Errorf("hint does not say how to fix it: %q", f.Hint)
	}
	if f.Exit() != errdefs.ExitAuth {
		t.Errorf("exit = %d, want %d", f.Exit(), errdefs.ExitAuth)
	}
}

// TestLoginPageIsNotAParseError is the whole reason for sniffing the response:
// a dead cookie makes Slack serve HTML with a 200, and reporting that as
// "invalid character '<'" would send the reader looking in the wrong place.
func TestLoginPageIsNotAParseError(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!DOCTYPE html><html><head><title>Sign in to Slack</title>"))
	}))

	_, err := c.AuthTest(context.Background())
	if !errors.Is(err, ErrLoginPage) {
		t.Errorf("error = %v, want errors.Is ErrLoginPage", err)
	}
	if !errors.Is(err, errdefs.ErrAuth) {
		t.Errorf("error = %v, want it to classify as an auth failure", err)
	}
	if strings.Contains(err.Error(), "invalid character") {
		t.Errorf("error surfaced as a JSON parse failure: %v", err)
	}
}

// TestLoginRedirectIsNotFollowed covers the other shape of a dead cookie: Go
// would otherwise follow the 302 and hand us the login page's body.
func TestLoginRedirectIsNotFollowed(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://slack.com/signin", http.StatusFound)
	}))

	_, err := c.AuthTest(context.Background())
	if !errors.Is(err, ErrLoginPage) {
		t.Errorf("error = %v, want errors.Is ErrLoginPage", err)
	}
}

// TestBaseURLAdoptedFromAuthTest is what makes the client-only endpoints
// reachable on an Enterprise Grid, which answers on a different host than the
// one derivable from the workspace name.
func TestBaseURLAdoptedFromAuthTest(t *testing.T) {
	c, srv := newTestClient(t, jsonHandler(
		`{"ok":true,"url":"https://acme.enterprise.slack.com/","enterprise_id":"E01ABCDEF"}`))

	id, err := c.AuthTest(context.Background())
	if err != nil {
		t.Fatalf("AuthTest: %v", err)
	}
	if !id.IsEnterprise {
		t.Error("IsEnterprise = false, want true when enterprise_id is set")
	}
	if got := c.BaseURL(); got != "https://acme.enterprise.slack.com/api/" {
		t.Errorf("BaseURL = %q, want the host auth.test reported", got)
	}
	_ = srv
}

func TestCredentialsValidate(t *testing.T) {
	cases := []struct {
		name    string
		creds   Credentials
		wantErr bool
	}{
		{"complete", testCreds(), false},
		{"no workspace", Credentials{Token: "xoxc-1", Cookie: "xoxd-1"}, true},
		{"no token", Credentials{Workspace: "acme", Cookie: "xoxd-1"}, true},
		// An xoxc token is a browser session credential and is meaningless
		// without the cookie it was issued against.
		{"xoxc without cookie", Credentials{Workspace: "acme", Token: "xoxc-1-2-3-abc"}, true},
		{"non-xoxc without cookie", Credentials{Workspace: "acme", Token: "xoxp-1"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.creds.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestTokenHintNeverRevealsTheToken(t *testing.T) {
	c, err := New(testCreds(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	hint := c.TokenHint()
	if strings.Contains(hint, "abcdef0123456789") {
		t.Errorf("TokenHint = %q, leaks the token body", hint)
	}
	if !strings.HasPrefix(hint, "xoxc-") {
		t.Errorf("TokenHint = %q, want a recognisable prefix", hint)
	}
}

func TestContextCancellationIsNotReportedAsSlackFailure(t *testing.T) {
	c, _ := newTestClient(t, jsonHandler(`{"ok":true}`))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.AuthTest(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want errors.Is context.Canceled", err)
	}
	if errors.Is(err, errdefs.ErrAuth) {
		t.Errorf("a cancelled call was reported as an auth failure: %v", err)
	}
}
