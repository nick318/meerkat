package uikit

// The ⌘F strip: one search line over whatever sits under it, the count of
// what it found, and the two steps between the hits.
//
// Two things wear it — the SQL editor over its buffer and the query tab
// over its result — and they are one control in the user's hands, so they
// are one control here: the same keys, the same count, the same words.
// What a hit *is* stays with the owner. The editor's hits are byte ranges
// and the grid's are cells, so the bar is handed a [FindCount] and two
// callbacks and paints; it decides nothing.
//
// **It is a strip, not a popover.** A popover over the editor covers the
// very lines being searched, and one over the grid covers its header — the
// row that says which column a hit is in. A strip pushes the pane down by
// one row and hides nothing.
//
// **The bar never sits inside its owner's key context.** The grid binds a
// bare `space` to ticking a row and the editor binds `enter` to a line
// break; either one as an ancestor of the search line would take those
// keys from it. Each owner paints the bar beside its own context, never
// under it, and gives the bar a context of its own.
//
// The walk itself — [Step] and [FirstFrom] — is plain functions over
// sorted hits, so it is argued with in a test rather than in a window.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/nick318/meerkat/internal/theme"
)

// fieldWidth is the strip's search box: wide enough for a statement
// fragment, never so wide that the count drifts away from it.
const fieldWidth float32 = 260.

// FontSize is the search line's text, the size of the column find's.
const FontSize float32 = 12.

// MaxHits: past this many hits the count stops counting. A needle of one
// letter over a large result would otherwise walk every value in it on
// each keystroke, and "10,000+" is as much of an answer as the exact
// number.
const MaxHits = 10_000

// FindCount is what the search found, as far as the bar is concerned.
type FindCount struct {
	// Typed is whether anything is typed. An empty line is a question not
	// yet asked and says nothing, rather than "no matches".
	Typed bool
	// Total is how many hits there are.
	Total int
	// Current is which of them the owner is on, when it is on one (nil
	// when not). The caret can sit between hits after the user clicks
	// away, and "3 of 12" would then be a claim about a hit nobody is
	// looking at.
	Current *int
	// Capped is that the count stopped at [MaxHits], so there are more
	// than it says.
	Capped bool
}

// Label is the count beside the line. Pure, so the wording is testable
// without a window.
func (f FindCount) Label() string {
	if !f.Typed {
		return ""
	}
	if f.Total == 0 {
		return "no matches"
	}
	plus := ""
	if f.Capped {
		plus = "+"
	}
	total := grouped(f.Total)
	if f.Current != nil {
		return fmt.Sprintf("%s of %s%s", grouped(*f.Current+1), total, plus)
	}
	return fmt.Sprintf("%s%s found", total, plus)
}

// Missed is that nothing answers what was typed. The count then takes the
// error's ink, so a needle that has gone wrong reads as such from the
// corner of the eye.
func (f FindCount) Missed() bool { return f.Typed && f.Total == 0 }

// grouped writes `12,345`: a count long enough to need reading is grouped
// in threes.
func grouped(count int) string {
	digits := strconv.Itoa(count)
	var out strings.Builder
	for ix, digit := range digits {
		if ix > 0 && (len(digits)-ix)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteRune(digit)
	}
	return out.String()
}

// Step is which hit a step from `at` lands on: the first one past it going
// forward, the last one before it going back. Hits are sorted by cmp.
//
// **The walk wraps**, as every editor's find does: one press past the
// last hit comes round to the first, and the count says where the walk
// is. With nothing to stand on (`at` nil), forward starts at the first
// hit and back at the last. The second result is false only when there
// are no hits. (Rust: `step<T: Ord>`; Go has no `Ord` for tuples, so the
// order comes as `cmp`, as `slices.SortFunc` takes it.)
func Step[T any](hits []T, at *T, forward bool, cmp func(a, b T) int) (int, bool) {
	if len(hits) == 0 {
		return 0, false
	}
	if at == nil {
		if forward {
			return 0, true
		}
		return len(hits) - 1, true
	}
	if forward {
		past := sort.Search(len(hits), func(i int) bool { return cmp(hits[i], *at) > 0 })
		if past == len(hits) {
			return 0, true
		}
		return past, true
	}
	before := sort.Search(len(hits), func(i int) bool { return cmp(hits[i], *at) >= 0 })
	if before == 0 {
		return len(hits) - 1, true
	}
	return before - 1, true
}

// FirstFrom is the first hit at or after `at`, wrapping to the first of
// all. This is where a keystroke in the line lands: the search moves on
// from where the user was when they opened it, not from the top of the
// buffer, and a hit already under them stays put.
func FirstFrom[T any](hits []T, at T, cmp func(a, b T) int) (int, bool) {
	if len(hits) == 0 {
		return 0, false
	}
	from := sort.Search(len(hits), func(i int) bool { return cmp(hits[i], at) >= 0 })
	if from == len(hits) {
		return 0, true
	}
	return from, true
}

// OnStep is called with `true` for the next hit and `false` for the one
// before.
type OnStep func(forward bool)

// OnClose is called when the strip's close button is pressed.
type OnClose func()

// FindBar builds the strip. `id` must be unique among the strips on screen.
func FindBar(c *ui.Context, id string, query *TextField, count FindCount, onStep OnStep, onClose OnClose) ui.Element {
	colors := &theme.Current().Colors
	countInk := colors.TextMuted
	if count.Missed() {
		countInk = colors.Error
	}

	// A click on the strip is not a click on the text or the rows under
	// it: mygo hit-tests the topmost element painted under the pointer,
	// so the strip occludes what it covers without being told (GPUI's
	// `.occlude()`).
	return ui.Row(c.Key(id)).
		Shrink(0).
		AlignItems(ui.Center).
		Gap(8).
		Padding(6, 14).
		BorderWidth(0, 0, 1, 0).
		BorderColor(colors.Hairline.Color()).
		Background(colors.Panel.Color()).
		Children(func() {
			ui.Row(c).
				Shrink(0).
				Width(fieldWidth).
				AlignItems(ui.Center).
				Gap(8).
				Padding(4, 9).
				Border(1, colors.BorderStrong.Color()).
				Radius(6).
				Background(colors.Elevated.Color()).
				Children(func() {
					SearchGlyph(c, colors.TextFaint)
					ui.Box(c).Grow(1).Basis(0).MinWidth(0).Children(func() { query.View(c) })
				})
			ui.Box(c).
				Shrink(0).
				MinWidth(72).
				FontSize(theme.SizeCaption).
				TextColor(countInk.Color()).
				Children(func() { ui.Text(c, count.Label()) })
			stepButton(c, "find-previous", "↑", count.Total > 0).OnClick(func() { onStep(false) })
			stepButton(c, "find-next", "↓", count.Total > 0).OnClick(func() { onStep(true) })
			ui.Box(c).Grow(1).Basis(0)
			// The keys, written down, because a gesture nothing on screen
			// names is a gesture nobody finds.
			ui.Row(c).
				Shrink(0).
				Gap(10).
				FontSize(theme.SizeCaption).
				TextColor(colors.TextMuted.Color()).
				Children(func() {
					ui.Text(c, "⏎ next")
					ui.Text(c, "⇧⏎ previous")
					ui.Text(c, "esc close")
				})
			closeButton := ui.Box(c.Key("find-close")).
				Shrink(0).
				Padding(2, 6).
				Radius(4).
				FontSize(theme.SizeBody).
				TextColor(colors.TextMuted.Color()).
				Cursor(ui.CursorPointer)
			if closeButton.Hovered() {
				closeButton.Background(colors.Hairline.Color()).TextColor(colors.Text.Color())
			}
			closeButton.OnClick(func() { onClose() }).Children(func() { ui.Text(c, "×") })
		})
}

// stepButton is one of the two steps. It goes faint rather than away with
// nothing to step to, so the strip does not reflow as the count reaches
// zero.
func stepButton(c *ui.Context, id, glyph string, live bool) ui.Element {
	colors := &theme.Current().Colors
	button := ui.Row(c.Key(id)).
		Shrink(0).
		Width(22).
		Height(20).
		AlignItems(ui.Center).
		Justify(ui.Center).
		Border(1, colors.BorderStrong.Color()).
		Radius(5).
		Background(colors.Elevated.Color()).
		FontSize(theme.SizeMeta).
		FontWeight(500)
	if live {
		button.TextColor(colors.TextSecondary.Color()).Cursor(ui.CursorPointer)
		if button.Hovered() {
			button.BorderColor(colors.TextFaint.Color())
		}
	} else {
		button.TextColor(colors.TextFaint.Color())
	}
	return button.Children(func() { ui.Text(c, glyph) })
}
