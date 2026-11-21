package query

import (
	"slices"
	"testing"
)

func TestANameIsQuotedOnlyWhenABareOneWouldReadAsSomethingElse(t *testing.T) {
	cases := map[string]string{
		"users":       "users",
		"user_2":      "user_2",
		"price$usd":   "price$usd",
		"Users":       `"Users"`,
		"order":       `"order"`,
		"my table":    `"my table"`,
		"2fa":         `"2fa"`,
		`say "hi"`:    `"say ""hi"""`,
		"":            `""`,
		"café":        "café",
		"approved?":   `"approved?"`,
		"tablesample": `"tablesample"`,
	}
	for name, want := range cases {
		if got := QuoteIdentIfNeeded(name); got != want {
			t.Errorf("%q = %s, want %s", name, got, want)
		}
	}
	if !slices.IsSorted(reservedWords) {
		t.Fatal("reservedWords is not sorted")
	}
}
