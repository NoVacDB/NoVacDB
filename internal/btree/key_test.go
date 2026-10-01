package btree

import (
	"bytes"
	"cmp"
	"errors"
	"math"
	"math/rand/v2"
	"testing"
)

// encode appends v to key with the matching Append function.
func encode(key []byte, v Value) []byte {
	switch v.Kind {
	case KindNull:
		return AppendNull(key)
	case KindBool:
		return AppendBool(key, v.Bool)
	case KindInt64:
		return AppendInt64(key, v.Int64)
	case KindFloat64:
		return AppendFloat64(key, v.Float64)
	default:
		return AppendBytes(key, v.Bytes)
	}
}

func encodeAll(vs []Value) []byte {
	var key []byte
	for _, v := range vs {
		key = encode(key, v)
	}
	return key
}

// compareValues is the order keys must have: SQL comparison of one column,
// with NULL last, -0 = +0 and NaN above everything else (all NaNs equal).
func compareValues(a, b Value) int {
	if a.Kind == KindNull || b.Kind == KindNull {
		return cmp.Compare(boolInt(a.Kind == KindNull), boolInt(b.Kind == KindNull))
	}
	switch a.Kind {
	case KindBool:
		return cmp.Compare(boolInt(a.Bool), boolInt(b.Bool))
	case KindInt64:
		return cmp.Compare(a.Int64, b.Int64)
	case KindFloat64:
		an, bn := math.IsNaN(a.Float64), math.IsNaN(b.Float64)
		if an || bn {
			return cmp.Compare(boolInt(an), boolInt(bn))
		}
		return cmp.Compare(a.Float64, b.Float64)
	default:
		return bytes.Compare(a.Bytes, b.Bytes)
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func compareRows(a, b []Value) int {
	for i := range a {
		if c := compareValues(a[i], b[i]); c != 0 {
			return c
		}
	}
	return 0
}

var interestingFloats = []float64{
	math.Inf(-1), -math.MaxFloat64, -1e300, -2.5, -1, -math.SmallestNonzeroFloat64,
	math.Copysign(0, -1), 0, math.SmallestNonzeroFloat64, 1, 2.5, 1e300,
	math.MaxFloat64, math.Inf(1), math.NaN(), -math.NaN(), math.Float64frombits(0x7FF0000000000001),
	math.Float64frombits(0xFFFFFFFFFFFFFFFF),
}

var interestingInts = []int64{math.MinInt64, math.MinInt64 + 1, -256, -1, 0, 1, 255, 256, math.MaxInt64 - 1, math.MaxInt64}

// randValue returns a random value of kind k (or NULL, sometimes).
func randValue(rng *rand.Rand, k Kind) Value {
	if rng.IntN(8) == 0 {
		return Value{Kind: KindNull}
	}
	v := Value{Kind: k}
	switch k {
	case KindBool:
		v.Bool = rng.IntN(2) == 1
	case KindInt64:
		switch rng.IntN(3) {
		case 0:
			v.Int64 = interestingInts[rng.IntN(len(interestingInts))]
		case 1:
			v.Int64 = int64(rng.IntN(7)) - 3
		default:
			v.Int64 = int64(rng.Uint64())
		}
	case KindFloat64:
		switch rng.IntN(3) {
		case 0:
			v.Float64 = interestingFloats[rng.IntN(len(interestingFloats))]
		case 1:
			v.Float64 = float64(rng.IntN(7)-3) / 2
		default:
			v.Float64 = math.Float64frombits(rng.Uint64())
		}
	case KindBytes:
		// Small alphabets with 0x00, 0x01 and 0xFF make prefixes and
		// escapes common.
		alphabet := []byte{0x00, 0x01, 0xFF, 'a'}
		b := make([]byte, rng.IntN(5))
		for i := range b {
			b[i] = alphabet[rng.IntN(len(alphabet))]
		}
		v.Bytes = b
	}
	return v
}

func sign(x int) int { return cmp.Compare(x, 0) }

func TestKeyOrderMatchesValueOrder(t *testing.T) {
	rng := rand.New(rand.NewPCG(testSeed(t), 1))
	kinds := []Kind{KindBool, KindInt64, KindFloat64, KindBytes}
	checked := 0
	for range 200 {
		schema := make([]Kind, 1+rng.IntN(3))
		for i := range schema {
			schema[i] = kinds[rng.IntN(len(kinds))]
		}
		rows := make([][]Value, 60)
		for i := range rows {
			rows[i] = make([]Value, len(schema))
			for j, k := range schema {
				rows[i][j] = randValue(rng, k)
			}
		}
		for _, a := range rows {
			for _, b := range rows {
				want := compareRows(a, b)
				got := sign(bytes.Compare(encodeAll(a), encodeAll(b)))
				if got != want {
					t.Fatalf("schema %v: %v vs %v: bytes compare %d, values compare %d", schema, a, b, got, want)
				}
				checked++
			}
		}
	}
	t.Logf("%d pairs checked", checked)
}

func TestKeyEdgeOrder(t *testing.T) {
	// Each list is strictly increasing.
	var keys [][]byte
	for _, v := range interestingInts {
		keys = append(keys, AppendInt64(nil, v))
	}
	keys = append(keys, AppendNull(nil))
	checkIncreasing(t, "ints", keys)

	keys = nil
	for _, f := range []float64{math.Inf(-1), -math.MaxFloat64, -1, -math.SmallestNonzeroFloat64, 0,
		math.SmallestNonzeroFloat64, 1, math.MaxFloat64, math.Inf(1), math.NaN()} {
		keys = append(keys, AppendFloat64(nil, f))
	}
	keys = append(keys, AppendNull(nil))
	checkIncreasing(t, "floats", keys)

	keys = nil
	for _, s := range [][]byte{{}, {0}, {0, 0}, {0, 1}, {0, 0xFF}, {1}, {'a'}, {'a', 0}, {'a', 0, 0}, {'a', 1}, {'a', 'a'}, {'b'}, {0xFF}, {0xFF, 0xFF}} {
		keys = append(keys, AppendBytes(nil, s))
	}
	keys = append(keys, AppendNull(nil))
	checkIncreasing(t, "bytes", keys)

	checkIncreasing(t, "bools", [][]byte{AppendBool(nil, false), AppendBool(nil, true), AppendNull(nil)})

	// A shorter string sorts first even when the next field is large.
	short := AppendInt64(AppendBytes(nil, []byte("ab")), math.MaxInt64)
	long := AppendInt64(AppendBytes(nil, []byte("ab\x00")), math.MinInt64)
	checkIncreasing(t, "composite", [][]byte{short, long})

	// Equal values encode equally.
	if !bytes.Equal(AppendFloat64(nil, 0), AppendFloat64(nil, math.Copysign(0, -1))) {
		t.Error("-0 and +0 encode differently")
	}
	if !bytes.Equal(AppendFloat64(nil, math.NaN()), AppendFloat64(nil, math.Float64frombits(0xFFF0000000000123))) {
		t.Error("two NaNs encode differently")
	}
	if !bytes.Equal(AppendString(nil, "a\x00b"), AppendBytes(nil, []byte("a\x00b"))) {
		t.Error("AppendString and AppendBytes differ")
	}
}

func checkIncreasing(t *testing.T, name string, keys [][]byte) {
	t.Helper()
	for i := 1; i < len(keys); i++ {
		if bytes.Compare(keys[i-1], keys[i]) >= 0 {
			t.Errorf("%s: key %d %x is not below key %d %x", name, i-1, keys[i-1], i, keys[i])
		}
	}
}

func TestKeyGoldenBytes(t *testing.T) {
	cases := []struct {
		name string
		got  []byte
		want []byte
	}{
		{"null", AppendNull(nil), []byte{0x05}},
		{"false", AppendBool(nil, false), []byte{0x01, 0x00}},
		{"true", AppendBool(nil, true), []byte{0x01, 0x01}},
		{"int 0", AppendInt64(nil, 0), []byte{0x02, 0x80, 0, 0, 0, 0, 0, 0, 0}},
		{"int -1", AppendInt64(nil, -1), []byte{0x02, 0x7F, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}},
		{"int min", AppendInt64(nil, math.MinInt64), []byte{0x02, 0, 0, 0, 0, 0, 0, 0, 0}},
		{"int 258", AppendInt64(nil, 258), []byte{0x02, 0x80, 0, 0, 0, 0, 0, 0x01, 0x02}},
		{"float 0", AppendFloat64(nil, 0), []byte{0x03, 0x80, 0, 0, 0, 0, 0, 0, 0}},
		{"float 1", AppendFloat64(nil, 1), []byte{0x03, 0xBF, 0xF0, 0, 0, 0, 0, 0, 0}},
		{"float -1", AppendFloat64(nil, -1), []byte{0x03, 0x40, 0x0F, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}},
		{"float nan", AppendFloat64(nil, math.NaN()), []byte{0x03, 0xFF, 0xF8, 0, 0, 0, 0, 0, 0}},
		{"bytes", AppendBytes(nil, []byte{'a', 0, 'b'}), []byte{0x04, 'a', 0x00, 0xFF, 'b', 0x00, 0x01}},
		{"empty", AppendString(nil, ""), []byte{0x04, 0x00, 0x01}},
		{"append", AppendBool(AppendInt64(nil, 1), true), []byte{0x02, 0x80, 0, 0, 0, 0, 0, 0, 1, 0x01, 0x01}},
	}
	for _, c := range cases {
		if !bytes.Equal(c.got, c.want) {
			t.Errorf("%s: got % x, want % x", c.name, c.got, c.want)
		}
	}
}

func TestDecodeKeyRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(testSeed(t), 2))
	kinds := []Kind{KindBool, KindInt64, KindFloat64, KindBytes}
	for range 5000 {
		vs := make([]Value, rng.IntN(5))
		for i := range vs {
			vs[i] = randValue(rng, kinds[rng.IntN(len(kinds))])
		}
		key := encodeAll(vs)
		got, err := DecodeKey(key)
		if err != nil {
			t.Fatalf("decoding %x: %v", key, err)
		}
		if len(got) != len(vs) {
			t.Fatalf("decoded %d fields, want %d", len(got), len(vs))
		}
		for i := range vs {
			if got[i].Kind != vs[i].Kind || compareValues(got[i], vs[i]) != 0 {
				t.Fatalf("field %d: got %+v, want %+v", i, got[i], vs[i])
			}
		}
		if again := encodeAll(got); !bytes.Equal(again, key) {
			t.Fatalf("re-encoding %x gave %x", key, again)
		}
	}
	// Canonical forms come back as such.
	got, err := DecodeKey(AppendFloat64(nil, math.Copysign(0, -1)))
	if err != nil || math.Signbit(got[0].Float64) {
		t.Errorf("-0 decoded as %v, %v", got, err)
	}
	if vs, err := DecodeKey(nil); err != nil || len(vs) != 0 {
		t.Errorf("empty key: %v, %v", vs, err)
	}
}

func TestDecodeKeyRejectsInvalid(t *testing.T) {
	bad := map[string][]byte{
		"unknown tag":       {0x06},
		"zero tag":          {0x00},
		"bool value 2":      {0x01, 0x02},
		"truncated bool":    {0x01},
		"truncated int":     {0x02, 0x80, 0},
		"truncated float":   {0x03, 0x80},
		"negative zero":     {0x03, 0x7F, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
		"other NaN":         {0x03, 0xFF, 0xF8, 0, 0, 0, 0, 0, 1},
		"negative NaN":      {0x03, 0x00, 0x07, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
		"unterminated":      {0x04, 'a'},
		"dangling escape":   {0x04, 'a', 0x00},
		"bad escape":        {0x04, 0x00, 0x02, 0x00, 0x01},
		"trailing garbage":  append(AppendInt64(nil, 1), 0x09),
		"valid then broken": append(AppendBool(nil, true), 0x02, 1),
	}
	for name, key := range bad {
		if vs, err := DecodeKey(key); !errors.Is(err, ErrBadKey) {
			t.Errorf("%s: DecodeKey(% x) = %v, %v; want ErrBadKey", name, key, vs, err)
		}
	}
}

func FuzzDecodeKey(f *testing.F) {
	f.Add([]byte{})
	f.Add(AppendBytes(AppendInt64(nil, -5), []byte{0, 1, 0xFF}))
	f.Add(AppendNull(AppendFloat64(AppendBool(nil, true), math.NaN())))
	f.Add([]byte{0x04, 0x00})
	f.Fuzz(func(t *testing.T, key []byte) {
		vs, err := DecodeKey(key)
		if err != nil {
			if !errors.Is(err, ErrBadKey) {
				t.Fatalf("unexpected error type: %v", err)
			}
			return
		}
		// A valid key has exactly one encoding.
		if again := encodeAll(vs); !bytes.Equal(again, key) {
			t.Fatalf("% x decodes to %v, which encodes to % x", key, vs, again)
		}
	})
}
