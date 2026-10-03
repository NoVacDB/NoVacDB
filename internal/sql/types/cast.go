package types

import (
	"math"

	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
)

// CanCast reports whether an explicit cast from one type to another exists.
func CanCast(from, to Type) bool {
	switch {
	case from == to, from == Unknown, from == Text, to == Text:
		return true
	case from.IsNumeric() && to.IsNumeric():
		return true
	case from == Int4 && to == Bool, from == Bool && to == Int4:
		return true
	}
	return false
}

// CannotCast is the error for a cast that does not exist.
func CannotCast(from, to Type) *sqlerr.Error {
	return sqlerr.New(sqlerr.CannotCoerce, "cannot cast type %s to %s", from, to)
}

// Cast converts v to type to, as an explicit CAST does.
func Cast(v Value, to Type) (Value, error) {
	if !CanCast(v.T, to) {
		return Value{}, CannotCast(v.T, to)
	}
	if v.Null {
		return Null(to), nil
	}
	from := v.T
	switch {
	case from == to:
		return v, nil
	case from == Unknown || from == Text:
		return Parse(v.S, to)
	case to == Text && from == Bool:
		// PostgreSQL's boolean-to-text cast spells the word out, unlike
		// the output form (t, f).
		if v.Bool() {
			return NewText("true"), nil
		}
		return NewText("false"), nil
	case to == Text:
		return NewText(Format(v)), nil
	case from.IsInteger() && to.IsInteger():
		return intResult(to, v.I)
	case from.IsInteger() && to == Float8:
		return NewFloat8(float64(v.I)), nil
	case from == Float8 && to.IsInteger():
		return floatToInt(v.F, to)
	case from == Int4 && to == Bool:
		return NewBool(v.I != 0), nil
	case from == Bool && to == Int4:
		return Value{T: Int4, I: v.I}, nil
	}
	return Value{}, CannotCast(from, to)
}

// floatToInt rounds half to even and range-checks, as PostgreSQL's
// float8-to-integer casts do.
func floatToInt(f float64, to Type) (Value, error) {
	r := math.RoundToEven(f)
	var ok bool
	if to == Int4 {
		ok = r >= math.MinInt32 && r <= math.MaxInt32
	} else {
		// 2^63 is the first double past int64's range; -2^63 fits.
		ok = r >= -(1<<63) && r < 1<<63
	}
	if !ok { // NaN fails both comparisons
		return Value{}, outOfRange(to)
	}
	return Value{T: to, I: int64(r)}, nil
}
