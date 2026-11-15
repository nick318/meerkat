package dbclient

import (
	"bytes"
	"strings"
	"testing"
)

func small() Limits {
	return Limits{
		MaxBytes:     4096,
		MaxCellBytes: 8,
	}
}

func row(n int64) []Value {
	return []Value{ValueInt(n)}
}

// The point of the whole design: what bounds a result is its size, so
// a narrow result runs to a row count no one would cap it at. Two
// `bigint` columns against the real budget: well past 120,000 rows.
func TestANarrowResultReachesALargeRowCount(t *testing.T) {
	sink := NewRowSink(DefaultLimits())
	var kept int64
	for sink.Push([]Value{ValueInt(kept), ValueInt(kept)}) {
		kept++
		if kept > 200_000 {
			break
		}
	}
	if kept <= 120_000 {
		t.Fatalf("only %d narrow rows fit the budget", kept)
	}
	if sink.Truncated() {
		t.Fatalf("a narrow result of %d rows was capped", kept)
	}
}

// The shape the cap is for. Rows of a megabyte each stop long before
// any row count would have saved the process.
func TestAWideResultIsCappedEarly(t *testing.T) {
	sink := NewRowSink(DefaultLimits())
	kept := 0
	for sink.Push([]Value{ValueText(strings.Repeat("x", MaxCellBytes))}) {
		kept++
		if kept >= 1000 {
			t.Fatalf("a megabyte a row should not reach %d rows", kept)
		}
	}
	if !sink.Truncated() {
		t.Fatal("the result was not marked truncated")
	}
	if sink.Bytes() > MaxBytes {
		t.Fatalf("bytes = %d, over the cap", sink.Bytes())
	}
}

func TestAResultThatEndsOnTheCapIsWhole(t *testing.T) {
	// Room for exactly three of these rows and nothing more.
	one := sizeOfVec + sizeOfValue
	sink := NewRowSink(Limits{
		MaxBytes:     one * 3,
		MaxCellBytes: 8,
	})
	for n := int64(0); n < 3; n++ {
		if !sink.Push(row(n)) {
			t.Fatalf("row %d was refused", n)
		}
	}
	result := sink.Finish(0)
	if len(result.Rows) != 3 {
		t.Fatalf("rows = %d", len(result.Rows))
	}
	if result.Truncated {
		t.Fatal("a result that ends on the cap was marked truncated")
	}
}

func TestTheRowPastTheCapIsDroppedAndReported(t *testing.T) {
	one := sizeOfVec + sizeOfValue
	sink := NewRowSink(Limits{
		MaxBytes:     one * 3,
		MaxCellBytes: 8,
	})
	for n := int64(0); n < 3; n++ {
		if !sink.Push(row(n)) {
			t.Fatalf("row %d was refused", n)
		}
	}
	if sink.Push(row(3)) {
		t.Fatal("the fourth row was kept")
	}
	result := sink.Finish(0)
	if len(result.Rows) != 3 {
		t.Fatalf("rows = %d", len(result.Rows))
	}
	if !result.Truncated {
		t.Fatal("the result was not marked truncated")
	}
}

// A single value bigger than the whole budget must still render. An
// empty grid would say the query returned nothing, which is a lie.
func TestTheFirstRowIsKeptHoweverBigItIs(t *testing.T) {
	sink := NewRowSink(Limits{
		MaxBytes:     16,
		MaxCellBytes: 1024,
	})
	if !sink.Push([]Value{ValueText(strings.Repeat("x", 500))}) {
		t.Fatal("the first row was refused")
	}
	if sink.Push([]Value{ValueText(strings.Repeat("x", 500))}) {
		t.Fatal("the second row was kept")
	}
	result := sink.Finish(0)
	if len(result.Rows) != 1 {
		t.Fatalf("rows = %d", len(result.Rows))
	}
	if !result.Truncated {
		t.Fatal("the result was not marked truncated")
	}
}

func TestACutCellSaysItWasCut(t *testing.T) {
	sink := NewRowSink(small())
	sink.Push([]Value{ValueText("abcdefghijkl")})
	result := sink.Finish(0)
	if got := result.Rows[0][0]; !got.Equal(ValueText("abcdefgh…")) {
		t.Fatalf("cell = %#v", got)
	}
}

// The cut lands on a character boundary, or the `String` is not UTF-8
// and the truncate panics.
func TestACutCellStaysUtf8(t *testing.T) {
	sink := NewRowSink(small())
	// Three-byte characters, so byte 8 sits inside one.
	sink.Push([]Value{ValueText("日本語です")})
	result := sink.Finish(0)
	if got := result.Rows[0][0]; !got.Equal(ValueText("日本…")) {
		t.Fatalf("cell = %#v", got)
	}
}

func TestBytesAreCutWithoutAMarker(t *testing.T) {
	// Wider than the 16 bytes `display` renders, as the real cap is.
	limits := small()
	limits.MaxCellBytes = 32
	sink := NewRowSink(limits)
	sink.Push([]Value{ValueBytes(bytes.Repeat([]byte{0xAB}, 400))})
	result := sink.Finish(0)
	if got := result.Rows[0][0]; !got.Equal(ValueBytes(bytes.Repeat([]byte{0xAB}, 32))) {
		t.Fatalf("cell = %#v", got)
	}
	// `display` is what marks it, as it does for any long byte string.
	if !strings.HasSuffix(result.Rows[0][0].Display(), "…") {
		t.Fatalf("display = %q", result.Rows[0][0].Display())
	}
}
