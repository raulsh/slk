package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/raulsh/slk/internal/output"
)

// fakeData drives the browser from fixtures, so the view is testable without
// a terminal, a network or a Slack account.
type fakeData struct {
	convs []output.Conversation
	msgs  map[string][]output.Message
	vips  map[string]bool
	err   error
}

func (f fakeData) Conversations(context.Context) ([]output.Conversation, error) {
	return f.convs, f.err
}

func (f fakeData) ResolveNames(_ context.Context, cs []output.Conversation) ([]output.Conversation, error) {
	return cs, f.err
}

func (f fakeData) Messages(_ context.Context, id string, _ int) ([]output.Message, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.msgs[id], nil
}

func (f fakeData) IsVIP(id string) bool { return f.vips[id] }

func testData() fakeData {
	return fakeData{
		convs: []output.Conversation{
			{ID: "C1", Name: "#eng", Type: "channel", HasUnreads: true, MentionCount: 2, LatestTS: "300"},
			{ID: "C2", Name: "#general", Type: "channel", LatestTS: "200"},
			{ID: "D1", Name: "@alice", Type: "dm", User: "U1", HasUnreads: true, LatestTS: "100"},
		},
		msgs: map[string][]output.Message{
			"C1": {
				{TS: "1", Time: "2026-09-08T09:00:00Z", UserName: "raul", Text: "deploy is green"},
				{TS: "2", Time: "2026-09-08T09:01:00Z", UserName: "alice", Text: "thanks", VIP: true},
			},
		},
		vips: map[string]bool{"U1": true},
	}
}

// drive runs the model through a sequence of messages and returns the view.
func drive(t *testing.T, m *model, msgs ...tea.Msg) string {
	t.Helper()
	var mm tea.Model = m
	for _, msg := range msgs {
		var cmd tea.Cmd
		mm, cmd = mm.Update(msg)
		// Run any command synchronously so loads resolve inside the test.
		for cmd != nil {
			produced := cmd()
			if produced == nil {
				break
			}
			if _, isTick := produced.(interface{ String() string }); isTick {
				break
			}
			mm, cmd = mm.Update(produced)
		}
	}
	return mm.View()
}

func TestListRendersConversations(t *testing.T) {
	m := New(context.Background(), testData(), 50)
	view := drive(t, m,
		tea.WindowSizeMsg{Width: 120, Height: 30},
		convsLoaded{convs: testData().convs})

	for _, want := range []string{"#eng", "#general", "@alice", "NAME", "UNREAD"} {
		if !strings.Contains(view, want) {
			t.Errorf("view is missing %q:\n%s", want, view)
		}
	}
	if !strings.Contains(view, "3 conversations") {
		t.Errorf("view does not report the count:\n%s", view)
	}
}

// TestVIPMarkAppears covers the user's own classification reaching the list,
// which is the whole point of reading it out of Slack's prefs.
func TestVIPMarkAppears(t *testing.T) {
	m := New(context.Background(), testData(), 50)
	view := drive(t, m,
		tea.WindowSizeMsg{Width: 120, Height: 30},
		convsLoaded{convs: testData().convs})

	if !strings.Contains(view, "★") {
		t.Errorf("no VIP mark in the list:\n%s", view)
	}
}

func TestUnreadToggleFilters(t *testing.T) {
	m := New(context.Background(), testData(), 50)
	drive(t, m,
		tea.WindowSizeMsg{Width: 120, Height: 30},
		convsLoaded{convs: testData().convs})

	view := drive(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'u'}})

	if !strings.Contains(view, "unread") {
		t.Errorf("the unread toggle is not reflected in the title:\n%s", view)
	}
	if strings.Contains(view, "#general") {
		t.Errorf("#general has no unreads but survived the filter:\n%s", view)
	}
	if !strings.Contains(view, "#eng") {
		t.Errorf("#eng has unreads and should remain:\n%s", view)
	}
}

// TestFilterCapturesKeys is the bug this guards: while the filter has focus,
// typing "u" must insert a character rather than toggling the unread view.
func TestFilterCapturesKeys(t *testing.T) {
	m := New(context.Background(), testData(), 50)
	drive(t, m,
		tea.WindowSizeMsg{Width: 120, Height: 30},
		convsLoaded{convs: testData().convs})

	drive(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	drive(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'u'}})

	if m.unreadOnly {
		t.Error("typing 'u' into the filter toggled the unread view")
	}
	if got := m.filter.Value(); got != "u" {
		t.Errorf("filter value = %q, want %q", got, "u")
	}
}

func TestFilterNarrowsByName(t *testing.T) {
	m := New(context.Background(), testData(), 50)
	drive(t, m,
		tea.WindowSizeMsg{Width: 120, Height: 30},
		convsLoaded{convs: testData().convs})

	drive(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	for _, r := range "gene" {
		drive(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}

	if len(m.shown) != 1 || m.shown[0].Name != "#general" {
		t.Errorf("shown = %v, want just #general", names(m.shown))
	}
}

func TestEscClearsFilter(t *testing.T) {
	m := New(context.Background(), testData(), 50)
	drive(t, m,
		tea.WindowSizeMsg{Width: 120, Height: 30},
		convsLoaded{convs: testData().convs})

	drive(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	drive(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	drive(t, m, tea.KeyMsg{Type: tea.KeyEsc})

	if m.filtering {
		t.Error("still filtering after esc")
	}
	if m.filter.Value() != "" {
		t.Errorf("filter value = %q, want empty", m.filter.Value())
	}
	if len(m.shown) != 3 {
		t.Errorf("shown %d conversations, want all 3 back", len(m.shown))
	}
}

func TestOpenLoadsMessages(t *testing.T) {
	d := testData()
	m := New(context.Background(), d, 50)
	drive(t, m,
		tea.WindowSizeMsg{Width: 120, Height: 30},
		convsLoaded{convs: d.convs})

	// #eng sorts first, so the cursor is already on it.
	view := drive(t, m, msgsLoaded{msgs: d.msgs["C1"]})

	if m.mode != modeMessages {
		t.Fatalf("mode = %v, want modeMessages", m.mode)
	}
	for _, want := range []string{"deploy is green", "thanks", "raul", "alice"} {
		if !strings.Contains(view, want) {
			t.Errorf("message view is missing %q:\n%s", want, view)
		}
	}
}

func TestEscReturnsToList(t *testing.T) {
	d := testData()
	m := New(context.Background(), d, 50)
	drive(t, m,
		tea.WindowSizeMsg{Width: 120, Height: 30},
		convsLoaded{convs: d.convs},
		msgsLoaded{msgs: d.msgs["C1"]})

	view := drive(t, m, tea.KeyMsg{Type: tea.KeyEsc})

	if m.mode != modeList {
		t.Errorf("mode = %v, want modeList", m.mode)
	}
	if !strings.Contains(view, "conversations") {
		t.Errorf("did not return to the list:\n%s", view)
	}
}

// TestLoadErrorIsShownNotFatal keeps a transient failure from ending the
// session: the browser is long-lived, so it must report and stay usable.
func TestLoadErrorIsShownNotFatal(t *testing.T) {
	m := New(context.Background(), testData(), 50)
	view := drive(t, m,
		tea.WindowSizeMsg{Width: 120, Height: 30},
		convsLoaded{err: errors.New("slack said no")})

	if !strings.Contains(view, "slack said no") {
		t.Errorf("the error is not surfaced:\n%s", view)
	}
	if m.mode != modeList {
		t.Errorf("mode = %v, want to stay on the list", m.mode)
	}
}

func TestQuitOnQ(t *testing.T) {
	m := New(context.Background(), testData(), 50)
	drive(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatal("pressing q produced no command, want tea.Quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("pressing q did not quit")
	}
}

// TestNarrowTerminalKeepsTheNameColumn checks the width arithmetic: losing
// padding is fine, losing the column that identifies the row is not.
func TestNarrowTerminalKeepsTheNameColumn(t *testing.T) {
	for _, width := range []int{40, 60, 80, 200} {
		cols := conversationColumns(width)
		if cols[1].Title != "NAME" {
			t.Fatalf("column 1 is %q, want NAME", cols[1].Title)
		}
		if cols[1].Width < 16 {
			t.Errorf("width %d: name column = %d, want at least 16", width, cols[1].Width)
		}
	}
}

func names(cs []output.Conversation) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}
