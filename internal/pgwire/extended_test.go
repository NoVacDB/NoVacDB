package pgwire

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

func TestExtendedBackendMessageBytes(t *testing.T) {
	cases := []struct {
		name  string
		build func(*Buffer)
		want  []byte
	}{
		{"parse complete", (*Buffer).ParseComplete, []byte{'1', 0, 0, 0, 4}},
		{"bind complete", (*Buffer).BindComplete, []byte{'2', 0, 0, 0, 4}},
		{"close complete", (*Buffer).CloseComplete, []byte{'3', 0, 0, 0, 4}},
		{"no data", (*Buffer).NoData, []byte{'n', 0, 0, 0, 4}},
		{"portal suspended", (*Buffer).PortalSuspended, []byte{'s', 0, 0, 0, 4}},
		{"parameter description", func(b *Buffer) { b.ParameterDescription([]uint32{23, 1184}) },
			[]byte{'t', 0, 0, 0, 14, 0, 2, 0, 0, 0, 23, 0, 0, 4, 0xa0}},
		{"no parameters", func(b *Buffer) { b.ParameterDescription(nil) }, []byte{'t', 0, 0, 0, 6, 0, 0}},
		{"binary column", func(b *Buffer) {
			b.RowDescription([]FieldDescription{{Name: "n", TypeOID: OIDInt8, Size: 8, Format: FormatBinary}})
		}, append([]byte{'T', 0, 0, 0, 26, 0, 1, 'n', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 20, 0, 8}, 0xff, 0xff, 0xff, 0xff, 0, 1)},
	}
	for _, c := range cases {
		var b Buffer
		c.build(&b)
		if !bytes.Equal(b.Bytes(), c.want) {
			t.Errorf("%s:\n got %v\nwant %v", c.name, b.Bytes(), c.want)
		}
	}
}

func TestFrontendMessageBytes(t *testing.T) {
	cases := []struct {
		name      string
		got, want []byte
	}{
		{"parse", Parse{Name: "s", Query: "SELECT $1", ParamTypes: []uint32{23}}.Encode(),
			append([]byte{'P', 0, 0, 0, 22, 's', 0}, "SELECT $1\x00\x00\x01\x00\x00\x00\x17"...)},
		{"bind", Bind{Portal: "p", Statement: "s", ParamFormats: []int16{1}, Params: [][]byte{{0, 0, 0, 7}, nil},
			ResultFormats: []int16{0, 1}}.Encode(),
			[]byte{'B', 0, 0, 0, 32, 'p', 0, 's', 0, 0, 1, 0, 1, 0, 2, 0, 0, 0, 4, 0, 0, 0, 7, 0xff, 0xff, 0xff, 0xff, 0, 2, 0, 0, 0, 1}},
		{"describe", EncodeDescribe('S', "s"), []byte{'D', 0, 0, 0, 7, 'S', 's', 0}},
		{"close", EncodeClose('P', ""), []byte{'C', 0, 0, 0, 6, 'P', 0}},
		{"execute", Execute{Portal: "", MaxRows: 10}.Encode(), []byte{'E', 0, 0, 0, 9, 0, 0, 0, 0, 10}},
		{"query", EncodeQuery("SELECT 1"), append([]byte{'Q', 0, 0, 0, 13}, "SELECT 1\x00"...)},
		{"sync", EncodeSync(), []byte{'S', 0, 0, 0, 4}},
		{"flush", EncodeFlush(), []byte{'H', 0, 0, 0, 4}},
	}
	for _, c := range cases {
		if !bytes.Equal(c.got, c.want) {
			t.Errorf("%s:\n got %v\nwant %v", c.name, c.got, c.want)
		}
	}
}

func TestExtendedRoundTrip(t *testing.T) {
	parses := []Parse{
		{},
		{Name: "stmt", Query: "SELECT $1, $2", ParamTypes: []uint32{0, 25}},
	}
	for _, p := range parses {
		got, err := ParseParse(p.Encode()[5:])
		if err != nil || !reflect.DeepEqual(got, p) {
			t.Errorf("%+v: %+v %v", p, got, err)
		}
	}
	binds := []Bind{
		{},
		{Portal: "p", Statement: "s", Params: [][]byte{nil, {}, []byte("abc")}},
		{ParamFormats: []int16{1}, Params: [][]byte{{1}}, ResultFormats: []int16{1}},
		{ParamFormats: []int16{0, 1}, Params: [][]byte{[]byte("1"), {0, 0, 0, 1}}, ResultFormats: []int16{0, 1, 0}},
	}
	for _, b := range binds {
		got, err := ParseBind(b.Encode()[5:])
		if err != nil || !reflect.DeepEqual(got, b) {
			t.Errorf("%+v: %+v %v", b, got, err)
		}
	}
	// NULL and the empty value stay distinct.
	got, _ := ParseBind(binds[1].Encode()[5:])
	if got.Params[0] != nil || got.Params[1] == nil {
		t.Fatalf("%#v", got.Params)
	}
	for _, kind := range []byte{'S', 'P'} {
		d, err := ParseDescribe(EncodeDescribe(kind, "x")[5:], "Describe")
		if err != nil || d != (Describe{kind, "x"}) {
			t.Errorf("%c: %+v %v", kind, d, err)
		}
	}
	e, err := ParseExecute(Execute{Portal: "p", MaxRows: -5}.Encode()[5:])
	if err != nil || e != (Execute{"p", -5}) {
		t.Errorf("%+v %v", e, err)
	}
}

func TestExtendedRejectsMalformedBodies(t *testing.T) {
	valid := map[string][]byte{
		"parse":    Parse{Name: "s", Query: "q", ParamTypes: []uint32{1, 2}}.Encode()[5:],
		"bind":     Bind{Portal: "p", Statement: "s", ParamFormats: []int16{0, 1}, Params: [][]byte{[]byte("ab"), nil}, ResultFormats: []int16{1}}.Encode()[5:],
		"describe": EncodeDescribe('S', "s")[5:],
		"execute":  Execute{Portal: "p", MaxRows: 1}.Encode()[5:],
	}
	decode := func(kind string, b []byte) error {
		var err error
		switch kind {
		case "parse":
			_, err = ParseParse(b)
		case "bind":
			_, err = ParseBind(b)
		case "describe":
			_, err = ParseDescribe(b, "Describe")
		case "execute":
			_, err = ParseExecute(b)
		}
		return err
	}
	for kind, b := range valid {
		if err := decode(kind, b); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		// Every truncation, and a trailing byte.
		for n := range len(b) {
			if err := decode(kind, b[:n]); !errors.Is(err, ErrProtocol) {
				t.Errorf("%s cut to %d: %v", kind, n, err)
			}
		}
		if err := decode(kind, append(append([]byte(nil), b...), 0)); !errors.Is(err, ErrProtocol) {
			t.Errorf("%s with a trailing byte: %v", kind, err)
		}
	}
	bad := map[string][]byte{
		// Counts larger than the body.
		"parse":    {'s', 0, 'q', 0, 0xff, 0xff},
		"bind":     {0, 0, 0, 0, 0, 1, 0xff, 0xff, 0xff, 0xfe, 0, 0}, // a length of -2
		"describe": {'X', 's', 0},
	}
	for kind, b := range bad {
		if err := decode(kind, b); !errors.Is(err, ErrProtocol) {
			t.Errorf("%s %v: %v", kind, b, err)
		}
	}
	// Format counts other than 0, 1 or one per value.
	b := Bind{ParamFormats: []int16{0, 0}, Params: [][]byte{nil, nil, nil}}.Encode()[5:]
	if _, err := ParseBind(b); !errors.Is(err, ErrProtocol) {
		t.Fatal(err)
	}
	// Counts are unsigned: 40000 parameter types.
	p := Parse{ParamTypes: make([]uint32, 40000)}
	if got, err := ParseParse(p.Encode()[5:]); err != nil || len(got.ParamTypes) != 40000 {
		t.Fatal(len(got.ParamTypes), err)
	}
	// A value longer than the rest of the body.
	b = Bind{Params: [][]byte{[]byte("abcd")}}.Encode()[5:]
	b[9] = 50
	if _, err := ParseBind(b); !errors.Is(err, ErrProtocol) {
		t.Fatal(err)
	}
}

// FuzzExtendedMessages decodes arbitrary bodies as each extended-protocol
// message: no panic, and whatever is accepted encodes back to the same
// bytes.
func FuzzExtendedMessages(f *testing.F) {
	f.Add(byte('P'), Parse{Name: "s", Query: "SELECT $1", ParamTypes: []uint32{23}}.Encode()[5:])
	f.Add(byte('B'), Bind{Portal: "p", ParamFormats: []int16{1}, Params: [][]byte{{1, 2}, nil}, ResultFormats: []int16{1, 0}}.Encode()[5:])
	f.Add(byte('D'), EncodeDescribe('P', "p")[5:])
	f.Add(byte('E'), Execute{Portal: "p", MaxRows: 3}.Encode()[5:])
	f.Fuzz(func(t *testing.T, typ byte, body []byte) {
		var again []byte
		switch typ % 4 {
		case 0:
			p, err := ParseParse(body)
			if err != nil {
				return
			}
			again = p.Encode()
		case 1:
			b, err := ParseBind(body)
			if err != nil {
				return
			}
			again = b.Encode()
		case 2:
			d, err := ParseDescribe(body, "Describe")
			if err != nil {
				return
			}
			again = EncodeDescribe(d.Kind, d.Name)
		case 3:
			e, err := ParseExecute(body)
			if err != nil {
				return
			}
			again = e.Encode()
		}
		if !bytes.Equal(again[5:], body) {
			t.Fatalf("%x re-encodes as %x", body, again[5:])
		}
	})
}
