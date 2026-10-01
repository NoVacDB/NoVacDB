package wal

import (
	"bytes"
	"errors"
	"testing"

	"github.com/vikrant-choudhary06/NoVacDB/internal/vfs"
)

// FuzzDecodeRecord: arbitrary bytes never panic; anything that decodes is
// canonical (it re-encodes to the same bytes) and its prefixes are short.
func FuzzDecodeRecord(f *testing.F) {
	f.Add(AppendRecord(nil, 32, 1, []byte("hello")))
	f.Add(AppendRecord(nil, 1<<40, 0xFFFF, nil))
	f.Add(AppendRecord(nil, 7, 2, bytes.Repeat([]byte{9}, 300)))
	f.Add(make([]byte, 64))
	f.Add([]byte{})
	f.Add(bytes.Repeat([]byte{0xFF}, 40))
	f.Fuzz(func(t *testing.T, buf []byte) {
		rec, n, err := DecodeRecord(buf)
		if err != nil {
			if !errors.Is(err, ErrShortRecord) && !errors.Is(err, ErrBadRecord) && !errors.Is(err, ErrChecksum) {
				t.Fatalf("unclassified error %v", err)
			}
			return
		}
		if n < RecordHeaderSize || n > len(buf) || len(rec.Payload) != n-RecordHeaderSize || len(rec.Payload) > MaxPayload {
			t.Fatalf("inconsistent decode: n=%d payload=%d len=%d", n, len(rec.Payload), len(buf))
		}
		if rec.Type == 0 {
			t.Fatal("decoded type 0")
		}
		if again := AppendRecord(nil, rec.LSN, rec.Type, rec.Payload); !bytes.Equal(again, buf[:n]) {
			t.Fatal("record is not canonical: re-encoding differs")
		}
		if _, _, err := DecodeRecord(buf[:n-1]); !errors.Is(err, ErrShortRecord) {
			t.Fatalf("one byte short: err = %v", err)
		}
	})
}

// FuzzDecodeSegmentHeader: arbitrary bytes never panic; an accepted header is
// canonical.
func FuzzDecodeSegmentHeader(f *testing.F) {
	f.Add(AppendSegmentHeader(nil, 0))
	f.Add(AppendSegmentHeader(nil, 123456))
	f.Add(make([]byte, SegmentHeaderSize))
	f.Add([]byte{1, 2, 3})
	f.Fuzz(func(t *testing.T, buf []byte) {
		start, err := DecodeSegmentHeader(buf)
		if err != nil {
			return
		}
		if !bytes.Equal(AppendSegmentHeader(nil, start), buf[:SegmentHeaderSize]) {
			t.Fatal("accepted header is not canonical")
		}
	})
}

// FuzzOpen: an arbitrary file as the only segment. Open must not panic. If it
// accepts the file, the log must be fully usable: appending works, flushing
// works, and reopening shows exactly what was appended on top of what Open
// reported.
func FuzzOpen(f *testing.F) {
	m := vfs.NewMemFS(1)
	_ = m.MkdirAll("/db")
	_ = m.SyncDir("/")
	w, err := Open(m, testDir, Options{})
	if err != nil {
		f.Fatal(err)
	}
	for i := range 4 {
		if _, err := w.Append(bg, RecordType(i+1), bytes.Repeat([]byte{byte(i)}, 10*i)); err != nil {
			f.Fatal(err)
		}
	}
	if err := w.Close(bg); err != nil {
		f.Fatal(err)
	}
	valid := readFile(f, m, testDir+"/"+SegmentName(0))
	f.Add(valid)
	f.Add(valid[:len(valid)-3])
	f.Add(append(bytes.Clone(valid), 1, 2, 3, 4))
	f.Add(AppendSegmentHeader(nil, 0))
	f.Add(AppendSegmentHeader(nil, 0)[:20])
	f.Add([]byte{})
	f.Add(make([]byte, 100))

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<16 {
			data = data[:1<<16]
		}
		fsys := newFS(t)
		writeFile(t, fsys, testDir+"/"+SegmentName(0), data)
		w, err := Open(fsys, testDir, Options{SegmentSize: 300})
		if err != nil {
			return
		}
		end := w.EndLSN()
		if end < SegmentHeaderSize || w.DurableEnd() != end {
			t.Fatalf("EndLSN %d DurableEnd %d", end, w.DurableEnd())
		}
		lsn, err := w.Append(bg, 9, []byte("fuzz"))
		if err != nil || lsn != end {
			t.Fatalf("Append = %d, %v; want LSN %d", lsn, err, end)
		}
		if err := w.Close(bg); err != nil {
			t.Fatal(err)
		}
		w2, err := Open(fsys, testDir, Options{SegmentSize: 300})
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		if want := end + LSN(RecordSize(4)); w2.EndLSN() != want {
			t.Fatalf("reopened EndLSN %d, want %d", w2.EndLSN(), want)
		}
		recs, _ := readLog(t, fsys, testDir)
		if len(recs) == 0 || recs[len(recs)-1].LSN != lsn || string(recs[len(recs)-1].Payload) != "fuzz" {
			t.Fatalf("appended record missing from the log: %v", recs)
		}
	})
}
