package cli

import (
	"regexp"

	"github.com/raulsh/slk/internal/errdefs"
)

// errdefsUsage re-labels a validation failure from a helper package as the
// caller's mistake, so a bad --since exits 2 rather than 1.
func errdefsUsage(err error) error {
	if err == nil {
		return nil
	}
	return errdefs.Usagef("%s", err)
}

// mentionPattern finds the user ids referenced inside message text, so they
// can be resolved in the same batch as the authors.
var mentionPattern = regexp.MustCompile(`<@([UWB][A-Z0-9]{6,})`)

func mentionIDs(text string) []string {
	m := mentionPattern.FindAllStringSubmatch(text, -1)
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(m))
	for _, g := range m {
		out = append(out, g[1])
	}
	return out
}
