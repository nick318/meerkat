package dbpostgres

// The value decoding half of `crates/db_postgres/src/lib.rs`: `decode`
// down to `format_offset_date_time`, in the Rust file's order. A run is
// prepared and asks for every column in binary, as sqlx's was, so every
// reading here starts from the bytes the server sent.
//
// Go port: sqlx did part of this work itself — it named the built-in
// types, decoded the scalars it knew, and rendered them through `time`,
// `bigdecimal`, `uuid` and `serde_json`. Those renderings are what the
// Rust build painted, so they are reproduced byte for byte in
// `sqlxcompat.go`; this file keeps the Rust functions and their order.

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nick318/meerkat/internal/dbclient"
)

// columnDecode is sqlx's `Error::ColumnDecode`, which every `try_get` in
// `decode` raised through its `?`.
func columnDecode(index int, source error) error {
	return fmt.Errorf("error occurred while decoding column %d: %w", index, source)
}

// decode reads column `i` of a row. `raw` is nil for a NULL; `format` is
// the column's wire format (1 binary, 0 text); `typeInfo` is the type the
// column was resolved to, as sqlx resolved it.
func decode(raw []byte, format int16, typeInfo *pgTypeInfo, i int) (dbclient.Value, error) {
	if raw == nil {
		return dbclient.ValueNull{}, nil
	}
	typeName := typeInfo.name

	switch typeName {
	case "BOOL":
		value, err := sqlxBool(raw)
		if err != nil {
			return nil, columnDecode(i, err)
		}
		return dbclient.ValueBool(value), nil
	case "INT2":
		value, err := sqlxInt(raw, 2)
		if err != nil {
			return nil, columnDecode(i, err)
		}
		return dbclient.ValueInt(value), nil
	case "INT4":
		value, err := sqlxInt(raw, 4)
		if err != nil {
			return nil, columnDecode(i, err)
		}
		return dbclient.ValueInt(value), nil
	case "INT8":
		value, err := sqlxInt(raw, 8)
		if err != nil {
			return nil, columnDecode(i, err)
		}
		return dbclient.ValueInt(value), nil
	// Go port: `row.try_get::<i64, _>(i).unwrap_or_default()`. sqlx's
	// `i64` is compatible with `INT8` alone, so the `try_get` on an `OID`
	// column always failed its type check and the default — zero — is
	// what the Rust build painted. Kept as it was: a mechanical port.
	case "OID":
		return dbclient.ValueInt(0), nil
	case "FLOAT4":
		if len(raw) != 4 {
			return nil, columnDecode(i, fmt.Errorf("expected 4 bytes on the wire, got %d", len(raw)))
		}
		return dbclient.ValueFloat(float64(math.Float32frombits(binary.BigEndian.Uint32(raw)))), nil
	case "FLOAT8":
		if len(raw) != 8 {
			return nil, columnDecode(i, fmt.Errorf("expected 8 bytes on the wire, got %d", len(raw)))
		}
		return dbclient.ValueFloat(math.Float64frombits(binary.BigEndian.Uint64(raw))), nil
	// NUMERIC has no lossless primitive, so render the decimal text.
	// sqlx rebuilds the scale from the wire digits and ignores the
	// declared one, so `numeric(10, 2)` 1280.00 renders as "1280".
	// The value stays exact; only trailing zeroes are lost.
	case "NUMERIC":
		text, err := bigDecimalString(raw)
		if err != nil {
			return nil, columnDecode(i, err)
		}
		return dbclient.ValueText(text), nil
	case "TEXT", "VARCHAR", "BPCHAR", "CHAR", "NAME", "CITEXT", "UNKNOWN":
		text, err := strictUTF8(raw)
		if err != nil {
			return nil, columnDecode(i, err)
		}
		return dbclient.ValueText(text), nil
	case "UUID":
		text, err := uuidString(raw)
		if err != nil {
			return nil, columnDecode(i, err)
		}
		return dbclient.ValueText(text), nil
	case "TIMESTAMPTZ":
		dt, err := sqlxPrimitiveDateTime(raw)
		if err != nil {
			return nil, err
		}
		return dbclient.ValueText(formatOffsetDateTime(dt)), nil
	case "TIMESTAMP":
		dt, err := sqlxPrimitiveDateTime(raw)
		if err != nil {
			return nil, err
		}
		return dbclient.ValueText(formatPrimitiveDateTime(dt)), nil
	case "DATE":
		date, err := sqlxDate(raw)
		if err != nil {
			return nil, err
		}
		return dbclient.ValueText(formatDate(date)), nil
	case "TIME":
		value, err := sqlxTime(raw)
		if err != nil {
			return nil, columnDecode(i, err)
		}
		return dbclient.ValueText(formatTime(value)), nil
	case "JSON", "JSONB":
		text, err := sqlxJSON(raw, typeName == "JSONB")
		if err != nil {
			return nil, columnDecode(i, err)
		}
		return dbclient.ValueText(text), nil
	case "BYTEA":
		bytes := make([]byte, len(raw))
		copy(bytes, raw)
		return dbclient.ValueBytes(bytes), nil
	}
	// Everything sqlx has no decoder for: read the wire bytes.
	return offTheWire(raw, format, typeInfo, typeName)
}

// offTheWire renders a value sqlx cannot decode, from the bytes the server
// sent.
//
// sqlx knows the built-in types by OID and calls everything else by the
// name it reads back from the catalog — `xid8`, `tsvector`. There is no
// `Decode` for those, so `try_get` on any Rust type fails, and the old
// fallback painted `<xid8>` in every cell of the column: the type name
// is the one thing about the value the user already knew.
//
// The wire carries enough to do better. A statement sent unprepared
// comes back in **text** format, which is Postgres's own rendering of
// the value and is right for every type there will ever be. A prepared
// statement — which is the path a run takes — comes back in **binary**,
// where each type is its own layout and nothing generic can be said, so
// the ones worth reading are decoded by hand and the rest keep the type
// name they had.
//
// An **array** is one of those. sqlx decodes `text[]` into a `Vec`, but
// only of one dimension and only of a Rust type named up front, so every
// array went to the placeholder and a `text[]` column painted `<TEXT[]>`
// in every cell. See [array].
func offTheWire(raw []byte, format int16, typeInfo *pgTypeInfo, typeName string) (dbclient.Value, error) {
	bytes := raw
	if format == formatText {
		return dbclient.ValueText(fromUTF8Lossy(bytes)), nil
	}
	// A domain over an array is sent as the array it is.
	arrayType := typeInfo
	for arrayType.kind == kindDomain {
		arrayType = arrayType.elem
	}
	if arrayType.kind == kindArray {
		elementType := arrayType.elem
		items, err := arrayWireOf(bytes)
		if err != nil {
			return nil, err
		}
		if isVector(arrayType.name) {
			return dbclient.ValueText(vector(&items, elementType)), nil
		}
		return dbclient.ValueText(array(&items, elementType)), nil
	}
	// The name comes from the catalog, so it arrives as the server spells
	// it; the built-in names sqlx uses are upper case.
	switch asciiLowercase(typeName) {
	case "xid":
		word, err := fixed(bytes, 4)
		if err != nil {
			return nil, err
		}
		return transactionID(uint64(binary.BigEndian.Uint32(word))), nil
	case "xid8":
		word, err := fixed(bytes, 8)
		if err != nil {
			return nil, err
		}
		return transactionID(binary.BigEndian.Uint64(word)), nil
	case "tsvector":
		text, err := tsvector(bytes)
		if err != nil {
			return nil, err
		}
		return dbclient.ValueText(text), nil
	}
	return dbclient.ValueText(fmt.Sprintf("<%s>", typeName)), nil
}

// The wire formats a column can come back in, as the protocol numbers
// them. Rust's `PgValueFormat`.
const (
	formatText   int16 = 0
	formatBinary int16 = 1
)

// MaxArrayDimensions is Postgres's own limit on an array's dimensions,
// `MAXDIM`. A header that claims more is not an array the server wrote.
const maxArrayDimensions = 6

// arrayWire is an array as it crossed the wire: the length and the lower
// bound of each dimension, and every element in row-major order, `None`
// for a NULL.
type arrayWire struct {
	lengths []int
	lowers  []int32
	items   []arrayItem
}

// arrayItem is Rust's `Option<&[u8]>`: `some` is false for a NULL.
type arrayItem struct {
	some  bool
	bytes []byte
}

// arrayWireOf splits an array's binary form, as `array_send` writes it,
// into its parts.
//
// The wire form is a header — the number of dimensions, a has-nulls
// flag, the element's OID, then a length and a lower bound for each
// dimension — followed by every element, each a length (-1 for NULL)
// and that many bytes. An empty array has no dimensions at all, so a
// dimension of length zero is refused: the server never sends one, and
// `[i32::MAX, 0]` would otherwise be two billion brace pairs around
// nothing. Every element costs at least its four-byte length, which is
// what bounds the count by the bytes that are actually there.
//
// Go port: Rust's `array_wire`, renamed so the type can keep its name.
func arrayWireOf(bytes []byte) (arrayWire, error) {
	wire := wireBytes{bytes}
	n, err := wire.i32()
	if err != nil {
		return arrayWire{}, err
	}
	if n < 0 || int(n) > maxArrayDimensions {
		return arrayWire{}, fmt.Errorf("array has an impossible number of dimensions")
	}
	dimensions := int(n)
	if _, err := wire.take(4); err != nil { // has nulls: every element says so itself
		return arrayWire{}, err
	}
	if _, err := wire.take(4); err != nil { // the element's OID: sqlx already resolved the type
		return arrayWire{}, err
	}

	lengths := make([]int, 0, dimensions)
	lowers := make([]int32, 0, dimensions)
	for range dimensions {
		length, err := wire.i32()
		if err != nil {
			return arrayWire{}, err
		}
		if length <= 0 {
			return arrayWire{}, fmt.Errorf("array dimension has no length")
		}
		lengths = append(lengths, int(length))
		lower, err := wire.i32()
		if err != nil {
			return arrayWire{}, err
		}
		lowers = append(lowers, lower)
	}
	count := 0
	if dimensions != 0 {
		count = 1
		overflow := false
		for _, length := range lengths {
			if count > math.MaxInt/length {
				overflow = true
				break
			}
			count *= length
		}
		if overflow || count > len(wire.rest)/4 {
			return arrayWire{}, fmt.Errorf("array promises more elements than it holds")
		}
	}

	items := make([]arrayItem, 0, count)
	for range count {
		length, err := wire.i32()
		if err != nil {
			return arrayWire{}, err
		}
		if length == -1 {
			items = append(items, arrayItem{})
			continue
		}
		if length < 0 {
			return arrayWire{}, fmt.Errorf("array element has a negative length")
		}
		item, err := wire.take(int(length))
		if err != nil {
			return arrayWire{}, err
		}
		items = append(items, arrayItem{some: true, bytes: item})
	}
	if len(wire.rest) != 0 {
		return arrayWire{}, fmt.Errorf("array has bytes past its last element")
	}
	return arrayWire{
		lengths: lengths,
		lowers:  lowers,
		items:   items,
	}, nil
}

// array renders an array the way `array_out` renders it: `{a,"b c",NULL}`,
// `{{1,2},{3,4}}`, and `[0:1]={1,2}` when a dimension does not start
// at 1.
//
// The text form is the target because it is what the same statement
// paints when it comes back unprepared, and a cell must not read two
// ways depending on how the statement was sent. So a `bool[]` reads
// `{t,f}`, as `psql` shows it, and not the way the scalar `true` does.
//
// Each element is read by the type sqlx resolved for the column, through
// [element]. One it cannot read keeps the type's name, so an array of
// an exotic type still shows its shape — and a value this reader gets
// wrong costs its own element, not the whole result: an error here would
// fail every row of the run.
func array(array *arrayWire, elementType *pgTypeInfo) string {
	var out strings.Builder
	anyOffset := false
	for _, lower := range array.lowers {
		if lower != 1 {
			anyOffset = true
		}
	}
	if anyOffset {
		for k, lower := range array.lowers {
			upper := int64(lower) + int64(array.lengths[k]) - 1
			out.WriteString(fmt.Sprintf("[%d:%d]", lower, upper))
		}
		out.WriteByte('=')
	}
	if len(array.lengths) == 0 {
		out.WriteString("{}")
	} else {
		next := 0
		arrayLevel(&out, array.lengths, array.items, &next, elementType)
	}
	return out.String()
}

// arrayLevel is one level of braces: the elements themselves at the
// innermost dimension, and a brace group per entry of every dimension
// outside it. `next` is the iterator over `items`.
func arrayLevel(out *strings.Builder, lengths []int, items []arrayItem, next *int, elementType *pgTypeInfo) {
	out.WriteByte('{')
	for n := range lengths[0] {
		if n > 0 {
			out.WriteByte(',')
		}
		if len(lengths) > 1 {
			arrayLevel(out, lengths[1:], items, next, elementType)
			continue
		}
		if *next < len(items) && items[*next].some {
			arrayItemTo(out, elementOrName(items[*next].bytes, elementType))
		} else {
			out.WriteString("NULL")
		}
		*next++
	}
	out.WriteByte('}')
}

// isVector says whether a type is `int2vector` or `oidvector`. They are
// arrays on the wire — the catalog files them as such, so sqlx calls them
// one — but their own output functions print them space-separated with no
// braces: `1 2`, as `pg_index.indkey` reads in `psql`.
func isVector(typeName string) bool {
	return strings.EqualFold(typeName, "int2vector") || strings.EqualFold(typeName, "oidvector")
}

func vector(array *arrayWire, elementType *pgTypeInfo) string {
	parts := make([]string, 0, len(array.items))
	for _, item := range array.items {
		if item.some {
			parts = append(parts, elementOrName(item.bytes, elementType))
		} else {
			parts = append(parts, "NULL")
		}
	}
	return strings.Join(parts, " ")
}

// arrayItemTo writes one element, quoted by `array_out`'s rule: an empty
// string, the word `NULL` in any case, and anything holding a brace, a
// comma, a quote, a backslash or white space goes in quotes, with a quote
// or a backslash inside it escaped by a backslash. Otherwise `{"a,b"}`
// would read as two elements and `{"NULL"}` as none.
//
// Go port: Rust's `array_item`, renamed so it does not clash with the
// `arrayItem` type.
func arrayItemTo(out *strings.Builder, item string) {
	quoted := item == "" || strings.EqualFold(item, "null") || strings.ContainsFunc(item, func(character rune) bool {
		switch character {
		case '{', '}', ',', '"', '\\', ' ', '\t', '\n', '\r', '\x0b', '\x0c':
			return true
		}
		return false
	})
	if !quoted {
		out.WriteString(item)
		return
	}
	out.WriteByte('"')
	for _, character := range item {
		if character == '"' || character == '\\' {
			out.WriteByte('\\')
		}
		out.WriteRune(character)
	}
	out.WriteByte('"')
}

func elementOrName(bytes []byte, typeInfo *pgTypeInfo) string {
	text, err := element(bytes, typeInfo)
	if err != nil {
		return fmt.Sprintf("<%s>", typeInfo.name)
	}
	return text
}

// element reads one element of an array, from its binary form, as its
// type's own output function prints it.
//
// The scalar path has sqlx to decode a value, but an element has no
// `PgValueRef` of its own to hand sqlx, so the common types are read by
// hand here. The name is matched the way [decode] matches it — sqlx's
// upper-case names for the built-ins — and the kind covers what has no
// fixed name: an enum's element is its label, and a domain is its base
// type.
func element(bytes []byte, typeInfo *pgTypeInfo) (string, error) {
	switch typeInfo.kind {
	case kindEnum:
		return fromUTF8Lossy(bytes), nil
	case kindDomain:
		return element(bytes, typeInfo.elem)
	}
	name := typeInfo.name
	switch asciiUppercase(name) {
	case "BOOL":
		if len(bytes) != 1 {
			return "", fmt.Errorf("expected 1 byte on the wire, got %d", len(bytes))
		}
		if bytes[0] == 0 {
			return "f", nil
		}
		return "t", nil
	case "INT2":
		word, err := fixed(bytes, 2)
		if err != nil {
			return "", err
		}
		return strconv.FormatInt(int64(int16(binary.BigEndian.Uint16(word))), 10), nil
	case "INT4":
		word, err := fixed(bytes, 4)
		if err != nil {
			return "", err
		}
		return strconv.FormatInt(int64(int32(binary.BigEndian.Uint32(word))), 10), nil
	case "INT8":
		word, err := fixed(bytes, 8)
		if err != nil {
			return "", err
		}
		return strconv.FormatInt(int64(binary.BigEndian.Uint64(word)), 10), nil
	case "OID", "XID":
		word, err := fixed(bytes, 4)
		if err != nil {
			return "", err
		}
		return strconv.FormatUint(uint64(binary.BigEndian.Uint32(word)), 10), nil
	case "XID8":
		word, err := fixed(bytes, 8)
		if err != nil {
			return "", err
		}
		return strconv.FormatUint(binary.BigEndian.Uint64(word), 10), nil
	case "FLOAT4":
		word, err := fixed(bytes, 4)
		if err != nil {
			return "", err
		}
		value := math.Float32frombits(binary.BigEndian.Uint32(word))
		return float(float64(value), rustLowerExp(float64(value), 32), rustDisplay(float64(value), 32), 6), nil
	case "FLOAT8":
		word, err := fixed(bytes, 8)
		if err != nil {
			return "", err
		}
		value := math.Float64frombits(binary.BigEndian.Uint64(word))
		return float(value, rustLowerExp(value, 64), rustDisplay(value, 64), 15), nil
	case "NUMERIC":
		return numeric(bytes)
	// `"char"`, the one-byte type, is the name sqlx spells with its
	// quotes; `CHAR` is `bpchar`.
	case "TEXT", "VARCHAR", "BPCHAR", "CHAR", "\"CHAR\"", "NAME", "CITEXT", "UNKNOWN", "JSON", "XML":
		return fromUTF8Lossy(bytes), nil
	// `jsonb` leads with a format version, which is 1.
	case "JSONB":
		if len(bytes) > 0 && bytes[0] == 1 {
			return fromUTF8Lossy(bytes[1:]), nil
		}
		return "", fmt.Errorf("unknown jsonb format")
	case "UUID":
		return uuidString(bytes)
	case "BYTEA":
		var hex strings.Builder
		hex.WriteString("\\x")
		for _, b := range bytes {
			hex.WriteString(fmt.Sprintf("%02x", b))
		}
		return hex.String(), nil
	case "DATE":
		word, err := fixed(bytes, 4)
		if err != nil {
			return "", err
		}
		switch days := int32(binary.BigEndian.Uint32(word)); days {
		case math.MaxInt32:
			return "infinity", nil
		case math.MinInt32:
			return "-infinity", nil
		default:
			date, era := calendarDate(int64(days))
			return date + era, nil
		}
	case "TIME":
		word, err := fixed(bytes, 8)
		if err != nil {
			return "", err
		}
		return clock(int64(binary.BigEndian.Uint64(word)))
	// `timestamptz` is stored in UTC and sqlx decodes the scalar as
	// UTC, so the element reads the same way, offset and all.
	case "TIMESTAMP", "TIMESTAMPTZ":
		word, err := fixed(bytes, 8)
		if err != nil {
			return "", err
		}
		switch micros := int64(binary.BigEndian.Uint64(word)); micros {
		case math.MaxInt64:
			return "infinity", nil
		case math.MinInt64:
			return "-infinity", nil
		default:
			date, era := calendarDate(divEuclid(micros, microsPerDay))
			time, err := clock(remEuclid(micros, microsPerDay))
			if err != nil {
				return "", err
			}
			offset := ""
			if strings.EqualFold(name, "TIMESTAMPTZ") {
				offset = " +00:00"
			}
			return fmt.Sprintf("%s %s%s%s", date, time, offset, era), nil
		}
	}
	return fmt.Sprintf("<%s>", name), nil
}

const microsPerDay int64 = 86_400_000_000

// calendarDate is the date `days` after 2000-01-01 — day zero of every
// Postgres date and timestamp on the wire — as Postgres prints it: the
// year in at least four digits, and the era apart, because there is no
// year zero and a year before 1 AD prints as `0001 … BC`.
//
// The arithmetic is Howard Hinnant's `civil_from_days`, by hand rather
// than through `time`: Postgres dates run to 5874897 AD and `time`
// stops at 9999, so a date the server is happy to hold would come back
// as an error.
func calendarDate(days int64) (string, string) {
	year, month, day := civilFromDays(days)
	if year <= 0 {
		return fmt.Sprintf("%04d-%02d-%02d", 1-year, month, day), " BC"
	}
	return fmt.Sprintf("%04d-%02d-%02d", year, month, day), ""
}

// civilFromDays is the arithmetic of [calendarDate], apart, so the scalar
// path — sqlx through `time` — can reach the same proleptic calendar with
// the astronomical year (year 0 is 1 BC) that `time` counts in.
func civilFromDays(days int64) (int64, int64, int64) {
	// Days from 0000-03-01, the start of the proleptic cycle.
	z := days + 10_957 + 719_468
	era := divEuclid(z, 146_097)
	dayOfEra := remEuclid(z, 146_097)
	yearOfEra := (dayOfEra - dayOfEra/1460 + dayOfEra/36_524 - dayOfEra/146_096) / 365
	dayOfYear := dayOfEra - (365*yearOfEra + yearOfEra/4 - yearOfEra/100)
	monthIndex := (5*dayOfYear + 2) / 153
	day := dayOfYear - (153*monthIndex+2)/5 + 1
	month := monthIndex - 9
	if monthIndex < 10 {
		month = monthIndex + 3
	}
	year := yearOfEra + era*400
	if month <= 2 {
		year++
	}
	return year, month, day
}

// clock is the time of day `micros` after midnight, the way `format_time`
// writes the scalar: seconds, and the microseconds only when there are
// some. `24:00:00` is a time Postgres accepts, so the end of the day is
// one.
func clock(micros int64) (string, error) {
	if micros < 0 || micros > microsPerDay {
		return "", fmt.Errorf("time of day out of range")
	}
	seconds := micros / 1_000_000
	hour, minute, second := seconds/3600, seconds/60%60, seconds%60
	if micro := micros % 1_000_000; micro != 0 {
		return fmt.Sprintf("%02d:%02d:%02d.%06d", hour, minute, second, micro), nil
	}
	return fmt.Sprintf("%02d:%02d:%02d", hour, minute, second), nil
}

// float is a float the way Postgres prints it: the shortest digits that
// read back as the same value, plain while the exponent is from -4 to one
// below the type's digits (6 for `float4`, 15 for `float8`) and written
// as `1e+15` outside that. `plain` and `scientific` are Rust's own
// shortest renderings, taken from the value's own type, since a
// `float4` widened to `f64` first would print as `0.10000000149011612`.
func float(value float64, scientific, plain string, digits int32) string {
	if math.IsNaN(value) {
		return "NaN"
	}
	if math.IsInf(value, 0) {
		if value > 0.0 {
			return "Infinity"
		}
		return "-Infinity"
	}
	mantissa, exponentText, ok := strings.Cut(scientific, "e")
	if !ok {
		return plain
	}
	exponent, err := strconv.ParseInt(exponentText, 10, 32)
	if err != nil {
		return plain
	}
	if -4 <= exponent && exponent < int64(digits) {
		return plain
	}
	sign := '+'
	if exponent < 0 {
		sign = '-'
		exponent = -exponent
	}
	return fmt.Sprintf("%se%c%02d", mantissa, sign, exponent)
}

// numeric renders `numeric`'s binary form the way `numeric_out` renders
// it.
//
// The wire form is a digit count, a weight, a sign and a display scale,
// then the digits in base 10000, the first one standing for
// 10000^weight. A digit past the ones sent is zero. The display scale is
// what keeps `1280.00` from reading as `1280`: the scalar path loses it
// to `BigDecimal`, but an element has no need to.
func numeric(bytes []byte) (string, error) {
	wire := wireBytes{bytes}
	count, err := wire.u16()
	if err != nil {
		return "", err
	}
	rawWeight, err := wire.u16()
	if err != nil {
		return "", err
	}
	weight := int32(int16(rawWeight))
	sign, err := wire.u16()
	if err != nil {
		return "", err
	}
	rawScale, err := wire.u16()
	if err != nil {
		return "", err
	}
	scale := int(rawScale)
	digits := make([]uint16, 0, count)
	for range int(count) {
		d, err := wire.u16()
		if err != nil {
			return "", err
		}
		digits = append(digits, d)
	}
	if len(wire.rest) != 0 {
		return "", fmt.Errorf("numeric has bytes past its last digit")
	}
	var negative bool
	switch sign {
	case 0x0000:
		negative = false
	case 0x4000:
		negative = true
	case 0xC000:
		return "NaN", nil
	case 0xD000:
		return "Infinity", nil
	case 0xF000:
		return "-Infinity", nil
	default:
		return "", fmt.Errorf("numeric has an unknown sign")
	}
	digit := func(k int32) uint16 {
		if k < 0 || int(k) >= len(digits) {
			return 0
		}
		return digits[k]
	}

	var out strings.Builder
	if negative {
		out.WriteByte('-')
	}
	if weight < 0 {
		out.WriteByte('0')
	} else {
		out.WriteString(strconv.FormatUint(uint64(digit(0)), 10))
		for k := int32(1); k <= weight; k++ {
			out.WriteString(fmt.Sprintf("%04d", digit(k)))
		}
	}
	if scale > 0 {
		var fraction strings.Builder
		k := weight + 1
		for fraction.Len() < scale {
			fraction.WriteString(fmt.Sprintf("%04d", digit(k)))
			k++
		}
		out.WriteByte('.')
		out.WriteString(fraction.String()[:scale])
	}
	return out.String(), nil
}

// wireBytes is a cursor over the bytes of one value that fails, rather
// than panics, when a count promises more than is there.
type wireBytes struct {
	rest []byte
}

func (w *wireBytes) take(n int) ([]byte, error) {
	if len(w.rest) < n {
		return nil, fmt.Errorf("value ended early on the wire")
	}
	head, rest := w.rest[:n], w.rest[n:]
	w.rest = rest
	return head, nil
}

func (w *wireBytes) i32() (int32, error) {
	head, err := w.take(4)
	if err != nil {
		return 0, err
	}
	word, err := fixed(head, 4)
	if err != nil {
		return 0, err
	}
	return int32(binary.BigEndian.Uint32(word)), nil
}

func (w *wireBytes) u16() (uint16, error) {
	head, err := w.take(2)
	if err != nil {
		return 0, err
	}
	word, err := fixed(head, 2)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(word), nil
}

// fixed is Rust's `fixed::<N>`: the bytes, when there are exactly `n` of
// them.
func fixed(bytes []byte, n int) ([]byte, error) {
	if len(bytes) != n {
		return nil, fmt.Errorf("expected %d bytes on the wire, got %d", n, len(bytes))
	}
	return bytes, nil
}

// transactionID: a transaction id is 64 bits *unsigned*, so a value past
// `i64::MAX` has no `Value::Int` to go in. No cluster has ever counted
// that far, but rendering it as a negative number would be a lie rather
// than a limit, so those digits go through as text.
func transactionID(id uint64) dbclient.Value {
	if id > math.MaxInt64 {
		return dbclient.ValueText(strconv.FormatUint(id, 10))
	}
	return dbclient.ValueInt(int64(id))
}

// tsvector renders `tsvector`'s binary form the way `tsvector_out` renders
// it: `'fox':3 'quick':2A`.
//
// The wire form is a count of lexemes, then for each one the lexeme as a
// NUL-terminated string, a count of positions, and that many `u16`s
// holding the position in the low 14 bits and the weight in the top two.
// Weight 3 prints as `A` down to 1 as `C`; 0 is the default and prints as
// nothing. A quote or a backslash inside a lexeme is doubled, as
// Postgres doubles it, or the rendering could not be read back.
func tsvector(bytes []byte) (string, error) {
	take := func(n int) ([]byte, error) {
		if len(bytes) < n {
			return nil, fmt.Errorf("tsvector ended early")
		}
		head, rest := bytes[:n], bytes[n:]
		bytes = rest
		return head, nil
	}

	head, err := take(4)
	if err != nil {
		return "", err
	}
	lexemes := binary.BigEndian.Uint32(head)
	var out strings.Builder
	for range lexemes {
		end := -1
		for k, b := range bytes {
			if b == 0 {
				end = k
				break
			}
		}
		if end < 0 {
			return "", fmt.Errorf("tsvector lexeme is not terminated")
		}
		lexemeBytes, err := take(end)
		if err != nil {
			return "", err
		}
		lexeme := fromUTF8Lossy(lexemeBytes)
		if _, err := take(1); err != nil {
			return "", err
		}

		if out.Len() != 0 {
			out.WriteByte(' ')
		}
		out.WriteByte('\'')
		for _, character := range lexeme {
			if character == '\'' || character == '\\' {
				out.WriteRune(character)
			}
			out.WriteRune(character)
		}
		out.WriteByte('\'')

		head, err := take(2)
		if err != nil {
			return "", err
		}
		positions := binary.BigEndian.Uint16(head)
		for n := range positions {
			head, err := take(2)
			if err != nil {
				return "", err
			}
			entry := binary.BigEndian.Uint16(head)
			if n == 0 {
				out.WriteByte(':')
			} else {
				out.WriteByte(',')
			}
			out.WriteString(strconv.FormatUint(uint64(entry&0x3fff), 10))
			switch entry >> 14 {
			case 3:
				out.WriteByte('A')
			case 2:
				out.WriteByte('B')
			case 1:
				out.WriteByte('C')
			}
		}
	}
	return out.String(), nil
}

// The `time` crate's `Display` output is not ISO-8601, and pulling in a
// format description would add a direct dependency for three lines, so
// build the ISO strings by hand.
//
// Go port: `timeDate`, `timeTime` and `primitiveDateTime` stand for the
// `time` crate's `Date`, `Time` and `PrimitiveDateTime`; sqlx built them
// (see `sqlxcompat.go`) and these functions printed them.

func formatDate(date timeDate) string {
	return fmt.Sprintf("%04d-%02d-%02d", date.year, date.month, date.day)
}

func formatTime(time timeTime) string {
	if time.micro == 0 {
		return fmt.Sprintf("%02d:%02d:%02d", time.hour, time.minute, time.second)
	}
	return fmt.Sprintf("%02d:%02d:%02d.%06d", time.hour, time.minute, time.second, time.micro)
}

func formatPrimitiveDateTime(dt primitiveDateTime) string {
	return fmt.Sprintf("%s %s", formatDate(dt.date), formatTime(dt.time))
}

// formatOffsetDateTime prints an `OffsetDateTime`. sqlx decoded every
// `timestamptz` with `assume_utc`, so the offset is always zero, and the
// sign and the two fields below come out as `+00:00`.
func formatOffsetDateTime(dt primitiveDateTime) string {
	hours, minutes := 0, 0
	sign := '+'
	return fmt.Sprintf("%s %s %c%02d:%02d", formatDate(dt.date), formatTime(dt.time), sign, hours, minutes)
}

// --- Small helpers Rust had in its standard library. ---

// divEuclid is `i64::div_euclid`.
func divEuclid(a, b int64) int64 {
	q := a / b
	if a%b < 0 {
		if b > 0 {
			q--
		} else {
			q++
		}
	}
	return q
}

// remEuclid is `i64::rem_euclid`.
func remEuclid(a, b int64) int64 {
	r := a % b
	if r < 0 {
		if b < 0 {
			r -= b
		} else {
			r += b
		}
	}
	return r
}

// asciiLowercase is `str::to_ascii_lowercase`.
func asciiLowercase(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// asciiUppercase is `str::to_ascii_uppercase`.
func asciiUppercase(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'a' <= c && c <= 'z' {
			b[i] = c - ('a' - 'A')
		}
	}
	return string(b)
}

// rustLowerExp is Rust's `format!("{value:e}")`: the shortest digits that
// read back as the same value of the given width, as `1.234567e6`.
func rustLowerExp(value float64, bitSize int) string {
	text := strconv.FormatFloat(value, 'e', -1, bitSize)
	mantissa, exponent, ok := strings.Cut(text, "e")
	if !ok {
		return text
	}
	n, err := strconv.Atoi(exponent)
	if err != nil {
		return text
	}
	return mantissa + "e" + strconv.Itoa(n)
}

// rustDisplay is Rust's `value.to_string()` for a float: the shortest
// digits, always in plain notation.
func rustDisplay(value float64, bitSize int) string {
	switch {
	case math.IsNaN(value):
		return "NaN"
	case math.IsInf(value, 1):
		return "inf"
	case math.IsInf(value, -1):
		return "-inf"
	}
	return strconv.FormatFloat(value, 'f', -1, bitSize)
}

// fromUTF8Lossy is `String::from_utf8_lossy`: each maximal invalid
// subsequence becomes one U+FFFD, as the standard library's own decoder
// draws the line.
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

// strictUTF8 is `String::from_utf8`, with `Utf8Error`'s wording.
func strictUTF8(bytes []byte) (string, error) {
	if utf8.Valid(bytes) {
		return string(bytes), nil
	}
	validUpTo := 0
	for validUpTo < len(bytes) {
		r, size := utf8.DecodeRune(bytes[validUpTo:])
		if r == utf8.RuneError && size == 1 {
			break
		}
		validUpTo += size
	}
	rest := bytes[validUpTo:]
	length := maximalSubpart(rest)
	if complete := length < len(rest) || !truncatedPrefix(rest); complete {
		return "", fmt.Errorf("invalid utf-8 sequence of %d bytes from index %d", length, validUpTo)
	}
	return "", fmt.Errorf("incomplete utf-8 byte sequence from index %d", validUpTo)
}

// maximalSubpart is how many bytes at the front of `bytes` form the
// longest prefix of a well-formed sequence — at least one. The bytes start
// with an invalid sequence.
func maximalSubpart(bytes []byte) int {
	first := bytes[0]
	var need int
	low, high := byte(0x80), byte(0xBF)
	switch {
	case first >= 0xC2 && first <= 0xDF:
		need = 1
	case first == 0xE0:
		need, low = 2, 0xA0
	case first >= 0xE1 && first <= 0xEC, first == 0xEE, first == 0xEF:
		need = 2
	case first == 0xED:
		need, high = 2, 0x9F
	case first == 0xF0:
		need, low = 3, 0x90
	case first >= 0xF1 && first <= 0xF3:
		need = 3
	case first == 0xF4:
		need, high = 3, 0x8F
	default:
		return 1
	}
	n := 1
	for k := 0; k < need && n < len(bytes); k++ {
		b := bytes[n]
		if k > 0 {
			low, high = 0x80, 0xBF
		}
		if b < low || b > high {
			break
		}
		n++
	}
	return n
}

// truncatedPrefix says the bytes are a well-formed sequence cut short by
// the end of the input, which `Utf8Error` reports as incomplete.
func truncatedPrefix(bytes []byte) bool {
	n := maximalSubpart(bytes)
	if n != len(bytes) {
		return false
	}
	first := bytes[0]
	switch {
	case first >= 0xC2 && first <= 0xDF:
		return n < 2
	case first >= 0xE0 && first <= 0xEF:
		return n < 3
	case first >= 0xF0 && first <= 0xF4:
		return n < 4
	}
	return false
}
