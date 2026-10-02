package sqllogic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/vikrant-choudhary06/NoVacDB/internal/pgwire"
	"github.com/vikrant-choudhary06/NoVacDB/internal/server"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/executor"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
	"github.com/vikrant-choudhary06/NoVacDB/internal/vfs"
)

// wireSession runs SQL through a NoVacDB server, over loopback TCP, with a
// minimal protocol client: the same results must come back as from the
// executor directly.
type wireSession struct {
	db  *executor.DB
	srv *server.Server
	c   net.Conn
	rd  *pgwire.Reader
}

func openWire(ctx context.Context, fsys vfs.FS, dir string, opts executor.Options) (Session, error) {
	db, err := executor.Open(ctx, fsys, dir, opts)
	if err != nil {
		return nil, err
	}
	srv, err := server.New(server.Config{DB: db, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	go func() { _ = srv.Serve(ln) }()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		return nil, err
	}
	w := &wireSession{db: db, srv: srv, c: c, rd: pgwire.NewReader(bufio.NewReader(c))}
	_ = c.SetDeadline(time.Now().Add(time.Minute))
	if _, err := c.Write(pgwire.EncodeStartup(pgwire.ProtocolVersion30, []pgwire.Param{{Name: "user", Value: "tester"}})); err != nil {
		return nil, err
	}
	if _, err := w.readUntilReady(); err != nil {
		return nil, fmt.Errorf("startup: %w", err)
	}
	return w, nil
}

func cstrings(b []byte) []string {
	parts := bytes.Split(b, []byte{0})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, string(p))
	}
	return out
}

// oidLetter maps a type OID to the runner's type letter.
var oidLetter = map[uint32]byte{
	pgwire.OIDInt4: 'I', pgwire.OIDInt8: 'I', pgwire.OIDFloat8: 'R',
	pgwire.OIDText: 'T', pgwire.OIDBool: 'B', pgwire.OIDTimestampTZ: 'D',
}

// readUntilReady collects the results of one Query, as the server sends
// them, up to ReadyForQuery.
func (w *wireSession) readUntilReady() ([]Result, error) {
	var out []Result
	var cur *Result
	var qerr error
	for {
		typ, body, err := w.rd.ReadMessage()
		if err != nil {
			return nil, err
		}
		switch typ {
		case 'T':
			n := int(binary.BigEndian.Uint16(body))
			body = body[2:]
			letters := make([]byte, n)
			for i := range n {
				end := bytes.IndexByte(body, 0)
				body = body[end+1:]
				l, ok := oidLetter[binary.BigEndian.Uint32(body[6:])]
				if !ok {
					return nil, fmt.Errorf("unknown type OID %d", binary.BigEndian.Uint32(body[6:]))
				}
				letters[i] = l
				body = body[18:]
			}
			cur = &Result{Types: string(letters)}
		case 'D':
			n := int(binary.BigEndian.Uint16(body))
			body = body[2:]
			row := make([]string, n)
			for i := range n {
				l := int32(binary.BigEndian.Uint32(body))
				body = body[4:]
				switch {
				case l < 0:
					row[i] = "NULL"
				case l == 0:
					row[i] = "(empty)"
				default:
					row[i] = string(body[:l])
					body = body[l:]
				}
			}
			cur.Rows = append(cur.Rows, row)
		case 'C':
			if cur == nil {
				cur = &Result{}
			}
			cur.Tag = string(bytes.TrimSuffix(body, []byte{0}))
			out = append(out, *cur)
			cur = nil
		case 'E':
			f := map[byte]string{}
			for _, fld := range cstrings(body) {
				if fld != "" {
					f[fld[0]] = fld[1:]
				}
			}
			e := sqlerr.New(f['C'], "%s", f['M'])
			e.Detail, e.Hint = f['D'], f['H']
			if p, err := strconv.Atoi(f['P']); err == nil {
				e.Position = p
			}
			qerr = e
		case 'Z':
			return out, qerr
		case 'R', 'S', 'K', 'N', 'I':
		default:
			return nil, fmt.Errorf("unexpected message %q", typ)
		}
	}
}

func (w *wireSession) Exec(_ context.Context, sql string) ([]Result, error) {
	b := append([]byte{'Q', 0, 0, 0, 0}, sql...)
	b = append(b, 0)
	binary.BigEndian.PutUint32(b[1:], uint32(len(b)-1))
	_ = w.c.SetDeadline(time.Now().Add(time.Minute))
	if _, err := w.c.Write(b); err != nil {
		return nil, err
	}
	return w.readUntilReady()
}

func (w *wireSession) Close(ctx context.Context) error {
	_, _ = w.c.Write([]byte{'X', 0, 0, 0, 4})
	_ = w.c.Close()
	return errors.Join(w.srv.Close(), w.db.Close(ctx))
}

func (w *wireSession) Abandon() {
	_ = w.c.Close()
	_ = w.srv.Close()
}

// TestSQLLogicOverTheWire runs every test file through the server: the
// protocol must carry exactly what the executor returns.
func TestSQLLogicOverTheWire(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.test"))
	if err != nil || len(files) == 0 {
		t.Fatal(files, err)
	}
	for _, name := range files {
		t.Run(filepath.Base(name), func(t *testing.T) {
			f, err := os.Open(name)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			n, err := (&Runner{Frames: 256, Open: openWire}).RunFile(context.Background(), name, f)
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				t.Fatal("no records")
			}
		})
	}
}
