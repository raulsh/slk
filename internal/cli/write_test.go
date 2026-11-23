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
	routes  map[string]func(form url.Values) string
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
			"auth.test":             `{"ok":true,"user_id":"U999ZZZZZ","team_id":"T0123ABCD"}`,
			"reactions.add":         `{"ok":true}`,
			"reactions.remove":      `{"ok":true}`,
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
	if fn, routed := f.routes[method]; routed {
		body, ok = fn(r.PostForm), true
	}
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

// reply replaces the canned answer for one method.
func (f *fakeSlack) reply(method, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[method] = body
}

// route answers one method from its request, for a test where the reply
// depends on what was asked.
func (f *fakeSlack) route(method string, fn func(form url.Values) string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.routes == nil {
		f.routes = map[string]func(url.Values) string{}
	}
	f.routes[method] = fn
}

// stall makes one method hang until the client gives up.
func (f *fakeSlack) stall(method string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stalls[method] = true
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
	f.stall("chat.postMessage")

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

const testPermalink = "https://acme.slack.com/archives/C111AAAAA/p1725806400123456"

// reactedEnvelope is the part of a react result the tests read.
type reactedEnvelope struct {
	OK      bool `json:"ok"`
	Results []struct {
		DryRun  bool   `json:"dry_run"`
		Action  string `json:"action"`
		Channel string `json:"channel"`
		TS      string `json:"ts"`
		Name    string `json:"name"`
		Changed bool   `json:"changed"`
	} `json:"results"`
}

func decodeReacted(t *testing.T, r result) reactedEnvelope {
	t.Helper()
	var env reactedEnvelope
	if err := json.Unmarshal([]byte(r.stdout), &env); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q, stderr %q)", err, r.stdout, r.stderr)
	}
	if len(env.Results) != 1 {
		t.Fatalf("results = %d, want 1 (stdout %q)", len(env.Results), r.stdout)
	}
	return env
}

// TestReactRefusesBeforeTheNetwork mirrors the send refusals: a bad
// invocation, writes switched off, or nobody to consent all stop the command
// before it asks Slack anything.
func TestReactRefusesBeforeTheNetwork(t *testing.T) {
	cases := []struct {
		name        string
		allowWrites bool
		args        []string
		want        int
	}{
		{"nothing", true, nil, errdefs.ExitUsage},
		{"permalink alone", true, []string{testPermalink}, errdefs.ExitUsage},
		{"channel and ts without emoji", true, []string{"general", "1725806400.123456"}, errdefs.ExitUsage},
		{"pasted emoji", true, []string{testPermalink, "👍"}, errdefs.ExitUsage},
		{"truncated ts", true, []string{"general", "1725806400", "eyes"}, errdefs.ExitUsage},
		{"user id as channel", true, []string{"U444DDDDD", "1725806400.123456", "eyes"}, errdefs.ExitUsage},
		{"permalink plus ts", true, []string{testPermalink, "1725806400.123456", "eyes"}, errdefs.ExitUsage},
		{"too many", true, []string{"general", "1725806400.123456", "eyes", "tada"}, errdefs.ExitUsage},
		{"writes disabled", false, []string{"--yes", testPermalink, "eyes"}, errdefs.ExitWriteRefused},
		{"no terminal and no --yes", true, []string{testPermalink, "eyes"}, errdefs.ExitWriteRefused},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := writeConfig(t, tc.allowWrites)
			f := newFakeSlack(t)
			args := append([]string{"react", "--config", cfg}, tc.args...)

			got := runAgainst(t, f, strings.NewReader(""), false, args...)
			if got.code != tc.want {
				t.Errorf("exit = %d, want %d (stderr: %s)", got.code, tc.want, got.stderr)
			}
			if n := f.total(); n != 0 {
				t.Errorf("made %d requests, want none before refusing", n)
			}
		})
	}
}

// TestReactAddsAndRemoves covers both directions and both target forms, with
// the emoji normalised to the bare name Slack wants.
func TestReactAddsAndRemoves(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		method string
		action string
	}{
		{"permalink", []string{testPermalink, ":Eyes:"}, "reactions.add", "add"},
		{"channel and ts", []string{"general", "p1725806400123456", "eyes"}, "reactions.add", "add"},
		{"remove", []string{"--remove", testPermalink, "eyes"}, "reactions.remove", "remove"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := writeConfig(t, true)
			f := newFakeSlack(t)
			args := append([]string{"react", "--config", cfg, "--json", "--yes"}, tc.args...)

			got := runAgainst(t, f, strings.NewReader(""), false, args...)
			if got.code != errdefs.ExitOK {
				t.Fatalf("exit = %d, want 0 (stderr: %s)", got.code, got.stderr)
			}
			form := f.last(t, tc.method)
			if form.Get("channel") != "C111AAAAA" || form.Get("timestamp") != "1725806400.123456" || form.Get("name") != "eyes" {
				t.Errorf("%s form = %v, want eyes on C111AAAAA 1725806400.123456", tc.method, form)
			}
			r := decodeReacted(t, got).Results[0]
			if r.Action != tc.action || !r.Changed || r.DryRun {
				t.Errorf("result = %+v, want a real %s that changed the message", r, tc.action)
			}
		})
	}
}

// TestReactIsIdempotent: after a timeout an agent will retry, and finding the
// reaction already there - or already gone - must read as success, not as a
// failure that sends it looking for what went wrong.
func TestReactIsIdempotent(t *testing.T) {
	cases := []struct {
		method string
		reply  string
		flags  []string
	}{
		{"reactions.add", `{"ok":false,"error":"already_reacted"}`, nil},
		{"reactions.remove", `{"ok":false,"error":"no_reaction"}`, []string{"--remove"}},
	}
	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			cfg := writeConfig(t, true)
			f := newFakeSlack(t)
			f.reply(tc.method, tc.reply)
			args := append([]string{"react", "--config", cfg, "--json", "--yes"}, tc.flags...)
			args = append(args, testPermalink, "eyes")

			got := runAgainst(t, f, strings.NewReader(""), false, args...)
			if got.code != errdefs.ExitOK {
				t.Fatalf("exit = %d, want 0 (stderr: %s)", got.code, got.stderr)
			}
			if r := decodeReacted(t, got).Results[0]; r.Changed {
				t.Errorf("changed = true, want false when the state already held")
			}
		})
	}
}

// TestReactDryRunChangesNothing: the rehearsal resolves the target and stops.
func TestReactDryRunChangesNothing(t *testing.T) {
	cfg := writeConfig(t, true)
	f := newFakeSlack(t)

	got := runAgainst(t, f, strings.NewReader(""), false, "react", "--config", cfg, "--json", "--dry-run", "general", "1725806400.123456", "eyes")
	if got.code != errdefs.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", got.code, got.stderr)
	}
	if n := f.count("reactions.add") + f.count("reactions.remove"); n != 0 {
		t.Errorf("made %d reaction writes under --dry-run", n)
	}
	if r := decodeReacted(t, got).Results[0]; !r.DryRun || r.Channel != "C111AAAAA" {
		t.Errorf("result = %+v, want a rehearsal aimed at C111AAAAA", r)
	}
}

// TestIsReadMethod pins which methods debug call runs without consent: every
// read slk itself makes, and nothing that writes.
func TestIsReadMethod(t *testing.T) {
	cases := map[string]bool{
		"auth.test":                    true,
		"client.counts":                true,
		"client.userBoot":              true,
		"conversations.history":        true,
		"conversations.replies":        true,
		"users.conversations":          true,
		"users.prefs.get":              true,
		"users.info":                   true,
		"bots.info":                    true,
		"search.messages":              true,
		"subscriptions.thread.getView": true,
		"chat.postMessage":             false,
		"chat.delete":                  false,
		"conversations.mark":           false,
		"conversations.open":           false,
		"reactions.add":                false,
		"notamethod":                   false,
	}
	for method, want := range cases {
		if got := isReadMethod(method); got != want {
			t.Errorf("isReadMethod(%q) = %v, want %v", method, got, want)
		}
	}
}

// TestDebugCallIsGatedForWrites closes the side door: without the gate, a
// caller refused by send could make the very same post through debug call.
func TestDebugCallIsGatedForWrites(t *testing.T) {
	post := []string{"debug", "call", "chat.postMessage", "channel=C111AAAAA", "text=hi"}
	cases := []struct {
		name        string
		allowWrites bool
		flags       []string
		want        int
		posts       int
	}{
		{"writes disabled", false, []string{"--yes"}, errdefs.ExitWriteRefused, 0},
		{"no terminal and no --yes", true, nil, errdefs.ExitWriteRefused, 0},
		{"dry run", true, []string{"--dry-run"}, errdefs.ExitOK, 0},
		{"consented", true, []string{"--yes"}, errdefs.ExitOK, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := writeConfig(t, tc.allowWrites)
			f := newFakeSlack(t)
			args := append(append([]string{}, post...), "--config", cfg)
			args = append(args, tc.flags...)

			got := runAgainst(t, f, strings.NewReader(""), false, args...)
			if got.code != tc.want {
				t.Errorf("exit = %d, want %d (stderr: %s)", got.code, tc.want, got.stderr)
			}
			if n := f.count("chat.postMessage"); n != tc.posts {
				t.Errorf("chat.postMessage called %d times, want %d", n, tc.posts)
			}
			if tc.posts == 0 && f.total() != 0 {
				t.Errorf("made %d requests, want none when not calling the method", f.total())
			}
		})
	}

	t.Run("reads stay ungated", func(t *testing.T) {
		cfg := writeConfig(t, false)
		f := newFakeSlack(t)
		f.reply("conversations.history", `{"ok":true,"messages":[]}`)

		got := runAgainst(t, f, strings.NewReader(""), false,
			"debug", "call", "conversations.history", "channel=C111AAAAA", "--config", cfg)
		if got.code != errdefs.ExitOK {
			t.Errorf("exit = %d, want 0 for a read with writes off (stderr: %s)", got.code, got.stderr)
		}
		if n := f.count("conversations.history"); n != 1 {
			t.Errorf("conversations.history called %d times, want 1", n)
		}
	})
}
