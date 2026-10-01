package btree

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Kind is the type of one field of an encoded key. Its value is the field's
// tag byte, so kinds sort in tag order; NULL sorts after every value.
type Kind uint8

// Field kinds, in the order their tags sort.
const (
	KindBool    Kind = 0x01
	KindInt64   Kind = 0x02
	KindFloat64 Kind = 0x03
	KindBytes   Kind = 0x04
	KindNull    Kind = 0x05
)

// ErrBadKey means a byte string is not a valid encoded key.
var ErrBadKey = errors.New("btree: invalid encoded key")

// Bytes-field escaping: a 0x00 byte is written as 0x00 0xFF, and the field
// ends with 0x00 0x01.
const (
	escByte  = 0x00
	escZero  = 0xFF
	escTerm  = 0x01
	signBit  = uint64(1) << 63
	floatLen = 8
)

// canonicalNaN is the one NaN that keys store. It is the quiet NaN with no
// payload; after the float transform it sorts above +Inf.
const canonicalNaN = 0x7FF8000000000000

// AppendNull appends a NULL field to key.
func AppendNull(key []byte) []byte { return append(key, byte(KindNull)) }

// AppendBool appends a boolean field to key; false sorts before true.
func AppendBool(key []byte, v bool) []byte {
	b := byte(0)
	if v {
		b = 1
	}
	return append(key, byte(KindBool), b)
}

// AppendInt64 appends a signed integer field to key.
func AppendInt64(key []byte, v int64) []byte {
	key = append(key, byte(KindInt64))
	return binary.BigEndian.AppendUint64(key, uint64(v)^signBit)
}

// AppendFloat64 appends a floating-point field to key. -0 is stored as +0,
// and every NaN as one NaN that sorts after +Inf.
func AppendFloat64(key []byte, v float64) []byte {
	key = append(key, byte(KindFloat64))
	return binary.BigEndian.AppendUint64(key, floatToSortable(v))
}

// AppendBytes appends a byte-string field to key. A string sorts before every
// longer string it is a prefix of, so fields after it still compare in order.
func AppendBytes(key, v []byte) []byte {
	key = append(key, byte(KindBytes))
	for _, c := range v {
		if c == escByte {
			key = append(key, escByte, escZero)
		} else {
			key = append(key, c)
		}
	}
	return append(key, escByte, escTerm)
}

// AppendString appends a text field to key. Text compares by its bytes.
func AppendString(key []byte, v string) []byte {
	key = append(key, byte(KindBytes))
	for i := 0; i < len(v); i++ {
		if v[i] == escByte {
			key = append(key, escByte, escZero)
		} else {
			key = append(key, v[i])
		}
	}
	return append(key, escByte, escTerm)
}

// floatToSortable maps a float64 to a uint64 whose unsigned order is the
// float order (with -0 = +0 and NaN greatest).
func floatToSortable(v float64) uint64 {
	var bits uint64
	switch {
	case v != v: // NaN
		bits = canonicalNaN
	case v == 0:
		bits = 0 // both zeros become +0
	default:
		bits = math.Float64bits(v)
	}
	if bits&signBit != 0 {
		return ^bits
	}
	return bits | signBit
}

// sortableToFloat reverses floatToSortable.
func sortableToFloat(u uint64) uint64 {
	if u&signBit != 0 {
		return u &^ signBit
	}
	return ^u
}

// Value is one decoded key field.
type Value struct {
	Kind    Kind
	Bool    bool
	Int64   int64
	Float64 float64
	Bytes   []byte // a fresh copy
}

// DecodeKey splits an encoded key into its fields. It accepts exactly the
// byte strings the Append functions produce: anything else, including a
// non-canonical float (-0 or another NaN) or a truncated field, is ErrBadKey.
// An empty key has no fields.
func DecodeKey(key []byte) ([]Value, error) {
	var out []Value
	for pos := 0; pos < len(key); {
		v, n, err := decodeField(key[pos:])
		if err != nil {
			return nil, fmt.Errorf("decoding key field %d at byte %d: %w", len(out), pos, err)
		}
		out = append(out, v)
		pos += n
	}
	return out, nil
}

// decodeField decodes the field at the start of b and returns its length.
func decodeField(b []byte) (Value, int, error) {
	v := Value{Kind: Kind(b[0])}
	switch v.Kind {
	case KindNull:
		return v, 1, nil
	case KindBool:
		if len(b) < 2 || b[1] > 1 {
			return Value{}, 0, fmt.Errorf("bad boolean: %w", ErrBadKey)
		}
		v.Bool = b[1] == 1
		return v, 2, nil
	case KindInt64:
		if len(b) < 1+8 {
			return Value{}, 0, fmt.Errorf("truncated integer: %w", ErrBadKey)
		}
		v.Int64 = int64(binary.BigEndian.Uint64(b[1:]) ^ signBit)
		return v, 9, nil
	case KindFloat64:
		if len(b) < 1+floatLen {
			return Value{}, 0, fmt.Errorf("truncated float: %w", ErrBadKey)
		}
		u := binary.BigEndian.Uint64(b[1:])
		bits := sortableToFloat(u)
		f := math.Float64frombits(bits)
		// Only canonical encodings are valid, so a key has one encoding.
		if floatToSortable(f) != u {
			return Value{}, 0, fmt.Errorf("non-canonical float %#016x: %w", bits, ErrBadKey)
		}
		v.Float64 = f
		return v, 9, nil
	case KindBytes:
		data := []byte{}
		for i := 1; i < len(b); i++ {
			if b[i] != escByte {
				data = append(data, b[i])
				continue
			}
			if i+1 >= len(b) {
				break
			}
			switch b[i+1] {
			case escZero:
				data = append(data, escByte)
				i++
			case escTerm:
				v.Bytes = data
				return v, i + 2, nil
			default:
				return Value{}, 0, fmt.Errorf("bad escape %#02x in string: %w", b[i+1], ErrBadKey)
			}
		}
		return Value{}, 0, fmt.Errorf("unterminated string: %w", ErrBadKey)
	default:
		return Value{}, 0, fmt.Errorf("unknown tag %#02x: %w", b[0], ErrBadKey)
	}
}
