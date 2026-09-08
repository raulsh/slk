package output

import (
	"os"

	"github.com/charmbracelet/lipgloss"
	ltable "github.com/charmbracelet/lipgloss/table"
	"github.com/muesli/termenv"
	"golang.org/x/term"
)

// table builds an aligned table.
//
// This replaces text/tabwriter for one concrete reason: tabwriter measures a
// cell by counting its bytes, so it cannot see ANSI colour codes or account
// for a double-width rune. A coloured cell or a "★" therefore silently shifted
// every column after it. lipgloss measures printable width instead, which is
// the actual requirement here.
type table struct {
	w       *Writer
	headers []string
	rows    [][]string
	// right holds the column indices to right-align, for counts.
	right map[int]bool
	// bare drops the border and the header, for a flowing log where chrome
	// would compete with the content.
	bare bool
}

func (w *Writer) table(headers ...string) *table {
	return &table{w: w, headers: headers, right: map[int]bool{}}
}

// rightAlign marks columns as right-aligned, which is what makes a column of
// numbers scannable.
func (t *table) rightAlign(cols ...int) *table {
	for _, c := range cols {
		t.right[c] = true
	}
	return t
}

// borderless drops the frame but keeps the width-aware alignment, which is
// the only reason this type exists.
func (t *table) borderless() *table {
	t.bare = true
	return t
}

func (t *table) row(cells ...string) *table {
	t.rows = append(t.rows, cells)
	return t
}

func (t *table) empty() bool { return len(t.rows) == 0 }

// render writes the table.
func (t *table) render() error {
	if len(t.rows) == 0 {
		return nil
	}

	// Bind the renderer to the actual output stream rather than os.Stdout, so
	// a piped run and a test buffer both get plain text without the caller
	// having to remember to ask.
	r := lipgloss.NewRenderer(t.w.out)
	if !t.w.opts.Color {
		// Named, not 0: termenv's zero Profile is TrueColor, so the obvious
		// literal turns colour fully on rather than off.
		r.SetColorProfile(termenv.Ascii)
	}

	header := r.NewStyle().Bold(true).Padding(0, 1)
	cell := r.NewStyle().Padding(0, 1)
	cellRight := cell.Align(lipgloss.Right)
	border := r.NewStyle().Foreground(lipgloss.Color("240"))

	tbl := ltable.New().
		Rows(t.rows...).
		StyleFunc(func(row, col int) lipgloss.Style {
			if row == ltable.HeaderRow {
				return header
			}
			if t.right[col] {
				return cellRight
			}
			return cell
		})

	if t.bare {
		tbl = tbl.Border(lipgloss.HiddenBorder()).
			BorderTop(false).BorderBottom(false).
			BorderLeft(false).BorderRight(false).
			BorderColumn(false).BorderRow(false)
	} else {
		tbl = tbl.Border(lipgloss.RoundedBorder()).
			BorderStyle(border).
			BorderRow(false).
			BorderColumn(false).
			Headers(t.headers...)
	}

	if width := terminalWidth(t.w.out); width > 0 {
		tbl = tbl.Width(width)
	}

	_, err := t.w.out.Write([]byte(tbl.String() + "\n"))
	return err
}

// terminalWidth reports the usable width, or 0 when the target is not a
// terminal - in which case the table should size itself to its content rather
// than pad to an imagined 80 columns.
func terminalWidth(w any) int {
	f, ok := w.(*os.File)
	if !ok {
		return 0
	}
	width, _, err := term.GetSize(int(f.Fd()))
	if err != nil || width <= 0 {
		return 0
	}
	return width
}
