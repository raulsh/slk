package config

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/raulsh/slk/internal/errdefs"
	"gopkg.in/yaml.v3"
)

// fileConfig and fileProfile are the on-disk shapes, used only for writing.
//
// Their plain strings are how a real credential reaches the file while Secret
// keeps redacting everywhere else. Marshalling a Config directly is therefore
// always safe, and this is the one place that is deliberately not.
type fileConfig struct {
	DefaultProfile string                 `yaml:"default_profile"`
	AllowWrites    bool                   `yaml:"allow_writes"`
	Profiles       map[string]fileProfile `yaml:"profiles"`
	Defaults       Defaults               `yaml:"defaults"`
	Output         Output                 `yaml:"output"`
	Cache          Cache                  `yaml:"cache"`
}

type fileProfile struct {
	Workspace    string `yaml:"workspace"`
	Token        string `yaml:"token"`
	Cookie       string `yaml:"cookie"`
	CookieDS     string `yaml:"cookie_ds,omitempty"`
	EnterpriseID string `yaml:"enterprise_id,omitempty"`
	AllowWrites  *bool  `yaml:"allow_writes,omitempty"`
}

// header is prepended to a freshly written file. It exists so that someone
// hand-editing discovers the permission rule and the write switch by reading
// their own file, rather than by hitting an error and searching for the cause.
const header = `# slk credentials and defaults.
#
# This file holds Slack session credentials for a real user account: anyone who
# can read it can act as you in Slack. It must stay mode 0600; slk refuses to
# read it otherwise.
#
# allow_writes gates ` + "`slk send`" + ` and ` + "`slk mark`" + `. It ships false so that an
# automated caller cannot post to your workspace by accident. Set it true here,
# or per profile, when you actually want those commands.
`

// Write persists cfg atomically at mode 0600.
//
// Only `configure` calls this. The temp-file-and-rename dance matters because
// the alternative - truncating the real file and writing into it - can leave
// an interrupted run with no credentials at all.
func Write(cfg Config, path string) error {
	if path == "" {
		return fmt.Errorf("%w: no configuration path to write to", errdefs.ErrConfig)
	}

	fc := fileConfig{
		DefaultProfile: cfg.DefaultProfile,
		AllowWrites:    cfg.AllowWrites,
		Profiles:       make(map[string]fileProfile, len(cfg.Profiles)),
		Defaults:       cfg.Defaults,
		Output:         cfg.Output,
		Cache:          cfg.Cache,
	}
	for name, p := range cfg.Profiles {
		fc.Profiles[name] = fileProfile{
			Workspace:    p.Workspace,
			Token:        p.Token.Reveal(),
			Cookie:       p.Cookie.Reveal(),
			CookieDS:     p.CookieDS.Reveal(),
			EnterpriseID: p.EnterpriseID,
			AllowWrites:  p.AllowWrites,
		}
	}

	body, err := yaml.Marshal(fc)
	if err != nil {
		return fmt.Errorf("encode configuration: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".config-*.yaml")
	if err != nil {
		return fmt.Errorf("create temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		// A failed write must not leave a world-readable fragment of a
		// credential lying next to the real file.
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	// Chmod before writing, so the credential is never briefly readable.
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod temporary file: %w", err)
	}
	if _, err := tmp.WriteString(header); err != nil {
		return fmt.Errorf("write configuration: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		return fmt.Errorf("write configuration: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync configuration: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close configuration: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("install %s: %w", path, err)
	}
	return nil
}

// auditPerms refuses a credential file that anyone but its owner can read.
//
// It refuses rather than warns for a specific reason: the primary caller is an
// automated one, and a warning on stderr is invisible to it, so warning would
// be functionally a no-op for the case that matters. ssh refuses loose private
// keys and nobody argues.
//
// The carve-out for a file holding no credentials keeps a secrets-from-env
// setup working with a 0644 defaults file.
func (c Config) auditPerms() error {
	if c.mode == 0 || c.mode&0o077 == 0 {
		return nil
	}
	if !c.hasCredentials() {
		return nil
	}
	if os.Getenv("SLK_INSECURE_CONFIG_PERMS") != "" {
		return nil
	}
	return errdefs.WithHint(
		fmt.Errorf("%w: %w: %s is mode %#o", errdefs.ErrConfig, ErrInsecurePerms, c.path, c.mode),
		fmt.Sprintf("Anyone who can read this file can act as you in Slack. Fix it with:\n\n    chmod 600 %s\n\nTo override anyway, set SLK_INSECURE_CONFIG_PERMS=1.", c.path))
}

func (c Config) hasCredentials() bool {
	for _, p := range c.Profiles {
		if !p.Token.Empty() || !p.Cookie.Empty() {
			return true
		}
	}
	return false
}

// NormalizeToken strips what comes along when a token is copied out of a
// browser: surrounding quotes, and an Authorization header's Bearer prefix.
func NormalizeToken(v string) string {
	v = strings.TrimSpace(v)
	v = strings.Trim(v, `'"`)
	v = strings.TrimPrefix(v, "Bearer ")
	v = strings.TrimPrefix(v, "bearer ")
	return strings.TrimSpace(v)
}

// ParseCookieHeader pulls the d and d-s values out of anything from a bare
// value to a whole `Cookie:` header line.
//
// A whole header line is what people actually copy out of DevTools, so
// accepting one is not a convenience but the difference between working and
// not on the first try.
func ParseCookieHeader(s string) (d, ds string) {
	s = strings.TrimSpace(s)
	for _, p := range []string{"Cookie:", "cookie:", "-H", "--header"} {
		s = strings.TrimSpace(strings.TrimPrefix(s, p))
	}
	s = strings.Trim(s, `'"`)
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(s, "Cookie:"), "cookie:"))

	// A value pasted on its own. A Cookie header never starts with xoxd-, so
	// the prefix is an unambiguous discriminator - and testing it beats
	// looking for "=", which a base64 value can legitimately contain.
	if strings.HasPrefix(s, "xoxd-") {
		return s, ""
	}

	for _, part := range strings.Split(s, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `'"`)
		switch strings.TrimSpace(name) {
		case "d":
			d = value
		case "d-s":
			ds = value
		}
	}
	return d, ds
}

// reURLSafe matches the characters a percent-encoded cookie value may contain.
var reURLSafe = regexp.MustCompile(`[-._~%a-zA-Z0-9]+`)

// NormalizeCookie returns v in the percent-encoded form that belongs after
// `d=` in a Cookie header.
//
// The browser stores this value already encoded, and Go's cookie handling
// never percent-encodes - it only strips bytes it considers invalid. So an
// already-encoded value has to pass through untouched, and a decoded one has
// to be encoded, and telling them apart is what this heuristic does.
func NormalizeCookie(v string) (string, error) {
	v = strings.TrimSpace(v)
	v = strings.Trim(v, `'"`)
	if v == "" {
		return "", errors.New("is empty")
	}
	if reURLSafe.ReplaceAllString(v, "") != "" {
		v = url.QueryEscape(v)
	}
	if err := validateCookie(v); err != nil {
		return "", err
	}
	return v, nil
}

func validateCookie(v string) error {
	if !strings.HasPrefix(v, "xoxd-") {
		return errors.New("must start with xoxd- (this is the value of the browser's `d` cookie)")
	}
	if _, err := url.QueryUnescape(v); err != nil {
		return fmt.Errorf("is not valid percent-encoding: %w", err)
	}
	return nil
}

func sortStrings(s []string) { slices.Sort(s) }

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
