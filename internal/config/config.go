// Package config loads slk's credentials and defaults from defaults, a YAML
// file, and the environment, in that order of increasing precedence.
//
// It is a separate package because the file it owns holds session credentials
// for a real Slack account: the permission checks, the redaction rules, and
// the single audited path by which a credential reaches the network all need
// one place to live.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/raulsh/slk/internal/errdefs"
	"gopkg.in/yaml.v3"
)

// Sentinels. Each carries a distinct remedy, which is why they are not one
// error: "nothing is set up" and "you typed the wrong profile name" need
// different things from the reader.
var (
	ErrNoProfile      = errors.New("no credential profile is configured")
	ErrUnknownProfile = errors.New("unknown profile")
	ErrInsecurePerms  = errors.New("credential file permissions are too permissive")
)

// Config is the fully resolved configuration.
type Config struct {
	DefaultProfile string `yaml:"default_profile"`

	// AllowWrites gates `send` and `mark`. False is the shipped default: a
	// fresh install is read-only until the human who owns the credentials says
	// otherwise, so an automated caller cannot post by accident. Deliberately
	// not omitempty - a kill switch nobody can see in the file is one nobody
	// turns on deliberately.
	AllowWrites bool `yaml:"allow_writes"`

	Profiles map[string]Profile `yaml:"profiles"`
	Defaults Defaults           `yaml:"defaults"`
	Output   Output             `yaml:"output"`
	Cache    Cache              `yaml:"cache"`

	// path is where this Config was read from, so errors can name the file and
	// `config path` needs no second resolution.
	path string
	// mode is the file mode as found, so the permission audit can report the
	// mode it objected to.
	mode os.FileMode
	// selected is the profile name resolved by Select, recorded so the output
	// envelope can tell an agent which credentials were used.
	selected string
}

// Profile is one workspace's credential pair.
//
// This is the internal web-client credential pair, not an app token: the xoxc
// token is the bearer the Slack web client sends, and the xoxd cookie is the
// browser session it is bound to. Neither works without the other.
type Profile struct {
	Workspace    string `yaml:"workspace"`
	Token        Secret `yaml:"token"`
	Cookie       Secret `yaml:"cookie"`
	CookieDS     Secret `yaml:"cookie_ds,omitempty"`
	EnterpriseID string `yaml:"enterprise_id,omitempty"`

	// AllowWrites overrides the top-level switch for this workspace. A nil
	// pointer means inherit, which is not the same as an explicit false - that
	// distinction is what lets one profile be writable while the default stays
	// read-only.
	AllowWrites *bool `yaml:"allow_writes,omitempty"`
}

type Defaults struct {
	Limit    int      `yaml:"limit"`
	MaxTotal int      `yaml:"max_total"`
	Timeout  Duration `yaml:"timeout"`
}

type Output struct {
	Mode     string `yaml:"mode"`
	Color    string `yaml:"color"`
	Timezone string `yaml:"timezone"`
}

type Cache struct {
	Enabled          bool     `yaml:"enabled"`
	Dir              string   `yaml:"dir"`
	UsersTTL         Duration `yaml:"users_ttl"`
	ConversationsTTL Duration `yaml:"conversations_ttl"`
}

// Default returns a fully populated working configuration.
//
// It must be complete, because Load unmarshals the file *into* this rather
// than into a zero value: that is what makes a partial file override only the
// keys it actually mentions.
func Default() Config {
	return Config{
		AllowWrites: false,
		Profiles:    map[string]Profile{},
		Defaults: Defaults{
			Limit:    50,
			MaxTotal: 1000,
			Timeout:  Duration(30 * time.Second),
		},
		Output: Output{
			Mode:     "text",
			Color:    "auto",
			Timezone: "Local",
		},
		Cache: Cache{
			Enabled:          true,
			UsersTTL:         Duration(7 * 24 * time.Hour),
			ConversationsTTL: Duration(24 * time.Hour),
		},
	}
}

// DefaultPath is the config location, honouring XDG_CONFIG_HOME through
// os.UserConfigDir.
func DefaultPath() (string, error) {
	dir, err := userConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate config directory: %w", err)
	}
	return filepath.Join(dir, "slk", "config.yaml"), nil
}

// userConfigDir is a variable so tests can redirect the config location
// without mutating the process environment.
var userConfigDir = os.UserConfigDir

// Load resolves configuration from path (optional) then the environment.
//
// A missing file is not an error: the credential environment variables alone
// are a working configuration, which is what lets a containerised agent run
// with nothing on disk.
func Load(path string) (Config, error) {
	cfg := Default()

	if path == "" {
		if p := os.Getenv("SLK_CONFIG"); p != "" {
			path = p
		} else {
			p, err := DefaultPath()
			if err != nil {
				return cfg, err
			}
			path = p
		}
	}
	cfg.path = path

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("%w: parse %s: %w", errdefs.ErrConfig, path, err)
		}
		info, serr := os.Stat(path)
		if serr == nil {
			cfg.mode = info.Mode().Perm()
		}
		if err := cfg.auditPerms(); err != nil {
			return cfg, err
		}
	case errors.Is(err, os.ErrNotExist):
		// Defaults stand. Whether that is usable is Select's problem, not
		// Load's: the environment may carry a whole profile.
	default:
		return cfg, fmt.Errorf("%w: read %s: %w", errdefs.ErrConfig, path, err)
	}

	cfg.applyEnv()

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Path reports where this Config was read from.
func (c Config) Path() string { return c.path }

// SelectedName reports the profile Select resolved.
func (c Config) SelectedName() string { return c.selected }

// applyEnv overlays SLK_* variables.
//
// One explicit if per variable, in the house style. The credential variables
// overlay the *selected* profile rather than replacing the file, so "use my
// acme profile but with a fresh token" is the same mechanism as "run with no
// config file at all".
func (c *Config) applyEnv() {
	if v := os.Getenv("SLK_ALLOW_WRITES"); v == "1" || v == "true" {
		c.AllowWrites = true
	}
	if v := os.Getenv("SLK_PROFILE"); v != "" {
		c.DefaultProfile = v
	}

	name := c.DefaultProfile
	if name == "" {
		name = envProfileName
	}
	p := c.Profiles[name]

	overlaid := false
	if v := os.Getenv("SLK_WORKSPACE"); v != "" {
		p.Workspace, overlaid = v, true
	}
	if v := os.Getenv("SLK_TOKEN"); v != "" {
		p.Token, overlaid = Secret(NormalizeToken(v)), true
	}
	if v := os.Getenv("SLK_COOKIE"); v != "" {
		d, ds := ParseCookieHeader(v)
		p.Cookie, overlaid = Secret(d), true
		if ds != "" && p.CookieDS.Empty() {
			p.CookieDS = Secret(ds)
		}
	}
	if v := os.Getenv("SLK_COOKIE_DS"); v != "" {
		p.CookieDS, overlaid = Secret(v), true
	}
	if v := os.Getenv("SLK_ENTERPRISE_ID"); v != "" {
		p.EnterpriseID, overlaid = v, true
	}

	if !overlaid {
		return
	}
	if c.Profiles == nil {
		c.Profiles = map[string]Profile{}
	}
	c.Profiles[name] = p
	// Naming the synthetic profile as the default is what makes a
	// credentials-only environment work with no --profile flag.
	if c.DefaultProfile == "" {
		c.DefaultProfile = name
	}
}

// envProfileName is the profile the credential environment variables form when
// there is no file to overlay.
const envProfileName = "env"

// Select resolves which profile to use.
//
// Precedence: --profile, then SLK_PROFILE or default_profile as already folded
// in by applyEnv, then the sole profile if exactly one exists. The
// sole-profile rule matters because most people have one workspace and should
// never have to learn what a profile is.
func (c *Config) Select(flagProfile string) (Profile, error) {
	name := flagProfile
	if name == "" {
		name = c.DefaultProfile
	}
	if name == "" && len(c.Profiles) == 1 {
		for k := range c.Profiles {
			name = k
		}
	}

	if name == "" {
		return Profile{}, fmt.Errorf("%w: %w; run `slk configure`", errdefs.ErrConfig, ErrNoProfile)
	}

	p, ok := c.Profiles[name]
	if !ok {
		return Profile{}, fmt.Errorf("%w: %w %q; configured: %s",
			errdefs.ErrConfig, ErrUnknownProfile, name, c.profileNames())
	}
	c.selected = name
	return p, nil
}

// WritesAllowed resolves the two-level write switch.
func (c Config) WritesAllowed(p Profile) bool {
	if p.AllowWrites != nil {
		return *p.AllowWrites
	}
	return c.AllowWrites
}

func (c Config) profileNames() string {
	if len(c.Profiles) == 0 {
		return "(none)"
	}
	names := make([]string, 0, len(c.Profiles))
	for k := range c.Profiles {
		names = append(names, k)
	}
	sortStrings(names)
	return strings.Join(names, ", ")
}

// Validate reports every problem at once, keyed by YAML path rather than Go
// field name, so a reader can find what to edit.
//
// It deliberately says nothing about allow_writes: false is valid, shipped and
// recommended, so refusing to load a read-only config would be absurd and a
// warning would fire on every correct invocation. That is policy, not validity.
func (c Config) Validate() error {
	var problems []string

	if c.DefaultProfile != "" && c.DefaultProfile != envProfileName {
		if _, ok := c.Profiles[c.DefaultProfile]; !ok {
			problems = append(problems, fmt.Sprintf(
				"default_profile %q names a profile that does not exist", c.DefaultProfile))
		}
	}

	for _, name := range sortedKeys(c.Profiles) {
		p := c.Profiles[name]
		key := "profiles." + name
		if p.Workspace == "" {
			problems = append(problems, key+".workspace is empty")
		}
		if p.Token.Empty() {
			problems = append(problems, key+".token is empty")
		} else if !strings.HasPrefix(p.Token.Reveal(), "xoxc-") {
			// Worth the words: pasting a bot token is the single most likely
			// first-run mistake, and "invalid token" would send the reader
			// looking in the wrong place entirely.
			problems = append(problems, key+
				".token must start with xoxc- (this is the web client's bearer token, not a bot token)")
		}
		if p.Cookie.Empty() {
			problems = append(problems, key+".cookie is empty (the value of the `d` cookie)")
		} else if err := validateCookie(p.Cookie.Reveal()); err != nil {
			problems = append(problems, key+".cookie "+err.Error())
		}
	}

	switch c.Output.Mode {
	case "text", "json", "jsonl":
	default:
		problems = append(problems, fmt.Sprintf("output.mode %q is not text, json or jsonl", c.Output.Mode))
	}
	switch c.Output.Color {
	case "auto", "always", "never":
	default:
		problems = append(problems, fmt.Sprintf("output.color %q is not auto, always or never", c.Output.Color))
	}
	if c.Output.Timezone != "" && c.Output.Timezone != "Local" {
		if _, err := time.LoadLocation(c.Output.Timezone); err != nil {
			problems = append(problems, fmt.Sprintf("output.timezone %q is not a known IANA timezone", c.Output.Timezone))
		}
	}

	if c.Defaults.Limit < 0 {
		problems = append(problems, "defaults.limit cannot be negative")
	}
	if c.Defaults.MaxTotal < 0 {
		problems = append(problems, "defaults.max_total cannot be negative")
	}
	if c.Defaults.Timeout.Std() < 0 {
		problems = append(problems, "defaults.timeout cannot be negative")
	}
	if c.Cache.UsersTTL.Std() < 0 {
		problems = append(problems, "cache.users_ttl cannot be negative")
	}
	if c.Cache.ConversationsTTL.Std() < 0 {
		problems = append(problems, "cache.conversations_ttl cannot be negative")
	}

	if len(problems) > 0 {
		return fmt.Errorf("%w: invalid configuration:\n  - %s",
			errdefs.ErrConfig, strings.Join(problems, "\n  - "))
	}
	return nil
}
