package cli

import (
	"os"
	"slices"

	"github.com/raulsh/slk/internal/config"
	"github.com/raulsh/slk/internal/output"
	"github.com/spf13/cobra"
)

func newConfigCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect the configuration file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newConfigPathCmd(g), newConfigShowCmd(g))
	return cmd
}

func newConfigPathCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the configuration file path",
		Long: `Print where slk looks for its configuration.

Exits 0 whether or not the file exists, so $(slk config path) is safe in a
script that is about to create it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := g.configPath()
			if err != nil {
				return err
			}
			_, statErr := os.Stat(path)
			return g.out.ConfigPath(output.ConfigPath{Path: path, Exists: statErr == nil})
		},
	}
}

func newConfigShowCmd(g *globals) *cobra.Command {
	var reveal bool

	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show the configured profiles, with credentials redacted",
		Long: `Show the configured profiles.

Credentials are redacted to a recognisable stub. --reveal prints them in full,
which is what makes copying a profile between machines a pipe:

    slk config show --reveal --profile acme | ssh host slk configure --stdin

All profiles are shown unless --profile narrows it, so one call answers "which
workspaces can I read?".`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := g.configPath()
			if err != nil {
				return err
			}
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}

			var rows []output.ConfigProfile
			for _, name := range sortedProfileNames(cfg.Profiles) {
				if g.profile != "" && name != g.profile {
					continue
				}
				p := cfg.Profiles[name]
				rows = append(rows, output.ConfigProfile{
					Name:         name,
					Workspace:    p.Workspace,
					Token:        show(p.Token, reveal),
					Cookie:       show(p.Cookie, reveal),
					CookieDS:     show(p.CookieDS, reveal),
					EnterpriseID: p.EnterpriseID,
					Default:      name == cfg.DefaultProfile,
					AllowWrites:  cfg.WritesAllowed(p),
				})
			}

			if len(rows) == 0 {
				g.out.Warn("no profiles configured; run `slk configure`")
			}
			return g.out.ConfigProfiles(rows)
		},
	}

	cmd.Flags().BoolVar(&reveal, "reveal", false, "print credentials in full instead of redacted")
	return cmd
}

// show is the only place outside the transport and the file writer that may
// reveal a credential, and it does so only when explicitly asked.
func show(s config.Secret, reveal bool) string {
	if reveal {
		return s.Reveal()
	}
	return s.String()
}

func sortedProfileNames(m map[string]config.Profile) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	slices.Sort(names)
	return names
}
