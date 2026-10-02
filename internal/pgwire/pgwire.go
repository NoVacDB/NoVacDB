// Package pgwire implements the messages of PostgreSQL's frontend/backend
// protocol, version 3.0: framing, the startup packet, and the backend
// messages NoVacDB sends. It knows nothing about networking or the
// database, so it is tested and fuzzed on bytes. See
// docs/design/12-wire-protocol.md.
package pgwire

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Size limits (design doc section 2.2).
const (
	// MaxStartupPacket is the largest startup-phase packet, length
	// included (PostgreSQL's MAX_STARTUP_PACKET_LENGTH).
	MaxStartupPacket = 10000
	// MaxMessageSize is the largest regular message, counting its length
	// field but not its type byte.
	MaxMessageSize = 16 << 20
)

// Startup-phase request codes.
const (
	CodeCancelRequest = 80877102
	CodeSSLRequest    = 80877103
	CodeGSSENCRequest = 80877104
	// ProtocolVersion30 is protocol 3.0: major 3 in the high 16 bits.
	ProtocolVersion30 = 3 << 16
)

// ErrProtocol means the peer broke the protocol: a bad length, a malformed
// body. The server answers with FATAL 08P01 and closes the connection.
var ErrProtocol = errors.New("pgwire: protocol violation")

func protocolError(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), ErrProtocol)
}

// ReadStartupPacket reads one startup-phase packet: its code (the first
// four bytes after the length) and the rest of its body. It never reads
// more than MaxStartupPacket bytes.
func ReadStartupPacket(r io.Reader) (code uint32, body []byte, err error) {
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:4]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:4])
	if n < 8 || n > MaxStartupPacket {
		return 0, nil, protocolError("invalid length of startup packet: %d", n)
	}
	if _, err := io.ReadFull(r, hdr[4:]); err != nil {
		return 0, nil, unexpectedEOF(err)
	}
	body = make([]byte, n-8)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, unexpectedEOF(err)
	}
	return binary.BigEndian.Uint32(hdr[4:]), body, nil
}

// unexpectedEOF turns a clean EOF in the middle of a packet into
// io.ErrUnexpectedEOF.
func unexpectedEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// Param is one startup parameter.
type Param struct{ Name, Value string }

// Startup is a decoded StartupMessage.
type Startup struct {
	Major, Minor uint16
	Params       []Param // in the order sent
}

// ParseStartup decodes the body of a StartupMessage (after the length and
// the protocol code): NUL-terminated name and value pairs, ended by an
// empty name, with nothing after it.
func ParseStartup(code uint32, body []byte) (Startup, error) {
	s := Startup{Major: uint16(code >> 16), Minor: uint16(code)}
	for {
		name, rest, err := cstring(body)
		if err != nil {
			return Startup{}, protocolError("startup packet: %v", err)
		}
		body = rest
		if name == "" {
			if len(body) != 0 {
				return Startup{}, protocolError("startup packet: %d bytes after the last parameter", len(body))
			}
			return s, nil
		}
		value, rest, err := cstring(body)
		if err != nil {
			return Startup{}, protocolError("startup packet: parameter %q: %v", name, err)
		}
		body = rest
		s.Params = append(s.Params, Param{name, value})
	}
}

// cstring splits a NUL-terminated string off the front of b.
func cstring(b []byte) (string, []byte, error) {
	for i, c := range b {
		if c == 0 {
			return string(b[:i]), b[i+1:], nil
		}
	}
	return "", nil, errors.New("missing terminator")
}

// EncodeStartup builds a StartupMessage packet, length included. Tests and
// clients use it; the server only reads startup packets.
func EncodeStartup(code uint32, params []Param) []byte {
	b := make([]byte, 8, 64)
	binary.BigEndian.PutUint32(b[4:], code)
	for _, p := range params {
		b = append(append(b, p.Name...), 0)
		b = append(append(b, p.Value...), 0)
	}
	b = append(b, 0)
	binary.BigEndian.PutUint32(b, uint32(len(b)))
	return b
}

// EncodeRequest builds a body-less startup-phase request (SSLRequest,
// GSSENCRequest).
func EncodeRequest(code uint32) []byte {
	b := binary.BigEndian.AppendUint32(nil, 8)
	return binary.BigEndian.AppendUint32(b, code)
}

// Reader reads regular (typed) messages.
type Reader struct {
	r   *bufio.Reader
	max int
}

// NewReader returns a Reader on r accepting messages up to MaxMessageSize.
func NewReader(r *bufio.Reader) *Reader { return &Reader{r: r, max: MaxMessageSize} }

// ReadMessage reads one message: its type byte and its body. A length out
// of bounds is ErrProtocol, reported before any of the body is read.
func (r *Reader) ReadMessage() (typ byte, body []byte, err error) {
	typ, err = r.r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	var hdr [4]byte
	if _, err := io.ReadFull(r.r, hdr[:]); err != nil {
		return 0, nil, unexpectedEOF(err)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n < 4 || n > uint32(r.max) {
		return 0, nil, protocolError("invalid message length %d for message type %q", n, typ)
	}
	body, err = readBody(r.r, int(n-4))
	if err != nil {
		return 0, nil, unexpectedEOF(err)
	}
	return typ, body, nil
}

// readBody reads exactly n bytes. Large bodies grow as their bytes arrive,
// so a client that announces a big message and then stalls cannot make the
// server allocate it all up front.
func readBody(r io.Reader, n int) ([]byte, error) {
	const eager = 64 << 10
	if n <= eager {
		b := make([]byte, n)
		_, err := io.ReadFull(r, b)
		return b, err
	}
	var buf bytes.Buffer
	buf.Grow(eager)
	got, err := io.Copy(&buf, io.LimitReader(r, int64(n)))
	if err != nil {
		return nil, err
	}
	if got != int64(n) {
		return nil, io.ErrUnexpectedEOF
	}
	return buf.Bytes(), nil
}

// ParseQuery decodes a Query message body: one NUL-terminated string.
func ParseQuery(body []byte) (string, error) {
	q, rest, err := cstring(body)
	if err != nil || len(rest) != 0 {
		return "", protocolError("malformed Query message")
	}
	return q, nil
}
