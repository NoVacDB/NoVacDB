package server

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/vikrant-choudhary06/NoVacDB/internal/pgwire"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
)

// fuzzSetup starts every fuzzed session: a table with rows and indexes,
// and a named statement, so that mutated messages reach real work.
var fuzzSetup = bytes.Join([][]byte{
	startup("user", "fuzz"),
	pgwire.EncodeQuery("CREATE TABLE t (id int PRIMARY KEY, s text UNIQUE, f double precision, ts timestamptz); " +
		"INSERT INTO t VALUES (1, 'one', 1.5, '2024-01-01'), (2, NULL, 'NaN', 'infinity')"),
	pgwire.Parse{Name: "get", Query: "SELECT * FROM t WHERE id >= $1 ORDER BY id"}.Encode(),
	pgwire.EncodeSync(),
}, nil)

// FuzzSession sends arbitrary bytes, as messages, to a session after its
// startup. Whatever arrives, the server never fails internally (a
// contained panic would be a FATAL XX000), never hangs, and ends the
// session cleanly when the client goes.
func FuzzSession(f *testing.F) {
	for _, seed := range [][]byte{
		bytes.Join([][]byte{
			pgwire.Bind{Statement: "get", Params: [][]byte{[]byte("1")}, ResultFormats: []int16{1}}.Encode(),
			pgwire.EncodeDescribe('P', ""), pgwire.Execute{MaxRows: 1}.Encode(), pgwire.Execute{}.Encode(), pgwire.EncodeSync(),
		}, nil),
		bytes.Join([][]byte{
			pgwire.Parse{Query: "INSERT INTO t VALUES ($1, $2, $3, $4)", ParamTypes: []uint32{21, 1043, 700, 1184}}.Encode(),
			pgwire.Bind{ParamFormats: []int16{1}, Params: [][]byte{{0, 9}, []byte("x"), {0x3f, 0x80, 0, 0}, make([]byte, 8)}}.Encode(),
			pgwire.Execute{}.Encode(), pgwire.EncodeSync(),
		}, nil),
		bytes.Join([][]byte{
			pgwire.EncodeQuery("SELECT s, f FROM t; UPDATE t SET f = f * 2"), pgwire.EncodeClose('S', "get"),
			pgwire.EncodeFlush(), pgwire.Bind{Statement: "get"}.Encode(), pgwire.EncodeSync(),
		}, nil),
		{'X', 0, 0, 0, 4},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		var logs syncBuffer
		_, addr := startServer(t, Config{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(20 * time.Second))
		out := make(chan []byte, 1)
		go func() {
			b, _ := io.ReadAll(c)
			out <- b
		}()
		_, _ = c.Write(fuzzSetup)
		_, _ = c.Write(in)
		// End the input but keep reading: the server must answer what it
		// got and then end the session.
		_ = c.(*net.TCPConn).CloseWrite()
		b := <-out
		rd := pgwire.NewReader(bufio.NewReader(bytes.NewReader(b)))
		for {
			typ, body, err := rd.ReadMessage()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					t.Fatalf("reading the answers: %v; input %q", err, in)
				}
				break
			}
			if (typ == 'E' || typ == 'N') && fields(body)['C'] == sqlerr.InternalError {
				t.Fatalf("internal error %v; input %q", fields(body), in)
			}
		}
		if strings.Contains(logs.String(), "panic") {
			t.Fatalf("panic logged: %s; input %q", logs.String(), in)
		}
	})
}
