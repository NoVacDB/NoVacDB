package sqlerr

import (
	"errors"
	"fmt"
	"io"
	"testing"
)

func TestCharPos(t *testing.T) {
	sql := "aé😀b"
	cases := map[int]int{0: 1, 1: 2, 3: 3, 7: 4, 8: 5, 100: 5}
	for off, want := range cases {
		if got := CharPos(sql, off); got != want {
			t.Errorf("CharPos(%d) = %d, want %d", off, got, want)
		}
	}
}

func TestErrorFormattingAndWrapping(t *testing.T) {
	e := New(SyntaxError, "syntax error at or near %q", "x").At("ab x", 3).WithHint("try %s", "y").WithDetail("d")
	if e.Position != 4 || e.Hint != "try y" || e.Detail != "d" {
		t.Fatalf("%+v", e)
	}
	if got := e.Error(); got != `syntax error at or near "x" (SQLSTATE 42601) at character 4` {
		t.Fatalf("Error() = %q", got)
	}
	if New(SyntaxError, "m").At("x", -1).Position != 0 {
		t.Fatal("negative offset set a position")
	}
	if got := New(DivisionByZero, "division by zero").Error(); got != "division by zero (SQLSTATE 22012)" {
		t.Fatalf("Error() = %q", got)
	}
	w := Wrap(io.EOF, IOError, "reading")
	if !errors.Is(w, io.EOF) || w.Code != IOError {
		t.Fatal("Wrap lost its cause")
	}
	// From finds an *Error through wrapping, and wraps anything else.
	outer := fmt.Errorf("context: %w", e)
	if From(outer) != e || Code(outer) != SyntaxError {
		t.Fatal("From did not find the wrapped error")
	}
	plain := errors.New("boom")
	if got := From(plain); got.Code != InternalError || !errors.Is(got, plain) || got.Message != "boom" {
		t.Fatalf("From(plain) = %+v", got)
	}
	if From(nil) != nil || Code(nil) != "" {
		t.Fatal("nil error")
	}
}
