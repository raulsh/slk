package output

import (
	"regexp"
	"strings"
)

// entityRef matches Slack's angle-bracket entity references.
//
// Slack's mrkdwn markup itself is left alone - bold, code fences and lists are
// perfectly readable as typed - but an entity reference is not: <@U0123ABCD>
// tells a reader nothing, and a consumer paying per token should not have to
// spend a second call turning it into a name.
var entityRef = regexp.MustCompile(`<([^<>]*)>`)

// Resolve rewrites entity references and reports the ids it substituted.
//
// It is a pure function taking the lookup, so the same code serves text and
// JSON rendering identically, and so it is testable without a cache or a
// network.
func Resolve(text string, lookup func(id string) (string, bool)) (string, []string) {
	var hit []string

	out := entityRef.ReplaceAllStringFunc(text, func(m string) string {
		inner := m[1 : len(m)-1]
		body, label, hasLabel := strings.Cut(inner, "|")

		switch {
		case strings.HasPrefix(body, "@"):
			id := body[1:]
			// Slack often supplies the label itself. Using it costs nothing
			// and skips the cache entirely, which on real message text is a
			// large share of all references.
			if hasLabel && label != "" {
				return "@" + label
			}
			if name, ok := lookup(id); ok {
				hit = append(hit, id)
				return "@" + name
			}
			return "@" + id

		case strings.HasPrefix(body, "#"):
			id := body[1:]
			if hasLabel && label != "" {
				return "#" + label
			}
			if name, ok := lookup(id); ok {
				hit = append(hit, id)
				return "#" + name
			}
			return "#" + id

		case strings.HasPrefix(body, "!subteam^"):
			// The label usually already carries the @, so normalise to
			// exactly one rather than assuming either way.
			if hasLabel && label != "" {
				return "@" + strings.TrimPrefix(label, "@")
			}
			return "@group"

		case body == "!here", body == "!channel", body == "!everyone":
			return "@" + body[1:]

		case strings.HasPrefix(body, "!date^"):
			// <!date^ts^format|fallback>: the fallback is the rendered form.
			if hasLabel && label != "" {
				return label
			}
			return body

		case strings.HasPrefix(body, "mailto:"):
			if hasLabel && label != "" {
				return label
			}
			return strings.TrimPrefix(body, "mailto:")

		default:
			// A link. Keep both the label and the target: a summariser wants
			// the words, and anything acting on it wants the URL.
			if hasLabel && label != "" && label != body {
				return label + " (" + body + ")"
			}
			return body
		}
	})

	// Only these three are escaped by Slack, so only these three are undone.
	// Decoding more would corrupt message text that legitimately contains an
	// entity-looking string.
	out = strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">").Replace(out)
	return out, hit
}

// ResolveInto resolves text and records the substituted ids on the Writer, so
// the envelope's names map covers exactly the ids that appear in the results.
func (w *Writer) ResolveInto(text string) string {
	if w.resolver == nil {
		out, _ := Resolve(text, func(string) (string, bool) { return "", false })
		return out
	}
	out, hit := Resolve(text, func(id string) (string, bool) {
		name, ok := w.resolver(id)
		return name, ok
	})
	for _, id := range hit {
		if name, ok := w.resolver(id); ok {
			w.seen[id] = name
		}
	}
	return out
}
