package resolve

import (
	"testing"

	"github.com/raulsh/slk/internal/output"
)

// testDirectory is a cache as a fresh fetch would leave it, with no disk and
// no network behind it.
func testDirectory() *Directory {
	return &Directory{
		Conversations: []output.Conversation{
			{ID: "C111AAAAA", Name: "general", Type: "channel"},
			{ID: "C222BBBBB", Name: "security", Type: "channel"},
			{ID: "C333CCCCC", Name: "eng-incidents", Type: "channel"},
			{ID: "G444DDDDD", Name: "testing", Type: "private"},
			{ID: "D555EEEEE", Type: "dm", User: "U666FFFFF"},
		},
		Users: map[string]output.User{
			"U666FFFFF": {ID: "U666FFFFF", Name: "samantha"},
		},
		Bots: map[string]string{},
	}
}

// TestIsIDIsCaseSensitive pins the rule that tells an id from a name. Slack ids
// are upper case and names are lower case, so folding case made every
// seven-letter name that starts with an id prefix look like an id.
func TestIsIDIsCaseSensitive(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"C0123ABCD", true},
		{"D0123ABCD", true},
		{"G0123ABCD", true},
		{"U0123ABCD", true},
		{"W0123ABCD", true},
		{"general", false},
		{"support", false},
		{"security", false},
		{"testing", false},
		{"backend", false},
		{"dmitriy", false},
		{"c0123abcd", false},
		{"#general", false},
		{"@samantha", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsID(tc.in); got != tc.want {
			t.Errorf("IsID(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestNamesThatLookLikeIDsResolveByName is the regression the case rule fixes:
// `--channel general` and `--dm samantha` used to be sent to Slack as the ids
// GENERAL and samantha.
func TestNamesThatLookLikeIDsResolveByName(t *testing.T) {
	d := testDirectory()

	channels := []struct{ in, want string }{
		{"general", "C111AAAAA"},
		{"#general", "C111AAAAA"},
		{"security", "C222BBBBB"},
		{"testing", "G444DDDDD"},
		{"C111AAAAA", "C111AAAAA"},
	}
	for _, tc := range channels {
		got, err := d.Channel(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("Channel(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}

	for _, in := range []string{"samantha", "@samantha", "U666FFFFF", "D555EEEEE"} {
		got, err := d.DM(in)
		if err != nil || got != "D555EEEEE" {
			t.Errorf("DM(%q) = %q, %v; want D555EEEEE", in, got, err)
		}
	}
}
