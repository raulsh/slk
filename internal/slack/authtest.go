package slack

import (
	"context"
	"net/url"
	"strings"
)

type authTestResponse struct {
	URL                 string `json:"url"`
	Team                string `json:"team"`
	User                string `json:"user"`
	TeamID              string `json:"team_id"`
	UserID              string `json:"user_id"`
	EnterpriseID        string `json:"enterprise_id"`
	IsEnterpriseInstall bool   `json:"is_enterprise_install"`
	BotID               string `json:"bot_id"`
}

// AuthTest validates the credentials and reports who they belong to.
//
// It is the bootstrap for everything else, which is why its result is cached
// on the Client: the returned url is the real API base (an Enterprise Grid
// answers on <ws>.enterprise.slack.com, not <ws>.slack.com), the team id is
// needed for request routing, and the user id is what lets the unread logic
// filter out your own messages.
func (c *Client) AuthTest(ctx context.Context) (*Identity, error) {
	if c.identity != nil {
		return c.identity, nil
	}

	var r authTestResponse
	if err := c.call(ctx, "auth.test", nil, &r); err != nil {
		return nil, err
	}

	id := &Identity{
		UserID:       r.UserID,
		UserName:     r.User,
		TeamID:       r.TeamID,
		TeamName:     r.Team,
		EnterpriseID: r.EnterpriseID,
		URL:          r.URL,
		IsEnterprise: r.EnterpriseID != "" || r.IsEnterpriseInstall,
	}
	id.TeamDomain = domainOf(r.URL)

	// Adopt the base Slack itself reported. Hardcoding <workspace>.slack.com
	// works for the documented endpoints but not for the client-only ones on
	// a Grid, so taking the server's answer here is what makes those reachable
	// without the caller knowing whether it is on a Grid.
	if base := apiBase(r.URL); base != "" && base != c.base {
		c.log.Debug("adopting api base from auth.test", "from", c.base, "to", base)
		c.base = base
	}

	c.identity = id
	return id, nil
}

// Identity returns the cached identity, or nil if AuthTest has not run.
func (c *Client) Identity() *Identity { return c.identity }

// BaseURL reports the API base currently in use.
func (c *Client) BaseURL() string { return c.base }

// TokenHint is a redacted form of the token, so the caller can tell which
// credential answered without ever handling the credential.
func (c *Client) TokenHint() string {
	t := c.creds.Token
	if len(t) < 12 {
		return "…"
	}
	i := strings.Index(t, "-")
	if i < 0 || i > 8 {
		i = 4
	}
	return t[:i+1] + "…" + t[len(t)-4:]
}

func apiBase(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return "https://" + u.Host + "/api/"
}

func domainOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	host, _, _ := strings.Cut(u.Host, ":")
	sub, _, _ := strings.Cut(host, ".")
	return sub
}
