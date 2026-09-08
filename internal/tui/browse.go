// Package tui is slk's interactive browser.
//
// It is a separate package because it is the one part of slk that is not
// answerable in a single request/response: everything else prints and exits,
// which is what an agent needs, and this exists for the times a person wants
// to look around instead. Keeping the event loop out of internal/cli stops the
// batch commands from acquiring a dependency on a terminal.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/raulsh/slk/internal/output"
)

// Data is what the browser needs from the outside world.
//
// An interface rather than the Slack client, so the TUI is driven by whatever
// the caller has already assembled - and so it can be exercised with fixtures.
type Data interface {
	// Conversations returns the list to browse. It should be fast: any name
	// that costs a round trip belongs in ResolveNames instead.
	Conversations(ctx context.Context) ([]output.Conversation, error)
	// ResolveNames fills in the names that need extra calls - a DM shows its
	// peer's handle, which is one lookup per conversation. Splitting it out is
	// what lets the list paint immediately instead of waiting on a few dozen
	// sequential resolutions.
	ResolveNames(ctx context.Context, cs []output.Conversation) ([]output.Conversation, error)
	// Messages loads one conversation's recent history.
	Messages(ctx context.Context, channelID string, limit int) ([]output.Message, error)
	// IsVIP reports the user's own VIP marks.
	IsVIP(userID string) bool
}

type mode int

const (
	modeList mode = iota
	modeLoading
	modeMessages
)

type keymap struct {
	Up     key.Binding
	Down   key.Binding
	Open   key.Binding
	Back   key.Binding
	Filter key.Binding
	Unread key.Binding
	VIP    key.Binding
	Reload key.Binding
	Help   key.Binding
	Quit   key.Binding
}

var keys = keymap{
	Up:     key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
	Down:   key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
	Open:   key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open")),
	Back:   key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
	Filter: key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
	Unread: key.NewBinding(key.WithKeys("u"), key.WithHelp("u", "unread only")),
	VIP:    key.NewBinding(key.WithKeys("v"), key.WithHelp("v", "vip DMs")),
	Reload: key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "reload")),
	Help:   key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
	Quit:   key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
}

func (k keymap) ShortHelp() []key.Binding {
	return []key.Binding{k.Open, k.Filter, k.Unread, k.VIP, k.Quit}
}

func (k keymap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Up, k.Down, k.Open, k.Back},
		{k.Filter, k.Unread, k.VIP, k.Reload},
		{k.Help, k.Quit},
	}
}

type model struct {
	ctx  context.Context
	data Data

	mode    mode
	convs   []output.Conversation
	shown   []output.Conversation
	msgs    []output.Message
	current output.Conversation

	tbl    table.Model
	vp     viewport.Model
	filter textinput.Model
	spin   spinner.Model
	help   help.Model

	filtering  bool
	unreadOnly bool
	vipOnly    bool
	resolving  bool
	lastNote   string

	width, height int
	err           error
	limit         int
}

// styles are built once. lipgloss detects the terminal itself here, which is
// correct: the TUI only ever runs attached to one.
var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Padding(0, 1)
	headerStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("62")).Bold(true).Padding(0, 1)
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("204")).Padding(0, 1)
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	vipStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	senderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("86"))
	statusStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Padding(0, 1)
)

// New builds the browser.
func New(ctx context.Context, d Data, limit int) *model {
	fi := textinput.New()
	fi.Placeholder = "filter conversations"
	fi.Prompt = "/ "

	sp := spinner.New()
	sp.Spinner = spinner.Dot

	t := table.New(
		table.WithColumns(conversationColumns(80)),
		table.WithFocused(true),
	)
	st := table.DefaultStyles()
	st.Header = st.Header.BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("240")).BorderBottom(true).Bold(true)
	st.Selected = st.Selected.Foreground(lipgloss.Color("229")).
		Background(lipgloss.Color("57")).Bold(true)
	t.SetStyles(st)

	return &model{
		ctx:  ctx,
		data: d,
		tbl:  t,
		// Start loading, not listing: the zero mode would paint an empty
		// table and claim "0 conversations" while the first fetch is still
		// in flight, which reads as an empty workspace rather than as work
		// in progress.
		mode:   modeLoading,
		filter: fi,
		spin:   sp,
		help:   help.New(),
		limit:  limit,
	}
}

// Fixed column widths, and the per-cell horizontal padding the table style
// adds on top of them. Forgetting the padding is what silently clips the last
// column, because the overflow lands entirely on whatever is rightmost.
const (
	colVIP     = 2
	colType    = 8
	colUnread  = 7
	colMention = 3
	colLatest  = 6
	cellPad    = 2
	numCols    = 6
)

func conversationColumns(width int) []table.Column {
	fixed := colVIP + colType + colUnread + colMention + colLatest
	// The name column absorbs the slack, so a narrow terminal loses room for
	// the name rather than clipping a column that carries a distinct fact.
	name := width - fixed - (cellPad * numCols) - 2
	if name < 16 {
		name = 16
	}
	return []table.Column{
		{Title: "", Width: colVIP},
		{Title: "NAME", Width: name},
		{Title: "TYPE", Width: colType},
		{Title: "UNREAD", Width: colUnread},
		{Title: "@", Width: colMention},
		{Title: "LATEST", Width: colLatest},
	}
}

type convsLoaded struct {
	convs []output.Conversation
	err   error
}

// namesResolved carries the second phase of the conversation load.
type namesResolved struct {
	convs []output.Conversation
	err   error
}

type msgsLoaded struct {
	msgs []output.Message
	err  error
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.spin.Tick, m.loadConvs())
}

func (m *model) loadConvs() tea.Cmd {
	return func() tea.Msg {
		cs, err := m.data.Conversations(m.ctx)
		return convsLoaded{convs: cs, err: err}
	}
}

func (m *model) resolveNames(cs []output.Conversation) tea.Cmd {
	return func() tea.Msg {
		out, err := m.data.ResolveNames(m.ctx, cs)
		return namesResolved{convs: out, err: err}
	}
}

// log records a non-fatal problem without taking over the view.
func (m *model) log(err error) { m.lastNote = err.Error() }

func (m *model) loadMsgs(id string) tea.Cmd {
	return func() tea.Msg {
		ms, err := m.data.Messages(m.ctx, id, m.limit)
		return msgsLoaded{msgs: ms, err: err}
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.tbl.SetColumns(conversationColumns(msg.Width))
		m.tbl.SetHeight(maxInt(msg.Height-7, 3))
		m.vp = viewport.New(msg.Width, maxInt(msg.Height-5, 3))
		if m.mode == modeMessages {
			m.vp.SetContent(m.renderMessages())
		}
		m.help.Width = msg.Width
		return m, nil

	case convsLoaded:
		if msg.err != nil {
			m.err = msg.err
			m.mode = modeList
			return m, nil
		}
		m.convs = msg.convs
		m.applyFilter()
		m.mode = modeList
		// Paint now, resolve the expensive names next: the list is usable
		// immediately and the DM handles fill in a moment later.
		m.resolving = true
		return m, m.resolveNames(msg.convs)

	case namesResolved:
		m.resolving = false
		if msg.err != nil {
			// A name that would not resolve is not worth an error banner; the
			// list still works with raw ids for those rows.
			m.log(msg.err)
			return m, nil
		}
		m.convs = msg.convs
		m.applyFilter()
		return m, nil

	case msgsLoaded:
		if msg.err != nil {
			m.err = msg.err
			m.mode = modeList
			return m, nil
		}
		m.msgs = msg.msgs
		m.mode = modeMessages
		m.vp.SetContent(m.renderMessages())
		m.vp.GotoTop()
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	return m, nil
}

func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// While filtering, the text input owns almost every key: intercepting
	// them for shortcuts would make it impossible to type "u" in a filter.
	if m.filtering {
		switch msg.String() {
		case "esc":
			m.filtering = false
			m.filter.SetValue("")
			m.filter.Blur()
			m.applyFilter()
			return m, nil
		case "enter":
			m.filtering = false
			m.filter.Blur()
			return m, nil
		}
		var cmd tea.Cmd
		m.filter, cmd = m.filter.Update(msg)
		m.applyFilter()
		return m, cmd
	}

	switch {
	case key.Matches(msg, keys.Quit):
		return m, tea.Quit

	case key.Matches(msg, keys.Help):
		m.help.ShowAll = !m.help.ShowAll
		return m, nil

	case key.Matches(msg, keys.Back):
		if m.mode == modeMessages {
			m.mode = modeList
			m.err = nil
		}
		return m, nil
	}

	if m.mode == modeMessages {
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	}

	switch {
	case key.Matches(msg, keys.Filter):
		m.filtering = true
		m.filter.Focus()
		return m, textinput.Blink

	case key.Matches(msg, keys.Unread):
		m.unreadOnly = !m.unreadOnly
		m.applyFilter()
		return m, nil

	case key.Matches(msg, keys.VIP):
		m.vipOnly = !m.vipOnly
		m.applyFilter()
		return m, nil

	case key.Matches(msg, keys.Reload):
		m.mode = modeLoading
		m.lastNote = ""
		return m, tea.Batch(m.spin.Tick, m.loadConvs())

	case key.Matches(msg, keys.Open):
		if c, ok := m.selected(); ok {
			m.current = c
			m.mode = modeLoading
			m.err = nil
			return m, tea.Batch(m.spin.Tick, m.loadMsgs(c.ID))
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.tbl, cmd = m.tbl.Update(msg)
	return m, cmd
}

func (m *model) selected() (output.Conversation, bool) {
	i := m.tbl.Cursor()
	if i < 0 || i >= len(m.shown) {
		return output.Conversation{}, false
	}
	return m.shown[i], true
}

// applyFilter recomputes the visible rows from the active toggles.
func (m *model) applyFilter() {
	want := strings.ToLower(m.filter.Value())

	m.shown = nil
	for _, c := range m.convs {
		if m.unreadOnly && !c.HasUnreads {
			continue
		}
		if m.vipOnly && !m.isVIPConversation(c) {
			continue
		}
		if want != "" && !strings.Contains(strings.ToLower(c.Name), want) {
			continue
		}
		m.shown = append(m.shown, c)
	}

	rows := make([]table.Row, 0, len(m.shown))
	for _, c := range m.shown {
		vip := ""
		if m.isVIPConversation(c) {
			vip = "★"
		}
		unread := ""
		if c.HasUnreads {
			unread = "yes"
		}
		mentions := ""
		if c.MentionCount > 0 {
			mentions = fmt.Sprintf("%d", c.MentionCount)
		}
		rows = append(rows, table.Row{vip, c.Name, c.Type, unread, mentions, shortTime(c.LatestTime)})
	}
	m.tbl.SetRows(rows)
	if m.tbl.Cursor() >= len(rows) {
		m.tbl.SetCursor(maxInt(len(rows)-1, 0))
	}
}

// isVIPConversation reports whether a row is a DM with someone marked VIP.
//
// Only DMs: a channel would need message-level data to know whether a VIP has
// been active in it, which is not something the conversation list carries. The
// `v` toggle is labelled accordingly rather than promising more than it does.
func (m *model) isVIPConversation(c output.Conversation) bool {
	return c.Type == "dm" && m.data.IsVIP(c.User)
}

func (m *model) renderMessages() string {
	if len(m.msgs) == 0 {
		return dimStyle.Render("  no messages")
	}

	// Wrap rather than clip: a message cut off mid-word is worse than one
	// that takes three lines, and this is the view a person reads.
	const indent = "    "
	wrap := lipgloss.NewStyle().Width(maxInt(m.vp.Width-len(indent)-1, 20))

	var b strings.Builder
	for _, msg := range m.msgs {
		text := msg.TextRendered
		if text == "" {
			text = msg.Text
		}
		prefix := "  "
		if msg.VIP {
			prefix = vipStyle.Render("★ ")
		}
		_, _ = fmt.Fprintf(&b, "%s%s %s\n", prefix,
			dimStyle.Render(shortTime(msg.Time)),
			senderStyle.Render(senderOf(msg)))

		wrote := false
		for _, para := range strings.Split(text, "\n") {
			if strings.TrimSpace(para) == "" {
				continue
			}
			for _, line := range strings.Split(wrap.Render(para), "\n") {
				b.WriteString(indent + line + "\n")
			}
			wrote = true
		}
		// A message can legitimately carry no text - a file share, an image,
		// a reaction-only post. Saying so beats a blank gap the reader has to
		// interpret.
		if !wrote {
			b.WriteString(indent + dimStyle.Render(emptyBody(msg)) + "\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (m *model) View() string {
	if m.width == 0 {
		return "\n  starting…\n"
	}

	var b strings.Builder

	switch m.mode {
	case modeLoading:
		b.WriteString(titleStyle.Render(m.spin.View() + " loading…"))
		b.WriteString("\n")

	case modeMessages:
		b.WriteString(headerStyle.Render(m.current.Name))
		b.WriteString("\n")
		b.WriteString(m.vp.View())
		b.WriteString("\n")
		b.WriteString(statusStyle.Render(fmt.Sprintf(
			"%d messages · esc back · ↑/↓ scroll · q quit", len(m.msgs))))
		return b.String()

	case modeList:
		title := fmt.Sprintf("%d conversations", len(m.shown))
		var toggles []string
		if m.unreadOnly {
			toggles = append(toggles, "unread")
		}
		if m.vipOnly {
			toggles = append(toggles, "vip DMs")
		}
		if len(toggles) > 0 {
			title += " · " + strings.Join(toggles, "+")
		}
		if m.resolving {
			title += " · " + m.spin.View() + " naming DMs"
		}
		b.WriteString(headerStyle.Render("slk") + titleStyle.Render(title))
		b.WriteString("\n")
		if m.filtering || m.filter.Value() != "" {
			b.WriteString(m.filter.View())
			b.WriteString("\n")
		}
		b.WriteString(m.tbl.View())
		b.WriteString("\n")
	}

	if m.err != nil {
		b.WriteString(errStyle.Render("error: " + m.err.Error()))
		b.WriteString("\n")
	} else if m.lastNote != "" {
		b.WriteString(statusStyle.Render(m.lastNote))
		b.WriteString("\n")
	}
	b.WriteString(m.help.View(keys))
	return b.String()
}

// Run starts the browser. It blocks until the user quits.
func Run(ctx context.Context, d Data, limit int) error {
	p := tea.NewProgram(New(ctx, d, limit), tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := p.Run()
	return err
}

// emptyBody describes a message that rendered to nothing.
func emptyBody(m output.Message) string {
	switch {
	case len(m.Files) > 0:
		names := make([]string, 0, len(m.Files))
		for _, f := range m.Files {
			n := f.Name
			if n == "" {
				n = f.Title
			}
			if n == "" {
				n = f.Type
			}
			names = append(names, n)
		}
		return "[file: " + strings.Join(names, ", ") + "]"
	case m.AttachmentCount > 0:
		return fmt.Sprintf("[%d attachment(s) — slk messages --raw to see them]", m.AttachmentCount)
	default:
		return "[no text]"
	}
}

func senderOf(m output.Message) string {
	switch {
	case m.UserName != "":
		return m.UserName
	case m.User != "":
		return m.User
	case m.BotID != "":
		return m.BotID
	default:
		return "?"
	}
}

// shortTime renders an RFC3339 stamp compactly for a narrow column.
func shortTime(rfc string) string {
	if len(rfc) < 16 {
		return ""
	}
	// 2026-09-08T15:04:05-03:00 -> 15:04 today, else Sep 08.
	date, clock := rfc[:10], rfc[11:16]
	if strings.HasPrefix(nowDate(), date) {
		return clock
	}
	return date[5:]
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// nowDate is a variable so a test can pin "today".
var nowDate = func() string { return time.Now().Format("2006-01-02") }
