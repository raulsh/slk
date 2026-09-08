package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/raulsh/slk/internal/errdefs"
	"github.com/spf13/cobra"
)

func newDebugCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "debug",
		Short:  "Probe the Slack web-client API directly",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newDebugCallCmd(g))
	return cmd
}

func newDebugCallCmd(g *globals) *cobra.Command {
	var pretty bool

	cmd := &cobra.Command{
		Use:   "call <method> [key=value...]",
		Short: "Call an API method and print the raw response",
		Long: `Call an arbitrary Slack web-client API method with the stored credentials.

This is a development tool for the undocumented endpoints slk depends on. Their
response shapes come from reverse engineering rather than documentation, so
asking a real workspace what a method returns is how a struct here gets
verified instead of guessed.

    slk debug call auth.test
    slk debug call client.counts thread_counts_by_channel=true org_wide_aware=true
    slk debug call conversations.history channel=C0123ABCD limit=1`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			form := url.Values{}
			for _, kv := range args[1:] {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return errdefs.Usagef("argument %q is not key=value", kv)
				}
				form.Set(k, v)
			}

			s, err := g.newSession()
			if err != nil {
				return err
			}
			ctx, cancel := g.deadline(cmd.Context())
			defer cancel()

			// auth.test first, so the call goes to the base Slack itself
			// reports rather than the one guessed from the workspace name.
			if _, err := s.client.AuthTest(ctx); err != nil {
				return err
			}

			raw, err := s.client.CallRaw(ctx, args[0], form)
			if err != nil {
				return err
			}

			out := []byte(raw)
			if pretty {
				var buf bytes.Buffer
				if err := json.Indent(&buf, raw, "", "  "); err == nil {
					out = buf.Bytes()
				}
			}
			_, err = fmt.Fprintln(g.env.Stdout, string(out))
			return err
		},
	}

	cmd.Flags().BoolVar(&pretty, "pretty", true, "indent the JSON response")
	return cmd
}
