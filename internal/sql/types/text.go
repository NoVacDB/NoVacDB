package types

import (
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
)

func invalidInput(t Type, s string) *sqlerr.Error {
	return sqlerr.New(sqlerr.InvalidTextRepresentation, "invalid input syntax for type %s: %q", t, s)
}

// trimSpace removes the whitespace PostgreSQL's input functions skip.
func trimSpace(s string) string {
	return strings.Trim(s, " \t\n\r\f\v")
}

// Parse converts text to a value of type t using t's input form. Text and
// Unknown take the string as it is.
func Parse(s string, t Type) (Value, error) {
	switch t {
	case Int4, Int8:
		return parseInt(s, t)
	case Float8:
		return parseFloat(s)
	case Bool:
		return parseBool(s)
	case TimestampTZ:
		return parseTimestamp(s)
	case Text:
		return NewText(s), nil
	case Unknown:
		return NewUnknown(s), nil
	}
	return Value{}, sqlerr.New(sqlerr.InternalError, "no input function for %s", t)
}

// Format returns the text output form of a non-NULL value.
func Format(v Value) string {
	switch v.T {
	case Int4, Int8:
		return strconv.FormatInt(v.I, 10)
	case Float8:
		return formatFloat(v.F)
	case Bool:
		if v.Bool() {
			return "t"
		}
		return "f"
	case TimestampTZ:
		return formatTimestamp(v.I)
	}
	return v.S
}

// --- integers ------------------------------------------------------------

// parseInt reads an integer as PostgreSQL 16's input function does:
// whitespace, a sign, then decimal digits or a 0x/0o/0b prefix and digits,
// with single underscores between digits.
func parseInt(s string, t Type) (Value, error) {
	in := trimSpace(s)
	neg := false
	if in != "" && (in[0] == '+' || in[0] == '-') {
		neg = in[0] == '-'
		in = in[1:]
	}
	base := uint64(10)
	if len(in) > 1 && in[0] == '0' {
		switch in[1] {
		case 'x', 'X':
			base, in = 16, in[2:]
		case 'o', 'O':
			base, in = 8, in[2:]
		case 'b', 'B':
			base, in = 2, in[2:]
		}
		// A separator may follow the prefix.
		if base != 10 && len(in) > 1 && in[0] == '_' {
			in = in[1:]
		}
	}
	if in == "" {
		return Value{}, invalidInput(t, s)
	}
	limit := uint64(math.MaxInt64)
	if t == Int4 {
		limit = math.MaxInt32
	}
	if neg {
		limit++ // the magnitude of the most negative value
	}
	var mag uint64
	over := false
	for i := 0; i < len(in); i++ {
		c := in[i]
		if c == '_' {
			if i == 0 || i == len(in)-1 || in[i+1] == '_' {
				return Value{}, invalidInput(t, s)
			}
			continue
		}
		d, ok := digitValue(c)
		if !ok || uint64(d) >= base {
			return Value{}, invalidInput(t, s)
		}
		if mag > (limit-uint64(d))/base {
			over = true // keep going: a later bad character is a syntax error
			continue
		}
		mag = mag*base + uint64(d)
	}
	if over {
		return Value{}, sqlerr.New(sqlerr.NumericValueOutOfRange, "value %q is out of range for type %s", s, t)
	}
	v := int64(mag)
	if neg && mag > 0 {
		v = -int64(mag-1) - 1 // avoids overflow at the most negative value
	}
	return Value{T: t, I: v}, nil
}

func digitValue(c byte) (int, bool) {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0'), true
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10, true
	}
	return 0, false
}

// --- floats --------------------------------------------------------------

// isDecimal reports whether s is [sign] digits [. digits] [e [sign] digits]
// with at least one mantissa digit.
func isDecimal(s string) bool {
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	digits := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
		digits++
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
			digits++
		}
	}
	if digits == 0 {
		return false
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		exp := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
			exp++
		}
		if exp == 0 {
			return false
		}
	}
	return i == len(s)
}

func parseFloat(s string) (Value, error) {
	in := trimSpace(s)
	switch strings.ToLower(in) {
	case "nan":
		return NewFloat8(math.NaN()), nil
	case "infinity", "+infinity", "inf", "+inf":
		return NewFloat8(math.Inf(1)), nil
	case "-infinity", "-inf":
		return NewFloat8(math.Inf(-1)), nil
	}
	if !isDecimal(in) {
		return Value{}, invalidInput(Float8, s)
	}
	f, err := strconv.ParseFloat(in, 64)
	mantissa := strings.SplitN(strings.ToLower(in), "e", 2)[0]
	if err != nil || f == 0 && strings.ContainsAny(mantissa, "123456789") {
		return Value{}, sqlerr.New(sqlerr.NumericValueOutOfRange, "%q is out of range for type double precision", s)
	}
	return NewFloat8(f), nil
}

// formatFloat prints the shortest text that reads back as f, in fixed
// notation for decimal exponents -4 to 14 and in exponent notation
// otherwise, as PostgreSQL does.
func formatFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	e := strconv.FormatFloat(f, 'e', -1, 64)
	exp, _ := strconv.Atoi(e[strings.IndexByte(e, 'e')+1:])
	if exp < -4 || exp >= 15 {
		return e
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// --- booleans ------------------------------------------------------------

// parseBool accepts what PostgreSQL's parse_bool accepts.
func parseBool(s string) (Value, error) {
	in := strings.ToLower(trimSpace(s))
	n := len(in)
	if n > 0 {
		prefixOf := func(word string, min int) bool {
			return n >= min && n <= len(word) && word[:n] == in
		}
		switch in[0] {
		case 't':
			if prefixOf("true", 1) {
				return NewBool(true), nil
			}
		case 'f':
			if prefixOf("false", 1) {
				return NewBool(false), nil
			}
		case 'y':
			if prefixOf("yes", 1) {
				return NewBool(true), nil
			}
		case 'n':
			if prefixOf("no", 1) {
				return NewBool(false), nil
			}
		case 'o':
			// "o" alone is ambiguous between on and off.
			if prefixOf("on", 2) {
				return NewBool(true), nil
			}
			if prefixOf("off", 2) {
				return NewBool(false), nil
			}
		case '1':
			if n == 1 {
				return NewBool(true), nil
			}
		case '0':
			if n == 1 {
				return NewBool(false), nil
			}
		}
	}
	return Value{}, invalidInput(Bool, s)
}

// --- timestamps ----------------------------------------------------------

// pgEpochUnixMicros is 2000-01-01 00:00:00 UTC in Unix microseconds.
const pgEpochUnixMicros = 946684800 * 1000000

// Timestamp limits: the infinities, and the range of finite input.
const (
	TimestampInfinity    = math.MaxInt64
	TimestampNegInfinity = math.MinInt64
)

// TimestampFromTime converts a time to timestamptz microseconds.
func TimestampFromTime(t time.Time) int64 { return t.UnixMicro() - pgEpochUnixMicros }

func badTimestamp(s string) *sqlerr.Error {
	return sqlerr.New(sqlerr.InvalidDatetimeFormat, "invalid input syntax for type timestamp with time zone: %q", s)
}

func timestampRange(s string) *sqlerr.Error {
	return sqlerr.New(sqlerr.DatetimeFieldOverflow, "date/time field value out of range: %q", s)
}

// fixed reads exactly n digits at s[*i:].
func fixed(s string, i *int, n int) (int, bool) {
	if *i+n > len(s) {
		return 0, false
	}
	v := 0
	for k := 0; k < n; k++ {
		c := s[*i+k]
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + int(c-'0')
	}
	*i += n
	return v, true
}

// parseTimestamp reads ISO 8601: YYYY-MM-DD[( |T)HH:MM[:SS[.frac]]][zone],
// or epoch, infinity, -infinity. Without a zone the session time zone
// (UTC in Phase 4) applies.
func parseTimestamp(s string) (Value, error) {
	in := trimSpace(s)
	switch strings.ToLower(in) {
	case "epoch":
		return NewTimestampTZ(-pgEpochUnixMicros), nil
	case "infinity", "+infinity":
		return NewTimestampTZ(TimestampInfinity), nil
	case "-infinity":
		return NewTimestampTZ(TimestampNegInfinity), nil
	}
	i := 0
	year, ok1 := fixed(in, &i, 4)
	if !ok1 || i >= len(in) || in[i] != '-' {
		return Value{}, badTimestamp(s)
	}
	i++
	month, ok2 := fixed(in, &i, 2)
	if !ok2 || i >= len(in) || in[i] != '-' {
		return Value{}, badTimestamp(s)
	}
	i++
	day, ok3 := fixed(in, &i, 2)
	if !ok3 {
		return Value{}, badTimestamp(s)
	}
	var hour, minute, sec int
	var micros int64
	fracNonZero := false
	if i < len(in) && (in[i] == ' ' || in[i] == 'T' || in[i] == 't') && i+1 < len(in) && in[i+1] >= '0' && in[i+1] <= '9' {
		i++
		var ok bool
		if hour, ok = fixed(in, &i, 2); !ok || i >= len(in) || in[i] != ':' {
			return Value{}, badTimestamp(s)
		}
		i++
		if minute, ok = fixed(in, &i, 2); !ok {
			return Value{}, badTimestamp(s)
		}
		if i < len(in) && in[i] == ':' {
			i++
			if sec, ok = fixed(in, &i, 2); !ok {
				return Value{}, badTimestamp(s)
			}
			if i < len(in) && in[i] == '.' {
				i++
				start := i
				for i < len(in) && in[i] >= '0' && in[i] <= '9' {
					i++
				}
				if i == start {
					return Value{}, badTimestamp(s)
				}
				micros = fractionMicros(in[start:i])
				fracNonZero = strings.Trim(in[start:i], "0") != ""
			}
		}
	}
	offset, ok := parseZone(in[i:])
	if !ok {
		return Value{}, badTimestamp(s)
	}
	if year < 1 || month < 1 || month > 12 || day < 1 || day > daysIn(year, month) ||
		hour > 24 || minute > 59 || sec > 60 || hour == 24 && (minute != 0 || sec != 0 || fracNonZero) {
		return Value{}, timestampRange(s)
	}
	t := time.Date(year, time.Month(month), day, hour, minute, sec, 0, time.UTC)
	ts := TimestampFromTime(t) + micros - int64(offset)*1000000
	if time.UnixMicro(ts+pgEpochUnixMicros).UTC().Year() < 1 {
		return Value{}, timestampRange(s) // before 0001-01-01 UTC: BC dates are not supported
	}
	return NewTimestampTZ(ts), nil
}

// fractionMicros converts the digits of a fraction of a second to
// microseconds, rounding half to even (as PostgreSQL's rint does) on the
// exact decimal value: one rounding step, so .0000014995 is 1, not 2.
func fractionMicros(digits string) int64 {
	var us int64
	for k := 0; k < 6; k++ {
		us *= 10
		if k < len(digits) {
			us += int64(digits[k] - '0')
		}
	}
	if len(digits) <= 6 {
		return us
	}
	rest := digits[6:]
	switch {
	case rest[0] > '5', rest[0] == '5' && strings.Trim(rest[1:], "0") != "":
		us++ // above half
	case rest[0] == '5' && us%2 == 1:
		us++ // exactly half: to even
	}
	return us
}

func daysIn(year, month int) int {
	return time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// parseZone reads an optional zone: Z, UTC, GMT, or ±HH[[:]MM[[:]SS]],
// possibly after a space. It returns the offset east of UTC in seconds.
func parseZone(z string) (int, bool) {
	z = strings.TrimLeft(z, " ")
	switch strings.ToLower(z) {
	case "", "z", "utc", "gmt", "+00", "-00":
		return 0, true
	}
	if z[0] != '+' && z[0] != '-' {
		return 0, false
	}
	sign := 1
	if z[0] == '-' {
		sign = -1
	}
	i := 1
	h, ok := fixed(z, &i, 2)
	if !ok {
		return 0, false
	}
	m, sec := 0, 0
	readPart := func() (int, bool) {
		if i < len(z) && z[i] == ':' {
			i++
		}
		return fixed(z, &i, 2)
	}
	if i < len(z) {
		if m, ok = readPart(); !ok {
			return 0, false
		}
	}
	if i < len(z) {
		if sec, ok = readPart(); !ok {
			return 0, false
		}
	}
	if i != len(z) || h > 15 || m > 59 || sec > 59 {
		return 0, false
	}
	return sign * (h*3600 + m*60 + sec), true
}

// formatTimestamp prints PostgreSQL's ISO form in UTC:
// 2024-01-02 03:04:05.5+00.
func formatTimestamp(micros int64) string {
	switch micros {
	case TimestampInfinity:
		return "infinity"
	case TimestampNegInfinity:
		return "-infinity"
	}
	t := time.UnixMicro(micros + pgEpochUnixMicros).UTC()
	var b strings.Builder
	b.WriteString(t.Format("2006-01-02 15:04:05"))
	if us := t.Nanosecond() / 1000; us != 0 {
		frac := strconv.Itoa(1000000 + us)[1:]
		b.WriteByte('.')
		b.WriteString(strings.TrimRight(frac, "0"))
	}
	b.WriteString("+00")
	return b.String()
}
