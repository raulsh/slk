package cli

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/raulsh/slk/internal/errdefs"
)

// An unread thread in #general: read up to 000300, with replies at 000400 and
// 000500 since - and a thread read to the end after it, as the view orders them.
const unreadThreadsView = `{"ok":true,"has_more":true,"total_unread_replies":2,"max_ts":"9999999999.000000","threads":[
	{"root_msg":{"type":"message","user":"U444DDDDD","text":"root","ts":"1725806300.000100","thread_ts":"1725806300.000100",
		"reply_count":4,"latest_reply":"1725806300.000500","last_read":"1725806300.000300","subscribed":true,"channel":"C111AAAAA"},
	 "latest_replies":[
		{"type":"message","user":"U444DDDDD","text":"four","ts":"1725806300.000400","thread_ts":"1725806300.000100"},
		{"type":"message","user":"U444DDDDD","text":"five","ts":"1725806300.000500","thread_ts":"1725806300.000100"}]},
	{"root_msg":{"type":"message","user":"U444DDDDD","text":"old","ts":"1725800000.000100","thread_ts":"1725800000.000100",
		"reply_count":1,"latest_reply":"1725800000.000200","last_read":"1725800000.000200","subscribed":true,"channel":"C111AAAAA"},
	 "latest_replies":[{"type":"message","user":"U444DDDDD","text":"done","ts":"1725800000.000200","thread_ts":"1725800000.000100"}]}]}`

// The whole thread from the read marker on, as conversations.replies returns
// it: root first, then the last read reply, which the inclusive bound brings.
const unreadThreadReplies = `{"ok":true,"messages":[
	{"type":"message","user":"U444DDDDD","text":"root","ts":"1725806300.000100","thread_ts":"1725806300.000100","reply_count":4},
	{"type":"message","user":"U444DDDDD","text":"three","ts":"1725806300.000300","thread_ts":"1725806300.000100"},
	{"type":"message","user":"U444DDDDD","text":"four","ts":"1725806300.000400","thread_ts":"1725806300.000100"},
	{"type":"message","user":"U444DDDDD","text":"five","ts":"1725806300.000500","thread_ts":"1725806300.000100"}]}`

type threadsEnvelope struct {
	OK      bool     `json:"ok"`
	Count   int      `json:"count"`
	Notes   []string `json:"notes"`
	Results []struct {
		Channel       string `json:"channel"`
		ChannelName   string `json:"channel_name"`
		TS            string `json:"ts"`
		ThreadTS      string `json:"thread_ts"`
		Unread        bool   `json:"unread"`
		HasNewReplies bool   `json:"has_new_replies"`
		Permalink     string `json:"permalink"`
	} `json:"results"`
}

func decodeThreads(t *testing.T, r result) threadsEnvelope {
	t.Helper()
	if r.code != errdefs.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", r.code, r.stderr)
	}
	var env threadsEnvelope
	if err := json.Unmarshal([]byte(r.stdout), &env); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q)", err, r.stdout)
	}
	return env
}

// TestThreadsUnreadReadsEveryNewReply is the read half of the read-and-reply
// loop: every reply past the read marker, from a thread of any age, each with
// a permalink that send --thread takes back.
func TestThreadsUnreadReadsEveryNewReply(t *testing.T) {
	cfg := writeConfig(t, false)
	f := newFakeSlack(t)
	f.reply("subscriptions.thread.getView", unreadThreadsView)
	f.reply("conversations.replies", unreadThreadReplies)

	env := decodeThreads(t, runAgainst(t, f, strings.NewReader(""), false, "threads", "--config", cfg, "--unread", "--json"))

	if n := f.count("subscriptions.thread.getView"); n != 1 {
		t.Errorf("read %d pages of the view, want 1: the first read thread ends the unread ones", n)
	}
	q := f.last(t, "conversations.replies")
	if q.Get("channel") != "C111AAAAA" || q.Get("ts") != "1725806300.000100" || q.Get("oldest") != "1725806300.000300" {
		t.Errorf("replies request = %v, want the thread from its read marker", q)
	}

	if len(env.Results) != 3 {
		t.Fatalf("results = %d, want the root and the two unread replies", len(env.Results))
	}
	root, four, five := env.Results[0], env.Results[1], env.Results[2]
	if root.TS != "1725806300.000100" || !root.HasNewReplies || root.Unread || root.ChannelName != "#general" {
		t.Errorf("root = %+v, want the thread's first message, marked as having new replies", root)
	}
	if four.TS != "1725806300.000400" || five.TS != "1725806300.000500" || !four.Unread || !five.Unread {
		t.Errorf("replies = %+v, %+v; want 000400 and 000500, both unread", four, five)
	}
	if !strings.Contains(five.Permalink, "?thread_ts=1725806300.000100&cid=C111AAAAA") {
		t.Errorf("reply permalink = %q, want one that opens in the thread", five.Permalink)
	}
}

// TestThreadsUnreadWithNothingUnreadIsOneRequest: the common case for a
// polling agent must be cheap.
func TestThreadsUnreadWithNothingUnreadIsOneRequest(t *testing.T) {
	cfg := writeConfig(t, false)
	f := newFakeSlack(t)
	f.reply("subscriptions.thread.getView", `{"ok":true,"has_more":true,"total_unread_replies":0,"threads":[]}`)

	env := decodeThreads(t, runAgainst(t, f, strings.NewReader(""), false, "threads", "--config", cfg, "--unread", "--json"))
	if env.Count != 0 || len(env.Results) != 0 {
		t.Errorf("results = %d, want none", len(env.Results))
	}
	if !strings.Contains(strings.Join(env.Notes, "\n"), "no unread replies") {
		t.Errorf("notes = %q, want it to say nothing is unread", env.Notes)
	}
	if f.count("subscriptions.thread.getView") != 1 || f.count("conversations.replies") != 0 {
		t.Errorf("made %d view and %d replies requests, want 1 and 0",
			f.count("subscriptions.thread.getView"), f.count("conversations.replies"))
	}
}

// TestThreadsListsFromTheViewAlone: without --unread, the replies the view
// already carries are the listing, so it costs no request per thread.
func TestThreadsListsFromTheViewAlone(t *testing.T) {
	cfg := writeConfig(t, false)
	f := newFakeSlack(t)
	f.reply("subscriptions.thread.getView", strings.Replace(unreadThreadsView, `"has_more":true`, `"has_more":false`, 1))

	env := decodeThreads(t, runAgainst(t, f, strings.NewReader(""), false, "threads", "--config", cfg, "--json"))
	if len(env.Results) != 5 {
		t.Errorf("results = %d, want both roots and their three replies", len(env.Results))
	}
	if n := f.count("conversations.replies"); n != 0 {
		t.Errorf("conversations.replies called %d times, want none for a listing", n)
	}
	if env.Results[0].TS != "1725806300.000100" || env.Results[3].TS != "1725800000.000100" {
		t.Errorf("order = %s ... %s, want the view's order, each thread root first",
			env.Results[0].TS, env.Results[3].TS)
	}
}

// TestThreadFromAReplyShowsTheWholeThread: asked for a reply's ts, Slack
// answers with that reply alone. The permalinks `slk threads` hands out are
// mostly replies', so `slk thread` must still show the thread they belong to.
func TestThreadFromAReplyShowsTheWholeThread(t *testing.T) {
	const reply = `{"ok":true,"messages":[
		{"type":"message","user":"U444DDDDD","text":"five","ts":"1725806300.000500","thread_ts":"1725806300.000100"}]}`

	cases := []struct {
		name     string
		args     []string
		requests int
	}{
		{"reply permalink", []string{"https://acme.slack.com/archives/C111AAAAA/p1725806300000500"}, 2},
		{"reply permalink naming its thread",
			[]string{"https://acme.slack.com/archives/C111AAAAA/p1725806300000500?thread_ts=1725806300.000100&cid=C111AAAAA"}, 1},
		{"channel and reply ts", []string{"general", "1725806300.000500"}, 2},
		{"channel and root ts", []string{"general", "1725806300.000100"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := writeConfig(t, false)
			f := newFakeSlack(t)
			f.route("conversations.replies", func(form url.Values) string {
				if form.Get("ts") == "1725806300.000100" {
					return unreadThreadReplies
				}
				return reply
			})
			args := append([]string{"thread", "--config", cfg, "--json"}, tc.args...)

			env := decodeThreads(t, runAgainst(t, f, strings.NewReader(""), false, args...))
			if len(env.Results) != 4 || env.Results[0].TS != "1725806300.000100" {
				t.Errorf("results = %+v, want the whole thread, root first", env.Results)
			}
			if n := f.count("conversations.replies"); n != tc.requests {
				t.Errorf("made %d replies requests, want %d", n, tc.requests)
			}
		})
	}
}
