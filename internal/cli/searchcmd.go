package cli

import (
	"strings"

	"github.com/raulsh/slk/internal/errdefs"
	"github.com/raulsh/slk/internal/output"
	"github.com/raulsh/slk/internal/slack"
	"github.com/raulsh/slk/internal/slacktime"
	"github.com/spf13/cobra"
)

func newSearchCmd(g *globals) *cobra.Command {
	var (
		tf      timeFlags
		in      []string
		from    []string
		with    []string
		sortBy  string
		sortDir string
		limit   int
		page    int
	)

	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search messages",
		Long: `Search your Slack.

The flags build Slack's search syntax for you, so you need not know it, and the
positional query passes through verbatim so you can use it if you do.

One honest limitation: Slack's search has no precise time filter, only
day-granular after:/before: modifiers. So --since/--until widen the server-side
window by a day on each side and the results are then filtered locally to the
exact range you asked for. The assembled query is always echoed.`,
		Example: `  slk search "deploy failed"
  slk search "deploy" --in '#eng' --since 7d
  slk search --from @alice "release notes"`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rng, err := g.timeRange(tf.since, tf.until, tf.on)
			if err != nil {
				return err
			}
			for _, a := range args {
				if strings.Contains(a, `"`) {
					return errdefs.Usagef(
						"a double quote in the query cannot be escaped in Slack's search syntax; remove it")
				}
			}

			s, err := g.newSession()
			if err != nil {
				return err
			}
			ctx, cancel := g.deadline(cmd.Context())
			defer cancel()

			d, err := g.directory(ctx, s)
			if err != nil {
				return err
			}

			o := slack.SearchOptions{
				Query:   strings.Join(args, " "),
				In:      in,
				From:    from,
				With:    with,
				Sort:    sortBy,
				SortDir: sortDir,
				Count:   limit,
				Page:    page,
			}
			// Widen by a day on each side. Whether Slack's after:/before: are
			// inclusive is undocumented, and the local filter below restores
			// exact semantics either way - so over-fetching is the safe choice.
			if !rng.Since.IsZero() {
				o.After = rng.Since.AddDate(0, 0, -1).Format("2006-01-02")
			}
			if !rng.Until.IsZero() {
				o.Before = rng.Until.AddDate(0, 0, 1).Format("2006-01-02")
			}

			res, err := s.client.Search(ctx, o)
			if err != nil {
				return err
			}

			kept := res.Messages
			if !rng.IsZero() {
				var filtered []output.Message
				for _, m := range kept {
					ts, err := slacktime.Parse(m.TS)
					if err != nil || rng.Contains(ts) {
						filtered = append(filtered, m)
					}
				}
				if dropped := len(kept) - len(filtered); dropped > 0 {
					g.log.Debug("filtered search results outside the exact window", "dropped", dropped)
				}
				kept = filtered
			}

			g.finish(ctx, s, d, kept)
			for i := range kept {
				// Prefer the directory's rendering: search returns a bare
				// name, and "#eng" versus "eng" versus "@alice" is exactly the
				// distinction a reader needs.
				if name, ok := d.Name(kept[i].Channel); ok {
					kept[i].ChannelName = name
				} else if kept[i].ChannelName != "" {
					kept[i].ChannelName = "#" + kept[i].ChannelName
				}
			}

			g.out.SetWindow(window(rng, g.out.Location().String()))
			g.out.SetPage(&output.Page{
				Page:    res.Page,
				Pages:   res.Pages,
				Total:   res.Total,
				HasMore: res.HasMore,
			})
			// Always echo what was actually asked: when flags built the query,
			// this is the only way to see it.
			g.out.Info("query: " + res.Query)
			if !rng.IsZero() {
				g.out.Warn("slack search filters by day; the window was widened server-side and filtered locally")
			}
			return g.out.Messages(kept)
		},
	}

	tf.register(cmd)
	f := cmd.Flags()
	f.StringSliceVar(&in, "in", nil, "restrict to a channel or DM (repeatable)")
	f.StringSliceVar(&from, "from", nil, "restrict to an author (repeatable)")
	f.StringSliceVar(&with, "with", nil, "restrict to conversations with a person (repeatable)")
	f.StringVar(&sortBy, "sort", "score", "score or timestamp")
	f.StringVar(&sortDir, "sort-dir", "desc", "asc or desc")
	f.IntVar(&limit, "limit", 20, "results per page, up to 100")
	f.IntVar(&page, "page", 1, "1-based page number")
	return cmd
}
