package server

import (
	"encoding/binary"
	"math"
	"time"
	"unicode/utf8"

	"github.com/vikrant-choudhary06/NoVacDB/internal/pgwire"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/types"
)

// More type OIDs, accepted for parameters (design doc section 2.10).
const (
	oidInt2    = 21
	oidFloat4  = 700
	oidUnknown = 705
	oidVarchar = 1043
)

// typeInfo returns a column type's PostgreSQL OID and size.
func typeInfo(t types.Type) (oid uint32, size int16) {
	switch t {
	case types.Int4:
		return pgwire.OIDInt4, 4
	case types.Int8:
		return pgwire.OIDInt8, 8
	case types.Float8:
		return pgwire.OIDFloat8, 8
	case types.Bool:
		return pgwire.OIDBool, 1
	case types.TimestampTZ:
		return pgwire.OIDTimestampTZ, 8
	}
	return pgwire.OIDText, -1
}

// paramType maps a parameter type OID a client declares to the type the
// parameter gets: the six types themselves, 0 and unknown for "infer it",
// and smallint, real and varchar as the type that holds their values
// exactly (their binary forms are still read as theirs).
func paramType(oid uint32) (types.Type, bool) {
	switch oid {
	case 0, oidUnknown:
		return types.Unknown, true
	case pgwire.OIDInt4, oidInt2:
		return types.Int4, true
	case pgwire.OIDInt8:
		return types.Int8, true
	case pgwire.OIDFloat8, oidFloat4:
		return types.Float8, true
	case pgwire.OIDText, oidVarchar:
		return types.Text, true
	case pgwire.OIDBool:
		return types.Bool, true
	case pgwire.OIDTimestampTZ:
		return types.TimestampTZ, true
	}
	return 0, false
}

// The valid range of binary timestamptz values: the infinities, and the
// years 1 to 10000 in UTC, which the text form also reaches (9999-12-31
// with a negative zone offset passes into 10000).
var (
	minTimestamp = types.TimestampFromTime(time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC))
	maxTimestamp = types.TimestampFromTime(time.Date(10001, 1, 1, 0, 0, 0, 0, time.UTC)) - 1
)

// decodeParam converts a parameter value from the client: data in the
// format, for a parameter of type t sent as wire type oid (which decides
// the binary form). nil data is NULL.
func decodeParam(n int, data []byte, format int16, oid uint32, t types.Type) (types.Value, error) {
	if data == nil {
		return types.Null(t), nil
	}
	badBinary := func() (types.Value, error) {
		return types.Value{}, sqlerr.New(sqlerr.InvalidBinaryRepresentation, "incorrect binary data format in bind parameter %d", n)
	}
	switch format {
	case pgwire.FormatText:
		if err := checkUTF8(data); err != nil {
			return types.Value{}, err
		}
		return types.Parse(string(data), t)
	case pgwire.FormatBinary:
	default:
		return types.Value{}, sqlerr.New(sqlerr.InvalidParameterValue, "unsupported format code: %d", format)
	}
	be := binary.BigEndian
	switch {
	case oid == oidInt2 && len(data) == 2:
		return types.NewInt4(int32(int16(be.Uint16(data)))), nil
	case t == types.Int4 && oid != oidInt2 && len(data) == 4:
		return types.NewInt4(int32(be.Uint32(data))), nil
	case t == types.Int8 && len(data) == 8:
		return types.NewInt8(int64(be.Uint64(data))), nil
	case oid == oidFloat4 && len(data) == 4:
		return types.NewFloat8(float64(math.Float32frombits(be.Uint32(data)))), nil
	case t == types.Float8 && oid != oidFloat4 && len(data) == 8:
		return types.NewFloat8(math.Float64frombits(be.Uint64(data))), nil
	case t == types.Bool && len(data) == 1:
		return types.NewBool(data[0] != 0), nil
	case t == types.Text:
		if err := checkUTF8(data); err != nil {
			return types.Value{}, err
		}
		return types.NewText(string(data)), nil
	case t == types.TimestampTZ && len(data) == 8:
		v := int64(be.Uint64(data))
		if v != types.TimestampInfinity && v != types.TimestampNegInfinity && (v < minTimestamp || v > maxTimestamp) {
			return types.Value{}, sqlerr.New(sqlerr.DatetimeFieldOverflow, "timestamp out of range")
		}
		return types.NewTimestampTZ(v), nil
	}
	return badBinary()
}

// checkUTF8 rejects bytes that are not UTF-8, and NUL, which no text
// value can hold, as PostgreSQL does.
func checkUTF8(b []byte) error {
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == 0 || r == utf8.RuneError && size == 1 {
			return sqlerr.New(sqlerr.CharacterNotInRepertoire, "invalid byte sequence for encoding \"UTF8\": 0x%02x", b[i])
		}
		i += size
	}
	return nil
}

// appendValue appends a non-NULL value's form for a result column.
func appendValue(dst []byte, v types.Value, format int16) []byte {
	if format == pgwire.FormatText {
		return append(dst, types.Format(v)...)
	}
	be := binary.BigEndian
	switch v.T {
	case types.Int4:
		return be.AppendUint32(dst, uint32(int32(v.I)))
	case types.Int8, types.TimestampTZ:
		return be.AppendUint64(dst, uint64(v.I))
	case types.Float8:
		return be.AppendUint64(dst, math.Float64bits(v.F))
	case types.Bool:
		return append(dst, byte(v.I&1))
	}
	return append(dst, v.S...)
}
