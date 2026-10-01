package wal

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"testing"

	"github.com/vikrant-choudhary06/NoVacDB/internal/vfs"
)

// readAll reads every record from from to the end.
func readAll(t testing.TB, fsys vfs.FS, from LSN) ([]logRec, *Reader) {
	t.Helper()
	r, err := NewReader(fsys, testDir, from)
	if err != nil {
		t.Fatalf("NewReader(%d): %v", from, err)
	}
	var out []logRec
	for {
		rec, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out, r
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, logRec{rec.LSN, rec.Type, bytes.Clone(rec.Payload)})
	}
}

func TestReaderReadsWholeLogAcrossSegments(t *testing.T) {
	for _, segSize := range []int64{0, 150, 1} {
		t.Run(fmt.Sprintf("segment=%d", segSize), func(t *testing.T) {
			m, recs := buildLog(t, 60, segSize)
			first, err := FirstLSN(m, testDir)
			if err != nil || first != SegmentHeaderSize {
				t.Fatalf("FirstLSN = %d, %v", first, err)
			}
			got, r := readAll(t, m, first)
			sameRecs(t, got, recs, "read")
			last := recs[len(recs)-1]
			if want := last.LSN + LSN(RecordSize(len(last.Payload))); r.End() != want {
				t.Fatalf("End = %d, want %d", r.End(), want)
			}
			if _, err := r.Next(); !errors.Is(err, io.EOF) {
				t.Fatalf("Next after EOF = %v", err)
			}
		})
	}
}

func TestReaderStartsAtEveryRecordAndSegment(t *testing.T) {
	m, recs := buildLog(t, 40, 200)
	for i, rec := range recs {
		got, _ := readAll(t, m, rec.LSN)
		sameRecs(t, got, recs[i:], fmt.Sprintf("from record %d", i))
	}
	_, segs := readLog(t, m, testDir)
	for _, s := range segs {
		var want []logRec
		for _, rec := range recs {
			if rec.LSN >= s.Start {
				want = append(want, rec)
			}
		}
		got, _ := readAll(t, m, s.Start)
		sameRecs(t, got, want, fmt.Sprintf("from segment start %d", s.Start))
		got, _ = readAll(t, m, s.Start+SegmentHeaderSize)
		sameRecs(t, got, want, fmt.Sprintf("from first record of segment %d", s.Start))
	}
	// From the end: nothing, but not an error.
	last := segs[len(segs)-1]
	if got, r := readAll(t, m, last.Start+LSN(last.Size)); len(got) != 0 || r.End() != last.Start+LSN(last.Size) {
		t.Fatalf("from the end: %d records, End %d", len(got), r.End())
	}
}

func TestReaderRejectsPositionsThatAreNotRecords(t *testing.T) {
	m, recs := buildLog(t, 20, 200)
	_, segs := readLog(t, m, testDir)
	last := segs[len(segs)-1]
	bad := []LSN{
		recs[3].LSN + 1,                 // inside a record
		recs[len(recs)-1].LSN + 5,       // inside the last record
		last.Start + LSN(last.Size) + 1, // past the end
		segs[1].Start + 1,               // inside a segment header
		segs[1].Start + SegmentHeaderSize - 1,
		1 << 50,
	}
	for _, from := range bad {
		if _, err := NewReader(m, testDir, from); !errors.Is(err, ErrLSNNotFound) {
			t.Errorf("NewReader(%d) err = %v, want ErrLSNNotFound", from, err)
		}
	}
	// Before the first segment once the oldest segments are gone.
	for _, s := range segs[:2] {
		if err := m.Remove(path.Join(testDir, SegmentName(s.Start))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := NewReader(m, testDir, SegmentHeaderSize); !errors.Is(err, ErrLSNNotFound) {
		t.Errorf("reading removed history: err = %v", err)
	}
	if first, err := FirstLSN(m, testDir); err != nil || first != segs[2].Start+SegmentHeaderSize {
		t.Errorf("FirstLSN after removal = %d, %v", first, err)
	}
	got, _ := readAll(t, m, segs[2].Start)
	if len(got) == 0 || got[0].LSN < segs[2].Start {
		t.Fatalf("reading from the new oldest segment: %v", got)
	}
	// An empty directory has no log at all.
	if _, err := NewReader(newFS(t), testDir, 0); err == nil {
		t.Error("NewReader on a missing log succeeded")
	}
}

func TestReaderStopsCleanlyAtTornTail(t *testing.T) {
	m, recs := buildLog(t, 8, 0)
	name := path.Join(testDir, SegmentName(0))
	full := readFile(t, m, name)
	for cut := SegmentHeaderSize; cut <= len(full); cut++ {
		var want []logRec
		wantEnd := LSN(SegmentHeaderSize)
		for _, r := range recs {
			if end := r.LSN + LSN(RecordSize(len(r.Payload))); int(end) <= cut {
				want = append(want, r)
				wantEnd = end
			}
		}
		m2 := newFS(t)
		writeFile(t, m2, name, full[:cut])
		got, r := readAll(t, m2, SegmentHeaderSize)
		sameRecs(t, got, want, fmt.Sprintf("cut at %d", cut))
		if r.End() != wantEnd {
			t.Fatalf("cut at %d: End %d, want %d", cut, r.End(), wantEnd)
		}
		// The reader never modifies the log.
		if got := readFile(t, m2, name); len(got) != cut {
			t.Fatalf("reader changed the file")
		}
	}
}

func TestReaderStopsAtGarbageAndStaleRecordsInLastSegment(t *testing.T) {
	m, recs := buildLog(t, 5, 0)
	name := path.Join(testDir, SegmentName(0))
	clean := readFile(t, m, name)
	for i, tail := range [][]byte{
		bytes.Repeat([]byte{0}, 300),
		bytes.Repeat([]byte{0xFF}, 7),
		AppendRecord(nil, recs[1].LSN, recs[1].Type, recs[1].Payload), // valid checksum, wrong position
	} {
		m2 := newFS(t)
		writeFile(t, m2, name, append(bytes.Clone(clean), tail...))
		got, r := readAll(t, m2, SegmentHeaderSize)
		sameRecs(t, got, recs, fmt.Sprintf("tail %d", i))
		if int(r.End()) != len(clean) {
			t.Fatalf("tail %d: End %d, want %d", i, r.End(), len(clean))
		}
	}
}

func TestReaderReportsCorruptionBeforeTheLastSegment(t *testing.T) {
	m, recs := buildLog(t, 30, 150)
	_, segs := readLog(t, m, testDir)
	if len(segs) < 3 {
		t.Fatalf("setup: %d segments", len(segs))
	}
	victim := segs[1]
	name := path.Join(testDir, SegmentName(victim.Start))
	data := readFile(t, m, name)
	data[SegmentHeaderSize+RecordHeaderSize] ^= 0x40 // first record's payload
	writeFile(t, m, name, data)

	r, err := NewReader(m, testDir, SegmentHeaderSize)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for {
		rec, err := r.Next()
		if err == nil {
			if rec.LSN >= victim.Start {
				t.Fatalf("returned record %d from or after the damaged one", rec.LSN)
			}
			n++
			continue
		}
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("err = %v, want ErrCorrupt", err)
		}
		break
	}
	if want := segs[0].Recs; n != want {
		t.Fatalf("read %d records before the damage, want %d", n, want)
	}
	if _, err := r.Next(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("error not sticky: %v", err)
	}
	// Starting past the damaged record works but is caught at a later one;
	// starting inside the damaged segment beyond it is fine.
	if _, err := NewReader(m, testDir, recs[len(recs)-1].LSN); err != nil {
		t.Fatalf("reading from the last record: %v", err)
	}
	// Seeking through the damaged segment to a later record fails loudly.
	var later LSN
	for _, rec := range recs {
		if rec.LSN > victim.Start+SegmentHeaderSize && rec.LSN < victim.Start+LSN(victim.Size) {
			later = rec.LSN
			break
		}
	}
	if later != 0 {
		if _, err := NewReader(m, testDir, later); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("seeking past damage: err = %v, want ErrCorrupt", err)
		}
	}
}

func TestReaderTruncatedMiddleSegmentIsCorrupt(t *testing.T) {
	m, _ := buildLog(t, 30, 150)
	_, segs := readLog(t, m, testDir)
	name := path.Join(testDir, SegmentName(segs[1].Start))
	data := readFile(t, m, name)
	writeFile(t, m, name, data[:len(data)-5])
	if _, err := NewReader(m, testDir, SegmentHeaderSize); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v", err)
	}
}

func TestReaderIgnoresLeftoverSegment(t *testing.T) {
	m, recs := buildLog(t, 6, 0)
	_, segs := readLog(t, m, testDir)
	end := segs[0].Start + LSN(segs[0].Size)
	writeFile(t, m, path.Join(testDir, SegmentName(end)), []byte{1, 2, 3})
	got, r := readAll(t, m, SegmentHeaderSize)
	sameRecs(t, got, recs, "read")
	if r.End() != end {
		t.Fatalf("End %d, want %d", r.End(), end)
	}
}

func TestReaderOnLiveLog(t *testing.T) {
	// A reader sees what has been flushed when it opened each segment.
	m := newFS(t)
	w := mustOpen(t, m, Options{SegmentSize: 200})
	var recs []logRec
	for i := range 10 {
		p := []byte(fmt.Sprintf("rec-%d", i))
		recs = append(recs, logRec{mustAppend(t, w, 1, p), 1, p})
	}
	mustFlush(t, w)
	mustAppend(t, w, 1, []byte("buffered, not flushed"))
	got, _ := readAll(t, m, SegmentHeaderSize)
	sameRecs(t, got, recs, "live read")
	mustClose(t, w)
}
