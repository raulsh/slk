package slack

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// threadsView serves the threads view one page per cursor and records the
// cursor of every request, so a test can check how the walk pages - which is
// exactly what prior accounts of this endpoint got wrong.
type threadsView struct {
	mu      sync.Mutex
	pages   map[string]string // current_ts, "" for the first page -> body
	cursors []string
	forms   []url.Values
}

func (v *threadsView) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	cur := r.PostForm.Get("current_ts")

	v.mu.Lock()
	v.cursors = append(v.cursors, cur)
	v.forms = append(v.forms, r.PostForm)
	body, ok := v.pages[cur]
	v.mu.Unlock()

	if !ok {
		body = `{"ok":false,"error":"unexpected_cursor"}`
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

// viewThread renders one entry the way the view does: the complete root, and
// the newest replies.
func viewThread(channel, root, lastRead, latestReply string, replies ...string) string {
	rs := make([]string, 0, len(replies))
	for _, ts := range replies {
		rs = append(rs, fmt.Sprintf(`{"type":"message","user":"U2","text":"reply","ts":%q,"thread_ts":%q}`, ts, root))
	}
	return fmt.Sprintf(`{"root_msg":{"type":"message","user":"U1","text":"root","ts":%q,"thread_ts":%q,
		"reply_count":%d,"latest_reply":%q,"last_read":%q,"subscribed":true,"channel":%q},
		"latest_replies":[%s]}`,
		root, root, len(replies), latestReply, lastRead, channel, strings.Join(rs, ","))
}

// viewPage renders a page. max_ts is newer than every thread, as observed:
// a walk that sends it back gets the first page again.
func viewPage(hasMore bool, totalUnread int, threads ...string) string {
	return fmt.Sprintf(`{"ok":true,"has_more":%t,"total_unread_replies":%d,"new_threads_count":0,
		"max_ts":"9999999999.000000","threads":[%s]}`, hasMore, totalUnread, strings.Join(threads, ","))
}

var (
	readA   = viewThread("C111AAAAA", "1725806000.000100", "1725806500.000200", "1725806500.000200", "1725806400.000100", "1725806500.000200")
	readB   = viewThread("C222BBBBB", "1725805000.000100", "1725805400.000200", "1725805400.000200", "1725805400.000200")
	unreadC = viewThread("C333CCCCC", "1725804000.000100", "1725804100.000100", "1725804300.000300", "1725804200.000200", "1725804300.000300")
)

// TestFollowedThreadsPagesByLastLatestReply pins the cursor: the last
// thread's latest_reply, never the response's max_ts.
func TestFollowedThreadsPagesByLastLatestReply(t *testing.T) {
	v := &threadsView{pages: map[string]string{
		"":                  viewPage(true, 0, readA, readB),
		"1725805400.000200": viewPage(false, 0, unreadC),
	}}
	c, _ := newTestClient(t, v)

	got, more, err := c.FollowedThreads(context.Background(), FollowedThreadsOptions{})
	if err != nil {
		t.Fatalf("FollowedThreads: %v", err)
	}
	if len(got) != 3 || more {
		t.Errorf("got %d threads, more %v; want all 3 and nothing more", len(got), more)
	}
	if want := []string{"", "1725805400.000200"}; fmt.Sprint(v.cursors) != fmt.Sprint(want) {
		t.Errorf("cursors = %q, want %q", v.cursors, want)
	}
	if f := v.forms[0]; f.Get("limit") != "10" || f.Get("priority_mode") != "all" {
		t.Errorf("first request = %v, want limit 10 and priority_mode all", f)
	}
}

// TestFollowedThreadsConvertsTheView checks what an agent receives: the root
// as a message, the replies with permalinks that open in their thread, and
// the unread mark derived from the read marker.
func TestFollowedThreadsConvertsTheView(t *testing.T) {
	v := &threadsView{pages: map[string]string{"": viewPage(false, 2, unreadC, readB)}}
	c, _ := newTestClient(t, v)

	got, _, err := c.FollowedThreads(context.Background(), FollowedThreadsOptions{})
	if err != nil || len(got) != 2 {
		t.Fatalf("FollowedThreads = %d threads, %v; want 2", len(got), err)
	}

	th := got[0]
	if th.Channel != "C333CCCCC" || th.Root.TS != "1725804000.000100" || th.Root.ThreadTS != th.Root.TS {
		t.Errorf("thread = %s %s/%s, want the root of C333CCCCC", th.Channel, th.Root.TS, th.Root.ThreadTS)
	}
	if strings.Contains(th.Root.Permalink, "?thread_ts=") {
		t.Errorf("root permalink = %q, want it plain", th.Root.Permalink)
	}
	if len(th.Latest) != 2 || !strings.HasSuffix(th.Latest[1].Permalink,
		"/archives/C333CCCCC/p1725804300000300?thread_ts=1725804000.000100&cid=C333CCCCC") {
		t.Errorf("latest = %+v, want two replies whose permalinks open in the thread", th.Latest)
	}
	if !th.Unread() {
		t.Error("Unread() = false for a thread whose latest reply is past its read marker")
	}
	if got[1].Unread() {
		t.Error("Unread() = true for a thread read up to its latest reply")
	}
}

// TestFollowedThreadsUnreadWalk covers when an unread walk stops, which
// decides both what it costs and whether it can miss a thread.
func TestFollowedThreadsUnreadWalk(t *testing.T) {
	cases := []struct {
		name     string
		pages    map[string]string
		want     []string // channels of the threads returned
		requests int
	}{
		{
			"nothing unread is one request",
			map[string]string{"": viewPage(true, 0, readA, readB)},
			nil, 1,
		},
		{
			"the first read thread ends it",
			map[string]string{"": viewPage(true, 2, unreadC, readA)},
			[]string{"C333CCCCC"}, 1,
		},
		{
			"no unread thread first means the order changed, so keep looking",
			map[string]string{
				"":                  viewPage(true, 2, readA),
				"1725806500.000200": viewPage(false, 2, readB, unreadC),
			},
			[]string{"C333CCCCC"}, 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := &threadsView{pages: tc.pages}
			c, _ := newTestClient(t, v)

			got, _, err := c.FollowedThreads(context.Background(), FollowedThreadsOptions{UnreadOnly: true})
			if err != nil {
				t.Fatalf("FollowedThreads: %v", err)
			}
			var channels []string
			for _, th := range got {
				channels = append(channels, th.Channel)
			}
			if fmt.Sprint(channels) != fmt.Sprint(tc.want) {
				t.Errorf("threads = %v, want %v", channels, tc.want)
			}
			if len(v.cursors) != tc.requests {
				t.Errorf("made %d requests, want %d", len(v.cursors), tc.requests)
			}
		})
	}
}

// TestFollowedThreadsEndsWhereItMust: a cap reports that more exist, and a
// cursor that stops advancing ends the walk instead of repeating a page - and
// a thread two pages both list appears once.
func TestFollowedThreadsEndsWhereItMust(t *testing.T) {
	t.Run("max", func(t *testing.T) {
		v := &threadsView{pages: map[string]string{"": viewPage(true, 0, readA, readB)}}
		c, _ := newTestClient(t, v)

		got, more, err := c.FollowedThreads(context.Background(), FollowedThreadsOptions{Max: 1})
		if err != nil || len(got) != 1 || !more {
			t.Errorf("got %d threads, more %v, err %v; want 1 and more", len(got), more, err)
		}
	})

	t.Run("stuck cursor", func(t *testing.T) {
		v := &threadsView{pages: map[string]string{
			"":                  viewPage(true, 0, readA),
			"1725806500.000200": viewPage(true, 0, readA),
		}}
		c, _ := newTestClient(t, v)

		got, more, err := c.FollowedThreads(context.Background(), FollowedThreadsOptions{})
		if err != nil || len(got) != 1 || !more {
			t.Errorf("got %d threads, more %v, err %v; want the thread once, marked incomplete", len(got), more, err)
		}
		if len(v.cursors) != 2 {
			t.Errorf("made %d requests, want 2", len(v.cursors))
		}
	})
}
