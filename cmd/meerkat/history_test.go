package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nick318/meerkat/internal/storage"
)

func historyRun(statement string, ranAt int64) storage.QueryRun {
	elapsed := uint64(128)
	rows := uint64(6)
	return storage.QueryRun{
		Id:        1,
		Statement: statement,
		RanAt:     ranAt,
		Source:    storage.RunSourceUser,
		ElapsedMs: &elapsed,
		RowCount:  &rows,
		Affected:  nil,
		Error:     nil,
	}
}

// Local noon on a day, so a timezone shift cannot move the run into
// the day before or after it.
func historyNoon(daysAgo int) int64 {
	year, month, day := time.Now().Date()
	return time.Date(year, month, day-daysAgo, 12, 0, 0, 0, time.Local).Unix()
}

func historyOnlyRun(t *testing.T, rows []HistoryRow) *RunRow {
	t.Helper()
	row, ok := rows[1].(*RunRow)
	if !ok {
		t.Fatal("expected a run")
	}
	return row
}

func TestRunsSitUnderTheDayTheyRanOn(t *testing.T) {
	today := naiveDateOf(time.Now())
	rows := HistoryFlatten(
		[]storage.QueryRun{
			historyRun("select 1", historyNoon(0)),
			historyRun("select 2", historyNoon(0)),
			historyRun("select 3", historyNoon(1)),
			historyRun("select 4", historyNoon(9)),
		},
		today,
	)
	read := []string{}
	for _, row := range rows {
		switch row := row.(type) {
		case HistoryDay:
			read = append(read, fmt.Sprintf("[%s]", row.Label))
		case *RunRow:
			read = append(read, row.Statement)
		}
	}
	// One heading per day, however many runs it holds.
	want := []string{
		"[TODAY]",
		"select 1",
		"select 2",
		"[YESTERDAY]",
		"select 3",
	}
	if !reflect.DeepEqual(read[:5], want) {
		t.Fatalf("read = %q", read)
	}
	if !strings.HasPrefix(read[5], "[") {
		t.Fatalf("read[5] = %q", read[5])
	}
	if read[6] != "select 4" {
		t.Fatalf("read[6] = %q", read[6])
	}
}

func TestAFailedRunReadsAsOneLine(t *testing.T) {
	failed := historyRun("select * from user_setings", historyNoon(0))
	message := "relation \"user_setings\" does not exist\nLINE 1: ..."
	failed.Error = &message
	failed.ElapsedMs = nil
	failed.RowCount = nil

	rows := HistoryFlatten([]storage.QueryRun{failed}, naiveDateOf(time.Now()))
	row := historyOnlyRun(t, rows)
	if row.detail != "relation \"user_setings\" does not exist" {
		t.Fatalf("detail = %q", row.detail)
	}
	if row.timing != "error" {
		t.Fatalf("timing = %q", row.timing)
	}
	if !row.failed {
		t.Fatal("not failed")
	}
}

// A run that changed rows says so, and says it in a word the column
// cannot be read the other way round: the column otherwise means rows
// returned, and this run returned none.
func TestARunThatChangedRowsSaysSoRatherThanCountingNone(t *testing.T) {
	updated := historyRun("update t set a = 1", historyNoon(0))
	zero := uint64(0)
	one := uint64(1)
	updated.RowCount = &zero
	updated.Affected = &one
	rows := HistoryFlatten([]storage.QueryRun{updated}, naiveDateOf(time.Now()))
	if got := historyOnlyRun(t, rows).detail; got != "1 row changed" {
		t.Fatalf("detail = %q", got)
	}

	if got := historyChangedLabel(0); got != "no rows changed" {
		t.Fatalf("got %q", got)
	}
	if got := historyChangedLabel(18_412); got != "18.4k rows changed" {
		t.Fatalf("got %q", got)
	}

	// A run that returned rows keeps the count it always had.
	rows = HistoryFlatten([]storage.QueryRun{historyRun("select 1", historyNoon(0))}, naiveDateOf(time.Now()))
	if got := historyOnlyRun(t, rows).detail; got != "6 rows" {
		t.Fatalf("detail = %q", got)
	}
}

func TestCountsAndTimingsReadAsTheCompWritesThem(t *testing.T) {
	if got := historyRowsLabel(1); got != "1 row" {
		t.Fatalf("got %q", got)
	}
	if got := historyRowsLabel(200); got != "200 rows" {
		t.Fatalf("got %q", got)
	}
	if got := historyRowsLabel(18_412); got != "18.4k rows" {
		t.Fatalf("got %q", got)
	}

	slow := historyRun("select 1", historyNoon(0))
	elapsed := uint64(1_900)
	slow.ElapsedMs = &elapsed
	rows := HistoryFlatten([]storage.QueryRun{slow}, naiveDateOf(time.Now()))
	row := historyOnlyRun(t, rows)
	if row.timing != "1.9 s" {
		t.Fatalf("timing = %q", row.timing)
	}
	if !row.slow {
		t.Fatal("not slow")
	}
}
