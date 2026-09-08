package cli

import (
	"context"

	"github.com/raulsh/slk/internal/config"
	"github.com/raulsh/slk/internal/slack"
)

// session bundles a configured client with the configuration it came from, so
// a command can report which profile answered without re-resolving anything.
type session struct {
	client  *slack.Client
	cfg     config.Config
	profile config.Profile
	name    string
}

// newSession loads the configuration, selects a profile, and builds a client.
//
// This is the audited boundary: the two Reveal() calls below are the only way
// a credential reaches the network, which is what makes
// `grep -rn 'Reveal()'` a complete answer to "where can a token leak?".
func (g *globals) newSession() (*session, error) {
	path, err := g.configPath()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	profile, err := cfg.Select(g.profile)
	if err != nil {
		return nil, err
	}

	client, err := slack.New(slack.Credentials{
		Workspace:    profile.Workspace,
		Token:        profile.Token.Reveal(),
		Cookie:       profile.Cookie.Reveal(),
		CookieDS:     profile.CookieDS.Reveal(),
		EnterpriseID: profile.EnterpriseID,
	}, g.log)
	if err != nil {
		return nil, err
	}

	return &session{
		client:  client,
		cfg:     cfg,
		profile: profile,
		name:    cfg.SelectedName(),
	}, nil
}

// deadline applies --timeout as a context deadline. A zero or negative timeout
// means no limit, which is what a streaming command needs.
func (g *globals) deadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if g.timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, g.timeout)
}
