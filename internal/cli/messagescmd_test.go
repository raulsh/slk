package cli

import (
	"encoding/json"
	"net/url"
	"slices"
	"testing"

	"github.com/raulsh/slk/internal/errdefs"
)

// TestMessagesFromAcceptsWhatDMAccepts pins --from to the spellings --dm
// takes. It used to run before authors were resolved, when a person's message
// carries nothing but a user id, so --from @alice silently matched nothing.
func TestMessagesFromAcceptsWhatDMAccepts(t *testing.T) {
	users := map[string]string{
		"U0ALICE01": `{"ok":true,"user":{"id":"U0ALICE01","name":"alice","real_name":"Alice Adams",
			"profile":{"display_name":"Ally","email":"alice@acme.test"}}}`,
		"U0BOB0001": `{"ok":true,"user":{"id":"U0BOB0001","name":"bob"}}`,
		"U0CAROL01": `{"ok":true,"user":{"id":"U0CAROL01","name":"carol"}}`,
	}
	f := newFakeSlack(t, map[string]reply{
		"users.conversations": static(`{"ok":true,"channels":[{"id":"C0456EFGH","name":"eng","is_channel":true}]}`),
		"users.prefs.get":     static(`{"ok":true,"prefs":{"vip_users":""}}`),
		"conversations.history": static(`{"ok":true,"has_more":false,"messages":[
			{"type":"message","ts":"1792072800.000400","subtype":"bot_message","bot_id":"B0DEPLOY1","username":"deploybot","text":"deployed"},
			{"type":"message","ts":"1792072800.000300","user":"U0BOB0001","text":"lgtm, cc <@U0CAROL01>"},
			{"type":"message","ts":"1792072800.000200","user":"U0ALICE01","text":"ship it"},
			{"type":"message","ts":"1792072800.000100","user":"U0ALICE01","text":"deploy is green"}]}`),
		"users.info": func(form url.Values) string {
			if body, ok := users[form.Get("user")]; ok {
				return body
			}
			return `{"ok":false,"error":"user_not_found"}`
		},
	})

	cases := []struct {
		from     string
		wantUser string
		want     int
	}{
		{"@alice", "U0ALICE01", 2},
		{"alice", "U0ALICE01", 2},
		{"ALICE", "U0ALICE01", 2},
		{"Ally", "U0ALICE01", 2},
		{"alice@acme.test", "U0ALICE01", 2},
		{"U0ALICE01", "U0ALICE01", 2},
		{"deploybot", "", 1},
		{"@carol", "", 0},
	}

	for _, tc := range cases {
		t.Run(tc.from, func(t *testing.T) {
			got := runAgainst(t, f, testConfig, "messages", "--channel", "eng", "--from", tc.from, "--json")
			if got.code != errdefs.ExitOK {
				t.Fatalf("exit = %d, want 0 (stderr: %s)", got.code, got.stderr)
			}

			var env struct {
				Count   int `json:"count"`
				Results []struct {
					User string `json:"user"`
					Text string `json:"text"`
				} `json:"results"`
				Names struct {
					Users map[string]string `json:"users"`
				} `json:"names"`
			}
			if err := json.Unmarshal([]byte(got.stdout), &env); err != nil {
				t.Fatalf("stdout is not JSON: %v (%q)", err, got.stdout)
			}
			if env.Count != tc.want || len(env.Results) != tc.want {
				t.Fatalf("got %d messages, want %d: %+v", len(env.Results), tc.want, env.Results)
			}
			for _, m := range env.Results {
				if m.User != tc.wantUser {
					t.Errorf("message %q is from %q, want %q", m.Text, m.User, tc.wantUser)
				}
			}
			// Carol is mentioned only in a message the filter drops, and the
			// names map covers only the ids that appear in the results.
			if name, ok := env.Names.Users["U0CAROL01"]; ok {
				t.Errorf("names resolves U0CAROL01 to %q, but no result mentions it", name)
			}
		})
	}

	// Authors resolve one id at a time. Listing the member directory instead
	// is the traffic pattern that gets browser sessions invalidated.
	if slices.Contains(f.called(), "users.list") {
		t.Errorf("resolving authors enumerated the member directory: %v", f.called())
	}
}
