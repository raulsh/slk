package slack

import (
	"context"
	"net/url"
	"strconv"

	"github.com/raulsh/slk/internal/output"
	"github.com/raulsh/slk/internal/slacktime"
)

// threadsPageSize is the most the threads view returns in one page: asked for
// more, it answers with ten.
const threadsPageSize = 10

// maxThreadPages bounds a walk through the threads view, so a cursor that
// stops advancing or an account following thousands of threads ends the walk
// instead of the deadline.
const maxThreadPages = 20

// FollowedThread is one thread from the user's threads view: one they started,
// replied to, or chose to follow.
type FollowedThread struct {
	Channel string
	// Root is the thread's first message, complete: the view carries it, so
	// listing threads costs no request per thread.
	Root        output.Message
	LastRead    slacktime.TS
	LatestReply slacktime.TS
	// Latest holds the newest replies the view keeps with each thread - two,
	// as observed - oldest first.
	Latest []output.Message
}

// Unread reports whether the thread has replies newer than the user's read
// marker in it.
func (t FollowedThread) Unread() bool { return t.LatestReply.After(t.LastRead) }

// FollowedThreadsOptions selects from the threads view.
type FollowedThreadsOptions struct {
	UnreadOnly bool
	// Max ends the walk after this many threads; zero means no limit but
	// the page cap.
	Max int
}

type rawThreadRoot struct {
	rawMessage
	Channel    string `json:"channel"`
	LastRead   string `json:"last_read"`
	Subscribed *bool  `json:"subscribed"`
}

type rawThreadEntry struct {
	Root          rawThreadRoot `json:"root_msg"`
	LatestReplies []rawMessage  `json:"latest_replies"`
}

// FollowedThreads reads the user's threads view, newest activity first, and
// reports whether more threads were left unread by the walk.
//
// It is the only way to find a new reply on an old thread: conversations.history
// never returns replies, and client.counts says how many there are but not
// where. subscriptions.thread.getView is what the web client's Threads tab
// calls. Prior accounts of it disagree, so what this relies on was checked
// against a real workspace:
//
//   - a page holds at most ten threads, newest activity first;
//   - the next page is asked for with current_ts set to the last thread's
//     latest_reply, an exclusive bound - the response's max_ts is newer than
//     every thread on the page, and sending it back returns the same page;
//   - a thread whose replies are all read has latest_reply equal to last_read.
//
// An unread walk also relies on the view listing unread threads before read
// ones, as the Threads tab does, so the first read thread ends it. That is the
// one part attested by prior art rather than seen here - there was nothing
// unread to see - so a first page with no unread thread at all, while the view
// counts unread replies, is taken to mean the order changed: the walk carries
// on rather than report nothing.
func (c *Client) FollowedThreads(ctx context.Context, o FollowedThreadsOptions) ([]FollowedThread, bool, error) {
	var (
		out    []FollowedThread
		cursor string
		// Two pages can list the same thread when it gains a reply between
		// requests, which would otherwise show its messages twice.
		seen = map[string]bool{}
	)
	for page := 0; page < maxThreadPages; page++ {
		form := url.Values{}
		form.Set("limit", strconv.Itoa(threadsPageSize))
		form.Set("priority_mode", "all")
		if cursor != "" {
			form.Set("current_ts", cursor)
		}

		var resp struct {
			Threads            []rawThreadEntry `json:"threads"`
			HasMore            bool             `json:"has_more"`
			TotalUnreadReplies int              `json:"total_unread_replies"`
		}
		if err := c.call(ctx, "subscriptions.thread.getView", form, &resp); err != nil {
			return out, len(out) > 0, err
		}
		if page == 0 && o.UnreadOnly && resp.TotalUnreadReplies == 0 {
			// Nothing unread anywhere, by the view's own count.
			return nil, false, nil
		}

		sawRead := false
		for i, e := range resp.Threads {
			t, ok := e.toFollowed(c.workspaceHost())
			if !ok {
				continue
			}
			if o.UnreadOnly && !t.Unread() {
				sawRead = true
				continue
			}
			key := t.Channel + "/" + t.Root.TS
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, t)
			if o.Max > 0 && len(out) >= o.Max {
				return out, i < len(resp.Threads)-1 || resp.HasMore, nil
			}
		}

		if !resp.HasMore || len(resp.Threads) == 0 {
			return out, false, nil
		}
		if o.UnreadOnly && sawRead && len(out) > 0 {
			return out, false, nil
		}

		last := resp.Threads[len(resp.Threads)-1].Root
		next := last.LatestReply
		if next == "" {
			next = last.TS
		}
		if next == "" || next == cursor {
			// No way forward. Say the list may be incomplete rather than
			// ask for the same page again.
			return out, true, nil
		}
		cursor = next
	}
	return out, true, nil
}

// toFollowed converts one entry of the view, reporting false for anything that
// is not recognisably a followed thread.
func (e rawThreadEntry) toFollowed(host string) (FollowedThread, bool) {
	root := e.Root
	if root.Channel == "" || root.TS == "" {
		return FollowedThread{}, false
	}
	// The view lists only followed threads today. Should it ever include the
	// unfollowed ones, they are not this list.
	if root.Subscribed != nil && !*root.Subscribed {
		return FollowedThread{}, false
	}
	if root.ThreadTS == "" {
		root.ThreadTS = root.TS
	}

	t := FollowedThread{
		Channel: root.Channel,
		Root:    root.toOutput(root.Channel, host),
	}
	t.LastRead, _ = slacktime.Parse(root.LastRead)
	t.LatestReply, _ = slacktime.Parse(root.LatestReply)
	for _, r := range e.LatestReplies {
		t.Latest = append(t.Latest, r.toOutput(root.Channel, host))
	}
	return t, true
}
