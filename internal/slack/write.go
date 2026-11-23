package slack

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"

	"github.com/raulsh/slk/internal/errdefs"
	"github.com/raulsh/slk/internal/output"
)

// PostOptions is one message to post.
type PostOptions struct {
	Channel string
	// Text goes out as the message's text, verbatim. Escaping is the caller's
	// decision, because only the caller knows whether a "<" was meant as
	// markup.
	Text string
	// ThreadTS is the thread root to reply under; empty posts to the
	// conversation itself. It must be a root, never a reply - ThreadRoot finds
	// it.
	ThreadTS string
}

// PostMessage posts a message as the user and returns it as Slack stored it.
//
// The form carries the fields of the web client's own chat.postMessage that
// mean something to the server: channel, text, thread_ts, and a fresh
// client_msg_id. The rest of what the composer sends - the rich_text blocks it
// builds, unfurl, draft_id, xArgs and a client-side pending ts - is either UI
// bookkeeping or needs a mrkdwn-to-rich_text converter, and text renders the
// same mrkdwn without one.
//
// It never retries. A post that timed out may still have landed, and posting
// again would post twice; only the caller can check first.
func (c *Client) PostMessage(ctx context.Context, o PostOptions) (output.Message, error) {
	form := url.Values{}
	form.Set("channel", o.Channel)
	form.Set("text", o.Text)
	if o.ThreadTS != "" {
		form.Set("thread_ts", o.ThreadTS)
	}
	form.Set("client_msg_id", newClientMsgID())

	var resp struct {
		Channel string     `json:"channel"`
		TS      string     `json:"ts"`
		Message rawMessage `json:"message"`
	}
	if err := c.call(ctx, "chat.postMessage", form, &resp); err != nil {
		return output.Message{}, err
	}
	if resp.TS == "" {
		// Slack accepted something this cannot point at. Reporting success
		// without an id would leave the caller unable to thread, react or
		// even check what landed.
		return output.Message{}, fmt.Errorf("chat.postMessage: ok response carried no ts; the message may have been posted")
	}

	channel := resp.Channel
	if channel == "" {
		channel = o.Channel
	}
	rm := resp.Message
	rm.TS = resp.TS
	if rm.ThreadTS == "" {
		rm.ThreadTS = o.ThreadTS
	}
	return rm.toOutput(channel, c.workspaceHost()), nil
}

// ThreadRoot returns the ts of the thread a message belongs to, and in doing
// so proves the message exists.
//
// conversations.replies answers a reply's ts with that reply alone, carrying
// its thread_ts - verified against a real workspace - and a root's ts with the
// thread, root first. Either way the first message's thread_ts, or for an
// unthreaded message its own ts, is the root. That is what chat.postMessage
// needs: Slack's guidance is to thread under the root, never under a reply,
// and a thread_ts that names nothing is not reliably refused.
func (c *Client) ThreadRoot(ctx context.Context, channel, ts string) (string, error) {
	ms, err := c.Replies(ctx, RepliesOptions{Channel: channel, ThreadTS: ts, Limit: 1})
	if err != nil {
		return "", err
	}
	if len(ms) == 0 {
		return "", fmt.Errorf("conversations.replies: %w: no message %s in %s", errdefs.ErrNotFound, ts, channel)
	}
	if ms[0].ThreadTS != "" {
		return ms[0].ThreadTS, nil
	}
	return ms[0].TS, nil
}

// OpenDM returns the DM conversation with a user, creating it when there is
// none.
//
// conversations.open is idempotent - it returns the existing DM when one
// exists - so it is safe whenever the directory cache does not know the DM. It
// can still create a conversation, which is why only a gated write calls it,
// and never under --dry-run.
func (c *Client) OpenDM(ctx context.Context, userID string) (string, error) {
	form := url.Values{}
	form.Set("users", userID)
	form.Set("return_im", "true")

	var resp struct {
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
	}
	if err := c.call(ctx, "conversations.open", form, &resp); err != nil {
		return "", err
	}
	if resp.Channel.ID == "" {
		return "", fmt.Errorf("conversations.open: response carried no channel id")
	}
	return resp.Channel.ID, nil
}

// newClientMsgID makes the random UUID the web client attaches to every
// message it sends, so a post from slk carries the same fields as one typed
// into the browser.
func newClientMsgID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)     // crypto/rand.Read never fails, per its documentation
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// React adds a reaction to a message, or removes one, and reports whether the
// message changed.
//
// already_reacted and no_reaction mean the message is already in the state
// asked for. That is the outcome the caller wanted, not a failure, and
// treating it as success is what makes a retry after a timeout safe: unlike a
// post, a reaction cannot land twice.
func (c *Client) React(ctx context.Context, channel, ts, name string, remove bool) (bool, error) {
	method := "reactions.add"
	if remove {
		method = "reactions.remove"
	}
	form := url.Values{}
	form.Set("channel", channel)
	form.Set("timestamp", ts)
	form.Set("name", name)

	if err := c.call(ctx, method, form, nil); err != nil {
		if errors.Is(err, errNoChange) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
