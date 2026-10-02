package pgwire

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestBackendMessageBytes(t *testing.T) {
	cases := []struct {
		name  string
		build func(*Buffer)
		want  []byte
	}{
		{"encryption denied", (*Buffer).EncryptionDenied, []byte("N")},
		{"authentication ok", (*Buffer).AuthenticationOk, []byte{'R', 0, 0, 0, 8, 0, 0, 0, 0}},
		{"parameter status", func(b *Buffer) { b.ParameterStatus("TimeZone", "UTC") },
			append([]byte{'S', 0, 0, 0, 17}, "TimeZone\x00UTC\x00"...)},
		{"backend key data", func(b *Buffer) { b.BackendKeyData(0x01020304, 0xfffefdfc) },
			[]byte{'K', 0, 0, 0, 12, 1, 2, 3, 4, 0xff, 0xfe, 0xfd, 0xfc}},
		{"ready for query", func(b *Buffer) { b.ReadyForQuery(StatusIdle) }, []byte{'Z', 0, 0, 0, 5, 'I'}},
		{"negotiate, no options", func(b *Buffer) { b.NegotiateProtocolVersion(0, nil) },
			[]byte{'v', 0, 0, 0, 12, 0, 0, 0, 0, 0, 0, 0, 0}},
		{"negotiate, options", func(b *Buffer) { b.NegotiateProtocolVersion(0, []string{"_pq_.a", "_pq_.bc"}) },
			append([]byte{'v', 0, 0, 0, 27, 0, 0, 0, 0, 0, 0, 0, 2}, "_pq_.a\x00_pq_.bc\x00"...)},
		{"error, all fields", func(b *Buffer) {
			b.ErrorResponse(ErrorFields{Severity: "ERROR", Code: "42P01", Message: "m", Detail: "d", Hint: "h", Position: 12})
		}, append([]byte{'E', 0, 0, 0, 39}, "SERROR\x00VERROR\x00C42P01\x00Mm\x00Dd\x00Hh\x00P12\x00\x00"...)},
		{"notice, few fields", func(b *Buffer) { b.NoticeResponse(ErrorFields{Severity: "NOTICE", Code: "00000", Message: "x"}) },
			append([]byte{'N', 0, 0, 0, 31}, "SNOTICE\x00VNOTICE\x00C00000\x00Mx\x00\x00"...)},
	}
	for _, c := range cases {
		var b Buffer
		c.build(&b)
		if !bytes.Equal(b.Bytes(), c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.name, b.Bytes(), c.want)
		}
	}
	// Messages accumulate, and Reset empties the buffer.
	var b Buffer
	b.AuthenticationOk()
	b.ReadyForQuery(StatusIdle)
	if len(b.Bytes()) != 9+6 {
		t.Fatalf("%d bytes", len(b.Bytes()))
	}
	b.Reset()
	if len(b.Bytes()) != 0 {
		t.Fatal("Reset left bytes")
	}
}

func TestReadStartupPacket(t *testing.T) {
	good := EncodeStartup(ProtocolVersion30, []Param{{"user", "u"}, {"database", "d"}})
	code, body, err := ReadStartupPacket(bytes.NewReader(good))
	if err != nil || code != ProtocolVersion30 {
		t.Fatal(code, err)
	}
	s, err := ParseStartup(code, body)
	if err != nil || s.Major != 3 || s.Minor != 0 || !reflect.DeepEqual(s.Params, []Param{{"user", "u"}, {"database", "d"}}) {
		t.Fatalf("%+v %v", s, err)
	}
	if code, body, err := ReadStartupPacket(bytes.NewReader(EncodeRequest(CodeSSLRequest))); err != nil || code != CodeSSLRequest || len(body) != 0 {
		t.Fatal(code, body, err)
	}
	length := func(n uint32) []byte { return binary.BigEndian.AppendUint32(nil, n) }
	for name, in := range map[string][]byte{
		"length 7": append(length(7), 0, 3, 0, 0),
		"length 0": length(0),
		"too long": length(MaxStartupPacket + 1),
		"huge":     length(1 << 31),
	} {
		if _, _, err := ReadStartupPacket(bytes.NewReader(in)); !errors.Is(err, ErrProtocol) {
			t.Errorf("%s: %v", name, err)
		}
	}
	big := make([]byte, MaxStartupPacket)
	binary.BigEndian.PutUint32(big, MaxStartupPacket)
	if _, body, err := ReadStartupPacket(bytes.NewReader(big)); err != nil || len(body) != MaxStartupPacket-8 {
		t.Fatalf("a packet of exactly the limit: %d, %v", len(body), err)
	}
	// Truncation anywhere: EOF before anything, unexpected EOF after.
	if _, _, err := ReadStartupPacket(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Fatalf("empty: %v", err)
	}
	for i := 1; i < len(good); i++ {
		if _, _, err := ReadStartupPacket(bytes.NewReader(good[:i])); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("truncated to %d: %v", i, err)
		}
	}
}

func TestParseStartupRejectsMalformedBodies(t *testing.T) {
	for name, body := range map[string]string{
		"no terminator at all":    "user\x00u",
		"name without value":      "user\x00",
		"value unterminated":      "user\x00u",
		"bytes after terminator":  "user\x00u\x00\x00x",
		"missing final empty":     "user\x00u\x00",
		"empty body":              "",
		"two terminators then ok": "\x00\x00",
	} {
		if _, err := ParseStartup(ProtocolVersion30, []byte(body)); !errors.Is(err, ErrProtocol) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// An empty parameter list is well formed; the server decides about it.
	if s, err := ParseStartup(ProtocolVersion30, []byte{0}); err != nil || len(s.Params) != 0 {
		t.Fatal(s, err)
	}
	// Empty values and duplicates are kept in order.
	s, err := ParseStartup(3<<16|2, []byte("a\x00\x00a\x00x\x00\x00"))
	if err != nil || s.Minor != 2 || !reflect.DeepEqual(s.Params, []Param{{"a", ""}, {"a", "x"}}) {
		t.Fatalf("%+v %v", s, err)
	}
}

func message(typ byte, body string) []byte {
	b := append([]byte{typ}, binary.BigEndian.AppendUint32(nil, uint32(len(body)+4))...)
	return append(b, body...)
}

func TestReadMessage(t *testing.T) {
	in := append(message('Q', "SELECT 1\x00"), message('X', "")...)
	r := NewReader(bufio.NewReader(bytes.NewReader(in)))
	typ, body, err := r.ReadMessage()
	if err != nil || typ != 'Q' || string(body) != "SELECT 1\x00" {
		t.Fatal(typ, body, err)
	}
	if q, err := ParseQuery(body); err != nil || q != "SELECT 1" {
		t.Fatal(q, err)
	}
	if typ, body, err := r.ReadMessage(); err != nil || typ != 'X' || len(body) != 0 {
		t.Fatal(typ, body, err)
	}
	if _, _, err := r.ReadMessage(); !errors.Is(err, io.EOF) {
		t.Fatalf("at the end: %v", err)
	}
	// Lengths out of bounds are refused before the body is read.
	for _, n := range []uint32{0, 3, MaxMessageSize + 1, 1<<32 - 1} {
		in := append([]byte{'Q'}, binary.BigEndian.AppendUint32(nil, n)...)
		if _, _, err := NewReader(bufio.NewReader(bytes.NewReader(in))).ReadMessage(); !errors.Is(err, ErrProtocol) {
			t.Errorf("length %d: %v", n, err)
		}
	}
	// A large message within the limit is read whole, growing as it comes.
	big := strings.Repeat("x", 200<<10)
	if _, body, err := NewReader(bufio.NewReader(bytes.NewReader(message('d', big)))).ReadMessage(); err != nil || string(body) != big {
		t.Fatalf("large message: %d bytes, %v", len(body), err)
	}
	// Truncated anywhere: unexpected EOF.
	full := message('d', big)
	for _, cut := range []int{1, 3, 5, 100, 70 << 10, len(full) - 1} {
		if _, _, err := NewReader(bufio.NewReader(bytes.NewReader(full[:cut]))).ReadMessage(); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("cut at %d: %v", cut, err)
		}
	}
	for _, body := range []string{"", "SELECT 1", "a\x00b\x00"} {
		if _, err := ParseQuery([]byte(body)); !errors.Is(err, ErrProtocol) {
			t.Errorf("Query body %q: %v", body, err)
		}
	}
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r    io.Reader
	read int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += n
	return n, err
}

func TestOversizedMessageBodyIsNeverRead(t *testing.T) {
	in := append([]byte{'Q'}, binary.BigEndian.AppendUint32(nil, MaxMessageSize+100)...)
	in = append(in, make([]byte, 1<<20)...)
	cr := &countingReader{r: bytes.NewReader(in)}
	if _, _, err := NewReader(bufio.NewReaderSize(cr, 16)).ReadMessage(); !errors.Is(err, ErrProtocol) {
		t.Fatal(err)
	}
	if cr.read > 16 {
		t.Fatalf("read %d bytes of a message it refused", cr.read)
	}
}

func TestAnnouncedSizeIsNotAllocatedUpFront(t *testing.T) {
	// A client announces a message near the limit and sends a few bytes:
	// the server allocates for what arrived, not for what was announced.
	in := append([]byte{'d'}, binary.BigEndian.AppendUint32(nil, MaxMessageSize)...)
	in = append(in, "only this"...)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if _, _, err := NewReader(bufio.NewReader(bytes.NewReader(in))).ReadMessage(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
		t.Fatalf("allocated %d bytes for a message that sent 9", grew)
	}
}

func FuzzStartup(f *testing.F) {
	f.Add(EncodeStartup(ProtocolVersion30, []Param{{"user", "u"}, {"options", "-c a=b"}}))
	f.Add(EncodeRequest(CodeSSLRequest))
	f.Add([]byte{0, 0, 0, 8, 0, 3, 0, 0})
	f.Fuzz(func(t *testing.T, in []byte) {
		code, body, err := ReadStartupPacket(bytes.NewReader(in))
		if err != nil {
			return
		}
		s, err := ParseStartup(code, body)
		if err != nil {
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("error %v is not a protocol error", err)
			}
			return
		}
		// What parses re-encodes to the same packet.
		again := EncodeStartup(code, s.Params)
		if !bytes.Equal(again, in[:len(again)]) {
			t.Fatalf("re-encoded %q, read %q", again, in)
		}
	})
}

func FuzzMessageReader(f *testing.F) {
	f.Add(append(message('Q', "SELECT 1\x00"), message('X', "")...))
	f.Add([]byte{'Q', 0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, in []byte) {
		r := NewReader(bufio.NewReader(bytes.NewReader(in)))
		for range 100 {
			typ, body, err := r.ReadMessage()
			if err != nil {
				return
			}
			if len(body) > MaxMessageSize {
				t.Fatalf("message %q of %d bytes", typ, len(body))
			}
			if typ == 'Q' {
				_, _ = ParseQuery(body)
			}
		}
	})
}
