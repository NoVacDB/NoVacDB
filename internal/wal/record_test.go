package wal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math/rand/v2"
	"testing"
)

func TestRecordRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(testSeed(t), 1))
	for _, n := range []int{0, 1, 7, 8, 100, 4096, 65535, MaxPayload} {
		for _, typ := range []RecordType{1, 2, 255, 0xFFFF} {
			for _, lsn := range []LSN{0, 32, 1 << 40, 1<<64 - 1} {
				p := payload(rng, n)
				enc := AppendRecord(nil, lsn, typ, p)
				if len(enc) != RecordSize(n) {
					t.Fatalf("encoded %d bytes, RecordSize says %d", len(enc), RecordSize(n))
				}
				rec, size, err := DecodeRecord(enc)
				if err != nil || size != len(enc) {
					t.Fatalf("n=%d: size %d, err %v", n, size, err)
				}
				if rec.LSN != lsn || rec.Type != typ || !bytes.Equal(rec.Payload, p) {
					t.Fatalf("n=%d typ=%d lsn=%d: round trip changed the record", n, typ, lsn)
				}
			}
		}
	}
}

func TestRecordGoldenBytes(t *testing.T) {
	enc := AppendRecord(nil, 0x0102030405060708, 0x0A0B, []byte("hi"))
	want := []byte{
		0, 0, 0, 0, // CRC placeholder, checked below
		2, 0, 0, 0, // PayloadLen
		0x08, 0x07, 0x06, 0x05, 0x04, 0x03, 0x02, 0x01, // LSN
		0x0B, 0x0A, // Type
		0, 0, // Flags
		'h', 'i',
	}
	crc := crc32.Checksum(want[4:], crc32.MakeTable(crc32.Castagnoli))
	binary.LittleEndian.PutUint32(want, crc)
	if !bytes.Equal(enc, want) {
		t.Fatalf("encoding = % x\nwant       % x", enc, want)
	}
	const golden = 0xaddb1c28 // pinned after the independent check above: changing the checksum scope or polynomial breaks the format
	if crc != golden {
		t.Fatalf("golden crc changed: %#08x", crc)
	}
}

func TestAppendRecordAppends(t *testing.T) {
	dst := []byte("prefix")
	dst = AppendRecord(dst, 40, 3, []byte("abc"))
	if !bytes.HasPrefix(dst, []byte("prefix")) {
		t.Fatal("prefix clobbered")
	}
	rec, _, err := DecodeRecord(dst[len("prefix"):])
	if err != nil || rec.LSN != 40 {
		t.Fatalf("%v %v", rec, err)
	}
}

func TestDecodeIgnoresTrailingBytesAndAliases(t *testing.T) {
	enc := AppendRecord(nil, 50, 1, []byte("payload"))
	buf := append(bytes.Clone(enc), 0xDE, 0xAD)
	rec, size, err := DecodeRecord(buf)
	if err != nil || size != len(enc) {
		t.Fatal(size, err)
	}
	buf[RecordHeaderSize] = 'X'
	if rec.Payload[0] != 'X' {
		t.Fatal("payload should alias the input")
	}
	if cap(rec.Payload) != len(rec.Payload) {
		t.Fatal("payload capacity leaks into following bytes")
	}
}

func TestDecodeEveryPrefixIsShort(t *testing.T) {
	enc := AppendRecord(nil, 99, 4, bytes.Repeat([]byte{7}, 50))
	for n := range len(enc) {
		if _, _, err := DecodeRecord(enc[:n]); !errors.Is(err, ErrShortRecord) {
			t.Fatalf("prefix of %d bytes: err = %v, want ErrShortRecord", n, err)
		}
	}
}

func TestDecodeEverySingleByteFlipIsDetected(t *testing.T) {
	enc := AppendRecord(nil, 123456, 9, bytes.Repeat([]byte{0x5A}, 40))
	for i := range enc {
		for bit := range 8 {
			b := bytes.Clone(enc)
			b[i] ^= 1 << bit
			rec, _, err := DecodeRecord(b)
			if err == nil {
				t.Fatalf("flip of byte %d bit %d went undetected: %v", i, bit, rec)
			}
		}
	}
}

func TestDecodeRejectsInvalidFields(t *testing.T) {
	reseal := func(b []byte) []byte {
		binary.LittleEndian.PutUint32(b, crc32.Checksum(b[4:], crc32.MakeTable(crc32.Castagnoli)))
		return b
	}
	valid := AppendRecord(nil, 64, 5, []byte("data"))
	tests := []struct {
		name string
		buf  []byte
		want error
	}{
		{"type zero", reseal(func() []byte { b := bytes.Clone(valid); binary.LittleEndian.PutUint16(b[16:], 0); return b }()), ErrBadRecord},
		{"flags set", reseal(func() []byte { b := bytes.Clone(valid); binary.LittleEndian.PutUint16(b[18:], 1); return b }()), ErrBadRecord},
		{"length just over max", func() []byte {
			b := bytes.Clone(valid)
			binary.LittleEndian.PutUint32(b[4:], MaxPayload+1)
			return b
		}(), ErrBadRecord},
		{"length 0xFFFFFFFF", func() []byte {
			b := bytes.Clone(valid)
			binary.LittleEndian.PutUint32(b[4:], 0xFFFFFFFF)
			return b
		}(), ErrBadRecord},
		{"length beyond buffer", func() []byte {
			b := bytes.Clone(valid)
			binary.LittleEndian.PutUint32(b[4:], MaxPayload)
			return b
		}(), ErrShortRecord},
		{"all zeros", make([]byte, 100), ErrChecksum},
		{"nil", nil, ErrShortRecord},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := DecodeRecord(tc.buf); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestRecordDecodeDoesNotAllocate(t *testing.T) {
	enc := AppendRecord(nil, 64, 5, bytes.Repeat([]byte{1}, 1000))
	if n := testing.AllocsPerRun(100, func() { _, _, _ = DecodeRecord(enc) }); n != 0 {
		t.Fatalf("DecodeRecord allocated %v times", n)
	}
}

func BenchmarkAppendRecord(b *testing.B) {
	p := bytes.Repeat([]byte{1}, 100)
	buf := make([]byte, 0, 256)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf = AppendRecord(buf[:0], LSN(i), 1, p)
	}
}

func BenchmarkDecodeRecord(b *testing.B) {
	enc := AppendRecord(nil, 64, 5, bytes.Repeat([]byte{1}, 100))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, _, err := DecodeRecord(enc); err != nil {
			b.Fatal(err)
		}
	}
}
