package cli

import (
	"context"

	"github.com/raulsh/slk/internal/output"
	"github.com/raulsh/slk/internal/resolve"
	"github.com/raulsh/slk/internal/slack"
	"github.com/raulsh/slk/internal/slacktime"
)

// fetcher adapts the Slack client to what resolve needs, so resolve stays
// unaware of the transport's option types.
type fetcher struct{ c *slack.Client }

func (f fetcher) Conversations(ctx context.Context, r resolve.ConversationsRequest) ([]output.Conversation, error) {
	return f.c.Conversations(ctx, slack.ConversationsOptions{ExcludeArchived: r.ExcludeArchived})
}

func (f fetcher) UsersInfo(ctx context.Context, ids []string) (map[string]output.User, error) {
	return f.c.UsersInfo(ctx, ids)
}

func (f fetcher) BotsInfo(ctx context.Context, ids []string) (map[string]string, error) {
	return f.c.BotsInfo(ctx, ids)
}

func (f fetcher) VIPUsers(ctx context.Context) ([]string, error) {
	p, err := f.c.Prefs(ctx)
	if err != nil {
		return nil, err
	}
	return p.VIPUsers, nil
}

// directory opens the name cache for the selected profile and refreshes it if
// stale, then wires it into the writer so entity references resolve during
// rendering and the envelope's names map fills itself.
func (g *globals) directory(ctx context.Context, s *session) (*resolve.Directory, error) {
	ttl := s.cfg.Cache.ConversationsTTL.Std()
	d := resolve.Open(s.name, ttl, g.log)

	if g.noCache {
		if err := d.Refresh(ctx, fetcher{s.client}); err != nil {
			return nil, err
		}
	} else if err := d.EnsureFresh(ctx, fetcher{s.client}); err != nil {
		return nil, err
	}

	g.out.SetResolver(d.Name)
	return d, nil
}

// finish resolves the user ids appearing in a result set, rewrites entity
// references, and persists anything newly learned.
//
// Resolution happens here, at the render boundary, rather than per message:
// one batch of lookups instead of one per reference, and a miss never stalls
// the fetch.
func (g *globals) finish(ctx context.Context, s *session, d *resolve.Directory, ms []output.Message) {
	ids := map[string]bool{}
	for _, m := range ms {
		if m.User != "" {
			ids[m.User] = true
		}
	}
	for _, m := range ms {
		for _, id := range mentionIDs(m.Text) {
			ids[id] = true
		}
	}

	want := make([]string, 0, len(ids))
	for id := range ids {
		want = append(want, id)
	}
	if err := d.EnsureUsers(ctx, fetcher{s.client}, want); err != nil {
		g.log.Debug("could not resolve some names", "error", err)
	}

	// Bots are a separate namespace and a separate endpoint, and they post a
	// large share of the traffic in an engineering workspace, so leaving them
	// as raw ids would degrade the most common output.
	bots := map[string]bool{}
	for _, m := range ms {
		if m.BotID != "" && m.UserName == "" {
			bots[m.BotID] = true
		}
	}
	if len(bots) > 0 {
		botIDs := make([]string, 0, len(bots))
		for id := range bots {
			botIDs = append(botIDs, id)
		}
		if err := d.EnsureBots(ctx, fetcher{s.client}, botIDs); err != nil {
			g.log.Debug("could not resolve some bots", "error", err)
		}
	}

	for i := range ms {
		if name, ok := d.Name(ms[i].User); ok {
			ms[i].UserName = name
		}
		if ms[i].UserName == "" && ms[i].BotID != "" {
			if name, ok := d.BotName(ms[i].BotID); ok {
				ms[i].UserName = name
			}
		}
		ms[i].VIP = d.IsVIP(ms[i].User)
		if rendered := g.out.ResolveInto(ms[i].Text); rendered != ms[i].Text {
			ms[i].TextRendered = rendered
		}
	}

	if err := d.Save(); err != nil {
		g.log.Debug("could not save cache", "error", err)
	}
}

// timeRange resolves the shared --since/--until/--on flags.
func (g *globals) timeRange(since, until, on string) (slacktime.Range, error) {
	r, err := slacktime.ParseRange(since, until, on, g.out.Location())
	if err != nil {
		return r, errdefsUsage(err)
	}
	return r, nil
}

// window converts a resolved range into the envelope's echo of it, so a caller
// that asked for "7d" can state the window actually examined.
func window(r slacktime.Range, tz string) *output.Window {
	if r.IsZero() {
		return nil
	}
	w := &output.Window{TZ: tz}
	if !r.Since.IsZero() {
		w.Since = r.Since.Format("2006-01-02T15:04:05Z07:00")
		w.SinceTS = slacktime.FromTime(r.Since).String()
	}
	if !r.Until.IsZero() {
		w.Until = r.Until.Format("2006-01-02T15:04:05Z07:00")
		w.UntilTS = slacktime.FromTime(r.Until).String()
	}
	return w
}
