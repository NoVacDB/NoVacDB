// Package types implements NoVacDB's SQL types and values: NULL and
// three-valued logic, comparison, arithmetic, casts, the text input and
// output forms, the row (tuple) encoding and index key encoding. See
// docs/design/10-executor.md, sections 2.1 to 2.3.
package types

import (
	"math"
	"strconv"

	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/ast"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
)

// Type is a SQL type.
type Type uint8

// Types. The numbers are stored in the catalog; do not renumber.
const (
	// Unknown is the type of an untyped literal ('...' or NULL) until
	// context gives it one.
	Unknown     Type = 0
	Int4        Type = 1
	Int8        Type = 2
	Float8      Type = 3
	Text        Type = 4
	Bool        Type = 5
	TimestampTZ Type = 6
)

var typeNames = [...]string{
	Unknown: "unknown", Int4: "integer", Int8: "bigint", Float8: "double precision",
	Text: "text", Bool: "boolean", TimestampTZ: "timestamp with time zone",
}

// String returns PostgreSQL's name for the type, as used in messages.
func (t Type) String() string {
	if int(t) < len(typeNames) {
		return typeNames[t]
	}
	return "type " + strconv.Itoa(int(t))
}

// Valid reports whether t is a column type (not Unknown, not out of range).
func (t Type) Valid() bool { return t >= Int4 && t <= TimestampTZ }

// IsNumeric reports whether t is an integer or floating-point type.
func (t Type) IsNumeric() bool { return t == Int4 || t == Int8 || t == Float8 }

// IsInteger reports whether t is an integer type.
func (t Type) IsInteger() bool { return t == Int4 || t == Int8 }

// FromAST converts a parsed type name.
func FromAST(t ast.Type) Type {
	switch t {
	case ast.TypeInteger:
		return Int4
	case ast.TypeBigInt:
		return Int8
	case ast.TypeDouble:
		return Float8
	case ast.TypeText:
		return Text
	case ast.TypeBoolean:
		return Bool
	case ast.TypeTimestampTZ:
		return TimestampTZ
	}
	return Unknown
}

// Value is a SQL value. Which payload field is used depends on T: I for
// Int4, Int8, TimestampTZ and Bool (0 or 1), F for Float8, S for Text and
// Unknown. A NULL value has Null set and no payload.
type Value struct {
	T    Type
	Null bool
	I    int64
	F    float64
	S    string
}

// Constructors.

// NewInt4 returns an integer value.
func NewInt4(v int32) Value { return Value{T: Int4, I: int64(v)} }

// NewInt8 returns a bigint value.
func NewInt8(v int64) Value { return Value{T: Int8, I: v} }

// NewFloat8 returns a double precision value.
func NewFloat8(v float64) Value { return Value{T: Float8, F: v} }

// NewText returns a text value.
func NewText(s string) Value { return Value{T: Text, S: s} }

// NewBool returns a boolean value.
func NewBool(b bool) Value {
	if b {
		return Value{T: Bool, I: 1}
	}
	return Value{T: Bool}
}

// NewTimestampTZ returns a timestamptz value from microseconds since
// 2000-01-01 00:00:00 UTC.
func NewTimestampTZ(micros int64) Value { return Value{T: TimestampTZ, I: micros} }

// NewUnknown returns an untyped string literal.
func NewUnknown(s string) Value { return Value{T: Unknown, S: s} }

// Null returns the NULL of type t.
func Null(t Type) Value { return Value{T: t, Null: true} }

// Bool returns a boolean value's truth. The value must be a non-NULL Bool.
func (v Value) Bool() bool { return v.I != 0 }

// String formats the value for tests and messages: NULL, or the text
// output form.
func (v Value) String() string {
	if v.Null {
		return "NULL"
	}
	return Format(v)
}

// --- comparison and logic -----------------------------------------------

// Compare orders two non-NULL values of the same type (Int4 and Int8 may be
// mixed): -1, 0 or +1. Doubles order numerically with -0 = +0 and NaN equal
// to itself and above everything else; text orders by bytes.
func Compare(a, b Value) int {
	switch a.T {
	case Float8:
		return compareFloat(a.F, b.F)
	case Text, Unknown:
		switch {
		case a.S < b.S:
			return -1
		case a.S > b.S:
			return 1
		}
		return 0
	default:
		switch {
		case a.I < b.I:
			return -1
		case a.I > b.I:
			return 1
		}
		return 0
	}
}

func compareFloat(a, b float64) int {
	an, bn := math.IsNaN(a), math.IsNaN(b)
	switch {
	case an && bn:
		return 0
	case an:
		return 1
	case bn:
		return -1
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0 // equal, including -0 and +0
}

// Not is three-valued NOT of a Bool.
func Not(a Value) Value {
	if a.Null {
		return Null(Bool)
	}
	return NewBool(!a.Bool())
}

// And is three-valued AND of two Bools: FALSE wins over NULL.
func And(a, b Value) Value {
	if !a.Null && !a.Bool() || !b.Null && !b.Bool() {
		return NewBool(false)
	}
	if a.Null || b.Null {
		return Null(Bool)
	}
	return NewBool(true)
}

// Or is three-valued OR of two Bools: TRUE wins over NULL.
func Or(a, b Value) Value {
	if !a.Null && a.Bool() || !b.Null && b.Bool() {
		return NewBool(true)
	}
	if a.Null || b.Null {
		return Null(Bool)
	}
	return NewBool(false)
}

// --- arithmetic ----------------------------------------------------------

func outOfRange(t Type) *sqlerr.Error {
	switch t {
	case Int4:
		return sqlerr.New(sqlerr.NumericValueOutOfRange, "integer out of range")
	case Int8:
		return sqlerr.New(sqlerr.NumericValueOutOfRange, "bigint out of range")
	}
	return sqlerr.New(sqlerr.NumericValueOutOfRange, "value out of range: overflow")
}

func divisionByZero() *sqlerr.Error { return sqlerr.New(sqlerr.DivisionByZero, "division by zero") }

func underflow() *sqlerr.Error {
	return sqlerr.New(sqlerr.NumericValueOutOfRange, "value out of range: underflow")
}

// intResult checks that an integer result fits its type.
func intResult(t Type, v int64) (Value, error) {
	if t == Int4 && (v < math.MinInt32 || v > math.MaxInt32) {
		return Value{}, outOfRange(Int4)
	}
	return Value{T: t, I: v}, nil
}

// floatResult checks a double precision result for overflow (an infinite
// result from finite inputs) and underflow (a zero result that should not
// be).
func floatResult(r float64, inputsInf, canBeZero bool) (Value, error) {
	if math.IsInf(r, 0) && !inputsInf {
		return Value{}, outOfRange(Float8)
	}
	if r == 0 && !canBeZero {
		return Value{}, underflow()
	}
	return NewFloat8(r), nil
}

// Arithmetic operands must be non-NULL and of one type: Int4, Int8 or
// Float8 (the caller converts them first).

// Add returns a + b.
func Add(a, b Value) (Value, error) {
	if a.T == Float8 {
		return floatResult(a.F+b.F, math.IsInf(a.F, 0) || math.IsInf(b.F, 0), true)
	}
	r := a.I + b.I
	// Overflow iff the operands have the same sign and the result differs.
	if a.I >= 0 == (b.I >= 0) && r >= 0 != (a.I >= 0) {
		return Value{}, outOfRange(a.T)
	}
	return intResult(a.T, r)
}

// Sub returns a - b.
func Sub(a, b Value) (Value, error) {
	if a.T == Float8 {
		return floatResult(a.F-b.F, math.IsInf(a.F, 0) || math.IsInf(b.F, 0), true)
	}
	r := a.I - b.I
	if a.I >= 0 != (b.I >= 0) && r >= 0 != (a.I >= 0) {
		return Value{}, outOfRange(a.T)
	}
	return intResult(a.T, r)
}

// Mul returns a * b.
func Mul(a, b Value) (Value, error) {
	if a.T == Float8 {
		return floatResult(a.F*b.F, math.IsInf(a.F, 0) || math.IsInf(b.F, 0), a.F == 0 || b.F == 0)
	}
	if a.I == 0 || b.I == 0 {
		return Value{T: a.T}, nil
	}
	r := a.I * b.I
	if r/b.I != a.I || a.I == -1 && b.I == math.MinInt64 || b.I == -1 && a.I == math.MinInt64 {
		return Value{}, outOfRange(a.T)
	}
	return intResult(a.T, r)
}

// Div returns a / b; integer division truncates toward zero.
func Div(a, b Value) (Value, error) {
	if a.T == Float8 {
		if b.F == 0 {
			return Value{}, divisionByZero()
		}
		return floatResult(a.F/b.F, math.IsInf(a.F, 0), a.F == 0 || math.IsInf(b.F, 0))
	}
	if b.I == 0 {
		return Value{}, divisionByZero()
	}
	if b.I == -1 {
		return Neg(a) // MinInt / -1 overflows
	}
	return intResult(a.T, a.I/b.I)
}

// Mod returns a % b for integers, with the sign of a.
func Mod(a, b Value) (Value, error) {
	if b.I == 0 {
		return Value{}, divisionByZero()
	}
	if b.I == -1 {
		return Value{T: a.T}, nil // MinInt % -1 would trap; the answer is 0
	}
	return Value{T: a.T, I: a.I % b.I}, nil
}

// Neg returns -a.
func Neg(a Value) (Value, error) {
	if a.T == Float8 {
		return NewFloat8(-a.F), nil
	}
	if a.I == math.MinInt64 {
		return Value{}, outOfRange(a.T)
	}
	return intResult(a.T, -a.I)
}

// Abs returns |a|.
func Abs(a Value) (Value, error) {
	if a.T == Float8 {
		return NewFloat8(math.Abs(a.F)), nil
	}
	if a.I < 0 {
		return Neg(a)
	}
	return a, nil
}

// Pow returns a ^ b for doubles, with PostgreSQL's errors.
func Pow(a, b Value) (Value, error) {
	x, y := a.F, b.F
	if x == 0 && y < 0 {
		return Value{}, sqlerr.New("2201F", "zero raised to a negative power is undefined")
	}
	if x < 0 && !math.IsInf(x, 0) && y != math.Trunc(y) && !math.IsNaN(y) && !math.IsInf(y, 0) {
		return Value{}, sqlerr.New("2201F", "a negative number raised to a non-integer power yields a complex result")
	}
	r := math.Pow(x, y)
	if math.IsNaN(r) && !math.IsNaN(x) && !math.IsNaN(y) {
		return Value{}, outOfRange(Float8)
	}
	// As PostgreSQL: an infinite result from finite inputs overflows, and a
	// zero result from a non-zero base underflows.
	inputsInf := math.IsInf(x, 0) || math.IsInf(y, 0)
	return floatResult(r, inputsInf, x == 0 || inputsInf)
}

// Concat returns a || b, both non-NULL text.
func Concat(a, b Value) Value { return NewText(a.S + b.S) }
