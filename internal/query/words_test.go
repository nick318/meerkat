package query

import (
	"slices"
	"strings"
	"testing"
	"unicode"
)

// The verb readers take a word or two off the front of a statement, and
// what they take is what splitting the whole statement would have given.
func TestLeadingFieldsAreTheFrontOfTheSplit(t *testing.T) {
	preds := []func(rune) bool{
		func(c rune) bool { return unicode.IsSpace(c) || c == ';' },
		func(c rune) bool { return unicode.IsSpace(c) || c == ';' || c == '(' },
	}
	for _, body := range []string{
		"", " ", ";;", "begin", "  begin;", "start transaction", "rollback to sp",
		"insert(a) values", "\xff\xfe drop", "é ü", "a b c", "create table t (a int);",
	} {
		for _, pred := range preds {
			all := strings.FieldsFunc(body, pred)
			for n := 1; n <= 3; n++ {
				want := all[:min(n, len(all))]
				if got := leadingFields(body, pred, n); !slices.Equal(got, want) && !(len(got) == 0 && len(want) == 0) {
					t.Errorf("leadingFields(%q, %d) = %q, want %q", body, n, got, want)
				}
			}
		}
	}
}

// A statement a megabyte long is read for its verb off its front.
func TestAVerbIsReadOffTheFrontOfAVeryLongStatement(t *testing.T) {
	tail := strings.Repeat("(1, 'x'), ", 100_000)
	if v, ok := DdlVerbOf("-- load\n  CREATE TABLE t AS VALUES " + tail); !ok || v != DdlVerbCreate {
		t.Errorf("DdlVerbOf = %v, %v", v, ok)
	}
	if v, ok := CommandVerbOf("insert into t values " + tail); !ok || v != CommandVerbInsert {
		t.Errorf("CommandVerbOf = %v, %v", v, ok)
	}
	if v, ok := TransactionVerb("start transaction " + tail); !ok || v != TxVerbBegin {
		t.Errorf("TransactionVerb = %v, %v", v, ok)
	}
	if !ChangesNames("set search_path = " + tail) {
		t.Errorf("ChangesNames missed a SET")
	}
}

func BenchmarkDdlVerbOfLongStatement(b *testing.B) {
	statement := "insert into t values " + strings.Repeat("(1, 'x'), ", 100_000)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		DdlVerbOf(statement)
	}
}
