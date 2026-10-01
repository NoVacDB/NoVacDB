package storage

import (
	"bytes"
	"testing"
)

// FuzzDecodeHeader: arbitrary bytes must never panic, and a successful decode
// must return a valid type and survive a re-encode.
func FuzzDecodeHeader(f *testing.F) {
	f.Add(sealedPage(f, 1))
	f.Add(make([]byte, PageSize))
	f.Add([]byte{})
	f.Add(make([]byte, PageSize-1))
	f.Fuzz(func(t *testing.T, buf []byte) {
		h, err := DecodeHeader(buf)
		if err != nil {
			return
		}
		if !h.Type.Valid() {
			t.Fatalf("decoded invalid type %d without error", h.Type)
		}
		again := make([]byte, PageSize)
		if err := InitPage(again, h); err != nil {
			t.Fatalf("decoded header cannot be re-encoded: %v", err)
		}
		if h2, err := DecodeHeader(again); err != nil || h2 != h {
			t.Fatalf("re-decode = %+v, %v; want %+v", h2, err, h)
		}
	})
}

// FuzzVerify: arbitrary bytes and IDs must never panic, and any page that
// verifies must carry the requested ID, a valid type, and re-seal unchanged.
func FuzzVerify(f *testing.F) {
	f.Add(sealedPage(f, 1), uint64(1))
	f.Add(sealedPage(f, 1), uint64(2))
	f.Add(make([]byte, PageSize), uint64(0))
	f.Add([]byte("short"), uint64(0))
	f.Fuzz(func(t *testing.T, buf []byte, id uint64) {
		if Verify(buf, id) != nil {
			return
		}
		h, err := DecodeHeader(buf)
		if err != nil || h.ID != id {
			t.Fatalf("verified page decodes to %+v, %v for id %d", h, err, id)
		}
		again := bytes.Clone(buf)
		if err := Seal(again); err != nil || !bytes.Equal(again, buf) {
			t.Fatalf("verified page changes when re-sealed: %v", err)
		}
	})
}
