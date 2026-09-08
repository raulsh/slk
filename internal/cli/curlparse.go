package cli

import (
	"regexp"
	"strings"

	"github.com/raulsh/slk/internal/config"
	"github.com/raulsh/slk/internal/errdefs"
)

// curlCreds is what a DevTools "Copy as cURL" blob yields.
type curlCreds struct {
	Workspace    string
	Token        string
	Cookie       string
	CookieDS     string
	EnterpriseID string
}

// The token's shape is distinctive enough to find anywhere in the blob, which
// is what makes this robust: DevTools emits the token as a form field, as a
// multipart body part, or as a bearer header depending on the request picked,
// and pattern-matching the credential beats parsing three shell dialects of
// curl.
var (
	reCurlToken      = regexp.MustCompile(`xoxc-[0-9]+-[0-9]+-[0-9]+-[0-9a-zA-Z]{16,}`)
	reCurlHost       = regexp.MustCompile(`https://([a-z0-9][a-z0-9-]*)\.(?:enterprise\.)?slack\.com`)
	reCurlCookieD    = regexp.MustCompile(`(?:^|[;\s'"])d=(xoxd-[^;'"\s\\]+)`)
	reCurlCookieDS   = regexp.MustCompile(`(?:^|[;\s'"])d-s=([^;'"\s\\]+)`)
	reCurlEnterprise = regexp.MustCompile(`\b(E[0-9A-Z]{8,})\b`)
)

// hostsThatAreNotTheWorkspace are the Slack subdomains that appear in real
// requests but never name the workspace.
var hostsThatAreNotTheWorkspace = map[string]bool{
	"app": true, "files": true, "edgeapi": true, "api": true,
	"wss-primary": true, "wss-backup": true, "downloads": true, "slack": true,
}

// parseCurl extracts credentials from a pasted curl command.
//
// It reports what it found rather than failing on the first gap, so the caller
// can prompt for only the missing pieces instead of making the user start over.
func parseCurl(blob string) curlCreds {
	var c curlCreds

	if m := reCurlToken.FindString(blob); m != "" {
		c.Token = m
	}
	if m := reCurlCookieD.FindStringSubmatch(blob); m != nil {
		c.Cookie = m[1]
	}
	if m := reCurlCookieDS.FindStringSubmatch(blob); m != nil {
		c.CookieDS = m[1]
	}
	if m := reCurlEnterprise.FindStringSubmatch(blob); m != nil {
		c.EnterpriseID = m[1]
	}

	for _, m := range reCurlHost.FindAllStringSubmatch(blob, -1) {
		if !hostsThatAreNotTheWorkspace[m[1]] {
			c.Workspace = m[1]
			break
		}
	}
	return c
}

// missing names the credentials parseCurl could not find, for an error that
// tells the reader which pane to go back to.
func (c curlCreds) missing() []string {
	var out []string
	if c.Token == "" {
		out = append(out, "token (a request to /api/ carries it as a `token` form field)")
	}
	if c.Cookie == "" {
		out = append(out, "cookie (the `d` cookie, sent in the Cookie header)")
	}
	if c.Workspace == "" {
		out = append(out, "workspace (the subdomain in the request URL)")
	}
	return out
}

// intoProfile converts what was scraped into a profile, normalising each
// credential the same way the flag and prompt paths do.
func (c curlCreds) intoProfile() (config.Profile, error) {
	if m := c.missing(); len(m) > 0 {
		return config.Profile{}, errdefs.WithHint(
			errdefs.Usagef("could not find %s in the pasted curl command", strings.Join(m, ", ")),
			"Copy as cURL from a request to /api/ specifically - a request for a static asset\n"+
				"carries the cookie but no token. In DevTools: Network, filter on `/api/`,\n"+
				"right-click any row, Copy > Copy as cURL.")
	}

	cookie, err := config.NormalizeCookie(c.Cookie)
	if err != nil {
		return config.Profile{}, errdefs.Usagef("the `d` cookie %s", err)
	}
	return config.Profile{
		Workspace:    c.Workspace,
		Token:        config.Secret(config.NormalizeToken(c.Token)),
		Cookie:       config.Secret(cookie),
		CookieDS:     config.Secret(c.CookieDS),
		EnterpriseID: c.EnterpriseID,
	}, nil
}
