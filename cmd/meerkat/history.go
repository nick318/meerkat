// The query history list: every statement this connection has run,
// newest first, under a day heading.
//
// The rows are flattened the way the sidebar's catalog is, headings and
// runs sharing one height, because `uniform_list` measures once and only
// lays out what is on screen. A history of 500 runs then costs the same
// as a history of five.
//
// Grouping is by *local* day, not by elapsed seconds: a run at 00:05
// belongs to today even though it happened minutes ago, and one at 23:55
// yesterday belongs to yesterday even though it is nearer in time.
//
// Port notes: chrono's `NaiveDate` is a `time.Time` here, the local
// calendar day as midnight UTC (`naiveDateOf`), so the day arithmetic is
// a count of calendar days (`naiveNumDays`) exactly as chrono's, whatever
// daylight saving does to the length of a local day.

package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/nick318/meerkat/internal/storage"
	"github.com/nick318/meerkat/internal/theme"
	"github.com/nick318/meerkat/internal/uikit"
)

// HistoryRowHeight is one height for headings and runs alike, so the list
// can virtualize. A run card is 38px; the remaining 8px is the gap under
// it.
const HistoryRowHeight float32 = 46.

const historyCardHeight float32 = 38.

// Column widths from the comp: statement, rows, timing, clock.
const (
	historyRowsWidth   float32 = 88.
	historyTimingWidth float32 = 78.
	historyClockWidth  float32 = 92.
)

// Past this a run reads as slow and takes the deep accent, as the comp
// paints its 1.9 s row.
const historySlowMillis uint64 = 1_000

// OnOpenRun is called with the statement of the run the user clicked.
type OnOpenRun func(statement string)

// HistoryRow is the list, flattened: a heading per day, then that day's
// runs. One of HistoryDay or *RunRow.
type HistoryRow interface {
	isHistoryRow()
}

// HistoryDay is `HistoryRow::Day`.
type HistoryDay struct {
	Label string
}

// RunRow is one run, already rendered down to the four strings the card
// shows.
type RunRow struct {
	Statement string
	// What the second column says: `6 rows`, or the reason it failed.
	detail string
	// What the third column says: `128 ms`, or `error`.
	timing string
	// Local clock time, `09:41`.
	clock  string
	failed bool
	slow   bool
}

func (HistoryDay) isHistoryRow() {}
func (*RunRow) isHistoryRow()    {}

// HistoryFlatten groups the runs by the local day they ran on. The order
// the store returned them in is kept: newest first, within the day as
// well. (Rust: `history::flatten`.)
func HistoryFlatten(runs []storage.QueryRun, today time.Time) []HistoryRow {
	rows := make([]HistoryRow, 0, len(runs)+4)
	var current *time.Time
	for _, run := range runs {
		at, ok := localTimestamp(run.RanAt)
		if !ok {
			continue
		}
		day := naiveDateOf(at)
		if current == nil || !current.Equal(day) {
			rows = append(rows, HistoryDay{Label: historyDayLabel(day, today)})
			current = &day
		}
		detail := ""
		switch {
		case run.Error != nil:
			detail = historyFirstLine(*run.Error)
		// Rows changed win the line over rows returned. A statement
		// that changed rows returned none, so the count is the only
		// thing about it worth a column — and `0 rows` said nothing
		// at all about what it did.
		case run.Affected != nil:
			detail = historyChangedLabel(*run.Affected)
		case run.RowCount != nil:
			detail = historyRowsLabel(*run.RowCount)
		}
		timing := ""
		switch {
		case run.Error != nil:
			timing = "error"
		case run.ElapsedMs != nil:
			timing = uikit.FormatMillis(*run.ElapsedMs)
		}
		rows = append(rows, &RunRow{
			Statement: run.Statement,
			detail:    detail,
			timing:    timing,
			clock:     at.Format("15:04"),
			failed:    run.Error != nil,
			slow:      run.ElapsedMs != nil && *run.ElapsedMs >= historySlowMillis,
		})
	}
	return rows
}

// `TODAY`, `YESTERDAY`, then the date itself. The near days are named
// because that is how anyone looking for the query they just ran reads
// the list.
func historyDayLabel(day, today time.Time) string {
	switch naiveNumDays(today, day) {
	case 0:
		return "TODAY"
	case 1:
		return "YESTERDAY"
	default:
		return fmt.Sprintf(
			"%s %d %s",
			strings.ToUpper(day.Format("Mon")),
			day.Day(),
			strings.ToUpper(day.Format("Jan")),
		)
	}
}

// The row count as the comp writes it: `1 row`, `6 rows`, `18.4k rows`.
func historyRowsLabel(count uint64) string {
	switch count {
	case 1:
		return "1 row"
	default:
		return fmt.Sprintf("%s rows", uikit.FormatCount(count))
	}
}

// Rows changed, said so: `1 row changed`, `6 rows changed`. The word is
// there because the column otherwise reads as rows returned, and this run
// returned none.
func historyChangedLabel(count uint64) string {
	switch count {
	case 0:
		return "no rows changed"
	case 1:
		return "1 row changed"
	default:
		return fmt.Sprintf("%s rows changed", uikit.FormatCount(count))
	}
}

// A driver error runs to several lines; the card has one narrow column.
func historyFirstLine(error string) string {
	line, _, _ := strings.Cut(error, "\n")
	line = strings.TrimSuffix(line, "\r")
	return strings.TrimSpace(line)
}

// HistoryToday is today, in the local zone. The screen reads it once per
// load, so the headings do not shift while the list is on screen.
// (Rust: `history::today`.)
func HistoryToday() time.Time {
	return naiveDateOf(time.Now())
}

// naiveDateOf is chrono's `date_naive()`: the calendar day `t` falls on in
// its own zone, held as midnight UTC of that day so it carries no zone of
// its own.
func naiveDateOf(t time.Time) time.Time {
	year, month, day := t.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// naiveNumDays is chrono's `(later - earlier).num_days()` over two
// calendar days: a count of days, never of 24-hour stretches.
func naiveNumDays(later, earlier time.Time) int64 {
	return naiveDaysFromCivil(later) - naiveDaysFromCivil(earlier)
}

// naiveDaysFromCivil is the day number of `t`'s calendar day (days since
// 1970-01-01 in the proleptic Gregorian calendar), exact for any year.
func naiveDaysFromCivil(t time.Time) int64 {
	year, month, day := t.Date()
	y := int64(year)
	m := int64(month)
	d := int64(day)
	if m <= 2 {
		y--
	}
	era := y
	if era < 0 {
		era -= 399
	}
	era /= 400
	yoe := y - era*400
	mp := (m + 9) % 12
	doy := (153*mp+2)/5 + d - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return era*146097 + doe - 719468
}

// The range chrono 0.4.45 can hold: `NaiveDate::MIN` is -262143-01-01 and
// `NaiveDate::MAX` is 262142-12-31.
var (
	chronoMinTimestamp = time.Date(-262143, time.January, 1, 0, 0, 0, 0, time.UTC).Unix()
	chronoMaxTimestamp = time.Date(262142, time.December, 31, 23, 59, 59, 0, time.UTC).Unix()
)

// localTimestamp is `Local.timestamp_opt(secs, 0).single()`: the instant
// in the local zone, or false where chrono would answer `None` (a
// timestamp outside the range it can hold).
func localTimestamp(secs int64) (time.Time, bool) {
	if secs < chronoMinTimestamp || secs > chronoMaxTimestamp {
		return time.Time{}, false
	}
	return time.Unix(secs, 0).In(time.Local), true
}

// HistoryRowView is one row of the flattened list (Rust: `history_row`).
// Free-standing for the same reason the catalog's row is: the list's
// render closure outlives the borrow of the view it came from.
func HistoryRowView(
	c *ui.Context,
	ix int,
	row HistoryRow,
	open OnOpenRun,
	colors *theme.ThemeColors,
) ui.Element {
	switch row := row.(type) {
	case HistoryDay:
		return ui.Row(c).
			Height(HistoryRowHeight).
			AlignItems(ui.End).
			Padding(0, 0, 9, 0).
			Children(func() { uikit.SectionLabel(c, row.Label) })
	case *RunRow:
		run := row
		statement := run.Statement
		cell := func(text string, color theme.Hsla) ui.Element {
			return ui.Box(c).
				FontSize(theme.SizeCaption).
				TextColor(color.Color()).
				Children(func() { ui.Text(c, text).SingleLine() })
		}

		return ui.Box(c).
			Height(HistoryRowHeight).
			Children(func() {
				border := colors.Border
				background := colors.Elevated
				if run.failed {
					border = colors.ErrorBorder
					background = colors.ErrorSurface
				}
				card := ui.Row(c.Key(fmt.Sprintf("history-run-%d", ix))).
					Height(historyCardHeight).
					AlignItems(ui.Center).
					Gap(14).
					Padding(0, 12).
					Border(1, border.Color()).
					Radius(7).
					Background(background.Color()).
					Cursor(ui.CursorPointer)
				if card.Hovered() {
					if run.failed {
						card.BorderColor(colors.ErrorFaint.Color())
					} else {
						card.BorderColor(colors.TextFaint.Color())
					}
				}
				card.OnClick(func() { open(statement) })
				card.Children(func() {
					statementColor := colors.TextBody
					if run.failed {
						statementColor = colors.Error
					}
					ui.Box(c).
						Grow(1).Basis(0).
						MinWidth(0).
						FontSize(theme.SizeMeta).
						TextColor(statementColor.Color()).
						Children(func() { ui.Text(c, run.Statement).SingleLine() })
					ui.Box(c).Width(historyRowsWidth).Shrink(0).Children(func() {
						detailColor := colors.TextMuted
						if run.failed {
							detailColor = colors.ErrorSecondary
						}
						cell(run.detail, detailColor)
					})
					ui.Box(c).Width(historyTimingWidth).Shrink(0).Children(func() {
						var timingColor theme.Hsla
						switch {
						case run.failed:
							timingColor = colors.ErrorSecondary
						case run.slow:
							timingColor = colors.AccentDeep
						default:
							timingColor = colors.Ok
						}
						cell(run.timing, timingColor)
					})
					ui.Row(c).
						Width(historyClockWidth).
						Shrink(0).
						AlignItems(ui.Stretch).
						Justify(ui.End).
						Children(func() {
							clockColor := colors.TextFaint
							if run.failed {
								clockColor = colors.ErrorFaint
							}
							cell(run.clock, clockColor)
						})
				})
			})
	}
	return ui.Element{}
}

// HistoryFilterChip is one of the chips over the list. Active is the
// accent; inactive is the same bordered pill the toolbars use. The caller
// reads `Clicked()` off the element it returns.
func HistoryFilterChip(
	c *ui.Context,
	id string,
	label string,
	active bool,
	colors *theme.ThemeColors,
) ui.Element {
	border := colors.BorderStrong
	background := colors.Elevated
	weight := 400
	textColor := colors.TextSecondary
	hoverBorder := colors.TextFaint
	if active {
		border = colors.Accent
		background = colors.Selection
		weight = 500
		textColor = colors.AccentDeep
		hoverBorder = colors.AccentDeep
	}
	chip := ui.Box(c.Key(id)).
		Padding(5, 10).
		Border(1, border.Color()).
		Radius(6).
		Background(background.Color()).
		FontSize(theme.SizeBody).
		FontWeight(weight).
		TextColor(textColor.Color()).
		Cursor(ui.CursorPointer).
		Children(func() { ui.Text(c, label) })
	if chip.Hovered() {
		chip.BorderColor(hoverBorder.Color())
	}
	return chip
}
