package slack

import (
	"context"
	"strings"
)

// Prefs is the slice of the user's client preferences slk reads.
//
// users.prefs.get returns over 700 keys of web-client state. Only the few that
// describe how the user has organised their own workspace are modelled here -
// the rest is UI bookkeeping that would be noise to carry around.
type Prefs struct {
	// VIPUsers are the people the user has marked VIP in the Slack client.
	// Slack stores them as one comma-separated string of ids.
	VIPUsers []string
	// MutedChannels are needed by the unread path: a muted channel still
	// accrues unreads, it just does not badge.
	MutedChannels []string
	// Starred conversations, which Slack's UI shows as favourites.
	StarredChannels []string
}

// IsVIP reports whether an id is marked VIP.
func (p Prefs) IsVIP(id string) bool {
	for _, v := range p.VIPUsers {
		if v == id {
			return true
		}
	}
	return false
}

// Prefs fetches the user's client preferences.
//
// Undocumented, like every client.* endpoint: if it disappears, the callers
// degrade to not knowing which people are VIPs rather than failing, because
// that is a presentation detail and not the answer to anyone's question.
func (c *Client) Prefs(ctx context.Context) (Prefs, error) {
	var resp struct {
		Prefs struct {
			VIPUsers      string `json:"vip_users"`
			MutedChannels string `json:"muted_channels"`
			Starred       string `json:"starred"`
		} `json:"prefs"`
	}
	if err := c.call(ctx, "users.prefs.get", nil, &resp); err != nil {
		return Prefs{}, err
	}
	return Prefs{
		VIPUsers:        splitIDs(resp.Prefs.VIPUsers),
		MutedChannels:   splitIDs(resp.Prefs.MutedChannels),
		StarredChannels: splitIDs(resp.Prefs.Starred),
	}, nil
}

// splitIDs parses Slack's comma-separated id lists, which arrive empty rather
// than absent when nothing is set.
func splitIDs(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
