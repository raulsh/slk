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
    slk debug call conversations.history channel=C0123ABCD limit=1

A method whose name does not mark it as a read - chat.postMessage,
conversations.mark, anything unfamiliar - may change something in Slack, so it
needs the same consent as slk send: allow_writes, and --yes or a confirmation
at a terminal. Under --dry-run it is not called at all.`,
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

			// Without this, a caller refused by send could make the same
			// post through here, and allow_writes would protect nothing.
			if !isReadMethod(args[0]) {
				if err := g.gateWrite(s); err != nil {
					return err
				}
				if err := g.requireConsent(false); err != nil {
					return err
				}
				if g.dryRun {
					note(g.env.Stderr, "dry run: "+args[0]+" was not called")
					return nil
				}
				if err := g.confirmWrite("", fmt.Sprintf("Call %s, which may change something in Slack?", args[0])); err != nil {
					return err
				}
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

// readVerbs are the last segments of the method names that only read.
var readVerbs = map[string]bool{
	"list": true, "info": true, "history": true, "replies": true, "get": true,
	"counts": true, "test": true, "userBoot": true, "conversations": true,
}

// isReadMethod reports whether a method only reads, judged by its name - the
// one thing known about a method slk has never seen. It errs toward gating:
// an unfamiliar read costs a --yes, while an unfamiliar write let through
// would cost a post.
func isReadMethod(method string) bool {
	if strings.HasPrefix(method, "search.") {
		return true
	}
	return readVerbs[method[strings.LastIndex(method, ".")+1:]]
}
