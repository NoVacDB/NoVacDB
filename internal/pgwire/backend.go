package pgwire

import (
	"encoding/binary"
	"strconv"
)

// Transaction status in ReadyForQuery.
const (
	StatusIdle = 'I'
)

// Severity of an ErrorResponse or NoticeResponse.
const (
	SeverityError  = "ERROR"
	SeverityFatal  = "FATAL"
	SeverityNotice = "NOTICE"
)

// ErrorFields are the fields of an ErrorResponse or NoticeResponse that
// NoVacDB sends. Empty strings and a zero Position are omitted.
type ErrorFields struct {
	Severity string
	Code     string
	Message  string
	Detail   string
	Hint     string
	Position int // 1-based character position in the query
}

// Buffer accumulates backend messages; the caller writes Bytes to the
// connection and then Resets it.
type Buffer struct{ b []byte }

// Bytes returns the messages built so far.
func (w *Buffer) Bytes() []byte { return w.b }

// Reset empties the buffer.
func (w *Buffer) Reset() { w.b = w.b[:0] }

// begin starts a message of type typ and returns where its length goes.
func (w *Buffer) begin(typ byte) int {
	w.b = append(w.b, typ, 0, 0, 0, 0)
	return len(w.b) - 4
}

// end fills in the length of the message begun at at.
func (w *Buffer) end(at int) {
	binary.BigEndian.PutUint32(w.b[at:], uint32(len(w.b)-at))
}

func (w *Buffer) str(s string) { w.b = append(append(w.b, s...), 0) }

func (w *Buffer) int32(v int32) { w.b = binary.BigEndian.AppendUint32(w.b, uint32(v)) }

// EncryptionDenied is the single byte that declines SSLRequest and
// GSSENCRequest.
func (w *Buffer) EncryptionDenied() { w.b = append(w.b, 'N') }

// AuthenticationOk ('R', 0).
func (w *Buffer) AuthenticationOk() {
	at := w.begin('R')
	w.int32(0)
	w.end(at)
}

// ParameterStatus ('S') reports a session parameter.
func (w *Buffer) ParameterStatus(name, value string) {
	at := w.begin('S')
	w.str(name)
	w.str(value)
	w.end(at)
}

// BackendKeyData ('K') gives the client its cancel key.
func (w *Buffer) BackendKeyData(processID, secret uint32) {
	at := w.begin('K')
	w.int32(int32(processID))
	w.int32(int32(secret))
	w.end(at)
}

// ReadyForQuery ('Z') with a transaction status.
func (w *Buffer) ReadyForQuery(status byte) {
	at := w.begin('Z')
	w.b = append(w.b, status)
	w.end(at)
}

// NegotiateProtocolVersion ('v') names the newest minor version supported
// and the protocol options not recognised.
func (w *Buffer) NegotiateProtocolVersion(minor uint32, unrecognised []string) {
	at := w.begin('v')
	w.int32(int32(minor))
	w.int32(int32(len(unrecognised)))
	for _, o := range unrecognised {
		w.str(o)
	}
	w.end(at)
}

// ErrorResponse ('E').
func (w *Buffer) ErrorResponse(f ErrorFields) { w.fields('E', f) }

// NoticeResponse ('N').
func (w *Buffer) NoticeResponse(f ErrorFields) { w.fields('N', f) }

func (w *Buffer) fields(typ byte, f ErrorFields) {
	at := w.begin(typ)
	field := func(code byte, v string) {
		if v != "" {
			w.b = append(w.b, code)
			w.str(v)
		}
	}
	field('S', f.Severity)
	field('V', f.Severity)
	field('C', f.Code)
	field('M', f.Message)
	field('D', f.Detail)
	field('H', f.Hint)
	if f.Position > 0 {
		field('P', strconv.Itoa(f.Position))
	}
	w.b = append(w.b, 0)
	w.end(at)
}
