// Package resolve turns what a person types into the ids Slack wants, and ids
// back into names.
//
// It exists because every command accepts #general, general, C0123ABCD, @raul
// and a permalink URL for the same target, and doing that lookup in one place
// is what keeps the ambiguity rule - two matches is an error, not a coin flip -
// from being decided differently by each command.
//
// It also owns the on-disk cache, because the cache exists solely to make
// these lookups free: slk is invoked once per question, and re-fetching a
// large workspace to turn #general into an id would make every command pay a
// round trip it does not need.
package resolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/raulsh/slk/internal/errdefs"
	"github.com/raulsh/slk/internal/output"
)

var (
	ErrAmbiguous = errors.New("matches more than one conversation")
	ErrNoMatch   = errors.New("matches nothing")
)

// idPattern matches a Slack id, which passes through unverified: it is cheap,
// and it lets a caller reuse an id from a previous result with no lookup.
var idPattern = regexp.MustCompile(`^[CDGUWBTS][A-Z0-9]{6,}$`)

// IsID reports whether s is already a Slack id.
func IsID(s string) bool { return idPattern.MatchString(strings.ToUpper(s)) }

// Fetcher is the slice of the Slack client this package needs, declared here
// in the consumer so a test can supply a fake with no network.
type Fetcher interface {
	Conversations(ctx context.Context, o ConversationsRequest) ([]output.Conversation, error)
	UsersInfo(ctx context.Context, ids []string) (map[string]output.User, error)
	BotsInfo(ctx context.Context, ids []string) (map[string]string, error)
	VIPUsers(ctx context.Context) ([]string, error)
}

// ConversationsRequest mirrors the client's options without importing it.
type ConversationsRequest struct {
	ExcludeArchived bool
}

// Directory is the cached view of a workspace's conversations and users.
type Directory struct {
	path string
	log  *slog.Logger

	Conversations []output.Conversation  `json:"conversations"`
	Users         map[string]output.User `json:"users"`
	// Bots is a separate map because a bot id is a different namespace from a
	// user id and resolves through a different endpoint.
	Bots map[string]string `json:"bots"`
	// VIPs are the ids the user has marked VIP in the Slack client. Cached
	// alongside the directory because it is the user's own classification and
	// changes rarely.
	VIPs      []string  `json:"vips"`
	FetchedAt time.Time `json:"fetched_at"`

	dirty bool
	ttl   time.Duration
}

// Open loads the cache for a profile, or returns an empty directory.
//
// A corrupt or unreadable cache is a miss, not an error: the difference
// between a resilient tool and one a half-written file bricks.
func Open(profile string, ttl time.Duration, log *slog.Logger) *Directory {
	d := &Directory{
		Users: map[string]output.User{},
		Bots:  map[string]string{},
		ttl:   ttl,
		log:   log,
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return d
	}
	d.path = filepath.Join(dir, "slk", profile, "directory.json")

	data, err := os.ReadFile(d.path)
	if err != nil {
		return d
	}
	var loaded Directory
	if err := json.Unmarshal(data, &loaded); err != nil {
		log.Debug("discarding corrupt cache", "path", d.path, "error", err)
		return d
	}
	d.Conversations = loaded.Conversations
	if loaded.Users != nil {
		d.Users = loaded.Users
	}
	if loaded.Bots != nil {
		d.Bots = loaded.Bots
	}
	d.VIPs = loaded.VIPs
	d.FetchedAt = loaded.FetchedAt
	return d
}

// Stale reports whether the conversation list should be refetched.
func (d *Directory) Stale() bool {
	return len(d.Conversations) == 0 || time.Since(d.FetchedAt) > d.ttl
}

// Refresh refetches the conversation list.
func (d *Directory) Refresh(ctx context.Context, f Fetcher) error {
	convs, err := f.Conversations(ctx, ConversationsRequest{ExcludeArchived: false})
	if err != nil {
		return err
	}
	d.Conversations = convs
	d.FetchedAt = time.Now()
	d.dirty = true

	// Best effort: VIP marks are a presentation detail, so losing them must
	// degrade the output rather than fail the command.
	if vips, err := f.VIPUsers(ctx); err != nil {
		d.log.Debug("could not read vip marks", "error", err)
	} else {
		d.VIPs = vips
	}
	return nil
}

// IsVIP reports whether an id is one the user marked VIP.
func (d *Directory) IsVIP(id string) bool {
	for _, v := range d.VIPs {
		if v == id {
			return true
		}
	}
	return false
}

// MarkVIPs stamps the VIP flag onto a user list.
func (d *Directory) MarkVIPs(us []output.User) {
	for i := range us {
		us[i].VIP = d.IsVIP(us[i].ID)
	}
}

// EnsureFresh refreshes only when the cache is stale.
func (d *Directory) EnsureFresh(ctx context.Context, f Fetcher) error {
	if !d.Stale() {
		return nil
	}
	return d.Refresh(ctx, f)
}

// EnsureUsers fills in any of ids that are not cached.
//
// Demand-driven and batched at the render boundary, never per reference: a
// miss must not stall a fetch, and the whole member directory is never
// enumerated to answer a handful of questions.
func (d *Directory) EnsureUsers(ctx context.Context, f Fetcher, ids []string) error {
	var want []string
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := d.Users[id]; !ok {
			want = append(want, id)
		}
	}
	if len(want) == 0 {
		return nil
	}

	got, err := f.UsersInfo(ctx, want)
	if err != nil {
		return err
	}
	for id, u := range got {
		d.Users[id] = u
		d.dirty = true
	}
	return nil
}

// EnsureBots fills in any bot names that are not cached.
func (d *Directory) EnsureBots(ctx context.Context, f Fetcher, ids []string) error {
	var want []string
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := d.Bots[id]; !ok {
			want = append(want, id)
		}
	}
	if len(want) == 0 {
		return nil
	}
	got, err := f.BotsInfo(ctx, want)
	if err != nil {
		return err
	}
	for id, name := range got {
		d.Bots[id] = name
		d.dirty = true
	}
	return nil
}

// BotName reports a cached bot name.
func (d *Directory) BotName(id string) (string, bool) {
	name, ok := d.Bots[id]
	return name, ok
}

// Save persists the cache if anything changed.
func (d *Directory) Save() error {
	if !d.dirty || d.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(d.path), 0o700); err != nil {
		return fmt.Errorf("create cache directory: %w", err)
	}
	data, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("encode cache: %w", err)
	}
	// The cache holds channel names, DM partners and email addresses - not
	// credentials, but not public either.
	tmp := d.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write cache: %w", err)
	}
	return os.Rename(tmp, d.path)
}

// Name renders a display name for any id, falling back to the id itself so an
// unresolvable reference stays actionable.
func (d *Directory) Name(id string) (string, bool) {
	if u, ok := d.Users[id]; ok {
		return displayFor(u), true
	}
	if name, ok := d.Bots[id]; ok {
		return name, true
	}
	for _, c := range d.Conversations {
		if c.ID == id {
			return d.ConversationName(c), true
		}
	}
	return "", false
}

// ConversationName renders a conversation for display, resolving a DM to its
// peer's handle.
func (d *Directory) ConversationName(c output.Conversation) string {
	switch c.Type {
	case "dm":
		if u, ok := d.Users[c.User]; ok {
			return "@" + displayFor(u)
		}
		if c.User != "" {
			return "@" + c.User
		}
		return c.ID
	case "mpim":
		return c.Name
	default:
		return "#" + c.Name
	}
}

func displayFor(u output.User) string {
	if u.Name != "" {
		return u.Name
	}
	if u.Display != "" {
		return u.Display
	}
	return u.ID
}

// Channel resolves a channel target to an id.
func (d *Directory) Channel(s string) (string, error) {
	s = strings.TrimSpace(s)
	if IsID(s) {
		return strings.ToUpper(s), nil
	}
	if ch, _, _, err := ParsePermalink(s); err == nil {
		return ch, nil
	}

	want := strings.ToLower(strings.TrimPrefix(s, "#"))
	var exact, partial []output.Conversation
	for _, c := range d.Conversations {
		if c.Type == "dm" {
			continue
		}
		name := strings.ToLower(c.Name)
		switch {
		case name == want:
			exact = append(exact, c)
		case strings.Contains(name, want):
			partial = append(partial, c)
		}
	}
	return pick(s, exact, partial, d)
}

// User resolves a user target to a user id.
func (d *Directory) User(s string) (string, error) {
	s = strings.TrimSpace(s)
	if IsID(s) {
		return strings.ToUpper(s), nil
	}
	want := strings.ToLower(strings.TrimPrefix(s, "@"))

	var hits []string
	for id, u := range d.Users {
		if strings.EqualFold(u.Name, want) ||
			strings.EqualFold(u.Display, want) ||
			strings.EqualFold(u.Email, want) {
			hits = append(hits, id)
		}
	}
	sort.Strings(hits)
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return "", fmt.Errorf("%w: %w %q; try `slk users list --name %s`",
			errdefs.ErrNotFound, ErrNoMatch, s, want)
	default:
		return "", fmt.Errorf("%w: %q %w: %s", errdefs.ErrNotFound, s, ErrAmbiguous,
			strings.Join(hits, ", "))
	}
}

// DM resolves a person to the DM conversation with them.
func (d *Directory) DM(s string) (string, error) {
	s = strings.TrimSpace(s)
	if IsID(s) && strings.HasPrefix(strings.ToUpper(s), "D") {
		return strings.ToUpper(s), nil
	}

	uid := s
	if !IsID(s) {
		var err error
		uid, err = d.User(s)
		if err != nil {
			return "", err
		}
	}
	for _, c := range d.Conversations {
		if c.Type == "dm" && c.User == uid {
			return c.ID, nil
		}
	}
	// Opening a DM is a write, so a missing one is reported rather than
	// created behind the caller's back.
	return "", fmt.Errorf("%w: no DM history with %q", errdefs.ErrNotFound, s)
}

// pick applies the ambiguity rule and offers near misses on a miss.
func pick(input string, exact, partial []output.Conversation, d *Directory) (string, error) {
	if len(exact) == 1 {
		return exact[0].ID, nil
	}
	if len(exact) > 1 {
		return "", fmt.Errorf("%w: %q %w: %s", errdefs.ErrNotFound, input, ErrAmbiguous, names(exact, d))
	}
	if len(partial) == 1 {
		return partial[0].ID, nil
	}
	if len(partial) > 1 {
		sort.Slice(partial, func(i, j int) bool { return partial[i].Name < partial[j].Name })
		if len(partial) > 8 {
			partial = partial[:8]
		}
		return "", fmt.Errorf("%w: %q %w: %s", errdefs.ErrNotFound, input, ErrAmbiguous, names(partial, d))
	}
	return "", fmt.Errorf("%w: %w %q; try `slk conversations list --name %s`",
		errdefs.ErrNotFound, ErrNoMatch, input, strings.TrimPrefix(input, "#"))
}

func names(cs []output.Conversation, d *Directory) string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, d.ConversationName(c))
	}
	return strings.Join(out, ", ")
}

// permalinkPattern matches a Slack archive URL.
var permalinkPattern = regexp.MustCompile(
	`slack\.com/archives/([A-Z0-9]+)/p(\d{10})(\d{6})(?:\?thread_ts=([0-9.]+))?`)

// ParsePermalink extracts the channel, message ts and thread ts from an
// archive URL, so a link pasted straight out of Slack works as a target.
func ParsePermalink(u string) (channel, ts, threadTS string, err error) {
	m := permalinkPattern.FindStringSubmatch(u)
	if m == nil {
		return "", "", "", fmt.Errorf("not a slack permalink")
	}
	return m[1], m[2] + "." + m[3], m[4], nil
}
