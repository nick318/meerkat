package dbpostgres

// Reading a column off the wire.
//
// **Postgres renders every value itself, except the handful worth
// keeping as more than text.** A column can come back in one of two
// formats, and the client picks per column when it executes. In **text**
// the server runs the type's own output function, which is what `psql`
// prints and is right for every type there will ever be — an `interval`,
// an `inet`, an enum, a composite, a range, an array of any of them, a
// date in BC or at `infinity`, a `numeric` that is `NaN`. In **binary**
// each type is a layout of its own that the client has to know.
//
// The first edition asked for binary across the board, because sqlx did,
// and then had to decode by hand everything sqlx could not: arrays,
// `tsvector`, `numeric` — and everything it had no reader for came out as
// `<INTERVAL>`. Worse, the readers it did have were narrower than the
// server: `infinity` and a `numeric` `NaN` failed the whole result, a BC
// date lost its era and `24:00:00` wrapped to midnight. So binary is now
// asked for only where it buys a value the grid treats differently from
// text — a boolean, an integer, a byte string — and every other column is
// the server's own text. See [binaryColumn].

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nick318/meerkat/internal/dbclient"
)

// The wire formats a column can come back in, as the protocol numbers
// them.
const (
	formatText   int16 = 0
	formatBinary int16 = 1
)

// The OIDs of the built-in types read in binary. A domain over one of
// them is described by its base type, so it is read the same way.
const (
	oidBool  uint32 = 16
	oidBytea uint32 = 17
	oidInt8  uint32 = 20
	oidInt2  uint32 = 21
	oidInt4  uint32 = 23
	oidOid   uint32 = 26
	oidXid   uint32 = 28
	oidXid8  uint32 = 5069
)

// binaryColumn says whether a column of this type is asked for in binary.
//
// A boolean and an integer become a [dbclient.ValueBool] or
// [dbclient.ValueInt], and a `bytea` a [dbclient.ValueBytes], which the
// grid paints as binary rather than as text. A float is not on the list:
// Postgres prints it the shortest way that reads back the same, and
// spells `Infinity` and `NaN` as the user typed them; a `real` widened to
// a Go `float64` would print `0.1` as `0.10000000149011612`.
func binaryColumn(oid uint32) bool {
	switch oid {
	case oidBool, oidBytea, oidInt8, oidInt2, oidInt4, oidOid, oidXid, oidXid8:
		return true
	}
	return false
}

// resultFormats is the format code for every column of a described
// statement, in order.
func resultFormats(oids []uint32) []int16 {
	formats := make([]int16, len(oids))
	for i, oid := range oids {
		if binaryColumn(oid) {
			formats[i] = formatBinary
		}
	}
	return formats
}

// columnDecode names the column a value failed to read in.
func columnDecode(index int, source error) error {
	return fmt.Errorf("error occurred while decoding column %d: %w", index, source)
}

// decode reads column `i` of a row. `raw` is nil for a NULL; `format` is
// the column's wire format and `oid` its type.
func decode(raw []byte, format int16, oid uint32, i int) (dbclient.Value, error) {
	if raw == nil {
		return dbclient.ValueNull{}, nil
	}
	// A database whose encoding is `SQL_ASCII` sends its bytes as they
	// are stored, whatever `client_encoding` says, so text that is not
	// UTF-8 is drawn with a replacement character rather than failing
	// every row it is in.
	if format == formatText {
		return dbclient.ValueText(fromUTF8Lossy(raw)), nil
	}
	switch oid {
	case oidBool:
		if len(raw) != 1 {
			return nil, columnDecode(i, fmt.Errorf("expected 1 byte on the wire, got %d", len(raw)))
		}
		return dbclient.ValueBool(raw[0] != 0), nil
	case oidInt2:
		word, err := fixed(raw, 2)
		if err != nil {
			return nil, columnDecode(i, err)
		}
		return dbclient.ValueInt(int64(int16(binary.BigEndian.Uint16(word)))), nil
	case oidInt4:
		word, err := fixed(raw, 4)
		if err != nil {
			return nil, columnDecode(i, err)
		}
		return dbclient.ValueInt(int64(int32(binary.BigEndian.Uint32(word)))), nil
	case oidInt8:
		word, err := fixed(raw, 8)
		if err != nil {
			return nil, columnDecode(i, err)
		}
		return dbclient.ValueInt(int64(binary.BigEndian.Uint64(word))), nil
	// An `oid` and an `xid` are unsigned 32 bits, so they always fit.
	case oidOid, oidXid:
		word, err := fixed(raw, 4)
		if err != nil {
			return nil, columnDecode(i, err)
		}
		return dbclient.ValueInt(int64(binary.BigEndian.Uint32(word))), nil
	case oidXid8:
		word, err := fixed(raw, 8)
		if err != nil {
			return nil, columnDecode(i, err)
		}
		return transactionID(binary.BigEndian.Uint64(word)), nil
	case oidBytea:
		bytes := make([]byte, len(raw))
		copy(bytes, raw)
		return dbclient.ValueBytes(bytes), nil
	}
	// Nothing else is asked for in binary, so nothing else arrives in it;
	// a value that does is shown as the bytes it is rather than guessed at.
	bytes := make([]byte, len(raw))
	copy(bytes, raw)
	return dbclient.ValueBytes(bytes), nil
}

// fixed is the bytes, when there are exactly `n` of them.
func fixed(bytes []byte, n int) ([]byte, error) {
	if len(bytes) != n {
		return nil, fmt.Errorf("expected %d bytes on the wire, got %d", n, len(bytes))
	}
	return bytes, nil
}

// transactionID: a transaction id is 64 bits *unsigned*, so a value past
// `math.MaxInt64` has no [dbclient.ValueInt] to go in. No cluster has ever
// counted that far, but rendering it as a negative number would be a lie
// rather than a limit, so those digits go through as text.
func transactionID(id uint64) dbclient.Value {
	if id > math.MaxInt64 {
		return dbclient.ValueText(strconv.FormatUint(id, 10))
	}
	return dbclient.ValueInt(int64(id))
}

// fromUTF8Lossy turns each maximal invalid subsequence into one U+FFFD,
// where the standard library's own decoder draws the line.
func fromUTF8Lossy(bytes []byte) string {
	if utf8.Valid(bytes) {
		return string(bytes)
	}
	var out strings.Builder
	for len(bytes) > 0 {
		r, size := utf8.DecodeRune(bytes)
		if r != utf8.RuneError || size > 1 {
			out.Write(bytes[:size])
			bytes = bytes[size:]
			continue
		}
		out.WriteRune(utf8.RuneError)
		bytes = bytes[maximalSubpart(bytes):]
	}
	return out.String()
}

// maximalSubpart is how many bytes at the front of `bytes` form the
// longest prefix of a well-formed sequence — at least one. The bytes start
// with an invalid sequence.
func maximalSubpart(bytes []byte) int {
	first := bytes[0]
	var width int
	lo, hi := byte(0x80), byte(0xBF)
	switch {
	case first >= 0xC2 && first <= 0xDF:
		width = 2
	case first == 0xE0:
		width, lo = 3, 0xA0
	case first >= 0xE1 && first <= 0xEC, first == 0xEE, first == 0xEF:
		width = 3
	case first == 0xED:
		width, hi = 3, 0x9F
	case first == 0xF0:
		width, lo = 4, 0x90
	case first >= 0xF1 && first <= 0xF3:
		width = 4
	case first == 0xF4:
		width, hi = 4, 0x8F
	default:
		return 1
	}
	n := 1
	for n < width && n < len(bytes) {
		b := bytes[n]
		if n == 1 {
			if b < lo || b > hi {
				break
			}
		} else if b < 0x80 || b > 0xBF {
			break
		}
		n++
	}
	return n
}
