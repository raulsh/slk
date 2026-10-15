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

Methods that only read go straight through. Anything else - chat.postMessage,
conversations.mark, auth.revoke - would act as you, so it needs the same
opt-in as any other write: allow_writes: true for the profile.

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

			// A raw call can do anything the user can, so whatever is not
			// recognisably a read sits behind the same switch as every other
			// write - checked before anything reaches the network.
			if !isReadMethod(args[0]) && !s.cfg.WritesAllowed(s.profile) {
				return errdefs.WithHint(
					fmt.Errorf("%w: %s is not a read, and writes are disabled for profile %q",
						errdefs.ErrWriteRefused, args[0], s.name),
					"Sending it is the account owner's decision: set allow_writes: true in\n"+
						s.cfg.Path()+", at the top level or under this profile.")
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

// readVerbs are the final segments of the Slack methods that only read.
//
// An allowlist rather than a list of known writes, because Slack adds methods
// without notice: a read missing from here is refused until someone adds a
// word, while a write missing from a denylist would post as the user.
var readVerbs = map[string]bool{
	"test": true, "info": true, "list": true, "get": true,
	"history": true, "replies": true, "counts": true, "members": true,
	"conversations": true, "boot": true,
	"getPresence": true, "getPermalink": true, "lookupByEmail": true,
}

// isReadMethod reports whether a Slack method only reads. search.* is
// read-only as a namespace; everywhere else the verb that ends the name
// decides.
func isReadMethod(method string) bool {
	if strings.HasPrefix(method, "search.") {
		return true
	}
	return readVerbs[method[strings.LastIndex(method, ".")+1:]]
}
