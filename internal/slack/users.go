package slack

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/raulsh/slk/internal/output"
)

type rawUser struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	RealName string `json:"real_name"`
	Deleted  bool   `json:"deleted"`
	IsBot    bool   `json:"is_bot"`
	TZ       string `json:"tz"`
	Profile  struct {
		DisplayName string `json:"display_name"`
		RealName    string `json:"real_name"`
		Email       string `json:"email"`
		Title       string `json:"title"`
	} `json:"profile"`
}

func (ru rawUser) toOutput() output.User {
	u := output.User{
		ID:       ru.ID,
		Name:     ru.Name,
		RealName: ru.RealName,
		Display:  ru.Profile.DisplayName,
		Email:    ru.Profile.Email,
		Title:    ru.Profile.Title,
		TZ:       ru.TZ,
		Bot:      ru.IsBot,
		Deleted:  ru.Deleted,
	}
	if u.RealName == "" {
		u.RealName = ru.Profile.RealName
	}
	return u
}

// UserInfo fetches one user.
func (c *Client) UserInfo(ctx context.Context, id string) (output.User, error) {
	form := url.Values{}
	form.Set("user", id)

	var resp struct {
		User rawUser `json:"user"`
	}
	if err := c.call(ctx, "users.info", form, &resp); err != nil {
		return output.User{}, err
	}
	return resp.User.toOutput(), nil
}

// resolveConcurrency bounds parallel id lookups.
//
// users.info and bots.info are tier 4 (about 100/minute), so eight in flight
// is well inside the budget while turning a few dozen sequential round trips -
// which is what a workspace with thirty DMs actually costs - into one.
const resolveConcurrency = 8

// UsersInfo fetches several users.
//
// Deliberately not users.list. On a large workspace that is fifty paged
// requests at a tier-2 rate limit, and it is the exact traffic pattern
// reported to have got an xoxc session invalidated for "data scraping".
// users.info is tier 4 and demand-driven: we only ask about ids we actually
// saw. A failure on one id is skipped rather than failing the batch, because a
// deactivated or foreign account should not break rendering.
func (c *Client) UsersInfo(ctx context.Context, ids []string) (map[string]output.User, error) {
	return resolveMany(ctx, c, ids, func(ctx context.Context, id string) (output.User, error) {
		return c.UserInfo(ctx, id)
	})
}

// resolveMany runs a per-id lookup over a bounded worker pool, dropping the
// ones that fail.
func resolveMany[T any](
	ctx context.Context,
	c *Client,
	ids []string,
	fetch func(context.Context, string) (T, error),
) (map[string]T, error) {
	out := make(map[string]T, len(ids))
	if len(ids) == 0 {
		return out, nil
	}

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	sem := make(chan struct{}, resolveConcurrency)

	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			v, err := fetch(ctx, id)
			if err != nil {
				c.log.Debug("could not resolve id", "id", id, "error", err)
				return
			}
			mu.Lock()
			out[id] = v
			mu.Unlock()
		}(id)
	}
	wg.Wait()

	if ctx.Err() != nil {
		// Partial results are still worth returning: a name resolved is a
		// name rendered, and the caller falls back to raw ids for the rest.
		return out, ctx.Err()
	}
	return out, nil
}

// UsersListOptions selects a slice of the member directory.
type UsersListOptions struct {
	Limit          int
	IncludeBots    bool
	IncludeDeleted bool
}

// UsersList pages the member directory.
//
// This is the enumeration pattern described above, so it exists only behind an
// explicit `slk users list` and warns. Nothing calls it to fill a cache.
func (c *Client) UsersList(ctx context.Context, o UsersListOptions) ([]output.User, error) {
	limit := o.Limit
	if limit <= 0 {
		limit = 200
	}
	c.log.Warn("users.list enumerates the whole member directory; " +
		"this is the traffic pattern that has been reported to get xoxc sessions invalidated")

	var out []output.User
	cursor := ""
	for page := 0; page < maxPages; page++ {
		form := url.Values{}
		form.Set("limit", strconv.Itoa(min(limit, 200)))
		if cursor != "" {
			form.Set("cursor", cursor)
		}

		var resp struct {
			Members          []rawUser `json:"members"`
			ResponseMetadata struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := c.call(ctx, "users.list", form, &resp); err != nil {
			return out, err
		}

		for _, ru := range resp.Members {
			if ru.IsBot && !o.IncludeBots {
				continue
			}
			if ru.Deleted && !o.IncludeDeleted {
				continue
			}
			out = append(out, ru.toOutput())
			if len(out) >= limit {
				return out, nil
			}
		}

		next := resp.ResponseMetadata.NextCursor
		if next == "" || next == cursor {
			break
		}
		cursor = next
	}
	return out, nil
}

// SearchOptions builds a search.
//
// The modifiers are separate fields rather than a query string because the
// caller must never have to know Slack's search DSL: composition happens here,
// once.
type SearchOptions struct {
	Query   string
	In      []string
	From    []string
	With    []string
	After   string // YYYY-MM-DD
	Before  string
	Sort    string
	SortDir string
	Count   int
	Page    int
}

// BuildQuery assembles the Slack search string.
//
// The positional query passes through verbatim and last: raw DSL passthrough
// is the point, and silently mangling someone's query produces confidently
// wrong results.
func (o SearchOptions) BuildQuery() string {
	var parts []string
	add := func(prefix string, vals []string, sigil string) {
		for _, v := range vals {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			v = strings.TrimPrefix(v, sigil)
			parts = append(parts, prefix+sigil+v)
		}
	}
	add("in:", o.In, "#")
	add("from:", o.From, "@")
	add("with:", o.With, "@")
	if o.After != "" {
		parts = append(parts, "after:"+o.After)
	}
	if o.Before != "" {
		parts = append(parts, "before:"+o.Before)
	}
	if o.Query != "" {
		parts = append(parts, o.Query)
	}
	return strings.Join(parts, " ")
}

// SearchResult is a page of matches plus what was actually asked.
type SearchResult struct {
	Messages []output.Message
	// Query is the assembled string. Always echoed: a caller must be able to
	// see what was actually asked, especially when flags built it.
	Query   string
	Total   int
	Page    int
	Pages   int
	HasMore bool
}

// Search runs search.messages.
func (c *Client) Search(ctx context.Context, o SearchOptions) (SearchResult, error) {
	q := o.BuildQuery()
	count := o.Count
	if count <= 0 {
		count = 20
	}
	// Documented maximum.
	count = min(count, 100)
	page := max(o.Page, 1)

	form := url.Values{}
	form.Set("query", q)
	form.Set("count", strconv.Itoa(count))
	form.Set("page", strconv.Itoa(page))
	if o.Sort != "" {
		form.Set("sort", o.Sort)
	}
	if o.SortDir != "" {
		form.Set("sort_dir", o.SortDir)
	}

	var resp struct {
		Messages struct {
			Total   int `json:"total"`
			Matches []struct {
				rawMessage
				Channel struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"channel"`
				Permalink string `json:"permalink"`
			} `json:"matches"`
			Paging struct {
				Count int `json:"count"`
				Total int `json:"total"`
				Page  int `json:"page"`
				Pages int `json:"pages"`
			} `json:"paging"`
		} `json:"messages"`
	}
	if err := c.call(ctx, "search.messages", form, &resp); err != nil {
		return SearchResult{Query: q}, err
	}

	out := SearchResult{
		Query: q,
		Total: resp.Messages.Total,
		Page:  resp.Messages.Paging.Page,
		Pages: resp.Messages.Paging.Pages,
	}
	out.HasMore = out.Page < out.Pages

	for _, m := range resp.Messages.Matches {
		msg := m.toOutput(m.Channel.ID, c.workspaceHost())
		msg.ChannelName = m.Channel.Name
		if m.Permalink != "" {
			msg.Permalink = m.Permalink
		}
		out.Messages = append(out.Messages, msg)
	}
	return out, nil
}

// BotInfo resolves a bot id to its display name.
//
// A bot post frequently carries no username and no bot_profile - only a
// bot_id - and bots post a large share of the traffic in an engineering
// workspace, so leaving them as raw ids would degrade the most common output.
func (c *Client) BotInfo(ctx context.Context, id string) (string, error) {
	form := url.Values{}
	form.Set("bot", id)

	var resp struct {
		Bot struct {
			Name    string `json:"name"`
			Deleted bool   `json:"deleted"`
		} `json:"bot"`
	}
	if err := c.call(ctx, "bots.info", form, &resp); err != nil {
		return "", err
	}
	return resp.Bot.Name, nil
}

// BotsInfo resolves several bot ids, skipping any that fail: one unresolvable
// app must not cost the rest of the batch.
func (c *Client) BotsInfo(ctx context.Context, ids []string) (map[string]string, error) {
	return resolveMany(ctx, c, ids, func(ctx context.Context, id string) (string, error) {
		return c.BotInfo(ctx, id)
	})
}
