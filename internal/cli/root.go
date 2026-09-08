package cli

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/raulsh/slk/internal/errdefs"
	"github.com/raulsh/slk/internal/output"
	"github.com/spf13/cobra"
)

// globals holds the persistent flags and the collaborators built from them.
//
// One struct threaded through the tree, rather than package-level state,
// because two Execute calls in one test process must not see each other's
// flags.
type globals struct {
	env Env

	profile  string
	config   string
	asJSON   bool
	asJSONL  bool
	raw      bool
	noColor  bool
	logLevel string
	timeout  time.Duration
	noCache  bool
	full     bool
	dryRun   bool
	yes      bool

	log *slog.Logger
	out *output.Writer

	// cmd is the command actually being run, recorded so Execute can print the
	// right usage text for a usage error.
	cmd *cobra.Command
}

func newRoot(g *globals) *cobra.Command {
	root := &cobra.Command{
		Use:   "slk",
		Short: "Read your Slack from the command line, as you",
		Long: `slk reads Slack as you, using the credentials your own browser already holds,
so it sees your DMs and private channels and needs nobody's approval.

Output is shaped for a caller that pays per token: compact records with names
already resolved, and nothing but results on stdout.`,
		SilenceErrors:     true,
		SilenceUsage:      true,
		DisableAutoGenTag: true,
		// A bare `slk` should teach, not fail silently.
		RunE: func(cmd *cobra.Command, args []string) error {
			return errdefs.Usagef("no command given")
		},
	}

	f := root.PersistentFlags()
	f.StringVar(&g.profile, "profile", "", "credential profile to use")
	f.StringVar(&g.config, "config", "", "configuration file path (default ~/.config/slk/config.yaml)")
	f.BoolVar(&g.asJSON, "json", false, "emit a single JSON envelope on stdout")
	f.BoolVar(&g.asJSONL, "jsonl", false, "emit one JSON record per line")
	f.BoolVar(&g.raw, "raw", false, "include Slack's untouched payload; implies the machine format")
	f.BoolVar(&g.noColor, "no-color", false, "disable ANSI colour")
	f.StringVar(&g.logLevel, "log-level", "warn", "debug, info, warn or error (to stderr)")
	f.DurationVar(&g.timeout, "timeout", 30*time.Second, "wall-clock limit for the whole command")
	f.BoolVar(&g.noCache, "no-cache", false, "bypass the local id and name cache")
	f.BoolVar(&g.full, "full", false, "do not truncate long message text in the human view")
	f.BoolVar(&g.dryRun, "dry-run", false, "on a write, print what would be sent and exit without sending")
	f.BoolVar(&g.yes, "yes", false, "skip the confirmation prompt for a write")

	// Every pflag failure is the caller's mistake, so it must exit 2 rather
	// than being classified as an unknown internal error.
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		g.cmd = cmd
		return errdefs.Usagef("%s", err)
	})

	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		g.cmd = cmd
		return g.setup(cmd)
	}

	root.AddCommand(
		newAuthCmd(g),
		newBrowseCmd(g),
		newConfigureCmd(g),
		newConfigCmd(g),
		newConversationsCmd(g),
		newDebugCmd(g),
		newMessagesCmd(g),
		newSearchCmd(g),
		newThreadCmd(g),
		newUnreadCmd(g),
		newUsersCmd(g),
		newVersionCmd(g),
	)

	return root
}

// setup resolves the persistent flags into a logger and an output writer. It
// runs after flag parsing and before any command body, so every command can
// assume g.out and g.log exist.
func (g *globals) setup(cmd *cobra.Command) error {
	// Resolve the writer first, on a best-effort mode, so that even an invalid
	// flag combination is reported in the shape the caller asked for. An agent
	// that passed --json and got bare prose on stderr would have nothing to
	// parse at exactly the moment it needs to know what it got wrong.
	mode, modeErr := g.mode(cmd)
	if modeErr != nil {
		mode = g.fallbackMode()
	}

	lvl, err := parseLevel(g.logLevel)
	if err != nil {
		return err
	}
	// Text, to stderr: these lines are read by a human over the shoulder of a
	// machine-readable stdout, not ingested.
	g.log = slog.New(slog.NewTextHandler(g.env.Stderr, &slog.HandlerOptions{Level: lvl}))

	g.out = output.New(g.env.Stdout, g.env.Stderr, output.Options{
		Mode:    mode,
		Color:   g.colorEnabled(mode),
		Command: cmd.Name(),
		Full:    g.full,
	})
	return modeErr
}

// fallbackMode picks a machine format when the caller clearly wanted one, even
// though the combination they gave was invalid.
func (g *globals) fallbackMode() output.Mode {
	switch {
	case g.asJSONL:
		return output.ModeJSONL
	case g.asJSON:
		return output.ModeJSON
	default:
		return output.ModeText
	}
}

// mode resolves --json/--jsonl/--raw into a rendering shape.
//
// --raw implies the machine format for the command at hand rather than --json
// specifically, so that a streaming command resolves it to JSON Lines instead
// of a format it cannot produce.
func (g *globals) mode(cmd *cobra.Command) (output.Mode, error) {
	if g.asJSON && g.asJSONL {
		return output.ModeText, errdefs.Usagef("--json and --jsonl are mutually exclusive")
	}
	switch {
	case g.asJSONL:
		return output.ModeJSONL, nil
	case g.asJSON:
		if streams(cmd) {
			return output.ModeText, errdefs.Usagef(
				"--json builds one envelope around a complete result set, and a live stream has no end; use --jsonl")
		}
		return output.ModeJSON, nil
	case g.raw:
		if streams(cmd) {
			return output.ModeJSONL, nil
		}
		return output.ModeJSON, nil
	default:
		return output.ModeText, nil
	}
}

// streams reports whether a command produces an unbounded result. It is an
// annotation rather than a type switch so that adding a streaming command is a
// one-line change at its definition.
func streams(cmd *cobra.Command) bool {
	return cmd.Annotations["stream"] == "true"
}

// colorEnabled requires every one of the usual conditions. A machine format is
// never coloured, whatever the terminal looks like.
func (g *globals) colorEnabled(mode output.Mode) bool {
	if mode.Machine() || g.noColor {
		return false
	}
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	f, ok := g.env.Stdout.(*os.File)
	return ok && g.env.IsTTY(f)
}

func parseLevel(s string) (slog.Level, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return l, errdefs.Usagef("--log-level %q is not debug, info, warn or error", s)
	}
	return l, nil
}

func newVersionCmd(g *globals) *cobra.Command {
	var short bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if short {
				_, err := fmt.Fprintln(g.env.Stdout, g.env.Version)
				return err
			}
			return g.out.Version(output.NewVersion(g.env.Version, g.env.Commit))
		},
	}
	cmd.Flags().BoolVar(&short, "short", false, "print just the version number")
	return cmd
}
