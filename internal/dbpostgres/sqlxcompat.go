package dbpostgres

// Go port only. The Rust driver leaned on sqlx for four things pgx does
// differently, and each of them reaches what the user reads:
//
//   - **Type names and kinds.** sqlx named the built-in types by OID in
//     upper case (`INT4`, `TEXT[]`, `POINT`) and looked every other type
//     up in `pg_type` on the connection the statement was prepared on,
//     naming it `oid::regtype::text`. `decode` matches on those names and
//     the placeholder prints them, so the table and the lookup are
//     reproduced here exactly (`sqlx-postgres` 0.8.6, `type_info.rs` and
//     `connection/describe.rs`).
//   - **Scalar decoding.** `bool`, the integers, `uuid`, the `time` crate's
//     date and time types, and `BigDecimal` for `numeric` — with the same
//     range limits and the same failures.
//   - **`BigDecimal`'s `Display`** (`bigdecimal` 0.4.10, default
//     thresholds 5 and 15), which is what a `numeric` cell said.
//   - **`serde_json::Value`'s `Display`** (1.0.151, with `preserve_order`,
//     which the app build enables), which is what a `json`/`jsonb` cell
//     said: compact, keys in their order, numbers re-printed.

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// pgKind is sqlx's `PgTypeKind`, without the payloads nothing here reads.
type pgKind int

const (
	kindSimple pgKind = iota
	kindPseudo
	kindDomain
	kindComposite
	kindArray
	kindEnum
	kindRange
)

// pgTypeInfo is sqlx's `PgTypeInfo`: what a column's OID was resolved to.
// `name` is `TypeInfo::name()` — the upper-case display name of a built-in,
// the catalog's `regtype` spelling of anything else. `elem` is the
// element of an array, the base of a domain, or the subtype of a built-in
// range.
type pgTypeInfo struct {
	oid  uint32
	name string
	kind pgKind
	elem *pgTypeInfo
}

// builtinType is one row of sqlx's `PgType::try_from_oid` and
// `display_name` and `kind`.
type builtinType struct {
	oid  uint32
	name string
	kind pgKind
	elem uint32
}

var builtinTypes = []builtinType{
	{16, "BOOL", kindSimple, 0},
	{17, "BYTEA", kindSimple, 0},
	{18, "\"CHAR\"", kindSimple, 0},
	{19, "NAME", kindSimple, 0},
	{20, "INT8", kindSimple, 0},
	{21, "INT2", kindSimple, 0},
	{23, "INT4", kindSimple, 0},
	{25, "TEXT", kindSimple, 0},
	{26, "OID", kindSimple, 0},
	{114, "JSON", kindSimple, 0},
	{199, "JSON[]", kindArray, 114},
	{600, "POINT", kindSimple, 0},
	{601, "LSEG", kindSimple, 0},
	{602, "PATH", kindSimple, 0},
	{603, "BOX", kindSimple, 0},
	{604, "POLYGON", kindSimple, 0},
	{628, "LINE", kindSimple, 0},
	{629, "LINE[]", kindArray, 628},
	{650, "CIDR", kindSimple, 0},
	{651, "CIDR[]", kindArray, 650},
	{700, "FLOAT4", kindSimple, 0},
	{701, "FLOAT8", kindSimple, 0},
	{705, "UNKNOWN", kindSimple, 0},
	{718, "CIRCLE", kindSimple, 0},
	{719, "CIRCLE[]", kindArray, 718},
	{774, "MACADDR8", kindSimple, 0},
	{775, "MACADDR8[]", kindArray, 774},
	{790, "MONEY", kindSimple, 0},
	{791, "MONEY[]", kindArray, 790},
	{829, "MACADDR", kindSimple, 0},
	{869, "INET", kindSimple, 0},
	{1000, "BOOL[]", kindArray, 16},
	{1001, "BYTEA[]", kindArray, 17},
	{1002, "\"CHAR\"[]", kindArray, 18},
	{1003, "NAME[]", kindArray, 19},
	{1005, "INT2[]", kindArray, 21},
	{1007, "INT4[]", kindArray, 23},
	{1009, "TEXT[]", kindArray, 25},
	{1014, "CHAR[]", kindArray, 1042},
	{1015, "VARCHAR[]", kindArray, 1043},
	{1016, "INT8[]", kindArray, 20},
	{1017, "POINT[]", kindArray, 600},
	{1018, "LSEG[]", kindArray, 601},
	{1019, "PATH[]", kindArray, 602},
	{1020, "BOX[]", kindArray, 603},
	{1021, "FLOAT4[]", kindArray, 700},
	{1022, "FLOAT8[]", kindArray, 701},
	{1027, "POLYGON[]", kindArray, 604},
	{1028, "OID[]", kindArray, 26},
	{1040, "MACADDR[]", kindArray, 829},
	{1041, "INET[]", kindArray, 869},
	{1042, "CHAR", kindSimple, 0},
	{1043, "VARCHAR", kindSimple, 0},
	{1082, "DATE", kindSimple, 0},
	{1083, "TIME", kindSimple, 0},
	{1114, "TIMESTAMP", kindSimple, 0},
	{1115, "TIMESTAMP[]", kindArray, 1114},
	{1182, "DATE[]", kindArray, 1082},
	{1183, "TIME[]", kindArray, 1083},
	{1184, "TIMESTAMPTZ", kindSimple, 0},
	{1185, "TIMESTAMPTZ[]", kindArray, 1184},
	{1186, "INTERVAL", kindSimple, 0},
	{1187, "INTERVAL[]", kindArray, 1186},
	{1231, "NUMERIC[]", kindArray, 1700},
	{1266, "TIMETZ", kindSimple, 0},
	{1270, "TIMETZ[]", kindArray, 1266},
	{1560, "BIT", kindSimple, 0},
	{1561, "BIT[]", kindArray, 1560},
	{1562, "VARBIT", kindSimple, 0},
	{1563, "VARBIT[]", kindArray, 1562},
	{1700, "NUMERIC", kindSimple, 0},
	{2278, "VOID", kindPseudo, 0},
	{2249, "RECORD", kindSimple, 0},
	{2287, "RECORD[]", kindArray, 2249},
	{2950, "UUID", kindSimple, 0},
	{2951, "UUID[]", kindArray, 2950},
	{3802, "JSONB", kindSimple, 0},
	{3807, "JSONB[]", kindArray, 3802},
	{3904, "INT4RANGE", kindRange, 23},
	{3905, "INT4RANGE[]", kindArray, 3904},
	{3906, "NUMRANGE", kindRange, 1700},
	{3907, "NUMRANGE[]", kindArray, 3906},
	{3908, "TSRANGE", kindRange, 1114},
	{3909, "TSRANGE[]", kindArray, 3908},
	{3910, "TSTZRANGE", kindRange, 1184},
	{3911, "TSTZRANGE[]", kindArray, 3910},
	{3912, "DATERANGE", kindRange, 1082},
	{3913, "DATERANGE[]", kindArray, 3912},
	{3926, "INT8RANGE", kindRange, 20},
	{3927, "INT8RANGE[]", kindArray, 3926},
	{4072, "JSONPATH", kindSimple, 0},
	{4073, "JSONPATH[]", kindArray, 4072},
}

// builtins is `PgTypeInfo::try_from_oid`, built once.
var builtins = func() map[uint32]*pgTypeInfo {
	table := make(map[uint32]*pgTypeInfo, len(builtinTypes))
	for _, row := range builtinTypes {
		table[row.oid] = &pgTypeInfo{oid: row.oid, name: row.name, kind: row.kind}
	}
	for _, row := range builtinTypes {
		if row.elem != 0 {
			table[row.oid].elem = table[row.elem]
		}
	}
	return table
}()

// typeCache is sqlx's `cache_type_info`: what the catalog said about a
// type that is not built in, so it is asked once.
//
// Go port: sqlx kept one per physical connection; this one is kept per
// `PostgresConnection`, which every pooled connection and every session of
// it share. An OID names one type in one database, so the answer is the
// same on any of them — but the `regtype` spelling of a type outside the
// asker's `search_path` comes out schema-qualified, so the placeholder of
// such a type may be spelled as the first connection to ask saw it.
type typeCache struct {
	mu    sync.Mutex
	byOID map[uint32]*pgTypeInfo
}

func newTypeCache() *typeCache {
	return &typeCache{byOID: make(map[uint32]*pgTypeInfo)}
}

// lookup is `maybe_fetch_type_info_by_oid` with `should_fetch` false:
// a built-in, or what is cached, or nil.
func (c *typeCache) lookup(oid uint32) *pgTypeInfo {
	if info, ok := builtins[oid]; ok {
		return info
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byOID[oid]
}

// fieldTypes resolves every column of a prepared statement. `fetched` says
// the catalog was asked, which a caller holding the unnamed statement must
// know: the lookups went down the same connection and replaced it.
func (c *typeCache) fieldTypes(ctx context.Context, conn *pgx.Conn, fields []pgconn.FieldDescription) (types []*pgTypeInfo, fetched bool, err error) {
	types = make([]*pgTypeInfo, len(fields))
	for i, field := range fields {
		info := c.lookup(field.DataTypeOID)
		if info == nil {
			info, err = c.fetch(ctx, conn, field.DataTypeOID)
			if err != nil {
				return nil, true, err
			}
			fetched = true
		}
		types[i] = info
	}
	return types, fetched, nil
}

// fetch is sqlx's `fetch_type_by_oid`: one row of `pg_type`, and the kind
// worked out from `typtype` and `typcategory` by sqlx's own table. The
// element of an array and the base of a domain are resolved the same way,
// because `element` reads through them.
//
// Go port: sqlx also read an enum's labels, a composite's fields and a
// range's subtype; nothing in this driver reads those, so they are not
// asked for. The kind is still the one sqlx would have given.
func (c *typeCache) fetch(ctx context.Context, conn *pgx.Conn, oid uint32) (*pgTypeInfo, error) {
	if info := c.lookup(oid); info != nil {
		return info, nil
	}
	var (
		name                         string
		typType, category            string
		relationID, element, baseOID uint32
	)
	err := conn.QueryRow(ctx,
		"SELECT oid::regtype::text, typtype::text, typcategory::text, typrelid, typelem, typbasetype "+
			"FROM pg_catalog.pg_type WHERE oid = $1",
		oid,
	).Scan(&name, &typType, &category, &relationID, &element, &baseOID)
	if err != nil {
		return nil, err
	}
	info := &pgTypeInfo{oid: oid, name: name, kind: kindSimple}
	switch {
	case typType == "d":
		base, err := c.fetch(ctx, conn, baseOID)
		if err != nil {
			return nil, err
		}
		info.kind, info.elem = kindDomain, base
	case typType == "b" && category == "A":
		elem, err := c.fetch(ctx, conn, element)
		if err != nil {
			return nil, err
		}
		info.kind, info.elem = kindArray, elem
	case typType == "p" && category == "P":
		info.kind = kindPseudo
	case typType == "r" && category == "R":
		info.kind = kindRange
	case typType == "e" && category == "E":
		info.kind = kindEnum
	case typType == "c" && category == "C":
		info.kind = kindComposite
	}
	c.mu.Lock()
	c.byOID[oid] = info
	c.mu.Unlock()
	return info, nil
}

// --- Scalars, as sqlx decoded them from binary. ---

// sqlxBool is `bool::decode`: the first byte, non-zero for true.
func sqlxBool(raw []byte) (bool, error) {
	if len(raw) == 0 {
		return false, fmt.Errorf("index out of bounds: the len is 0 but the index is 0")
	}
	return raw[0] != 0, nil
}

// sqlxInt is `int_decode` and the `try_into` to the column's width.
func sqlxInt(raw []byte, width int) (int64, error) {
	switch {
	case len(raw) == 0:
		return 0, fmt.Errorf("Value Buffer found empty while decoding to integer type")
	case len(raw) >= 9:
		return 0, fmt.Errorf("Value Buffer exceeds 8 bytes while decoding to integer type. Buffer size = %d bytes ", len(raw))
	}
	var value int64
	for _, b := range raw {
		value = value<<8 | int64(b)
	}
	// `read_int` sign-extends from the bytes it was given.
	shift := uint(64 - 8*len(raw))
	value = value << shift >> shift
	if width < 8 {
		bits := uint(8 * width)
		if value < -(1<<(bits-1)) || value > (1<<(bits-1))-1 {
			return 0, fmt.Errorf("out of range integral type conversion attempted")
		}
	}
	return value, nil
}

// uuidString is `Uuid::from_slice` and its `Display`: lower-case,
// hyphenated.
func uuidString(raw []byte) (string, error) {
	if len(raw) != 16 {
		return "", fmt.Errorf("invalid length: expected 16 bytes, found %d", len(raw))
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}

// timeDate, timeTime and primitiveDateTime are the `time` crate's `Date`,
// `Time` and `PrimitiveDateTime`, as far as the formatting reads them.
// `year` is astronomical: 0 is 1 BC.
type timeDate struct {
	year  int64
	month int64
	day   int64
}

type timeTime struct {
	hour   int64
	minute int64
	second int64
	micro  int64
}

type primitiveDateTime struct {
	date timeDate
	time timeTime
}

// The `time` crate's range without its `large-dates` feature, which the
// Rust build did not enable.
const (
	timeMinYear = -9999
	timeMaxYear = 9999
)

// sqlxDate is `PG_EPOCH + Duration::days(days)`. The `time` crate panics
// on a date outside its range — `infinity` among them — and the panic is
// what the run came back with.
func sqlxDate(raw []byte) (timeDate, error) {
	days, err := sqlxInt(raw, 4)
	if err != nil {
		return timeDate{}, err
	}
	year, month, day := civilFromDays(days)
	if year < timeMinYear || year > timeMaxYear {
		return timeDate{}, fmt.Errorf("overflow adding duration to date")
	}
	return timeDate{year, month, day}, nil
}

// sqlxTime is `Time::MIDNIGHT + Duration::microseconds(us)`, which wraps
// round the clock: `24:00:00` comes out as midnight.
func sqlxTime(raw []byte) (timeTime, error) {
	us, err := sqlxInt(raw, 8)
	if err != nil {
		return timeTime{}, err
	}
	return clockOf(remEuclid(us, microsPerDay)), nil
}

func clockOf(micros int64) timeTime {
	seconds := micros / 1_000_000
	return timeTime{
		hour:   seconds / 3600,
		minute: seconds / 60 % 60,
		second: seconds % 60,
		micro:  micros % 1_000_000,
	}
}

// sqlxPrimitiveDateTime is `PG_EPOCH.midnight() + Duration::microseconds(us)`,
// which panics outside the `time` crate's range — so `infinity` does too.
// `timestamptz` decodes the same way and then assumes UTC.
func sqlxPrimitiveDateTime(raw []byte) (primitiveDateTime, error) {
	us, err := sqlxInt(raw, 8)
	if err != nil {
		return primitiveDateTime{}, err
	}
	year, month, day := civilFromDays(divEuclid(us, microsPerDay))
	if year < timeMinYear || year > timeMaxYear {
		return primitiveDateTime{}, fmt.Errorf("resulting value is out of range")
	}
	return primitiveDateTime{
		date: timeDate{year, month, day},
		time: clockOf(remEuclid(us, microsPerDay)),
	}, nil
}

// --- numeric → BigDecimal → Display ---

// The thresholds `bigdecimal`'s build script writes when nothing overrides
// them, which nothing in the Rust build did.
const (
	exponentialFormatLeadingZeroThreshold  = 5
	exponentialFormatTrailingZeroThreshold = 15
)

// bigDecimalString is `PgNumeric::decode`, `BigDecimal::try_from` and
// `BigDecimal`'s `Display`, in that order. The scale is rebuilt from the
// digits sent, `(digits - weight - 1) * 4`, and the display scale is not
// read — which is why `1280.00` paints `1280` and `1.50` paints `1.5000`.
func bigDecimalString(raw []byte) (string, error) {
	if len(raw) < 8 {
		return "", fmt.Errorf("numeric ended early on the wire")
	}
	numDigits := binary.BigEndian.Uint16(raw[0:2])
	weight := int16(binary.BigEndian.Uint16(raw[2:4]))
	sign := binary.BigEndian.Uint16(raw[4:6])
	if sign == 0xC000 {
		return "", fmt.Errorf("BigDecimal does not support NaN values")
	}
	if len(raw) < 8+2*int(numDigits) {
		return "", fmt.Errorf("numeric ended early on the wire")
	}
	digits := make([]int16, numDigits)
	for k := range digits {
		digits[k] = int16(binary.BigEndian.Uint16(raw[8+2*k:]))
	}
	var negative bool
	switch sign {
	case 0x0000:
		negative = false
	case 0x4000:
		negative = true
	default:
		return "", fmt.Errorf("invalid value for PgNumericSign: 0x%02X", sign)
	}

	if len(digits) == 0 {
		return "0", nil
	}
	scale := (int64(len(digits)) - int64(weight) - 1) * 4
	var magnitude strings.Builder
	for k, digit := range digits {
		if digit < 0 || digit >= 10000 {
			return "", fmt.Errorf("PgNumeric to BigDecimal: %dth digit is out of range %d", k, digit)
		}
		if k == 0 {
			magnitude.WriteString(strconv.Itoa(int(digit)))
		} else {
			magnitude.WriteString(fmt.Sprintf("%04d", digit))
		}
	}
	absInt := strings.TrimLeft(magnitude.String(), "0")
	if absInt == "" {
		// A zero has no sign and no scale: `BigInt` normalizes it and
		// `get_abs_int_scale` reads its scale as 0.
		return "0", nil
	}
	return dynamicallyFormatDecimal(absInt, negative, scale), nil
}

// dynamicallyFormatDecimal is `bigdecimal`'s, for `Display` with no
// precision asked for.
func dynamicallyFormatDecimal(absInt string, negative bool, scale int64) string {
	var leadingZeroCount uint64
	if scale >= 0 && uint64(scale) >= uint64(len(absInt)) {
		leadingZeroCount = uint64(scale) - uint64(len(absInt))
	}
	var trailingZeros uint64
	if scale <= 0 && scale != math.MinInt64 {
		trailingZeros = uint64(-scale)
	}

	var body string
	switch {
	case exponentialFormatLeadingZeroThreshold < leadingZeroCount:
		body = formatExponential(absInt, scale, "E")
	case exponentialFormatTrailingZeroThreshold < trailingZeros:
		body = absInt + "e" + fmt.Sprintf("%+d", -scale)
	default:
		body = formatFullScale(absInt, scale)
	}
	if negative {
		return "-" + body
	}
	return body
}

func formatFullScale(absInt string, scale int64) string {
	digits := absInt
	if scale <= 0 {
		exp := -scale
		// `zero_right_pad_integer_ascii_digits`: more than twenty zeros are
		// left as an exponent instead.
		if exp > 20 {
			return digits + fmt.Sprintf("e%+d", exp)
		}
		return digits + strings.Repeat("0", int(exp))
	}
	if scale < int64(len(digits)) {
		point := len(digits) - int(scale)
		return digits[:point] + "." + digits[point:]
	}
	return "0." + strings.Repeat("0", int(scale)-len(digits)) + digits
}

func formatExponential(absInt string, scale int64, eSymbol string) string {
	exp := -scale
	exponent := int64(len(absInt)) + exp - 1
	text := absInt
	if len(absInt) > 1 {
		text = absInt[:1] + "." + absInt[1:]
	}
	return text + eSymbol + fmt.Sprintf("%+d", exponent)
}

// --- json/jsonb → serde_json::Value → Display ---

// sqlxJSON is sqlx's `JsonValue` decode and `Value::to_string`.
func sqlxJSON(raw []byte, jsonb bool) (string, error) {
	buf := raw
	if jsonb {
		if len(buf) == 0 || buf[0] != 1 {
			version := -1
			if len(buf) > 0 {
				version = int(buf[0])
			}
			return "", fmt.Errorf("unsupported JSONB format version %d; please open an issue", version)
		}
		buf = buf[1:]
	}
	if !utf8.Valid(buf) {
		return "", fmt.Errorf("invalid unicode code point")
	}
	parser := jsonParser{input: buf, remainingDepth: 128}
	var out strings.Builder
	parser.skipWhitespace()
	if err := parser.value(&out); err != nil {
		return "", err
	}
	parser.skipWhitespace()
	if parser.pos != len(parser.input) {
		return "", parser.fail("trailing characters")
	}
	return out.String(), nil
}

// jsonParser reads a JSON text the way `serde_json::from_slice` reads it
// into a `Value` and writes the `Value` back the way its `Display` does,
// in one pass where the order allows it.
type jsonParser struct {
	input          []byte
	pos            int
	remainingDepth int
}

func (p *jsonParser) fail(message string) error {
	line, column := 1, 0
	for _, b := range p.input[:min(p.pos, len(p.input))] {
		if b == '\n' {
			line++
			column = 0
		} else {
			column++
		}
	}
	return fmt.Errorf("%s at line %d column %d", message, line, column)
}

func (p *jsonParser) skipWhitespace() {
	for p.pos < len(p.input) {
		switch p.input[p.pos] {
		case ' ', '\n', '\t', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *jsonParser) value(out *strings.Builder) error {
	if p.pos >= len(p.input) {
		return p.fail("EOF while parsing a value")
	}
	switch c := p.input[p.pos]; {
	case c == 'n':
		return p.literal("null", out)
	case c == 't':
		return p.literal("true", out)
	case c == 'f':
		return p.literal("false", out)
	case c == '"':
		text, err := p.string()
		if err != nil {
			return err
		}
		writeJSONString(out, text)
		return nil
	case c == '-' || ('0' <= c && c <= '9'):
		return p.number(out)
	case c == '[':
		p.remainingDepth--
		if p.remainingDepth == 0 {
			return p.fail("recursion limit exceeded")
		}
		p.pos++
		err := p.array(out)
		p.remainingDepth++
		return err
	case c == '{':
		p.remainingDepth--
		if p.remainingDepth == 0 {
			return p.fail("recursion limit exceeded")
		}
		p.pos++
		err := p.object(out)
		p.remainingDepth++
		return err
	}
	return p.fail("expected value")
}

func (p *jsonParser) literal(word string, out *strings.Builder) error {
	if !strings.HasPrefix(string(p.input[p.pos:]), word) {
		return p.fail("expected ident")
	}
	p.pos += len(word)
	out.WriteString(word)
	return nil
}

func (p *jsonParser) array(out *strings.Builder) error {
	out.WriteByte('[')
	p.skipWhitespace()
	if p.pos < len(p.input) && p.input[p.pos] == ']' {
		p.pos++
		out.WriteByte(']')
		return nil
	}
	for first := true; ; first = false {
		if !first {
			out.WriteByte(',')
		}
		p.skipWhitespace()
		if err := p.value(out); err != nil {
			return err
		}
		p.skipWhitespace()
		if p.pos >= len(p.input) {
			return p.fail("EOF while parsing a list")
		}
		switch p.input[p.pos] {
		case ',':
			p.pos++
		case ']':
			p.pos++
			out.WriteByte(']')
			return nil
		default:
			return p.fail("expected `,` or `]`")
		}
	}
}

// object keeps the keys in the order they came, a repeated key keeping its
// first place and its last value — `IndexMap::insert`, which is what
// `preserve_order` puts under `serde_json::Map`.
func (p *jsonParser) object(out *strings.Builder) error {
	var keys []string
	values := make(map[string]string)
	p.skipWhitespace()
	if p.pos < len(p.input) && p.input[p.pos] == '}' {
		p.pos++
		out.WriteString("{}")
		return nil
	}
	for {
		p.skipWhitespace()
		if p.pos >= len(p.input) {
			return p.fail("EOF while parsing an object")
		}
		if p.input[p.pos] != '"' {
			return p.fail("key must be a string")
		}
		key, err := p.string()
		if err != nil {
			return err
		}
		p.skipWhitespace()
		if p.pos >= len(p.input) || p.input[p.pos] != ':' {
			return p.fail("expected `:`")
		}
		p.pos++
		p.skipWhitespace()
		var value strings.Builder
		if err := p.value(&value); err != nil {
			return err
		}
		if _, seen := values[key]; !seen {
			keys = append(keys, key)
		}
		values[key] = value.String()
		p.skipWhitespace()
		if p.pos >= len(p.input) {
			return p.fail("EOF while parsing an object")
		}
		switch p.input[p.pos] {
		case ',':
			p.pos++
			continue
		case '}':
			p.pos++
		default:
			return p.fail("expected `,` or `}`")
		}
		break
	}
	out.WriteByte('{')
	for k, key := range keys {
		if k > 0 {
			out.WriteByte(',')
		}
		writeJSONString(out, key)
		out.WriteByte(':')
		out.WriteString(values[key])
	}
	out.WriteByte('}')
	return nil
}

func (p *jsonParser) string() (string, error) {
	p.pos++ // the opening quote
	var text strings.Builder
	for {
		if p.pos >= len(p.input) {
			return "", p.fail("EOF while parsing a string")
		}
		c := p.input[p.pos]
		switch {
		case c == '"':
			p.pos++
			return text.String(), nil
		case c == '\\':
			p.pos++
			if p.pos >= len(p.input) {
				return "", p.fail("EOF while parsing a string")
			}
			escape := p.input[p.pos]
			p.pos++
			switch escape {
			case '"':
				text.WriteByte('"')
			case '\\':
				text.WriteByte('\\')
			case '/':
				text.WriteByte('/')
			case 'b':
				text.WriteByte('\b')
			case 'f':
				text.WriteByte('\f')
			case 'n':
				text.WriteByte('\n')
			case 'r':
				text.WriteByte('\r')
			case 't':
				text.WriteByte('\t')
			case 'u':
				r, err := p.unicodeEscape()
				if err != nil {
					return "", err
				}
				text.WriteRune(r)
			default:
				return "", p.fail("invalid escape")
			}
		case c < 0x20:
			return "", p.fail("control character (\\u0000-\\u001F) found while parsing a string")
		default:
			text.WriteByte(c)
			p.pos++
		}
	}
}

func (p *jsonParser) hex4() (rune, error) {
	if p.pos+4 > len(p.input) {
		return 0, p.fail("EOF while parsing a string")
	}
	value, err := strconv.ParseUint(string(p.input[p.pos:p.pos+4]), 16, 16)
	if err != nil {
		return 0, p.fail("invalid escape")
	}
	p.pos += 4
	return rune(value), nil
}

func (p *jsonParser) unicodeEscape() (rune, error) {
	first, err := p.hex4()
	if err != nil {
		return 0, err
	}
	switch {
	case first >= 0xDC00 && first <= 0xDFFF:
		return 0, p.fail("lone leading surrogate in hex escape")
	case first >= 0xD800 && first <= 0xDBFF:
		if p.pos+2 > len(p.input) || p.input[p.pos] != '\\' || p.input[p.pos+1] != 'u' {
			return 0, p.fail("unexpected end of hex escape")
		}
		p.pos += 2
		second, err := p.hex4()
		if err != nil {
			return 0, err
		}
		if second < 0xDC00 || second > 0xDFFF {
			return 0, p.fail("lone leading surrogate in hex escape")
		}
		return utf16.DecodeRune(first, second), nil
	}
	return first, nil
}

// number reads a number as `serde_json` does without
// `arbitrary_precision`: an integer that fits `u64` (or, negative, `i64`)
// stays an integer, `-0` and everything else is an `f64`, and an `f64`
// that overflows is refused.
func (p *jsonParser) number(out *strings.Builder) error {
	start := p.pos
	negative := false
	if p.input[p.pos] == '-' {
		negative = true
		p.pos++
	}
	if p.pos >= len(p.input) {
		return p.fail("EOF while parsing a value")
	}
	if p.input[p.pos] == '0' {
		p.pos++
		if p.pos < len(p.input) && '0' <= p.input[p.pos] && p.input[p.pos] <= '9' {
			return p.fail("invalid number")
		}
	} else if '1' <= p.input[p.pos] && p.input[p.pos] <= '9' {
		for p.pos < len(p.input) && '0' <= p.input[p.pos] && p.input[p.pos] <= '9' {
			p.pos++
		}
	} else {
		return p.fail("invalid number")
	}
	integerEnd := p.pos
	isFloat := false
	if p.pos < len(p.input) && p.input[p.pos] == '.' {
		isFloat = true
		p.pos++
		digits := 0
		for p.pos < len(p.input) && '0' <= p.input[p.pos] && p.input[p.pos] <= '9' {
			p.pos++
			digits++
		}
		if digits == 0 {
			return p.fail("invalid number")
		}
	}
	if p.pos < len(p.input) && (p.input[p.pos] == 'e' || p.input[p.pos] == 'E') {
		isFloat = true
		p.pos++
		if p.pos < len(p.input) && (p.input[p.pos] == '+' || p.input[p.pos] == '-') {
			p.pos++
		}
		digits := 0
		for p.pos < len(p.input) && '0' <= p.input[p.pos] && p.input[p.pos] <= '9' {
			p.pos++
			digits++
		}
		if digits == 0 {
			return p.fail("invalid number")
		}
	}
	literal := string(p.input[start:p.pos])
	if !isFloat {
		magnitude := string(p.input[start:integerEnd])
		if negative {
			magnitude = magnitude[1:]
		}
		if significand, err := strconv.ParseUint(magnitude, 10, 64); err == nil {
			if !negative {
				out.WriteString(strconv.FormatUint(significand, 10))
				return nil
			}
			neg := -int64(significand)
			if neg >= 0 {
				out.WriteString(serdeFloat(-float64(significand)))
				return nil
			}
			out.WriteString(strconv.FormatInt(neg, 10))
			return nil
		}
	}
	value, err := strconv.ParseFloat(literal, 64)
	if err != nil || math.IsInf(value, 0) {
		return p.fail("number out of range")
	}
	out.WriteString(serdeFloat(value))
	return nil
}

// writeJSONString is `serde_json`'s string escaping: the quote, the
// backslash, and the control characters — five by their letters, the rest
// as `\u00xx` — and nothing else.
func writeJSONString(out *strings.Builder, text string) {
	const hex = "0123456789abcdef"
	out.WriteByte('"')
	for k := 0; k < len(text); k++ {
		c := text[k]
		switch {
		case c == '"':
			out.WriteString("\\\"")
		case c == '\\':
			out.WriteString("\\\\")
		case c == '\b':
			out.WriteString("\\b")
		case c == '\f':
			out.WriteString("\\f")
		case c == '\n':
			out.WriteString("\\n")
		case c == '\r':
			out.WriteString("\\r")
		case c == '\t':
			out.WriteString("\\t")
		case c < 0x20:
			out.WriteString("\\u00")
			out.WriteByte(hex[c>>4])
			out.WriteByte(hex[c&0xF])
		default:
			out.WriteByte(c)
		}
	}
	out.WriteByte('"')
}

// serdeFloat is how `serde_json` 1.0.151 writes a finite `f64`, through
// `zmij`: the shortest digits, in plain notation while the decimal
// exponent is from -5 to 15 (with `.0` after a whole number), and as
// `1.5e+16` / `1e-7` outside that.
func serdeFloat(value float64) string {
	if value == 0 {
		if math.Signbit(value) {
			return "-0.0"
		}
		return "0.0"
	}
	sign := ""
	if value < 0 {
		sign = "-"
		value = -value
	}
	scientific := strconv.FormatFloat(value, 'e', -1, 64)
	mantissa, exponentText, _ := strings.Cut(scientific, "e")
	exponent, _ := strconv.Atoi(exponentText)
	digits := strings.Replace(mantissa, ".", "", 1)
	length := len(digits)
	if -5 <= exponent && exponent <= 15 {
		switch {
		case length-1 <= exponent:
			// 1234e7 -> 12340000000.0
			return sign + digits + strings.Repeat("0", exponent+1-length) + ".0"
		case exponent >= 0:
			// 1234e-2 -> 12.34
			return sign + digits[:exponent+1] + "." + digits[exponent+1:]
		default:
			// 1234e-6 -> 0.001234
			return sign + "0." + strings.Repeat("0", -exponent-1) + digits
		}
	}
	// 1234e30 -> 1.234e+33
	text := digits[:1]
	if length > 1 {
		text += "." + digits[1:]
	}
	expSign := "+"
	if exponent < 0 {
		expSign = "-"
		exponent = -exponent
	}
	return sign + text + "e" + expSign + strconv.Itoa(exponent)
}
