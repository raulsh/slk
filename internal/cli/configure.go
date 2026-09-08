package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/raulsh/slk/internal/config"
	"github.com/raulsh/slk/internal/errdefs"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const devtoolsWalkthrough = `Both credentials come from a browser already signed in to your workspace.

The quickest way, which gets both at once:

  1. Open your workspace in Chrome or Firefox.
  2. Open DevTools (F12) and go to the Network tab.
  3. Filter on  /api/  and click any request that appears.
  4. Right-click it > Copy > Copy as cURL.
  5. Run:  slk configure --from-curl -   and paste, then Ctrl-D.

Or find them by hand:

  token   Network > any request to /api/ > Payload > the "token" field (xoxc-...)
  cookie  Application > Cookies > your workspace > the "d" row (xoxd-...)
`

type configureOpts struct {
	workspace    string
	token        string
	cookie       string
	cookieDS     string
	enterpriseID string
	fromCurl     string
	readStdin    bool
	setDefault   bool
	allowWrites  bool
	noVerify     bool
	force        bool
}

func newConfigureCmd(g *globals) *cobra.Command {
	var o configureOpts

	cmd := &cobra.Command{
		Use:   "configure",
		Short: "Store the credentials slk reads Slack with",
		Long: `Store a workspace's xoxc token and xoxd cookie.

The credentials are written to ~/.config/slk/config.yaml at mode 0600. The file
is meant to be hand-editable, so anything configure writes you can also just
type.

` + devtoolsWalkthrough,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runConfigure(cmd, &o)
		},
	}

	f := cmd.Flags()
	f.StringVar(&o.workspace, "workspace", "", "workspace subdomain, e.g. acme for acme.slack.com")
	f.StringVar(&o.token, "token", "", "xoxc token; - reads one line from stdin")
	f.StringVar(&o.cookie, "cookie", "", "the `d` cookie value, or a whole Cookie: header; - reads stdin")
	f.StringVar(&o.cookieDS, "cookie-ds", "", "the `d-s` cookie value, needed on some enterprise grids")
	f.StringVar(&o.enterpriseID, "enterprise-id", "", "enterprise grid id (E...), when applicable")
	f.StringVar(&o.fromCurl, "from-curl", "", "extract credentials from a DevTools \"Copy as cURL\"; - reads stdin, otherwise a file path")
	f.BoolVar(&o.readStdin, "stdin", false, "read the whole profile as YAML from stdin")
	f.BoolVar(&o.setDefault, "set-default", true, "make this the default profile")
	f.BoolVar(&o.allowWrites, "allow-writes", false, "permit slk send and slk mark for this profile")
	f.BoolVar(&o.noVerify, "no-verify", false, "skip the live credential check after writing")
	f.BoolVar(&o.force, "force", false, "overwrite an existing profile without confirming")

	return cmd
}

func (g *globals) runConfigure(cmd *cobra.Command, o *configureOpts) error {
	path, err := g.configPath()
	if err != nil {
		return err
	}

	// Load leniently: an invalid or absent existing file must not stop someone
	// from fixing it with the very command meant to fix it.
	cfg, err := config.Load(path)
	if err != nil {
		g.log.Debug("existing configuration not usable, starting from defaults", "error", err)
		cfg = config.Default()
	}

	profile, source, err := g.gatherProfile(cmd, o)
	if err != nil {
		return err
	}

	name := g.profile
	if name == "" {
		name = profile.Workspace
	}
	if name == "" {
		return errdefs.Usagef("no profile name and no workspace to derive one from")
	}

	if existing, ok := cfg.Profiles[name]; ok && !o.force {
		if err := g.confirmOverwrite(name, existing); err != nil {
			return err
		}
	}
	if o.allowWrites {
		yes := true
		profile.AllowWrites = &yes
	}

	if cfg.Profiles == nil {
		cfg.Profiles = map[string]config.Profile{}
	}
	cfg.Profiles[name] = profile
	if o.setDefault || cfg.DefaultProfile == "" {
		cfg.DefaultProfile = name
	}
	cfg.Path()

	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := config.Write(cfg, path); err != nil {
		return err
	}

	note(g.env.Stderr, fmt.Sprintf("wrote profile %q from %s to %s (mode 0600)", name, source, path))
	if !o.allowWrites && !cfg.AllowWrites {
		note(g.env.Stderr, "writes are disabled; `slk send` and `slk mark` need allow_writes: true")
	}

	// Verification needs the transport, which does not exist yet. Say so
	// rather than silently claiming the credentials are good.
	if !o.noVerify {
		note(g.env.Stderr, "not verified: `slk auth test` is not implemented yet")
	}
	return nil
}

// gatherProfile resolves the credentials from whichever input path the caller
// used, and reports which one it was so the confirmation line can say.
func (g *globals) gatherProfile(cmd *cobra.Command, o *configureOpts) (config.Profile, string, error) {
	switch {
	case o.fromCurl != "":
		blob, err := g.readBlob(o.fromCurl)
		if err != nil {
			return config.Profile{}, "", err
		}
		p, err := parseCurl(blob).intoProfile()
		if err != nil {
			return config.Profile{}, "", err
		}
		if o.workspace != "" {
			p.Workspace = o.workspace
		}
		return p, "a pasted curl command", nil

	case o.readStdin:
		p, err := g.readProfileYAML()
		return p, "stdin", err

	case o.token != "" || o.cookie != "":
		p, err := g.profileFromFlags(o)
		return p, "flags", err

	default:
		p, err := g.promptForProfile(o)
		return p, "an interactive prompt", err
	}
}

func (g *globals) profileFromFlags(o *configureOpts) (config.Profile, error) {
	token, err := g.resolveFlagValue(o.token, "--token")
	if err != nil {
		return config.Profile{}, err
	}
	rawCookie, err := g.resolveFlagValue(o.cookie, "--cookie")
	if err != nil {
		return config.Profile{}, err
	}

	d, ds := config.ParseCookieHeader(rawCookie)
	if d == "" {
		return config.Profile{}, errdefs.Usagef(
			"could not find a `d` cookie in --cookie; pass the xoxd- value or the whole Cookie: header")
	}
	cookie, err := config.NormalizeCookie(d)
	if err != nil {
		return config.Profile{}, errdefs.Usagef("--cookie %s", err)
	}
	if o.cookieDS != "" {
		ds = o.cookieDS
	}
	if o.workspace == "" {
		return config.Profile{}, errdefs.Usagef("--workspace is required alongside --token and --cookie")
	}

	return config.Profile{
		Workspace:    o.workspace,
		Token:        config.Secret(config.NormalizeToken(token)),
		Cookie:       config.Secret(cookie),
		CookieDS:     config.Secret(ds),
		EnterpriseID: o.enterpriseID,
	}, nil
}

func (g *globals) promptForProfile(o *configureOpts) (config.Profile, error) {
	p := newPrompter(g.env)
	if err := p.requireTerminal(); err != nil {
		return config.Profile{}, err
	}

	note(g.env.Stderr, devtoolsWalkthrough)

	workspace, err := p.line("Workspace subdomain", o.workspace)
	if err != nil {
		return config.Profile{}, err
	}
	if workspace == "" {
		return config.Profile{}, errdefs.Usagef("a workspace subdomain is required")
	}

	token, err := p.secret("xoxc token", false)
	if err != nil {
		return config.Profile{}, err
	}
	rawCookie, err := p.secret("xoxd cookie (the `d` value, or a whole Cookie: header)", false)
	if err != nil {
		return config.Profile{}, err
	}
	cookieDS, err := p.secret("d-s cookie, enterprise grids only", true)
	if err != nil {
		return config.Profile{}, err
	}

	d, ds := config.ParseCookieHeader(rawCookie)
	if d == "" {
		return config.Profile{}, errdefs.Usagef("could not find a `d` cookie in what you pasted")
	}
	cookie, err := config.NormalizeCookie(d)
	if err != nil {
		return config.Profile{}, errdefs.Usagef("the cookie %s", err)
	}
	if cookieDS == "" {
		cookieDS = ds
	}

	return config.Profile{
		Workspace:    workspace,
		Token:        config.Secret(config.NormalizeToken(token)),
		Cookie:       config.Secret(cookie),
		CookieDS:     config.Secret(cookieDS),
		EnterpriseID: o.enterpriseID,
	}, nil
}

// readProfileYAML reads a profile in exactly the shape `config show` writes, so
// copying one profile to another workspace is a pipe.
func (g *globals) readProfileYAML() (config.Profile, error) {
	data, err := io.ReadAll(g.env.Stdin)
	if err != nil {
		return config.Profile{}, fmt.Errorf("read stdin: %w", err)
	}
	var p config.Profile
	if err := yaml.Unmarshal(data, &p); err != nil {
		return config.Profile{}, errdefs.Usagef("parse the profile on stdin: %s", err)
	}
	return p, nil
}

// resolveFlagValue expands the "-" convention, which reads one line from stdin
// so a credential need never appear in a shell history or a process list.
func (g *globals) resolveFlagValue(v, flag string) (string, error) {
	if v != "-" {
		return v, nil
	}
	data, err := io.ReadAll(g.env.Stdin)
	if err != nil {
		return "", fmt.Errorf("read %s from stdin: %w", flag, err)
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(data)), "\n")
	if line == "" {
		return "", errdefs.Usagef("%s was given as - but stdin was empty", flag)
	}
	return strings.TrimSpace(line), nil
}

func (g *globals) readBlob(src string) (string, error) {
	if src == "-" {
		data, err := io.ReadAll(g.env.Stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return string(data), nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return "", fmt.Errorf("%w: read %s: %w", errdefs.ErrConfig, src, err)
	}
	return string(data), nil
}

func (g *globals) confirmOverwrite(name string, existing config.Profile) error {
	p := newPrompter(g.env)
	f, ok := g.env.Stdin.(*os.File)
	if !ok || !g.env.IsTTY(f) {
		return errdefs.Usagef(
			"profile %q already exists (workspace %s); pass --force to overwrite it",
			name, existing.Workspace)
	}
	ok, err := p.confirm(fmt.Sprintf("Overwrite profile %q (workspace %s)?", name, existing.Workspace))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: declined at the confirmation prompt", errdefs.ErrWriteRefused)
	}
	return nil
}

func (g *globals) configPath() (string, error) {
	if g.config != "" {
		return g.config, nil
	}
	if p := os.Getenv("SLK_CONFIG"); p != "" {
		return p, nil
	}
	return config.DefaultPath()
}
