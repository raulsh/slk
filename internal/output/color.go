package output

// ANSI codes, hand-rolled rather than pulled in as a dependency: there are
// four uses in the whole tool, and a colour library would be more code to
// audit than the escape sequences it replaces.
const (
	reset  = "\x1b[0m"
	bold   = "\x1b[1m"
	dim    = "\x1b[2m"
	cyan   = "\x1b[36m"
	yellow = "\x1b[33m"
)

// colorize wraps s only when colour is enabled, so every call site can be
// unconditional and the gating decision lives in exactly one place.
func (w *Writer) colorize(code, s string) string {
	if !w.opts.Color || s == "" {
		return s
	}
	return code + s + reset
}
