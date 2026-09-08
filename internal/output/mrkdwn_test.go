package output

import "testing"

func TestResolve(t *testing.T) {
	names := map[string]string{
		"U0123ABCD": "raul",
		"C0123ABCD": "general",
	}
	lookup := func(id string) (string, bool) {
		n, ok := names[id]
		return n, ok
	}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"user ref", "hi <@U0123ABCD>", "hi @raul"},
		{"user ref with label", "hi <@U0123ABCD|raul>", "hi @raul"},
		// An unresolvable id must stay actionable rather than going blank.
		{"unknown user", "hi <@U999ZZZZ>", "hi @U999ZZZZ"},
		{"channel ref", "see <#C0123ABCD>", "see #general"},
		{"channel ref with label", "see <#C0123ABCD|general>", "see #general"},
		{"here", "<!here> deploy", "@here deploy"},
		{"channel broadcast", "<!channel> ping", "@channel ping"},
		{"everyone", "<!everyone>", "@everyone"},
		{"subteam", "<!subteam^S0123|@platform>", "@platform"},
		{"link with label", "<https://x.test|the docs>", "the docs (https://x.test)"},
		{"bare link", "<https://x.test>", "https://x.test"},
		{"mailto", "<mailto:a@b.test|a@b.test>", "a@b.test"},
		{"date", "<!date^1725806400^{date}|Sep 8, 2026>", "Sep 8, 2026"},
		{"html entities", "a &amp; b &lt;c&gt;", "a & b <c>"},
		// Markup itself is left alone: it reads fine as typed.
		{"markup untouched", "*bold* `code` >quote", "*bold* `code` >quote"},
		{"multiple refs", "<@U0123ABCD> and <#C0123ABCD>", "@raul and #general"},
		{"no refs", "plain text", "plain text"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := Resolve(tc.in, lookup)
			if got != tc.want {
				t.Errorf("Resolve(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestResolveReportsOnlySubstitutedIDs keeps the envelope's names map minimal:
// it must cover the ids that appear, and nothing else.
func TestResolveReportsOnlySubstitutedIDs(t *testing.T) {
	lookup := func(id string) (string, bool) {
		if id == "U1" {
			return "alice", true
		}
		return "", false
	}

	_, hit := Resolve("<@U1> <@U2> <@U1|bob>", lookup)
	if len(hit) != 1 || hit[0] != "U1" {
		t.Errorf("hit = %v, want exactly [U1]; U2 is unresolved and the labelled ref needs no lookup", hit)
	}
}

// TestLabelledRefSkipsTheCache matters for cost: Slack supplies the label on a
// large share of real references, and using it avoids a lookup entirely.
func TestLabelledRefSkipsTheCache(t *testing.T) {
	called := 0
	lookup := func(string) (string, bool) { called++; return "", false }

	got, _ := Resolve("<@U0123ABCD|raul> <#C0123ABCD|general>", lookup)
	if called != 0 {
		t.Errorf("lookup called %d times, want 0 when Slack supplied the labels", called)
	}
	if got != "@raul #general" {
		t.Errorf("Resolve = %q, want %q", got, "@raul #general")
	}
}
