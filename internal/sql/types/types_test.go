package types

import (
	"bytes"
	"errors"
	"math"
	"math/big"
	"math/rand/v2"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/ast"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
)

func testSeed(t testing.TB) uint64 {
	t.Helper()
	seed := uint64(1)
	if s := os.Getenv("NOVACDB_SEED"); s != "" {
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			t.Fatalf("bad NOVACDB_SEED %q: %v", s, err)
		}
		seed = v
	}
	return seed
}

func code(err error) string { return sqlerr.Code(err) }

func TestParseAndFormat(t *testing.T) {
	cases := []struct {
		typ  Type
		in   string
		out  string // "" with a code means an error
		code string
	}{
		// integer
		{Int4, "42", "42", ""}, {Int4, "  -17 ", "-17", ""}, {Int4, "+5", "5", ""},
		{Int4, "2147483647", "2147483647", ""}, {Int4, "-2147483648", "-2147483648", ""},
		{Int4, "2147483648", "", sqlerr.NumericValueOutOfRange}, {Int4, "-2147483649", "", sqlerr.NumericValueOutOfRange},
		{Int4, "0x1F", "31", ""}, {Int4, "-0o17", "-15", ""}, {Int4, "0b101", "5", ""}, {Int4, "1_000", "1000", ""},
		{Int4, "0x_ff", "255", ""}, {Int4, "-0", "0", ""}, {Int4, "007", "7", ""},
		{Int4, "", "", sqlerr.InvalidTextRepresentation}, {Int4, " ", "", sqlerr.InvalidTextRepresentation},
		{Int4, "1.5", "", sqlerr.InvalidTextRepresentation}, {Int4, "1e3", "", sqlerr.InvalidTextRepresentation},
		{Int4, "abc", "", sqlerr.InvalidTextRepresentation}, {Int4, "- 1", "", sqlerr.InvalidTextRepresentation},
		{Int4, "1_", "", sqlerr.InvalidTextRepresentation}, {Int4, "_1", "", sqlerr.InvalidTextRepresentation},
		{Int4, "1__0", "", sqlerr.InvalidTextRepresentation}, {Int4, "0x", "", sqlerr.InvalidTextRepresentation},
		{Int4, "0xg", "", sqlerr.InvalidTextRepresentation}, {Int4, "0b2", "", sqlerr.InvalidTextRepresentation},
		{Int4, "99999999999999999999x", "", sqlerr.InvalidTextRepresentation},
		{Int4, "99999999999999999999", "", sqlerr.NumericValueOutOfRange},
		// bigint
		{Int8, "9223372036854775807", "9223372036854775807", ""}, {Int8, "-9223372036854775808", "-9223372036854775808", ""},
		{Int8, "9223372036854775808", "", sqlerr.NumericValueOutOfRange}, {Int8, "-0x8000000000000000", "-9223372036854775808", ""},
		// double precision
		{Float8, "1.5", "1.5", ""}, {Float8, " -2.25e3 ", "-2250", ""}, {Float8, "1e15", "1e+15", ""},
		{Float8, "1e14", "100000000000000", ""}, {Float8, "0.0001", "0.0001", ""}, {Float8, "0.00001", "1e-05", ""},
		{Float8, "123456789012345678", "1.2345678901234568e+17", ""}, {Float8, ".5", "0.5", ""}, {Float8, "5.", "5", ""},
		{Float8, "-0", "-0", ""}, {Float8, "NaN", "NaN", ""}, {Float8, "nan", "NaN", ""}, {Float8, "Infinity", "Infinity", ""},
		{Float8, "-inf", "-Infinity", ""}, {Float8, "+INF", "Infinity", ""}, {Float8, "0.1", "0.1", ""},
		{Float8, "1e-310", "1e-310", ""}, {Float8, "1e400", "", sqlerr.NumericValueOutOfRange},
		{Float8, "1e-400", "", sqlerr.NumericValueOutOfRange}, {Float8, "0e-400", "0", ""},
		{Float8, "1_0", "", sqlerr.InvalidTextRepresentation}, {Float8, "0x1p3", "", sqlerr.InvalidTextRepresentation},
		{Float8, "", "", sqlerr.InvalidTextRepresentation}, {Float8, "1e", "", sqlerr.InvalidTextRepresentation},
		{Float8, ".", "", sqlerr.InvalidTextRepresentation}, {Float8, "infinityy", "", sqlerr.InvalidTextRepresentation},
		{Float8, "-nan", "", sqlerr.InvalidTextRepresentation},
		// boolean
		{Bool, "t", "t", ""}, {Bool, "TRUE", "t", ""}, {Bool, " yes ", "t", ""}, {Bool, "on", "t", ""}, {Bool, "1", "t", ""},
		{Bool, "tr", "t", ""}, {Bool, "f", "f", ""}, {Bool, "False", "f", ""}, {Bool, "no", "f", ""}, {Bool, "off", "f", ""},
		{Bool, "of", "f", ""}, {Bool, "0", "f", ""}, {Bool, "o", "", sqlerr.InvalidTextRepresentation},
		{Bool, "truee", "", sqlerr.InvalidTextRepresentation}, {Bool, "", "", sqlerr.InvalidTextRepresentation},
		{Bool, "2", "", sqlerr.InvalidTextRepresentation}, {Bool, "11", "", sqlerr.InvalidTextRepresentation},
		{Bool, "onn", "", sqlerr.InvalidTextRepresentation},
		// timestamptz
		{TimestampTZ, "2024-01-02 03:04:05", "2024-01-02 03:04:05+00", ""},
		{TimestampTZ, "2024-01-02T03:04:05Z", "2024-01-02 03:04:05+00", ""},
		{TimestampTZ, "2024-01-02 03:04:05.5", "2024-01-02 03:04:05.5+00", ""},
		{TimestampTZ, "2024-01-02 03:04:05.123456789", "2024-01-02 03:04:05.123457+00", ""},
		{TimestampTZ, "2024-01-02 03:04:05.0000005", "2024-01-02 03:04:05+00", ""}, // half to even: 0
		{TimestampTZ, "2024-01-02 03:04:05.0000015", "2024-01-02 03:04:05.000002+00", ""},
		{TimestampTZ, "2024-01-02 03:04:05.00000051", "2024-01-02 03:04:05.000001+00", ""},
		{TimestampTZ, "2024-01-02 03:04:05.0000014995", "2024-01-02 03:04:05.000001+00", ""}, // one rounding step
		{TimestampTZ, "2024-01-02 03:04:05.0000025", "2024-01-02 03:04:05.000002+00", ""},    // half to even
		{TimestampTZ, "2024-01-02 03:04:05.0000035", "2024-01-02 03:04:05.000004+00", ""},
		{TimestampTZ, "2024-01-02 03:04:05.9999995", "2024-01-02 03:04:06+00", ""},
		{TimestampTZ, "2024-01-02 24:00:00.000", "2024-01-03 00:00:00+00", ""},
		{TimestampTZ, "2024-01-02 24:00:00.0001", "", sqlerr.DatetimeFieldOverflow},
		{TimestampTZ, "2024-01-02 03:04:05+05:30", "2024-01-01 21:34:05+00", ""},
		{TimestampTZ, "2024-01-02 03:04:05 -0800", "2024-01-02 11:04:05+00", ""},
		{TimestampTZ, "2024-01-02 03:04:05-08", "2024-01-02 11:04:05+00", ""},
		{TimestampTZ, "2024-01-02 03:04:05+05:30:15", "2024-01-01 21:33:50+00", ""},
		{TimestampTZ, "2024-01-02 03:04 UTC", "2024-01-02 03:04:00+00", ""},
		{TimestampTZ, "2024-01-02", "2024-01-02 00:00:00+00", ""},
		{TimestampTZ, "2024-02-29 24:00:00", "2024-03-01 00:00:00+00", ""},
		{TimestampTZ, "2024-01-02 23:59:60", "2024-01-03 00:00:00+00", ""},
		{TimestampTZ, "0001-01-01 00:00:00", "0001-01-01 00:00:00+00", ""},
		{TimestampTZ, "9999-12-31 23:59:59.999999", "9999-12-31 23:59:59.999999+00", ""},
		{TimestampTZ, "9999-12-31 23:00:00-05", "10000-01-01 04:00:00+00", ""},
		{TimestampTZ, "epoch", "1970-01-01 00:00:00+00", ""},
		{TimestampTZ, "Infinity", "infinity", ""}, {TimestampTZ, "-infinity", "-infinity", ""},
		{TimestampTZ, "2023-02-29", "", sqlerr.DatetimeFieldOverflow},
		{TimestampTZ, "2024-13-01", "", sqlerr.DatetimeFieldOverflow},
		{TimestampTZ, "2024-01-01 25:00", "", sqlerr.DatetimeFieldOverflow},
		{TimestampTZ, "2024-01-01 24:00:01", "", sqlerr.DatetimeFieldOverflow},
		{TimestampTZ, "2024-01-01 10:61", "", sqlerr.DatetimeFieldOverflow},
		{TimestampTZ, "0000-01-01", "", sqlerr.DatetimeFieldOverflow},
		{TimestampTZ, "0001-01-01 00:00:00+01", "", sqlerr.DatetimeFieldOverflow},
		{TimestampTZ, "2024-1-01", "", sqlerr.InvalidDatetimeFormat},
		{TimestampTZ, "2024-01-01 10", "", sqlerr.InvalidDatetimeFormat},
		{TimestampTZ, "2024-01-01 10:00:00.", "", sqlerr.InvalidDatetimeFormat},
		{TimestampTZ, "2024-01-01 10:00:00 PST", "", sqlerr.InvalidDatetimeFormat},
		{TimestampTZ, "2024-01-01 10:00:00+16", "", sqlerr.InvalidDatetimeFormat},
		{TimestampTZ, "2024-01-01 10:00:00+5", "", sqlerr.InvalidDatetimeFormat},
		{TimestampTZ, "now", "", sqlerr.InvalidDatetimeFormat},
		{TimestampTZ, "", "", sqlerr.InvalidDatetimeFormat},
		// text
		{Text, " a b ", " a b ", ""}, {Text, "", "", ""},
	}
	for _, c := range cases {
		v, err := Parse(c.in, c.typ)
		if c.code != "" {
			if code(err) != c.code {
				t.Errorf("Parse(%q, %s) = %v, %v; want error %s", c.in, c.typ, v, err, c.code)
			}
			continue
		}
		if err != nil {
			t.Errorf("Parse(%q, %s): %v", c.in, c.typ, err)
			continue
		}
		if got := Format(v); got != c.out || v.T != c.typ {
			t.Errorf("Parse(%q, %s) formats as %q (%s), want %q", c.in, c.typ, got, v.T, c.out)
		}
	}
}

func TestErrorMessagesNameTheType(t *testing.T) {
	_, err := Parse("x", Int4)
	if e := sqlerr.From(err); e.Message != `invalid input syntax for type integer: "x"` {
		t.Errorf("message %q", e.Message)
	}
	_, err = Parse("9999999999", Int4)
	if e := sqlerr.From(err); e.Message != `value "9999999999" is out of range for type integer` {
		t.Errorf("message %q", e.Message)
	}
	_, err = Parse("x", TimestampTZ)
	if e := sqlerr.From(err); !strings.Contains(e.Message, "timestamp with time zone") {
		t.Errorf("message %q", e.Message)
	}
}

func TestTimestampValues(t *testing.T) {
	v, err := Parse("2000-01-01 00:00:00", TimestampTZ)
	if err != nil || v.I != 0 {
		t.Fatalf("PostgreSQL epoch = %d, %v", v.I, err)
	}
	v, _ = Parse("2000-01-01 00:00:01.000001", TimestampTZ)
	if v.I != 1000001 {
		t.Fatalf("micros %d", v.I)
	}
	v, _ = Parse("1999-12-31 23:59:59", TimestampTZ)
	if v.I != -1000000 {
		t.Fatalf("before epoch %d", v.I)
	}
	inf, _ := Parse("infinity", TimestampTZ)
	ninf, _ := Parse("-infinity", TimestampTZ)
	if Compare(ninf, v) >= 0 || Compare(v, inf) >= 0 {
		t.Fatal("infinities do not bound finite timestamps")
	}
}

// ---- arithmetic ----------------------------------------------------------

// refInt computes an integer operation exactly and reports whether the
// result fits the type.
func refInt(op string, a, b int64, t Type) (int64, bool, bool) {
	x, y := big.NewInt(a), big.NewInt(b)
	r := new(big.Int)
	switch op {
	case "+":
		r.Add(x, y)
	case "-":
		r.Sub(x, y)
	case "*":
		r.Mul(x, y)
	case "/":
		if b == 0 {
			return 0, false, true
		}
		r.Quo(x, y) // truncates toward zero
	case "%":
		if b == 0 {
			return 0, false, true
		}
		r.Rem(x, y)
	}
	lo, hi := big.NewInt(math.MinInt64), big.NewInt(math.MaxInt64)
	if t == Int4 {
		lo, hi = big.NewInt(math.MinInt32), big.NewInt(math.MaxInt32)
	}
	return r.Int64(), r.Cmp(lo) >= 0 && r.Cmp(hi) <= 0, false
}

var arith = map[string]func(a, b Value) (Value, error){"+": Add, "-": Sub, "*": Mul, "/": Div, "%": Mod}

func randInt(rng *rand.Rand, t Type) int64 {
	edges := []int64{0, 1, -1, 2, -2, math.MaxInt32, math.MinInt32, math.MaxInt64, math.MinInt64, 46341, -46341, 3037000500, -3037000500}
	var v int64
	switch rng.IntN(3) {
	case 0:
		v = edges[rng.IntN(len(edges))]
	case 1:
		v = int64(rng.IntN(201)) - 100
	default:
		v = int64(rng.Uint64())
	}
	if t == Int4 {
		v = int64(int32(v))
	}
	return v
}

func TestIntegerArithmeticAgainstBigInt(t *testing.T) {
	rng := rand.New(rand.NewPCG(testSeed(t), 1))
	counts := map[string]int{}
	for range 200000 {
		typ := []Type{Int4, Int8}[rng.IntN(2)]
		a, b := randInt(rng, typ), randInt(rng, typ)
		for op, f := range arith {
			want, fits, divZero := refInt(op, a, b, typ)
			got, err := f(Value{T: typ, I: a}, Value{T: typ, I: b})
			switch {
			case divZero:
				if code(err) != sqlerr.DivisionByZero {
					t.Fatalf("%d %s %d: %v, want division by zero", a, op, b, err)
				}
				counts["div0"]++
			case !fits:
				if code(err) != sqlerr.NumericValueOutOfRange {
					t.Fatalf("%s: %d %s %d = %v, %v; want out of range", typ, a, op, b, got, err)
				}
				counts["overflow"]++
			default:
				if err != nil || got.I != want || got.T != typ {
					t.Fatalf("%s: %d %s %d = %v, %v; want %d", typ, a, op, b, got, err, want)
				}
			}
		}
		neg, err := Neg(Value{T: typ, I: a})
		want, fits, _ := refInt("-", 0, a, typ)
		if fits != (err == nil) || err == nil && neg.I != want {
			t.Fatalf("-%d = %v, %v", a, neg, err)
		}
		abs, err := Abs(Value{T: typ, I: a})
		wantAbs := want
		if a >= 0 {
			wantAbs, fits = a, true
		}
		if fits != (err == nil) || err == nil && abs.I != wantAbs {
			t.Fatalf("abs(%d) = %v, %v", a, abs, err)
		}
	}
	if counts["div0"] == 0 || counts["overflow"] == 0 {
		t.Fatalf("no edge cases hit: %v", counts)
	}
	// The messages name the type.
	_, err := Add(NewInt4(math.MaxInt32), NewInt4(1))
	if sqlerr.From(err).Message != "integer out of range" {
		t.Error(err)
	}
	_, err = Mul(NewInt8(math.MaxInt64), NewInt8(2))
	if sqlerr.From(err).Message != "bigint out of range" {
		t.Error(err)
	}
}

func TestFloatArithmetic(t *testing.T) {
	f := NewFloat8
	inf, nan := math.Inf(1), math.NaN()
	cases := []struct {
		op   func(a, b Value) (Value, error)
		a, b float64
		want float64
		code string
	}{
		{Add, 1.5, 2.25, 3.75, ""}, {Sub, 1, 3, -2, ""}, {Mul, 1.5, 4, 6, ""}, {Div, 1, 4, 0.25, ""},
		{Add, math.MaxFloat64, math.MaxFloat64, 0, sqlerr.NumericValueOutOfRange},
		{Sub, -math.MaxFloat64, math.MaxFloat64, 0, sqlerr.NumericValueOutOfRange},
		{Mul, 1e200, 1e200, 0, sqlerr.NumericValueOutOfRange},
		{Mul, 1e-200, 1e-200, 0, sqlerr.NumericValueOutOfRange},
		{Mul, 0, 1e-200, 0, ""},
		{Div, 1, 0, 0, sqlerr.DivisionByZero}, {Div, 0, 0, 0, sqlerr.DivisionByZero},
		{Div, 1e300, 1e-300, 0, sqlerr.NumericValueOutOfRange},
		{Div, 1e-300, 1e300, 0, sqlerr.NumericValueOutOfRange},
		{Div, 0, 5, 0, ""}, {Div, 5, inf, 0, ""},
		{Add, inf, 1, inf, ""}, {Add, inf, -inf, nan, ""}, {Mul, inf, 0, nan, ""}, {Add, nan, 1, nan, ""},
		{Div, inf, 2, inf, ""},
		{Pow, 2, 10, 1024, ""}, {Pow, 2, -1, 0.5, ""}, {Pow, -8, 1.0 / 3, 0, "2201F"},
		{Pow, 0, -1, 0, "2201F"}, {Pow, 0, 0, 1, ""}, {Pow, 10, 400, 0, sqlerr.NumericValueOutOfRange},
		{Pow, 10, -400, 0, sqlerr.NumericValueOutOfRange}, {Pow, 0.5, 10000, 0, sqlerr.NumericValueOutOfRange},
		{Pow, -2, 3, -8, ""}, {Pow, 1, nan, 1, ""}, {Pow, nan, 0, 1, ""}, {Pow, nan, 1, nan, ""},
		{Pow, inf, -1, 0, ""}, {Pow, 2, inf, inf, ""}, {Pow, -inf, 0.5, inf, ""},
	}
	for i, c := range cases {
		got, err := c.op(f(c.a), f(c.b))
		if c.code != "" {
			if code(err) != c.code {
				t.Errorf("case %d (%v, %v): %v, %v; want %s", i, c.a, c.b, got, err, c.code)
			}
			continue
		}
		if err != nil || got.T != Float8 || compareFloat(got.F, c.want) != 0 {
			t.Errorf("case %d (%v, %v): %v, %v; want %v", i, c.a, c.b, got, err, c.want)
		}
	}
	if v, _ := Neg(f(0)); !math.Signbit(v.F) {
		t.Error("-0.0 lost its sign")
	}
	if v, _ := Abs(f(-2.5)); v.F != 2.5 {
		t.Error("abs")
	}
}

func TestThreeValuedLogic(t *testing.T) {
	T, F, N := NewBool(true), NewBool(false), Null(Bool)
	name := func(v Value) string {
		if v.Null {
			return "N"
		}
		if v.Bool() {
			return "T"
		}
		return "F"
	}
	vals := []Value{T, F, N}
	// Rows and columns T, F, N.
	and := []string{"TFN", "FFF", "NFN"}
	or := []string{"TTT", "TFN", "TNN"}
	for i, a := range vals {
		for j, b := range vals {
			if got := name(And(a, b)); got != string(and[i][j]) {
				t.Errorf("%s AND %s = %s", name(a), name(b), got)
			}
			if got := name(Or(a, b)); got != string(or[i][j]) {
				t.Errorf("%s OR %s = %s", name(a), name(b), got)
			}
		}
	}
	if name(Not(T)) != "F" || name(Not(F)) != "T" || name(Not(N)) != "N" {
		t.Error("NOT")
	}
}

// ---- comparison and keys -------------------------------------------------

func randValue(rng *rand.Rand, typ Type) Value {
	if rng.IntN(10) == 0 {
		return Null(typ)
	}
	switch typ {
	case Int4, Int8:
		return Value{T: typ, I: randInt(rng, typ)}
	case Float8:
		specials := []float64{0, math.Copysign(0, -1), math.NaN(), math.Inf(1), math.Inf(-1), 1, -1, math.SmallestNonzeroFloat64, math.MaxFloat64}
		if rng.IntN(3) == 0 {
			return NewFloat8(specials[rng.IntN(len(specials))])
		}
		if rng.IntN(2) == 0 {
			return NewFloat8(float64(rng.IntN(11)-5) / 2)
		}
		return NewFloat8(math.Float64frombits(rng.Uint64()))
	case Text:
		alphabet := []string{"", "a", "b", "\x00", "\xff"[:0] + "é", "Z", "ab"}
		var b strings.Builder
		for range rng.IntN(4) {
			b.WriteString(alphabet[rng.IntN(len(alphabet))])
		}
		return NewText(b.String())
	case Bool:
		return NewBool(rng.IntN(2) == 1)
	case TimestampTZ:
		switch rng.IntN(4) {
		case 0:
			return NewTimestampTZ([]int64{TimestampInfinity, TimestampNegInfinity, 0}[rng.IntN(3)])
		default:
			return NewTimestampTZ(rng.Int64N(500000000000000000) - 250000000000000000)
		}
	}
	return Null(typ)
}

var allTypes = []Type{Int4, Int8, Float8, Text, Bool, TimestampTZ}

// compareNullsLast is Compare extended with NULL after every value.
func compareNullsLast(a, b Value) int {
	switch {
	case a.Null && b.Null:
		return 0
	case a.Null:
		return 1
	case b.Null:
		return -1
	}
	return Compare(a, b)
}

func sign(x int) int {
	switch {
	case x < 0:
		return -1
	case x > 0:
		return 1
	}
	return 0
}

func TestKeyOrderMatchesCompare(t *testing.T) {
	rng := rand.New(rand.NewPCG(testSeed(t), 2))
	for range 3000 {
		typ := allTypes[rng.IntN(len(allTypes))]
		vals := make([]Value, 30)
		for i := range vals {
			vals[i] = randValue(rng, typ)
		}
		for _, a := range vals {
			for _, b := range vals {
				want := compareNullsLast(a, b)
				got := sign(bytes.Compare(AppendKey(nil, a), AppendKey(nil, b)))
				if got != want {
					t.Fatalf("%s: key order of %v vs %v is %d, Compare says %d", typ, a, b, got, want)
				}
			}
		}
	}
	// Compare is a total order: antisymmetric and transitive on samples.
	for range 2000 {
		typ := allTypes[rng.IntN(len(allTypes))]
		a, b, c := randValue(rng, typ), randValue(rng, typ), randValue(rng, typ)
		if a.Null || b.Null || c.Null {
			continue
		}
		if Compare(a, b) != -Compare(b, a) {
			t.Fatalf("not antisymmetric: %v %v", a, b)
		}
		if Compare(a, b) <= 0 && Compare(b, c) <= 0 && Compare(a, c) > 0 {
			t.Fatalf("not transitive: %v %v %v", a, b, c)
		}
	}
}

func TestCompareSpecialFloats(t *testing.T) {
	nan, inf := NewFloat8(math.NaN()), NewFloat8(math.Inf(1))
	if Compare(nan, nan) != 0 || Compare(nan, inf) != 1 || Compare(inf, nan) != -1 {
		t.Error("NaN must equal itself and sort above infinity")
	}
	if Compare(NewFloat8(0), NewFloat8(math.Copysign(0, -1))) != 0 {
		t.Error("-0 must equal +0")
	}
	if Compare(NewInt4(5), NewInt8(7)) != -1 {
		t.Error("integers of both widths compare")
	}
	if Compare(NewText("a"), NewText("b")) != -1 || Compare(NewText("é"), NewText("z")) != 1 {
		t.Error("text compares by bytes")
	}
	if Compare(NewBool(false), NewBool(true)) != -1 {
		t.Error("false < true")
	}
}

// ---- casts -----------------------------------------------------------------

func TestCasts(t *testing.T) {
	cases := []struct {
		v    Value
		to   Type
		out  string
		code string
	}{
		{NewInt4(5), Int8, "5", ""}, {NewInt8(5), Int4, "5", ""},
		{NewInt8(math.MaxInt32 + 1), Int4, "", sqlerr.NumericValueOutOfRange},
		{NewInt4(5), Float8, "5", ""}, {NewInt8(math.MaxInt64), Float8, "9.223372036854776e+18", ""},
		{NewFloat8(2.5), Int4, "2", ""}, {NewFloat8(3.5), Int4, "4", ""}, {NewFloat8(-2.5), Int8, "-2", ""},
		{NewFloat8(2147483647.4), Int4, "2147483647", ""}, {NewFloat8(2147483647.5), Int4, "", sqlerr.NumericValueOutOfRange},
		{NewFloat8(-2147483648.5), Int4, "-2147483648", ""}, {NewFloat8(-2147483649), Int4, "", sqlerr.NumericValueOutOfRange},
		{NewFloat8(9.223372036854775e18), Int8, "9223372036854774784", ""},
		{NewFloat8(9.223372036854775807e18), Int8, "", sqlerr.NumericValueOutOfRange},
		{NewFloat8(-9.223372036854775808e18), Int8, "-9223372036854775808", ""},
		{NewFloat8(math.NaN()), Int4, "", sqlerr.NumericValueOutOfRange}, {NewFloat8(math.Inf(-1)), Int8, "", sqlerr.NumericValueOutOfRange},
		{NewInt4(0), Bool, "f", ""}, {NewInt4(-3), Bool, "t", ""}, {NewBool(true), Int4, "1", ""},
		{NewInt4(42), Text, "42", ""}, {NewBool(false), Text, "false", ""}, {NewBool(true), Text, "true", ""}, {NewFloat8(0.1), Text, "0.1", ""},
		{NewTimestampTZ(0), Text, "2000-01-01 00:00:00+00", ""},
		{NewText(" 12 "), Int4, "12", ""}, {NewText("x"), Int4, "", sqlerr.InvalidTextRepresentation},
		{NewUnknown("t"), Bool, "t", ""}, {NewUnknown("2024-05-06"), TimestampTZ, "2024-05-06 00:00:00+00", ""},
		{NewInt8(1), Bool, "", sqlerr.CannotCoerce}, {NewBool(true), Float8, "", sqlerr.CannotCoerce},
		{NewFloat8(1), Bool, "", sqlerr.CannotCoerce}, {NewTimestampTZ(0), Int8, "", sqlerr.CannotCoerce},
		{NewInt4(1), TimestampTZ, "", sqlerr.CannotCoerce},
		{Null(Int8), Int4, "NULL", ""}, {Null(Bool), Float8, "", sqlerr.CannotCoerce},
	}
	for _, c := range cases {
		got, err := Cast(c.v, c.to)
		if c.code != "" {
			if code(err) != c.code {
				t.Errorf("CAST(%v AS %s) = %v, %v; want %s", c.v, c.to, got, err, c.code)
			}
			continue
		}
		if err != nil || got.String() != c.out || got.T != c.to {
			t.Errorf("CAST(%v AS %s) = %v (%s), %v; want %s", c.v, c.to, got, got.T, err, c.out)
		}
	}
	if !strings.Contains(CannotCast(Bool, Float8).Message, "cannot cast type boolean to double precision") {
		t.Error("cast message")
	}
}

func TestTextRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(testSeed(t), 3))
	for range 50000 {
		typ := allTypes[rng.IntN(len(allTypes))]
		v := randValue(rng, typ)
		if v.Null || typ == TimestampTZ && !timestampFormattable(v.I) {
			continue
		}
		s := Format(v)
		back, err := Parse(s, typ)
		if err != nil {
			t.Fatalf("%s %v formats as %q, which does not parse: %v", typ, v, s, err)
		}
		if Compare(back, v) != 0 || typ == Float8 && math.Signbit(back.F) != math.Signbit(v.F) && !math.IsNaN(v.F) {
			t.Fatalf("%s %v formats as %q, which parses as %v", typ, v, s, back)
		}
	}
}

// timestampFormattable reports whether the text form of micros parses
// back: infinities and years 1 to 9999.
func timestampFormattable(micros int64) bool {
	if micros == TimestampInfinity || micros == TimestampNegInfinity {
		return true
	}
	lo, _ := Parse("0001-01-01", TimestampTZ)
	hi, _ := Parse("9999-12-31 23:59:59.999999", TimestampTZ)
	return micros >= lo.I && micros <= hi.I
}

// ---- rows ------------------------------------------------------------------

func TestRowRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(testSeed(t), 4))
	for range 20000 {
		cols := make([]Type, rng.IntN(20))
		vals := make([]Value, len(cols))
		for i := range cols {
			cols[i] = allTypes[rng.IntN(len(allTypes))]
			vals[i] = randValue(rng, cols[i])
		}
		b, err := EncodeRow(vals, cols)
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeRow(b, cols)
		if err != nil {
			t.Fatalf("decoding %x: %v", b, err)
		}
		for i := range vals {
			if got[i].Null != vals[i].Null || got[i].T != cols[i] || !got[i].Null && (Compare(got[i], vals[i]) != 0 ||
				cols[i] == Float8 && math.Float64bits(got[i].F) != math.Float64bits(vals[i].F)) {
				t.Fatalf("column %d: %v, want %v", i, got[i], vals[i])
			}
		}
	}
}

func TestWideRows(t *testing.T) {
	// Up to MaxColumns, past the sizes where the null bitmap outgrows a
	// small first allocation: every column NULL, then every column set.
	for _, n := range []int{0, 1, 8, 9, 487, 488, 489, 490, 1000, MaxColumns} {
		cols := make([]Type, n)
		nulls, set := make([]Value, n), make([]Value, n)
		for i := range cols {
			cols[i] = Int4
			nulls[i] = Null(Int4)
			set[i] = NewInt4(int32(i))
		}
		for _, vals := range [][]Value{nulls, set} {
			b, err := EncodeRow(vals, cols)
			if err != nil {
				t.Fatalf("%d columns: %v", n, err)
			}
			got, err := DecodeRow(b, cols)
			if err != nil {
				t.Fatalf("%d columns: %v", n, err)
			}
			for i := range vals {
				if got[i].Null != vals[i].Null || !got[i].Null && got[i].I != vals[i].I {
					t.Fatalf("%d columns, column %d: %v, want %v", n, i, got[i], vals[i])
				}
			}
		}
	}
	if _, err := EncodeRow(make([]Value, MaxColumns+1), make([]Type, MaxColumns+1)); code(err) != sqlerr.InternalError {
		t.Fatal(err)
	}
}

func TestRowGoldenBytes(t *testing.T) {
	cols := []Type{Int4, Text, Bool, Int8, Float8, TimestampTZ, Int4}
	vals := []Value{NewInt4(-2), NewText("hé"), NewBool(true), Null(Int8), NewFloat8(1), NewTimestampTZ(1), Null(Int4)}
	b, err := EncodeRow(vals, cols)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		1, 7, 0, // version, 7 columns
		0x48,                   // NULLs: columns 3 and 6
		0xFE, 0xFF, 0xFF, 0xFF, // -2
		3, 0, 0, 0, 'h', 0xC3, 0xA9, // "hé"
		1,                            // true
		0, 0, 0, 0, 0, 0, 0xF0, 0x3F, // 1.0
		1, 0, 0, 0, 0, 0, 0, 0, // 1 microsecond
	}
	if !bytes.Equal(b, want) {
		t.Fatalf("got  % x\nwant % x", b, want)
	}
}

func TestRowLimitsAndErrors(t *testing.T) {
	big := NewText(strings.Repeat("x", MaxRowSize))
	if _, err := EncodeRow([]Value{big}, []Type{Text}); code(err) != sqlerr.ProgramLimitExceeded {
		t.Fatalf("oversized row: %v", err)
	}
	fits := NewText(strings.Repeat("x", MaxRowSize-3-1-4))
	if _, err := EncodeRow([]Value{fits}, []Type{Text}); err != nil {
		t.Fatalf("row of exactly the maximum size: %v", err)
	}
	if _, err := EncodeRow([]Value{NewInt4(1)}, []Type{Text}); code(err) != sqlerr.InternalError {
		t.Fatalf("type mismatch: %v", err)
	}
	if _, err := EncodeRow([]Value{NewInt4(1)}, nil); code(err) != sqlerr.InternalError {
		t.Fatalf("length mismatch: %v", err)
	}
	// Fewer stored columns than the table: the rest are NULL.
	b, _ := EncodeRow([]Value{NewInt4(7)}, []Type{Int4})
	got, err := DecodeRow(b, []Type{Int4, Text})
	if err != nil || got[0].I != 7 || !got[1].Null || got[1].T != Text {
		t.Fatalf("short row: %v, %v", got, err)
	}
	good, _ := EncodeRow([]Value{NewInt4(1), NewText("ab"), NewBool(true)}, []Type{Int4, Text, Bool})
	cols := []Type{Int4, Text, Bool}
	bad := map[string][]byte{
		"empty":         {},
		"version":       append([]byte{2}, good[1:]...),
		"too many cols": append(bytes.Clone(good[:1]), append([]byte{9, 0}, good[3:]...)...),
		"truncated":     good[:len(good)-1],
		"trailing":      append(bytes.Clone(good), 0),
		"bool 2":        append(bytes.Clone(good[:len(good)-1]), 2),
		"text too long": append(append(bytes.Clone(good[:8]), 0xFF, 0xFF, 0, 0), good[12:]...),
		"invalid utf8":  append(append(bytes.Clone(good[:12]), 0xFF, 'b'), good[14:]...),
		"short bitmap":  {1, 9, 0},
	}
	for name, b := range bad {
		if _, err := DecodeRow(b, cols); code(err) != sqlerr.DataCorrupted {
			t.Errorf("%s: %v", name, err)
		}
	}
	// More stored columns than the table has is damage, even when the
	// extra ones are NULL and take no bytes.
	wide, _ := EncodeRow([]Value{NewInt4(1), Null(Int4)}, []Type{Int4, Int4})
	if _, err := DecodeRow(wide, []Type{Int4}); code(err) != sqlerr.DataCorrupted {
		t.Errorf("row wider than its table: %v", err)
	}
}

func FuzzDecodeRow(f *testing.F) {
	cols := []Type{Int4, Text, Bool, Int8, Float8, TimestampTZ}
	good, _ := EncodeRow([]Value{NewInt4(1), NewText("x"), NewBool(true), Null(Int8), NewFloat8(2), NewTimestampTZ(3)}, cols)
	f.Add(good, uint8(6))
	f.Add([]byte{1, 0, 0}, uint8(0))
	f.Fuzz(func(t *testing.T, b []byte, n uint8) {
		cs := make([]Type, int(n)%40)
		for i := range cs {
			cs[i] = cols[i%len(cols)]
		}
		vals, err := DecodeRow(b, cs)
		if err != nil {
			if code(err) != sqlerr.DataCorrupted {
				t.Fatalf("unexpected error %v", err)
			}
			return
		}
		// What decodes encodes back to a row that decodes the same.
		again, err := EncodeRow(vals, cs)
		if err != nil {
			if code(err) == sqlerr.ProgramLimitExceeded {
				return
			}
			t.Fatal(err)
		}
		vals2, err := DecodeRow(again, cs)
		if err != nil {
			t.Fatal(err)
		}
		for i := range vals {
			if vals[i].Null != vals2[i].Null || !vals[i].Null && Compare(vals[i], vals2[i]) != 0 {
				t.Fatalf("column %d changed", i)
			}
		}
	})
}

func FuzzParseValue(f *testing.F) {
	for _, s := range []string{"42", "-1.5e3", "true", "2024-01-02 03:04:05.5+05:30", "0x1F", "NaN", "of"} {
		f.Add(s, uint8(0))
	}
	f.Fuzz(func(t *testing.T, s string, k uint8) {
		typ := allTypes[int(k)%len(allTypes)]
		v, err := Parse(s, typ)
		if err != nil {
			var e *sqlerr.Error
			if !errors.As(err, &e) || e.Code[:2] != "22" {
				t.Fatalf("Parse(%q, %s): unexpected error %v", s, typ, err)
			}
			return
		}
		if typ == TimestampTZ && !timestampFormattable(v.I) {
			return // years past 9999 print but are not accepted as input
		}
		back, err := Parse(Format(v), typ)
		if err != nil || Compare(back, v) != 0 {
			t.Fatalf("Parse(%q, %s) = %v formats as %q, which parses as %v, %v", s, typ, v, Format(v), back, err)
		}
	})
}

// ---- LIKE --------------------------------------------------------------------

// likeRegexp translates a LIKE pattern to an anchored regular expression.
func likeRegexp(pattern string) string {
	var b strings.Builder
	b.WriteString("(?s)^")
	runes := []rune(pattern)
	for i := 0; i < len(runes); i++ {
		switch r := runes[i]; r {
		case '%':
			b.WriteString(".*")
		case '_':
			b.WriteString(".")
		case '\\':
			i++
			b.WriteString(regexp.QuoteMeta(string(runes[i])))
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return b.String()
}

func TestLikeAgainstRegexp(t *testing.T) {
	rng := rand.New(rand.NewPCG(testSeed(t), 5))
	alphabet := []string{"a", "b", "%", "_", "\\", "é", "A"}
	matches := 0
	for range 100000 {
		var s, p strings.Builder
		for range rng.IntN(7) {
			s.WriteString([]string{"a", "b", "é", "%", "_", "A"}[rng.IntN(6)])
		}
		for range rng.IntN(6) {
			p.WriteString(alphabet[rng.IntN(len(alphabet))])
		}
		pattern := p.String()
		got, err := Like(s.String(), pattern, false)
		if strings.HasSuffix(strings.ReplaceAll(pattern, `\\`, ""), `\`) {
			if code(err) != sqlerr.InvalidEscapeSequence {
				t.Fatalf("pattern %q ending in an escape: %v", pattern, err)
			}
			continue
		}
		want := regexp.MustCompile(likeRegexp(pattern)).MatchString(s.String())
		if err != nil || got != want {
			t.Fatalf("%q LIKE %q = %v, %v; want %v", s.String(), pattern, got, err, want)
		}
		if got {
			matches++
		}
		ci, _ := Like(s.String(), pattern, true)
		wantCI := regexp.MustCompile(likeRegexp(Lower(pattern))).MatchString(Lower(s.String()))
		if ci != wantCI {
			t.Fatalf("%q ILIKE %q = %v, want %v", s.String(), pattern, ci, wantCI)
		}
	}
	if matches < 1000 {
		t.Fatalf("only %d matches: the test is too sparse", matches)
	}
}

func TestLowerUpper(t *testing.T) {
	if Lower("AbC-Ä") != "abc-Ä" || Upper("aBc-ä") != "ABC-ä" || Lower("x") != "x" {
		t.Error("ASCII-only case mapping")
	}
}

func TestTypesAndAST(t *testing.T) {
	m := map[ast.Type]Type{ast.TypeInteger: Int4, ast.TypeBigInt: Int8, ast.TypeDouble: Float8, ast.TypeText: Text,
		ast.TypeBoolean: Bool, ast.TypeTimestampTZ: TimestampTZ, ast.TypeInvalid: Unknown}
	for a, want := range m {
		if FromAST(a) != want {
			t.Errorf("FromAST(%s) = %s", a, FromAST(a))
		}
	}
	if TimestampTZ.String() != "timestamp with time zone" || Type(99).String() != "type 99" || Unknown.Valid() || !Text.Valid() {
		t.Error("Type methods")
	}
	if !Int8.IsInteger() || Float8.IsInteger() || !Float8.IsNumeric() || Text.IsNumeric() {
		t.Error("type classes")
	}
	if Null(Int4).String() != "NULL" {
		t.Error("NULL string")
	}
}
