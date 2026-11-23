package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/raulsh/slk/internal/errdefs"
	"github.com/raulsh/slk/internal/slack"
)

// fakeSlack stands in for the web API. It answers each method with a canned
// body and records every request, so a test can assert what was sent - and,
// for the refusals that matter most, that nothing was.
//
// The mutex is not decoration: the name lookups run requests in parallel, and
// CI runs the tests under -race.
type fakeSlack struct {
	url string

	mu      sync.Mutex
	calls   []fakeCall
	replies map[string]string
	stalls  map[string]bool
}

type fakeCall struct {
	method string
	form   url.Values
}

func newFakeSlack(t *testing.T) *fakeSlack {
	t.Helper()
	f := &fakeSlack{
		stalls: map[string]bool{},
		replies: map[string]string{
			"users.conversations": `{"ok":true,"channels":[
				{"id":"C111AAAAA","name":"general","is_channel":true},
				{"id":"C222BBBBB","name":"eng-incidents","is_channel":true},
				{"id":"D333CCCCC","is_im":true,"user":"U444DDDDD"}]}`,
			"users.info":            `{"ok":true,"user":{"id":"U444DDDDD","name":"alice"}}`,
			"conversations.replies": `{"ok":true,"messages":[{"ts":"1725806300.000100","thread_ts":"1725806300.000100","reply_count":2}]}`,
			"conversations.open":    `{"ok":true,"channel":{"id":"D999ZZZZZ"}}`,
			"chat.postMessage": `{"ok":true,"channel":"C111AAAAA","ts":"1725806400.123456",
				"message":{"type":"message","user":"U999ZZZZZ","ts":"1725806400.123456"}}`,
		},
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	f.url = srv.URL + "/api/"
	return f
}

func (f *fakeSlack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	method := strings.TrimPrefix(r.URL.Path, "/api/")

	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{method: method, form: r.PostForm})
	body, ok := f.replies[method]
	stall := f.stalls[method]
	f.mu.Unlock()

	if stall {
		// Hold the request until the client gives up, which is what a post
		// that reached Slack and never got an answer looks like.
		<-r.Context().Done()
		return
	}
	if !ok {
		body = `{"ok":false,"error":"unknown_method"}`
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func (f *fakeSlack) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.method == method {
			n++
		}
	}
	return n
}

func (f *fakeSlack) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// last returns the form of the most recent request for method.
func (f *fakeSlack) last(t *testing.T, method string) url.Values {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].method == method {
			return f.calls[i].form
		}
	}
	t.Fatalf("no %s request was made", method)
	return nil
}

// writeConfig isolates a test from the machine it runs on: its own config
// file, its own name cache, and none of the SLK_* variables - a developer's
// exported SLK_ALLOW_WRITES=1 must not be able to change a result.
func writeConfig(t *testing.T, allowWrites bool) string {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "SLK_") {
			t.Setenv(k, "")
		}
	}
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	t.Setenv("HOME", dir)

	path := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf(`allow_writes: %t
profiles:
  acme:
    workspace: acme
    token: xoxc-1-2-3-abcdef0123456789
    cookie: xoxd-AbC%%2Fd%%3D
`, allowWrites)
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// runAgainst executes the command tree against the fake, with stdin and the
// terminal check injected the way an agent's environment would supply them.
func runAgainst(t *testing.T, f *fakeSlack, stdin io.Reader, isTTY bool, args ...string) result {
	t.Helper()
	var stdout, stderr strings.Builder
	code := Execute(context.Background(), Env{
		Args:         args,
		Stdout:       &stdout,
		Stderr:       &stderr,
		Stdin:        stdin,
		IsTTY:        func(*os.File) bool { return isTTY },
		SlackOptions: []slack.Option{slack.WithBaseURL(f.url)},
		Version:      "1.2.3",
		Commit:       "abc1234",
	})
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// sentEnvelope is the part of a send result the tests read.
type sentEnvelope struct {
	OK      bool     `json:"ok"`
	Notes   []string `json:"notes"`
	Results []struct {
		DryRun      bool   `json:"dry_run"`
		Channel     string `json:"channel"`
		ChannelName string `json:"channel_name"`
		TS          string `json:"ts"`
		ThreadTS    string `json:"thread_ts"`
		Text        string `json:"text"`
	} `json:"results"`
	Error struct {
		Code string `json:"code"`
		Hint string `json:"hint"`
	} `json:"error"`
}

func decodeSent(t *testing.T, r result) sentEnvelope {
	t.Helper()
	var env sentEnvelope
	if err := json.Unmarshal([]byte(r.stdout), &env); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q, stderr %q)", err, r.stdout, r.stderr)
	}
	return env
}

// TestSendRefusesBeforeTheNetwork covers every way a send can be refused
// without asking Slack anything. Zero requests is part of the contract: a
// refused agent must cost nothing, and a refusal that had already resolved
// names or opened a DM would not be a refusal.
func TestSendRefusesBeforeTheNetwork(t *testing.T) {
	cases := []struct {
		name        string
		allowWrites bool
		stdin       string
		args        []string
		want        int
	}{
		{"no text", true, "", []string{"--channel", "general"}, errdefs.ExitUsage},
		{"no destination", true, "", []string{"hi"}, errdefs.ExitUsage},
		{"two destinations", true, "", []string{"--channel", "general", "--dm", "@alice", "hi"}, errdefs.ExitUsage},
		{"unquoted text", true, "", []string{"--channel", "general", "hello", "world"}, errdefs.ExitUsage},
		{"blank text", true, "", []string{"--channel", "general", "   "}, errdefs.ExitUsage},
		{"user id as channel", true, "", []string{"--channel", "U444DDDDD", "hi"}, errdefs.ExitUsage},
		{"permalink as channel", true, "",
			[]string{"--channel", "https://acme.slack.com/archives/C111AAAAA/p1725806400123456", "hi"}, errdefs.ExitUsage},
		{"permalink thread plus channel", true, "",
			[]string{"--thread", "https://acme.slack.com/archives/C111AAAAA/p1725806400123456", "--channel", "general", "hi"},
			errdefs.ExitUsage},
		{"truncated thread ts", true, "", []string{"--channel", "general", "--thread", "1725806400", "hi"}, errdefs.ExitUsage},
		{"broadcast flag without markup", true, "", []string{"--channel", "general", "--allow-broadcast", "hi"}, errdefs.ExitUsage},
		{"writes disabled", false, "", []string{"--channel", "general", "--yes", "hi"}, errdefs.ExitWriteRefused},
		{"writes disabled, dry run", false, "", []string{"--channel", "general", "--dry-run", "hi"}, errdefs.ExitWriteRefused},
		{"no terminal and no --yes", true, "", []string{"--channel", "general", "hi"}, errdefs.ExitWriteRefused},
		{"body on stdin and no --yes", true, "hi\n", []string{"--channel", "general", "-"}, errdefs.ExitWriteRefused},
		{"broadcast mention", true, "", []string{"--channel", "general", "--markup", "--yes", "<!channel> hi"}, errdefs.ExitWriteRefused},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := writeConfig(t, tc.allowWrites)
			f := newFakeSlack(t)
			args := append([]string{"send", "--config", cfg}, tc.args...)

			got := runAgainst(t, f, strings.NewReader(tc.stdin), false, args...)
			if got.code != tc.want {
				t.Errorf("exit = %d, want %d (stderr: %s)", got.code, tc.want, got.stderr)
			}
			if n := f.total(); n != 0 {
				t.Errorf("made %d requests, want none before refusing", n)
			}
		})
	}
}

// TestSendRefusalIsParseable: an agent that is refused asked for JSON, so it
// must get JSON saying why - with a code it can branch on.
func TestSendRefusalIsParseable(t *testing.T) {
	cfg := writeConfig(t, false)
	f := newFakeSlack(t)

	got := runAgainst(t, f, strings.NewReader(""), false, "send", "--config", cfg, "--json", "--channel", "general", "--yes", "hi")
	env := decodeSent(t, got)
	if env.OK || env.Error.Code != string(errdefs.CodeWriteRefused) {
		t.Errorf("envelope = ok %v, code %q; want a write_refused failure", env.OK, env.Error.Code)
	}
	if !strings.Contains(env.Error.Hint, "allow_writes: true") {
		t.Errorf("hint = %q, want it to say how to turn writes on", env.Error.Hint)
	}
}

// TestSendDryRunSendsNothing: a rehearsal resolves everything a real run
// would, and stops short of the write.
func TestSendDryRunSendsNothing(t *testing.T) {
	cfg := writeConfig(t, true)
	f := newFakeSlack(t)

	got := runAgainst(t, f, strings.NewReader(""), false, "send", "--config", cfg, "--json", "--dry-run", "--channel", "general", "a < b")
	if got.code != errdefs.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", got.code, got.stderr)
	}
	if n := f.count("chat.postMessage"); n != 0 {
		t.Errorf("chat.postMessage called %d times under --dry-run", n)
	}
	env := decodeSent(t, got)
	if len(env.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(env.Results))
	}
	r := env.Results[0]
	if !r.DryRun || r.TS != "" || r.Channel != "C111AAAAA" || r.ChannelName != "#general" {
		t.Errorf("result = %+v, want a rehearsal aimed at #general with no ts", r)
	}
	if r.Text != "a &lt; b" {
		t.Errorf("text = %q, want the escaped text that would be sent", r.Text)
	}
}

// TestSendTextIsLiteralUnlessMarkup pins the default that keeps model output
// intact, and the flag that lets deliberate markup through untouched.
func TestSendTextIsLiteralUnlessMarkup(t *testing.T) {
	cases := []struct {
		name  string
		flags []string
		in    string
		want  string
	}{
		{"literal by default", nil, "if a < b && c > d", "if a &lt; b &amp;&amp; c &gt; d"},
		{"markup untouched", []string{"--markup"}, "<@U444DDDDD> can you look?", "<@U444DDDDD> can you look?"},
		{"broadcast allowed", []string{"--markup", "--allow-broadcast"}, "<!here> deploy", "<!here> deploy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := writeConfig(t, true)
			f := newFakeSlack(t)
			args := append([]string{"send", "--config", cfg, "--yes", "--channel", "general"}, tc.flags...)
			args = append(args, tc.in)

			got := runAgainst(t, f, strings.NewReader(""), false, args...)
			if got.code != errdefs.ExitOK {
				t.Fatalf("exit = %d, want 0 (stderr: %s)", got.code, got.stderr)
			}
			form := f.last(t, "chat.postMessage")
			if form.Get("text") != tc.want || form.Get("channel") != "C111AAAAA" {
				t.Errorf("posted %q to %s, want %q to C111AAAAA", form.Get("text"), form.Get("channel"), tc.want)
			}
		})
	}
}

// TestSendReadsTheBodyFromStdin: "-" is the safe way to pass text that starts
// with a dash or spans lines, and the pipe's trailing newline is not part of it.
func TestSendReadsTheBodyFromStdin(t *testing.T) {
	cfg := writeConfig(t, true)
	f := newFakeSlack(t)

	got := runAgainst(t, f, strings.NewReader("- one\n- two\n"), false, "send", "--config", cfg, "--yes", "--channel", "general", "-")
	if got.code != errdefs.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", got.code, got.stderr)
	}
	if text := f.last(t, "chat.postMessage").Get("text"); text != "- one\n- two" {
		t.Errorf("text = %q, want the body without its trailing newline", text)
	}
}

// TestSendThreadReplyGoesUnderTheRoot is the read-and-reply loop: whatever
// message of a thread the caller holds - the root or a reply, as a ts or a
// permalink - the reply is posted under the thread's root.
func TestSendThreadReplyGoesUnderTheRoot(t *testing.T) {
	cases := [][]string{
		{"--channel", "general", "--thread", "1725806400.999999"},
		{"--thread", "https://acme.slack.com/archives/C111AAAAA/p1725806400999999"},
		{"--thread", "https://acme.slack.com/archives/C111AAAAA/p1725806400999999?thread_ts=1725806300.000100&cid=C111AAAAA"},
	}
	for _, target := range cases {
		t.Run(strings.Join(target, " "), func(t *testing.T) {
			cfg := writeConfig(t, true)
			f := newFakeSlack(t)
			args := append([]string{"send", "--config", cfg, "--json", "--yes"}, target...)
			args = append(args, "on it")

			got := runAgainst(t, f, strings.NewReader(""), false, args...)
			if got.code != errdefs.ExitOK {
				t.Fatalf("exit = %d, want 0 (stderr: %s)", got.code, got.stderr)
			}
			if ts := f.last(t, "conversations.replies").Get("ts"); ts != "1725806400.999999" {
				t.Errorf("looked up %q, want the message the caller named", ts)
			}
			post := f.last(t, "chat.postMessage")
			if post.Get("channel") != "C111AAAAA" || post.Get("thread_ts") != "1725806300.000100" {
				t.Errorf("posted to %s under %q, want C111AAAAA under the root 1725806300.000100",
					post.Get("channel"), post.Get("thread_ts"))
			}
			if env := decodeSent(t, got); len(env.Results) != 1 || env.Results[0].ThreadTS != "1725806300.000100" {
				t.Errorf("results = %+v, want the root reported as thread_ts", env.Results)
			}
		})
	}
}

// TestSendNeverGuessesAChannel: "incidents" matches #eng-incidents alone, which
// a read takes and a write must not.
func TestSendNeverGuessesAChannel(t *testing.T) {
	cfg := writeConfig(t, true)
	f := newFakeSlack(t)

	got := runAgainst(t, f, strings.NewReader(""), false, "send", "--config", cfg, "--yes", "--channel", "incidents", "hi")
	if got.code != errdefs.ExitNotFound {
		t.Errorf("exit = %d, want %d (stderr: %s)", got.code, errdefs.ExitNotFound, got.stderr)
	}
	if !strings.Contains(got.stderr, "#eng-incidents") {
		t.Errorf("stderr = %q, want the near miss suggested", got.stderr)
	}
	if n := f.count("chat.postMessage"); n != 0 {
		t.Errorf("chat.postMessage called %d times for a guessed channel", n)
	}
}

// TestSendDM covers both DM paths: an existing DM found by handle, after
// learning the names of DM partners, and a new one opened only for real.
func TestSendDM(t *testing.T) {
	t.Run("existing DM by handle", func(t *testing.T) {
		cfg := writeConfig(t, true)
		f := newFakeSlack(t)

		got := runAgainst(t, f, strings.NewReader(""), false, "send", "--config", cfg, "--yes", "--dm", "@alice", "hi")
		if got.code != errdefs.ExitOK {
			t.Fatalf("exit = %d, want 0 (stderr: %s)", got.code, got.stderr)
		}
		if ch := f.last(t, "chat.postMessage").Get("channel"); ch != "D333CCCCC" {
			t.Errorf("posted to %s, want the existing DM D333CCCCC", ch)
		}
		if n := f.count("conversations.open"); n != 0 {
			t.Errorf("conversations.open called %d times with a DM already there", n)
		}
	})

	t.Run("new DM, rehearsed", func(t *testing.T) {
		cfg := writeConfig(t, true)
		f := newFakeSlack(t)

		got := runAgainst(t, f, strings.NewReader(""), false, "send", "--config", cfg, "--json", "--dry-run", "--dm", "U555EEEEE", "hi")
		if got.code != errdefs.ExitOK {
			t.Fatalf("exit = %d, want 0 (stderr: %s)", got.code, got.stderr)
		}
		if n := f.count("conversations.open") + f.count("chat.postMessage"); n != 0 {
			t.Errorf("made %d writes under --dry-run, want none - opening a DM is a write", n)
		}
		env := decodeSent(t, got)
		if !strings.Contains(strings.Join(env.Notes, "\n"), "would open one") {
			t.Errorf("notes = %q, want the rehearsal to say a DM would be opened", env.Notes)
		}
	})

	t.Run("new DM, for real", func(t *testing.T) {
		cfg := writeConfig(t, true)
		f := newFakeSlack(t)

		got := runAgainst(t, f, strings.NewReader(""), false, "send", "--config", cfg, "--yes", "--dm", "U555EEEEE", "hi")
		if got.code != errdefs.ExitOK {
			t.Fatalf("exit = %d, want 0 (stderr: %s)", got.code, got.stderr)
		}
		if u := f.last(t, "conversations.open").Get("users"); u != "U555EEEEE" {
			t.Errorf("opened a DM with %q, want U555EEEEE", u)
		}
		if ch := f.last(t, "chat.postMessage").Get("channel"); ch != "D999ZZZZZ" {
			t.Errorf("posted to %s, want the DM just opened", ch)
		}
	})
}

// TestSendTimeoutSaysTheMessageMayHaveLanded: after a post times out, the
// usual advice - retry - is the one way to post twice, so the failure must
// say to check first. And it must have been tried exactly once.
func TestSendTimeoutSaysTheMessageMayHaveLanded(t *testing.T) {
	cfg := writeConfig(t, true)
	f := newFakeSlack(t)
	f.stalls["chat.postMessage"] = true

	got := runAgainst(t, f, strings.NewReader(""), false,
		"send", "--config", cfg, "--json", "--yes", "--timeout", "1s", "--channel", "general", "hi")
	if got.code != errdefs.ExitNetwork {
		t.Fatalf("exit = %d, want %d (stderr: %s)", got.code, errdefs.ExitNetwork, got.stderr)
	}
	if n := f.count("chat.postMessage"); n != 1 {
		t.Errorf("chat.postMessage called %d times, want exactly 1 - a write is never retried", n)
	}
	if env := decodeSent(t, got); !strings.Contains(env.Error.Hint, "may have happened") {
		t.Errorf("hint = %q, want it to say the post may have landed", env.Error.Hint)
	}
}

// TestSendConfirmsAtATerminal covers the interactive branch: with a person at
// the terminal and no --yes, the answer decides, and anything but yes is no.
func TestSendConfirmsAtATerminal(t *testing.T) {
	cases := []struct {
		answer string
		want   int
		posts  int
	}{
		{"n\n", errdefs.ExitWriteRefused, 0},
		{"\n", errdefs.ExitWriteRefused, 0},
		{"y\n", errdefs.ExitOK, 1},
	}
	for _, tc := range cases {
		t.Run(strings.TrimSpace(tc.answer), func(t *testing.T) {
			cfg := writeConfig(t, true)
			f := newFakeSlack(t)

			r, w, err := os.Pipe()
			if err != nil {
				t.Fatalf("pipe: %v", err)
			}
			t.Cleanup(func() { _ = r.Close() })
			if _, err := w.WriteString(tc.answer); err != nil {
				t.Fatalf("write answer: %v", err)
			}
			_ = w.Close()

			got := runAgainst(t, f, r, true, "send", "--config", cfg, "--channel", "general", "hi")
			if got.code != tc.want {
				t.Errorf("exit = %d, want %d (stderr: %s)", got.code, tc.want, got.stderr)
			}
			if n := f.count("chat.postMessage"); n != tc.posts {
				t.Errorf("chat.postMessage called %d times, want %d", n, tc.posts)
			}
			if !strings.Contains(got.stderr, "Send to #general?") {
				t.Errorf("stderr = %q, want the question naming the channel", got.stderr)
			}
		})
	}
}
