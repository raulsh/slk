package output

import (
	"fmt"
	"runtime"
)

// Version identifies the build. Schema travels with it so an agent can check
// the envelope contract it is about to parse before parsing anything else.
type Version struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Go      string `json:"go"`
	Schema  int    `json:"schema"`
}

// NewVersion fills in the parts the build does not supply.
func NewVersion(version, commit string) Version {
	return Version{
		Version: version,
		Commit:  commit,
		Go:      runtime.Version(),
		Schema:  Schema,
	}
}

// Version renders build information.
func (w *Writer) Version(v Version) error {
	switch w.opts.Mode {
	case ModeJSON:
		return w.encode(w.envelope(true, []Version{v}, 1, nil))
	case ModeJSONL:
		return w.encode(v)
	default:
		_, err := fmt.Fprintf(w.out, "slk %s (commit %s, %s)\n", v.Version, v.Commit, v.Go)
		return err
	}
}

// ConfigProfile is one credential profile as `config show` reports it.
//
// Token and Cookie are strings rather than config.Secret because this record
// is what gets marshalled: the redaction decision belongs to the command,
// which knows whether --reveal was passed, and encoding a Secret here would
// make revealing impossible rather than merely explicit.
type ConfigProfile struct {
	Name         string `json:"name"`
	Workspace    string `json:"workspace"`
	Token        string `json:"token"`
	Cookie       string `json:"cookie"`
	CookieDS     string `json:"cookie_ds,omitempty"`
	EnterpriseID string `json:"enterprise_id,omitempty"`
	Default      bool   `json:"default"`
	AllowWrites  bool   `json:"allow_writes"`
}

// ConfigProfiles renders the configured profiles.
func (w *Writer) ConfigProfiles(ps []ConfigProfile) error {
	switch w.opts.Mode {
	case ModeJSON:
		return w.encode(w.envelope(true, ps, len(ps), nil))
	case ModeJSONL:
		for _, p := range ps {
			if err := w.encode(p); err != nil {
				return err
			}
		}
		return nil
	default:
		t := w.table("PROFILE", "WORKSPACE", "TOKEN", "COOKIE", "WRITES")
		for _, p := range ps {
			name := p.Name
			if p.Default {
				// The default is the one that surprises people, so mark it
				// rather than making them cross-reference default_profile.
				name += " *"
			}
			t.row(name, p.Workspace, p.Token, p.Cookie, yesNo(p.AllowWrites))
		}
		if err := t.render(); err != nil {
			return err
		}
		if len(ps) > 0 {
			w.note("* default profile")
		}
		return nil
	}
}

// ConfigPath is where the configuration lives.
type ConfigPath struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
}

// ConfigPath renders the resolved configuration path.
func (w *Writer) ConfigPath(p ConfigPath) error {
	switch w.opts.Mode {
	case ModeJSON:
		return w.encode(w.envelope(true, []ConfigPath{p}, 1, nil))
	case ModeJSONL:
		return w.encode(p)
	default:
		// Bare, with no decoration, so `$(slk config path)` is usable.
		_, err := fmt.Fprintln(w.out, p.Path)
		return err
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// Auth is what `auth test` reports.
//
// Profile and TokenHint are here because the most common confusion in a
// multi-workspace setup is "I edited the wrong profile": saying which
// credentials answered costs one line and settles it.
type Auth struct {
	Profile       string `json:"profile"`
	Workspace     string `json:"workspace"`
	WorkspaceURL  string `json:"workspace_url"`
	TeamID        string `json:"team_id"`
	TeamName      string `json:"team_name"`
	UserID        string `json:"user_id"`
	UserName      string `json:"user_name"`
	EnterpriseID  string `json:"enterprise_id,omitempty"`
	Enterprise    bool   `json:"enterprise"`
	APIBase       string `json:"api_base"`
	TokenHint     string `json:"token_hint"`
	WritesAllowed bool   `json:"writes_allowed"`
}

// Auth renders an identity.
func (w *Writer) Auth(a Auth) error {
	switch w.opts.Mode {
	case ModeJSON:
		return w.encode(w.envelope(true, []Auth{a}, 1, nil))
	case ModeJSONL:
		return w.encode(a)
	default:
		rows := [][2]string{
			{"user", "@" + a.UserName + "  (" + a.UserID + ")"},
			{"team", a.TeamName + "  (" + a.TeamID + ")"},
			{"workspace", a.WorkspaceURL},
			{"profile", a.Profile},
			{"token", a.TokenHint},
			{"api base", a.APIBase},
			{"writes", yesNo(a.WritesAllowed)},
		}
		if a.Enterprise {
			id := a.EnterpriseID
			if id == "" {
				id = "yes"
			}
			rows = append(rows, [2]string{"enterprise", id})
		}
		t := w.table("", "")
		for _, r := range rows {
			t.row(w.colorize(dim, r[0]), r[1])
		}
		return t.render()
	}
}
