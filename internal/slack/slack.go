// Package slack talks to Slack's internal web-client API.
//
// All knowledge of that protocol lives here: the credential pair, the
// browser-shaped request envelope, the undocumented endpoints, and the mapping
// from Slack's error strings onto slk's sentinels. It is one package because
// the protocol is unofficial and can change without notice, and confining it
// means a change lands in one place rather than across every command.
//
// The credential is an xoxc token bound to an xoxd browser cookie. Neither
// works without the other, and together they are full access to a human's
// Slack account.
package slack

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

// Credentials is the plain-string credential pair.
//
// It is deliberately not config.Profile: this package must not depend on the
// config file's shape, and the conversion in internal/cli/client.go is the
// single audited point at which a credential leaves config.Secret.
type Credentials struct {
	Workspace    string
	Token        string
	Cookie       string
	CookieDS     string
	EnterpriseID string
}

// Validate checks what can be checked without a network call.
func (c Credentials) Validate() error {
	if c.Workspace == "" {
		return fmt.Errorf("%w: no workspace", ErrCredentials)
	}
	if c.Token == "" {
		return fmt.Errorf("%w: no token", ErrCredentials)
	}
	// An xoxc token is a browser session credential and is meaningless without
	// the cookie it was issued against, so catching this here saves a
	// round trip that would fail as not_authed and look like a server problem.
	if strings.HasPrefix(c.Token, "xoxc-") && c.Cookie == "" {
		return fmt.Errorf("%w: an xoxc token requires the d cookie", ErrCredentials)
	}
	return nil
}

// Client calls the Slack web-client API.
type Client struct {
	creds Credentials
	http  *http.Client
	base  string
	ua    string
	log   *slog.Logger

	// identity caches auth.test for the process, because it is the bootstrap
	// for everything else - the real API base URL, the team id used in
	// request routing, and the user id used to filter your own messages out
	// of unread results.
	identity *Identity
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient replaces the transport, so httptest can stand in.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithBaseURL overrides the API base, so httptest can stand in. It must end
// in a slash.
func WithBaseURL(u string) Option {
	return func(c *Client) {
		if u != "" && !strings.HasSuffix(u, "/") {
			u += "/"
		}
		c.base = u
	}
}

// WithUserAgent overrides the browser user agent.
func WithUserAgent(ua string) Option { return func(c *Client) { c.ua = ua } }

// New builds a Client.
func New(creds Credentials, log *slog.Logger, opts ...Option) (*Client, error) {
	if err := creds.Validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	c := &Client{
		creds: creds,
		base:  fmt.Sprintf("https://%s.slack.com/api/", creds.Workspace),
		ua:    defaultUserAgent,
		log:   log,
	}
	for _, o := range opts {
		o(c)
	}

	if c.http == nil {
		jar, err := newJar(creds)
		if err != nil {
			return nil, err
		}
		c.http = &http.Client{
			Jar:     jar,
			Timeout: 0, // the caller's context owns the deadline
			// Refusing redirects on /api/ is what turns "Slack bounced us to
			// the login page" into a classifiable error instead of an HTML
			// body that json.Decode reports as `invalid character '<'`.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) > 0 && strings.Contains(via[0].URL.Path, "/api/") {
					return http.ErrUseLastResponse
				}
				if len(via) >= 10 {
					return fmt.Errorf("stopped after 10 redirects")
				}
				return nil
			},
		}
	}
	return c, nil
}

// newJar puts the credential cookies on .slack.com.
//
// A cookie jar rather than a header-injecting RoundTripper, because
// gorilla/websocket's Dialer takes an http.CookieJar and does not go through a
// RoundTripper at all - one jar serves both the API calls and the RTM socket.
//
// The nil PublicSuffixList is safe here: we only ever store cookies for a
// domain we were handed, so there is no supercookie to guard against, and it
// avoids a dependency on x/net/publicsuffix.
func newJar(creds Credentials) (http.CookieJar, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("create cookie jar: %w", err)
	}

	u, err := url.Parse("https://slack.com/")
	if err != nil {
		return nil, fmt.Errorf("parse cookie url: %w", err)
	}

	cookies := []*http.Cookie{cookie("d", creds.Cookie)}
	switch {
	case creds.CookieDS != "":
		cookies = append(cookies, cookie("d-s", creds.CookieDS))
	case strings.HasPrefix(creds.Token, "xoxc-"):
		// Some enterprise grids require a d-s "session start" alongside d.
		// Prior art synthesises it as a unix timestamp slightly in the past;
		// it is undocumented, so this is a best guess that costs nothing when
		// the workspace does not care.
		cookies = append(cookies, cookie("d-s", fmt.Sprintf("%d", time.Now().Unix()-10)))
	}

	jar.SetCookies(u, cookies)
	return jar, nil
}

func cookie(name, value string) *http.Cookie {
	// HttpOnly and SameSite are directives a *server* sets on a cookie it
	// issues. These are cookies we send as a client, so those fields have no
	// meaning here and setting them would change nothing on the wire.
	return &http.Cookie{ //nolint:gosec // outbound client cookie, not a Set-Cookie
		Name:  name,
		Value: value,
		Path:  "/",
		// Domain-wide, so one entry covers <ws>.slack.com,
		// <ws>.enterprise.slack.com, edgeapi.slack.com and wss-primary.
		Domain:  ".slack.com",
		Secure:  true,
		Expires: time.Now().AddDate(1, 0, 0).UTC(),
	}
}

// Identity is who the credentials belong to, and where to send everything else.
type Identity struct {
	UserID       string
	UserName     string
	TeamID       string
	TeamName     string
	TeamDomain   string
	EnterpriseID string
	URL          string
	IsEnterprise bool
}
