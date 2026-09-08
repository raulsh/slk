package cli

import (
	"github.com/raulsh/slk/internal/output"
	"github.com/spf13/cobra"
)

func newAuthCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Check the stored credentials",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newAuthTestCmd(g))
	return cmd
}

func newAuthTestCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "test",
		Short: "Verify the credentials and report who they belong to",
		Long: `Call Slack's auth.test with the stored credentials.

This is the first thing to run after configure, and the first thing to run when
anything else starts failing: it distinguishes a dead credential from every
other kind of problem, and it reports which profile answered.

Exit 3 means the token or cookie has expired.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := g.newSession()
			if err != nil {
				return err
			}

			ctx, cancel := g.deadline(cmd.Context())
			defer cancel()

			id, err := s.client.AuthTest(ctx)
			if err != nil {
				return err
			}

			return g.out.Auth(output.Auth{
				Profile:       s.name,
				Workspace:     s.profile.Workspace,
				WorkspaceURL:  id.URL,
				TeamID:        id.TeamID,
				TeamName:      id.TeamName,
				UserID:        id.UserID,
				UserName:      id.UserName,
				EnterpriseID:  id.EnterpriseID,
				Enterprise:    id.IsEnterprise,
				APIBase:       s.client.BaseURL(),
				TokenHint:     s.client.TokenHint(),
				WritesAllowed: s.cfg.WritesAllowed(s.profile),
			})
		},
	}
}
