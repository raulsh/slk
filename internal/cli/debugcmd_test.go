package cli

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/raulsh/slk/internal/errdefs"
)

// writableConfig is the test profile after its owner opted in to writes.
const writableConfig = testConfig + "allow_writes: true\n"

// TestDebugCallHonoursTheWriteSwitch keeps the hidden raw-call command behind
// the same opt-in as every other write. It sends any method as the user, so
// without the switch a read-only install could still post.
func TestDebugCallHonoursTheWriteSwitch(t *testing.T) {
	replies := map[string]reply{
		"auth.test":             static(`{"ok":true,"user":"sam","user_id":"U0SAM0001","team":"Acme","team_id":"T0ACME001"}`),
		"conversations.history": static(`{"ok":true,"messages":[]}`),
		"chat.postMessage":      static(`{"ok":true}`),
	}

	t.Run("a write is refused before anything is sent", func(t *testing.T) {
		f := newFakeSlack(t, replies)
		got := runAgainst(t, f, testConfig,
			"debug", "call", "chat.postMessage", "channel=C0456EFGH", "text=hi", "--json")

		if got.code != errdefs.ExitWriteRefused {
			t.Fatalf("exit = %d, want %d (stderr: %s)", got.code, errdefs.ExitWriteRefused, got.stderr)
		}
		var env struct {
			Error struct {
				Code string `json:"code"`
				Hint string `json:"hint"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(got.stdout), &env); err != nil {
			t.Fatalf("stdout is not JSON: %v (%q)", err, got.stdout)
		}
		if env.Error.Code != string(errdefs.CodeWriteRefused) {
			t.Errorf("error.code = %q, want %q", env.Error.Code, errdefs.CodeWriteRefused)
		}
		if env.Error.Hint == "" {
			t.Error("error.hint is empty, want the remedy")
		}
		if calls := f.called(); len(calls) != 0 {
			t.Errorf("Slack received %v, want nothing", calls)
		}
	})

	t.Run("a read goes through", func(t *testing.T) {
		f := newFakeSlack(t, replies)
		got := runAgainst(t, f, testConfig,
			"debug", "call", "conversations.history", "channel=C0456EFGH", "limit=1")

		if got.code != errdefs.ExitOK {
			t.Fatalf("exit = %d, want 0 (stderr: %s)", got.code, got.stderr)
		}
		if !slices.Contains(f.called(), "conversations.history") {
			t.Errorf("Slack received %v, want conversations.history", f.called())
		}
	})

	t.Run("allow_writes lets a write through", func(t *testing.T) {
		f := newFakeSlack(t, replies)
		got := runAgainst(t, f, writableConfig,
			"debug", "call", "chat.postMessage", "channel=C0456EFGH", "text=hi")

		if got.code != errdefs.ExitOK {
			t.Fatalf("exit = %d, want 0 (stderr: %s)", got.code, got.stderr)
		}
		if !slices.Contains(f.called(), "chat.postMessage") {
			t.Errorf("Slack received %v, want chat.postMessage", f.called())
		}
	})
}

// TestIsReadMethod pins the allowlist. The write cases matter most: each is a
// way to act as the user that a read-only install must never reach.
func TestIsReadMethod(t *testing.T) {
	reads := []string{
		"auth.test", "api.test", "client.counts", "client.boot",
		"users.conversations", "conversations.history", "conversations.replies",
		"conversations.info", "conversations.members", "users.info", "users.list",
		"users.prefs.get", "users.getPresence", "users.lookupByEmail",
		"chat.getPermalink", "bots.info", "team.info", "emoji.list",
		"search.messages", "search.modules",
	}
	writes := []string{
		"chat.postMessage", "chat.update", "chat.delete", "chat.meMessage",
		"reactions.add", "reactions.remove", "conversations.mark",
		"conversations.join", "conversations.leave", "conversations.open",
		"conversations.archive", "conversations.setTopic", "users.prefs.set",
		"users.profile.set", "users.setPresence", "files.upload", "pins.add",
		"auth.revoke", "rtm.connect", "postMessage", "",
	}

	for _, m := range reads {
		if !isReadMethod(m) {
			t.Errorf("isReadMethod(%q) = false, want true", m)
		}
	}
	for _, m := range writes {
		if isReadMethod(m) {
			t.Errorf("isReadMethod(%q) = true, want false", m)
		}
	}
}
