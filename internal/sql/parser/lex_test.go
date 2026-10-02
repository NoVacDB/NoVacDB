package parser

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
)

// describe renders tokens compactly: kind:value, space separated, without
// the final EOF.
func describe(toks []Token) string {
	var parts []string
	for _, t := range toks {
		switch t.Kind {
		case EOF:
			continue
		case Ident:
			if t.Quoted {
				parts = append(parts, "qid:"+t.Str)
			} else {
				parts = append(parts, "id:"+t.Str)
			}
		case Keyword:
			parts = append(parts, "kw:"+t.Str)
		case String:
			parts = append(parts, fmt.Sprintf("str:%q", t.Str))
		case Integer:
			parts = append(parts, "int:"+t.Str)
		case Float:
			parts = append(parts, "num:"+t.Str)
		case Param:
			parts = append(parts, fmt.Sprintf("param:%d", t.Param))
		case Op:
			parts = append(parts, "op:"+t.Str)
		default:
			parts = append(parts, t.Str)
		}
	}
	return strings.Join(parts, " ")
}

func TestLexTokens(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"  \t\n\r\f ", ""},
		{"SELECT a FROM t", "kw:select id:a kw:from id:t"},
		{"SeLeCt MyTable", "kw:select id:mytable"},
		{"XYZ AZ", "id:xyz id:az"},
		{`"MyTable" "a""b" "select"`, `qid:MyTable qid:a"b qid:select`},
		{`"with space" "ünï"`, `qid:with space qid:ünï`},
		{"ÄbC_1$x", "id:Äbc_1$x"}, // only ASCII letters fold
		{"_x x1 x$", "id:_x id:x1 id:x$"},
		{"a.b", "id:a . id:b"},
		{"t.*", "id:t . op:*"},
		{"(a, b);", "( id:a , id:b ) ;"},
		{"a[1:2]", "id:a [ int:1 : int:2 ]"},
		{"x::int", "id:x op::: kw:int"},
		{"x :: int4", "id:x op::: id:int4"},
		{"'abc' 'it''s' ''", `str:"abc" str:"it's" str:""`},
		{`'back\slash'`, `str:"back\\slash"`},
		{"'a'\n'b'", `str:"ab"`},
		{"'a'  \n\t 'b'", `str:"ab"`},
		{"'a' 'b'", `str:"a" str:"b"`},
		{"'a' -- c\n 'b'", `str:"a" str:"b"`},
		{"'a'\n -- c\n 'b'", `str:"ab"`},
		{`E'a\nb\tc\\d\'e'`, `str:"a\nb\tc\\d'e"`},
		{`e'\b\f\r\q'`, `str:"\b\f\rq"`},
		{`E'\x414 \x41\x4a\x4A\xZ'`, `str:"A4 AJJxZ"`},
		{`E'\101\60\7'`, `str:"A0\a"`},
		{`E'\u00e9\U0001F600'`, `str:"é😀"`},
		{`E'\uD83D\uDE00'`, `str:"😀"`},
		{`E'\U0000D83D\U0000DE00' E'\uD83D\U0000DE00'`, `str:"😀" str:"😀"`},
		{`E'\xc3\xa9'`, `str:"é"`},
		{"E'a'\n'\\n'", `str:"a\n"`},
		{"$$it's $x$$ $tag$a$$b$tag$ $a1$x$a1$", `str:"it's $x" str:"a$$b" str:"x"`},
		{"$1 $23", "param:1 param:23"},
		{"1 42 0 007", "int:1 int:42 int:0 int:007"},
		{"1.5 1. .5 1e10 1.5E-3 1e+2 .5e1 1.e5", "num:1.5 num:1. num:.5 num:1e10 num:1.5E-3 num:1e+2 num:.5e1 num:1.e5"},
		{"0x1F 0XaB 0o17 0b101 0x_1f", "int:0x1F int:0XaB int:0o17 int:0b101 int:0x_1f"},
		{"1_000 1_000.5 1e1_0", "int:1_000 num:1_000.5 num:1e1_0"},
		{"1..2", "int:1 . num:.2"}, // as PostgreSQL: the integer stops before ".."
		{"a+b a + b", "id:a op:+ id:b id:a op:+ id:b"},
		{"a+-1", "id:a op:+ op:- int:1"},
		{"a*-1", "id:a op:* op:- int:1"},
		{"a<=b a>=b a<>b a!=b", "id:a op:<= id:b id:a op:>= id:b id:a op:<> id:b id:a op:<> id:b"},
		{"a||b", "id:a op:|| id:b"},
		{"a@-b", "id:a op:@- id:b"},
		{"a%-b", "id:a op:%- id:b"},
		{"a=-b", "id:a op:= op:- id:b"},
		{"a<-->b", "id:a op:<"},
		{"a+/*c*/b", "id:a op:+ id:b"},
		{"a -- comment\nb", "id:a id:b"},
		{"a --comment", "id:a"},
		{"a -- comment\rb", "id:a id:b"},
		{"a /* x /* nested */ y */ b", "id:a id:b"},
		{"select/**/1", "kw:select int:1"},
		{"1-1", "int:1 op:- int:1"},
		{"between Between", "kw:between kw:between"},
	}
	for _, c := range cases {
		toks, err := Lex(c.in)
		if err != nil {
			t.Errorf("Lex(%q): %v", c.in, err)
			continue
		}
		if got := describe(toks); got != c.want {
			t.Errorf("Lex(%q)\n got  %s\n want %s", c.in, got, c.want)
		}
	}
}

func TestLexPositions(t *testing.T) {
	sql := "SELECT  'x''y' ,\n\"Q\" /* c */ 1.5e3"
	toks, err := Lex(sql)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"SELECT", "'x''y'", ",", `"Q"`, "1.5e3", ""}
	if len(toks) != len(want) {
		t.Fatalf("%d tokens", len(toks))
	}
	for i, tk := range toks {
		if got := sql[tk.Pos:tk.End]; got != want[i] {
			t.Errorf("token %d spans %q, want %q", i, got, want[i])
		}
	}
	if toks[len(toks)-1].Pos != len(sql) {
		t.Error("EOF is not at the end")
	}
}

func TestLexKeywordCategories(t *testing.T) {
	toks, err := Lex("select integer is by user mytable")
	if err != nil {
		t.Fatal(err)
	}
	want := []KeywordCategory{Reserved, ColName, TypeFuncName, Unreserved, Reserved, 0}
	for i, w := range want {
		if toks[i].Category != w {
			t.Errorf("token %q: category %d, want %d", toks[i].Str, toks[i].Category, w)
		}
	}
	if toks[5].Kind != Ident {
		t.Error("mytable is not an identifier")
	}
	// Quoted keywords are identifiers.
	toks, _ = Lex(`"select"`)
	if toks[0].Kind != Ident {
		t.Error("a quoted keyword is not an identifier")
	}
}

func TestLexErrors(t *testing.T) {
	long := strings.Repeat("a", 64)
	cases := []struct {
		in   string
		code string
		pos  int // 1-based character position
		msg  string
	}{
		{"'abc", sqlerr.SyntaxError, 1, "unterminated quoted string"},
		{"x 'ab''", sqlerr.SyntaxError, 3, "unterminated quoted string"},
		{`E'abc\'`, sqlerr.SyntaxError, 1, "unterminated quoted string"},
		{`"abc`, sqlerr.SyntaxError, 1, "unterminated quoted identifier"},
		{`""`, sqlerr.SyntaxError, 1, "zero-length delimited identifier"},
		{"/* a /* b */", sqlerr.SyntaxError, 1, "unterminated /* comment"},
		{"$abc$ x", sqlerr.SyntaxError, 1, "unterminated dollar-quoted string"},
		{"$ x", sqlerr.SyntaxError, 1, `syntax error at or near "$"`},
		{"select ü\\", sqlerr.SyntaxError, 9, `syntax error at or near "\\"`},
		{"a { b", sqlerr.SyntaxError, 3, `syntax error at or near "{"`},
		{"12abc", sqlerr.SyntaxError, 1, `trailing junk after numeric literal at or near "12abc"`},
		{"0x", sqlerr.SyntaxError, 1, "trailing junk"},
		{"0xZ", sqlerr.SyntaxError, 1, "trailing junk"},
		{"0b102", sqlerr.SyntaxError, 1, "trailing junk"},
		{"1_", sqlerr.SyntaxError, 1, "trailing junk"},
		{"1__0", sqlerr.SyntaxError, 1, "trailing junk"},
		{"1e", sqlerr.SyntaxError, 1, "trailing junk"},
		{"1e+", sqlerr.SyntaxError, 1, "trailing junk"},
		{"1e_5", sqlerr.SyntaxError, 1, "trailing junk"},
		{"1.5x", sqlerr.SyntaxError, 1, "trailing junk"},
		{"$1a", sqlerr.SyntaxError, 1, "trailing junk after parameter"},
		{"$0", sqlerr.UndefinedParameter, 1, "there is no parameter $0"},
		{"$99999999999999999999", sqlerr.UndefinedParameter, 1, "there is no parameter"},
		{long, sqlerr.NameTooLong, 1, "more than the limit of 63"},
		{`"` + long + `"`, sqlerr.NameTooLong, 1, "more than the limit of 63"},
		{"a\xffb", sqlerr.CharacterNotInRepertoire, 2, "invalid byte sequence"},
		{"a\x00b", sqlerr.CharacterNotInRepertoire, 2, "0x00"},
		{`E'\x00'`, sqlerr.CharacterNotInRepertoire, 3, "0x00"},
		{`E'\0'`, sqlerr.CharacterNotInRepertoire, 3, "0x00"},
		{`E'\xff'`, sqlerr.CharacterNotInRepertoire, 1, "invalid byte sequence"},
		{`E'\u12'`, sqlerr.InvalidEscapeSequence, 3, "invalid Unicode escape"},
		{`E'\u0000'`, sqlerr.InvalidEscapeSequence, 3, "invalid Unicode escape value"},
		{`E'\U00110000'`, sqlerr.InvalidEscapeSequence, 3, "invalid Unicode escape value"},
		{`E'\uD83D'`, sqlerr.InvalidEscapeSequence, 3, "surrogate pair"},
		{`E'\uD83Dx'`, sqlerr.InvalidEscapeSequence, 3, "surrogate pair"},
		{`E'\uD83D\u0041'`, sqlerr.InvalidEscapeSequence, 3, "surrogate pair"},
		{`E'\uDE00'`, sqlerr.InvalidEscapeSequence, 3, "surrogate pair"},
		{`E'\uD83D\uDE0'`, sqlerr.InvalidEscapeSequence, 3, "surrogate pair"},
		{`E'\u12`, sqlerr.InvalidEscapeSequence, 3, "invalid Unicode escape"},
		{`E'\U0001F60'`, sqlerr.InvalidEscapeSequence, 3, "invalid Unicode escape"},
		{`E'\`, sqlerr.SyntaxError, 1, "unterminated quoted string"},
		{`E'\777'`, sqlerr.CharacterNotInRepertoire, 3, "0xff"},
		{"ü 'x", sqlerr.SyntaxError, 3, "unterminated"},
	}
	for _, c := range cases {
		_, err := Lex(c.in)
		var e *sqlerr.Error
		if !errors.As(err, &e) {
			t.Errorf("Lex(%q) = %v, want an error", c.in, err)
			continue
		}
		if e.Code != c.code || e.Position != c.pos || !strings.Contains(e.Message, c.msg) {
			t.Errorf("Lex(%q) = %q code %s at %d; want %s at %d containing %q", c.in, e.Message, e.Code, e.Position, c.code, c.pos, c.msg)
		}
	}
}

func TestLexLimits(t *testing.T) {
	ok := strings.Repeat("a", 63)
	if _, err := Lex(ok); err != nil {
		t.Fatalf("63-byte name: %v", err)
	}
	// The limit counts bytes after folding: 32 two-byte letters is 64 bytes.
	if _, err := Lex(strings.Repeat("é", 32)); sqlerr.Code(err) != sqlerr.NameTooLong {
		t.Fatalf("64-byte name: %v", err)
	}
	big := strings.Repeat(" ", MaxQueryLength) + "x"
	if _, err := Lex(big); sqlerr.Code(err) != sqlerr.ProgramLimitExceeded {
		t.Fatalf("oversized query: %v", err)
	}
	if _, err := Lex(strings.Repeat(" ", MaxQueryLength)); err != nil {
		t.Fatalf("query at the limit: %v", err)
	}
}

func TestLongNameHint(t *testing.T) {
	_, err := Lex(strings.Repeat("x", 70))
	e := sqlerr.From(err)
	if e.Hint == "" {
		t.Fatal("no hint for a long name")
	}
}

// relexes reports whether the text of a token lexes to the same token on
// its own.
func checkRelex(t *testing.T, sql string, tk Token) {
	t.Helper()
	text := sql[tk.Pos:tk.End]
	again, err := Lex(text)
	if err != nil {
		t.Fatalf("token %q of %q does not lex on its own: %v", text, sql, err)
	}
	if len(again) != 2 || again[0].Kind != tk.Kind || again[0].Str != tk.Str {
		t.Fatalf("token %q of %q re-lexes as %s", text, sql, describe(again))
	}
}

func FuzzLex(f *testing.F) {
	for _, s := range []string{
		"SELECT a, b FROM t WHERE x <= 10 AND y <> 'it''s' -- c\n",
		`E'\u00e9\x41' "Quoted""Id" $$dollar$$ $tag$x$tag$`,
		"1.5e-3 0x1F 1_000 .5 $1 a::int /* /* */ */ a+-b",
		"'a'\n'b' x||y a@-b",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, sql string) {
		toks, err := Lex(sql)
		if err != nil {
			var e *sqlerr.Error
			if !errors.As(err, &e) || e.Code == "" || e.Position < 0 {
				t.Fatalf("bad error %v", err)
			}
			return
		}
		prev := 0
		for i, tk := range toks {
			if tk.Pos < prev || tk.End < tk.Pos || tk.End > len(sql) {
				t.Fatalf("token %d out of order: [%d,%d) after %d", i, tk.Pos, tk.End, prev)
			}
			// Only whitespace and comments between tokens.
			gap := sql[prev:tk.Pos]
			if g, err := Lex(gap); err != nil || len(g) != 1 {
				t.Fatalf("gap %q between tokens is not blank", gap)
			}
			if tk.Kind == EOF && (i != len(toks)-1 || tk.Pos != len(sql)) {
				t.Fatal("EOF is not last or not at the end")
			}
			if tk.Kind != EOF && tk.End == tk.Pos {
				t.Fatalf("empty token %d", i)
			}
			switch tk.Kind {
			case Ident, Keyword, Integer, Float, Param:
				checkRelex(t, sql, tk)
			case Op:
				// An operator's text re-lexes to itself unless cutting
				// depended on what followed.
				if tk.Str != "<>" || sql[tk.Pos:tk.End] == "<>" {
					checkRelex(t, sql, tk)
				}
			}
			prev = tk.End
		}
	})
}
