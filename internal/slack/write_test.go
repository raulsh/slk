package slack

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/raulsh/slk/internal/errdefs"
)

// formRecorder answers every request with body and keeps the last form and
// path it saw, so a test can assert on exactly what went over the wire.
type formRecorder struct {
	body string
	form url.Values
	path string
}

func (f *formRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.form = r.PostForm
	f.path = r.URL.Path
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(f.body))
}

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TestPostMessageRequestShape pins the post's form: the fields the web
// client's own composer sends that mean something to the server, and the
// telemetry reason it sends with them.
func TestPostMessageRequestShape(t *testing.T) {
	rec := &formRecorder{body: `{"ok":true,"channel":"C123ABCDE","ts":"1725806400.123456",
		"message":{"type":"message","user":"U1","text":"a &lt; b","ts":"1725806400.123456"}}`}
	c, _ := newTestClient(t, rec)

	m, err := c.PostMessage(context.Background(), PostOptions{Channel: "C123ABCDE", Text: "a &lt; b"})
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}

	if rec.path != "/api/chat.postMessage" {
		t.Errorf("path = %q, want /api/chat.postMessage", rec.path)
	}
	want := map[string]string{
		"channel":   "C123ABCDE",
		"text":      "a &lt; b",
		"_x_reason": "webapp_message_send",
	}
	for k, v := range want {
		if got := rec.form.Get(k); got != v {
			t.Errorf("form %s = %q, want %q", k, got, v)
		}
	}
	if rec.form.Has("thread_ts") {
		t.Errorf("form thread_ts = %q, want it absent for a top-level post", rec.form.Get("thread_ts"))
	}
	if id := rec.form.Get("client_msg_id"); !uuidV4.MatchString(id) {
		t.Errorf("client_msg_id = %q, want a random UUID like the web client's", id)
	}

	if m.Channel != "C123ABCDE" || m.TS != "1725806400.123456" {
		t.Errorf("message = %s/%s, want C123ABCDE/1725806400.123456", m.Channel, m.TS)
	}
	if !strings.HasSuffix(m.Permalink, "/archives/C123ABCDE/p1725806400123456") {
		t.Errorf("permalink = %q, want it to point at the new message", m.Permalink)
	}
}

// TestPostMessageReplyPermalinkCarriesTheThread matters for the read-and-reply
// loop: the permalink a reply reports has to open in its thread, and has to
// be the form `send --thread` and ParsePermalink read back.
func TestPostMessageReplyPermalinkCarriesTheThread(t *testing.T) {
	rec := &formRecorder{body: `{"ok":true,"channel":"C123ABCDE","ts":"1725806400.123456",
		"message":{"type":"message","user":"U1","text":"on it","ts":"1725806400.123456",
		"thread_ts":"1725806300.000100"}}`}
	c, _ := newTestClient(t, rec)

	m, err := c.PostMessage(context.Background(), PostOptions{
		Channel: "C123ABCDE", Text: "on it", ThreadTS: "1725806300.000100",
	})
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	if got := rec.form.Get("thread_ts"); got != "1725806300.000100" {
		t.Errorf("form thread_ts = %q, want the root", got)
	}
	if m.ThreadTS != "1725806300.000100" {
		t.Errorf("ThreadTS = %q, want the root", m.ThreadTS)
	}
	wantSuffix := "/archives/C123ABCDE/p1725806400123456?thread_ts=1725806300.000100&cid=C123ABCDE"
	if !strings.HasSuffix(m.Permalink, wantSuffix) {
		t.Errorf("permalink = %q, want suffix %q", m.Permalink, wantSuffix)
	}
}

// TestPostMessageWithoutTSIsAnError: success with nothing to point at would
// leave the caller unable to thread, react or even check what landed.
func TestPostMessageWithoutTSIsAnError(t *testing.T) {
	c, _ := newTestClient(t, jsonHandler(`{"ok":true}`))
	if _, err := c.PostMessage(context.Background(), PostOptions{Channel: "C123ABCDE", Text: "hi"}); err == nil {
		t.Fatal("PostMessage succeeded on a response with no ts")
	}
}

// TestThreadRoot pins the rule that puts a reply in the right thread whatever
// message of it the caller pointed at: a reply's ts must become its root's,
// because Slack threads under roots and not reliably under anything else.
func TestThreadRoot(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"root comes back first", `{"ok":true,"messages":[
			{"ts":"1725806300.000100","thread_ts":"1725806300.000100","reply_count":3}]}`, "1725806300.000100"},
		{"reply comes back alone", `{"ok":true,"messages":[
			{"ts":"1725806400.123456","thread_ts":"1725806300.000100"}]}`, "1725806300.000100"},
		{"unthreaded message is its own root", `{"ok":true,"messages":[
			{"ts":"1725806400.123456"}]}`, "1725806400.123456"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &formRecorder{body: tc.body}
			c, _ := newTestClient(t, rec)

			got, err := c.ThreadRoot(context.Background(), "C123ABCDE", "1725806400.123456")
			if err != nil {
				t.Fatalf("ThreadRoot: %v", err)
			}
			if got != tc.want {
				t.Errorf("ThreadRoot = %q, want %q", got, tc.want)
			}
			if rec.path != "/api/conversations.replies" || rec.form.Get("ts") != "1725806400.123456" ||
				rec.form.Get("limit") != "1" {
				t.Errorf("request = %s %v, want conversations.replies for that ts with limit 1", rec.path, rec.form)
			}
		})
	}
}

// TestThreadRootOfNothingIsNotFound: a made-up ts must stop the post before it
// is sent, as exit 5 - not reach chat.postMessage.
func TestThreadRootOfNothingIsNotFound(t *testing.T) {
	for _, body := range []string{
		`{"ok":false,"error":"thread_not_found"}`,
		`{"ok":true,"messages":[]}`,
	} {
		c, _ := newTestClient(t, jsonHandler(body))
		_, err := c.ThreadRoot(context.Background(), "C123ABCDE", "1725806400.123456")
		if !errors.Is(err, errdefs.ErrNotFound) {
			t.Errorf("ThreadRoot on %s: error = %v, want ErrNotFound", body, err)
		}
	}
}

func TestOpenDM(t *testing.T) {
	rec := &formRecorder{body: `{"ok":true,"channel":{"id":"D0123ABCD"}}`}
	c, _ := newTestClient(t, rec)

	id, err := c.OpenDM(context.Background(), "U0123ABCD")
	if err != nil {
		t.Fatalf("OpenDM: %v", err)
	}
	if id != "D0123ABCD" {
		t.Errorf("OpenDM = %q, want D0123ABCD", id)
	}
	if rec.path != "/api/conversations.open" || rec.form.Get("users") != "U0123ABCD" ||
		rec.form.Get("return_im") != "true" {
		t.Errorf("request = %s %v, want conversations.open for that user", rec.path, rec.form)
	}

	c, _ = newTestClient(t, jsonHandler(`{"ok":true,"channel":{}}`))
	if _, err := c.OpenDM(context.Background(), "U0123ABCD"); err == nil {
		t.Error("OpenDM succeeded on a response with no channel id")
	}
}
