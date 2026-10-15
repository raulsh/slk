package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/raulsh/slk/internal/slack"
)

// testConfig is a read-only profile, as shipped.
const testConfig = `default_profile: acme
profiles:
  acme:
    workspace: acme
    token: xoxc-1-2-3-abcdef0123456789
    cookie: xoxd-AbC%2Fd%3D
`

// reply produces a canned response body from the request's form values.
type reply func(form url.Values) string

// static answers every request with the same body.
func static(body string) reply { return func(url.Values) string { return body } }

// fakeSlack stands in for the Slack web API, so a test can run a whole command
// and assert both what it printed and what it sent.
type fakeSlack struct {
	srv     *httptest.Server
	replies map[string]reply

	mu    sync.Mutex
	calls []string
}

func newFakeSlack(t *testing.T, replies map[string]reply) *fakeSlack {
	t.Helper()
	f := &fakeSlack{replies: replies}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSlack) serve(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	method := strings.TrimPrefix(r.URL.Path, "/api/")

	f.mu.Lock()
	f.calls = append(f.calls, method)
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	body := `{"ok":false,"error":"unknown_method"}`
	if rep, ok := f.replies[method]; ok {
		body = rep(r.Form)
	}
	_, _ = w.Write([]byte(body))
}

// called reports every method the fake has received, in order.
func (f *fakeSlack) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// runAgainst executes a command with its Slack client pointed at f and its
// configuration read from cfg.
func runAgainst(t *testing.T, f *fakeSlack, cfg string, args ...string) result {
	t.Helper()
	isolate(t)

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var stdout, stderr strings.Builder
	code := Execute(context.Background(), Env{
		Args:         append([]string{"--config", path}, args...),
		Stdout:       &stdout,
		Stderr:       &stderr,
		Stdin:        strings.NewReader(""),
		IsTTY:        func(*os.File) bool { return false },
		Version:      "1.2.3",
		Commit:       "abc1234",
		slackOptions: []slack.Option{slack.WithBaseURL(f.srv.URL + "/api/")},
	})
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// isolate keeps a test away from the developer's own slk setup: credentials
// exported in their shell, and the name cache in their home directory.
func isolate(t *testing.T) {
	t.Helper()
	for _, v := range []string{
		"SLK_CONFIG", "SLK_PROFILE", "SLK_WORKSPACE", "SLK_TOKEN",
		"SLK_COOKIE", "SLK_COOKIE_DS", "SLK_ENTERPRISE_ID", "SLK_ALLOW_WRITES",
	} {
		t.Setenv(v, "")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
}
