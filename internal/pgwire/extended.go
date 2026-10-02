package pgwire

import (
	"encoding/binary"
)

// Format codes of parameters and result columns.
const (
	FormatText   = 0
	FormatBinary = 1
)

// Parse is a Parse ('P') message: prepare Query as statement Name ("" is
// the unnamed statement), with the types of its first parameters (0 for
// "infer it").
type Parse struct {
	Name, Query string
	ParamTypes  []uint32
}

// Bind is a Bind ('B') message: make portal Portal from statement
// Statement with parameter values (nil is NULL) and the formats of the
// values and of the result columns. ParamFormats and ResultFormats hold
// zero codes (all text), one (for all), or one per value or column.
type Bind struct {
	Portal, Statement string
	ParamFormats      []int16
	Params            [][]byte
	ResultFormats     []int16
}

// Describe is a Describe ('D') or Close ('C') message's body: Kind is 'S'
// for a statement or 'P' for a portal.
type Describe struct {
	Kind byte
	Name string
}

// Execute is an Execute ('E') message: run Portal, returning at most
// MaxRows rows (0 for all).
type Execute struct {
	Portal  string
	MaxRows int32
}

// decoder reads a message body; the first error sticks.
type decoder struct {
	b   []byte
	bad bool
}

func (d *decoder) str() string {
	if d.bad {
		return ""
	}
	s, rest, err := cstring(d.b)
	if err != nil {
		d.bad = true
		return ""
	}
	d.b = rest
	return s
}

func (d *decoder) n(k int) []byte {
	if d.bad || len(d.b) < k {
		d.bad = true
		return nil
	}
	v := d.b[:k]
	d.b = d.b[k:]
	return v
}

func (d *decoder) int16() int16 {
	if v := d.n(2); v != nil {
		return int16(binary.BigEndian.Uint16(v))
	}
	return 0
}

func (d *decoder) int32() int32 {
	if v := d.n(4); v != nil {
		return int32(binary.BigEndian.Uint32(v))
	}
	return 0
}

// count reads a count, unsigned 16 bits as PostgreSQL reads it, of items
// at least size bytes each, that must fit in what is left.
func (d *decoder) count(size int) int {
	n := int(uint16(d.int16()))
	if n*size > len(d.b) {
		d.bad = true
		return 0
	}
	return n
}

func (d *decoder) done(what string) error {
	if d.bad || len(d.b) != 0 {
		return protocolError("malformed %s message", what)
	}
	return nil
}

// ParseParse decodes a Parse message body.
func ParseParse(body []byte) (Parse, error) {
	d := decoder{b: body}
	var p Parse
	p.Name = d.str()
	p.Query = d.str()
	n := d.count(4)
	if n > 0 {
		p.ParamTypes = make([]uint32, n)
	}
	for i := range n {
		p.ParamTypes[i] = uint32(d.int32())
	}
	return p, d.done("Parse")
}

// ParseBind decodes a Bind message body. A parameter format count other
// than 0, 1 or the number of values is a protocol violation, as in
// PostgreSQL; format codes are not checked here.
func ParseBind(body []byte) (Bind, error) {
	d := decoder{b: body}
	var b Bind
	b.Portal = d.str()
	b.Statement = d.str()
	formats := func() []int16 {
		n := d.count(2)
		if n == 0 {
			return nil
		}
		f := make([]int16, n)
		for i := range f {
			f[i] = d.int16()
		}
		return f
	}
	b.ParamFormats = formats()
	n := d.count(4)
	if n > 0 {
		b.Params = make([][]byte, n)
	}
	for i := range n {
		l := d.int32()
		switch {
		case l == -1:
		case l < 0:
			d.bad = true
		default:
			// A slice of the body: never nil, so an empty value is not
			// NULL.
			b.Params[i] = d.n(int(l))
		}
	}
	b.ResultFormats = formats()
	if err := d.done("Bind"); err != nil {
		return Bind{}, err
	}
	if k := len(b.ParamFormats); k > 1 && k != len(b.Params) {
		return Bind{}, protocolError("bind message has %d parameter formats but %d parameters", k, len(b.Params))
	}
	return b, nil
}

// ParseDescribe decodes a Describe or Close message body (what names the
// message in errors).
func ParseDescribe(body []byte, what string) (Describe, error) {
	d := decoder{b: body}
	var m Describe
	if k := d.n(1); k != nil {
		m.Kind = k[0]
	}
	m.Name = d.str()
	if err := d.done(what); err != nil {
		return Describe{}, err
	}
	if m.Kind != 'S' && m.Kind != 'P' {
		return Describe{}, protocolError("invalid %s message subtype %d", what, m.Kind)
	}
	return m, nil
}

// ParseExecute decodes an Execute message body.
func ParseExecute(body []byte) (Execute, error) {
	d := decoder{b: body}
	e := Execute{Portal: d.str(), MaxRows: d.int32()}
	return e, d.done("Execute")
}

// Frontend messages, encoded: for clients (tests and tools).

func frontend(typ byte, build func(w *Buffer)) []byte {
	var w Buffer
	at := w.begin(typ)
	build(&w)
	w.end(at)
	return w.b
}

func (w *Buffer) int16(v int16) { w.b = binary.BigEndian.AppendUint16(w.b, uint16(v)) }

// EncodeQuery encodes a Query ('Q') message.
func EncodeQuery(sql string) []byte { return frontend('Q', func(w *Buffer) { w.str(sql) }) }

// Encode encodes the Parse message.
func (p Parse) Encode() []byte {
	return frontend('P', func(w *Buffer) {
		w.str(p.Name)
		w.str(p.Query)
		w.int16(int16(len(p.ParamTypes)))
		for _, t := range p.ParamTypes {
			w.int32(int32(t))
		}
	})
}

// Encode encodes the Bind message.
func (b Bind) Encode() []byte {
	return frontend('B', func(w *Buffer) {
		w.str(b.Portal)
		w.str(b.Statement)
		w.int16(int16(len(b.ParamFormats)))
		for _, f := range b.ParamFormats {
			w.int16(f)
		}
		w.int16(int16(len(b.Params)))
		for _, v := range b.Params {
			if v == nil {
				w.int32(-1)
				continue
			}
			w.int32(int32(len(v)))
			w.b = append(w.b, v...)
		}
		w.int16(int16(len(b.ResultFormats)))
		for _, f := range b.ResultFormats {
			w.int16(f)
		}
	})
}

// EncodeDescribe encodes a Describe ('D') message.
func EncodeDescribe(kind byte, name string) []byte {
	return frontend('D', func(w *Buffer) { w.b = append(w.b, kind); w.str(name) })
}

// EncodeClose encodes a Close ('C') message.
func EncodeClose(kind byte, name string) []byte {
	return frontend('C', func(w *Buffer) { w.b = append(w.b, kind); w.str(name) })
}

// Encode encodes the Execute message.
func (e Execute) Encode() []byte {
	return frontend('E', func(w *Buffer) { w.str(e.Portal); w.int32(e.MaxRows) })
}

// EncodeSync encodes a Sync ('S') message.
func EncodeSync() []byte { return []byte{'S', 0, 0, 0, 4} }

// EncodeFlush encodes a Flush ('H') message.
func EncodeFlush() []byte { return []byte{'H', 0, 0, 0, 4} }

// Backend messages of the extended protocol.

func (w *Buffer) empty(typ byte) { w.b = append(w.b, typ, 0, 0, 0, 4) }

// ParseComplete ('1').
func (w *Buffer) ParseComplete() { w.empty('1') }

// BindComplete ('2').
func (w *Buffer) BindComplete() { w.empty('2') }

// CloseComplete ('3').
func (w *Buffer) CloseComplete() { w.empty('3') }

// NoData ('n'): the statement or portal returns no rows.
func (w *Buffer) NoData() { w.empty('n') }

// PortalSuspended ('s'): Execute's row limit was reached before the end.
func (w *Buffer) PortalSuspended() { w.empty('s') }

// ParameterDescription ('t'): the type OIDs of a statement's parameters.
func (w *Buffer) ParameterDescription(oids []uint32) {
	at := w.begin('t')
	w.int16(int16(len(oids)))
	for _, o := range oids {
		w.int32(int32(o))
	}
	w.end(at)
}
