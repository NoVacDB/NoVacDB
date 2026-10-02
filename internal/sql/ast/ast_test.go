package ast

import (
	"math"
	"testing"
)

func TestQuoteIdent(t *testing.T) {
	cases := map[string]string{
		"abc": "abc", "a_1$": "a_1$", "_x": "_x", "ünï": "ünï",
		"Abc": `"Abc"`, "1a": `"1a"`, "$a": `"$a"`, "a b": `"a b"`, `a"b`: `"a""b"`, "": `""`,
		"select": `"select"`, "key": `"key"`, "between": `"between"`, "like": `"like"`,
		"text": "text", // not a keyword
	}
	for in, want := range cases {
		if got := QuoteIdent(in); got != want {
			t.Errorf("QuoteIdent(%q) = %s, want %s", in, got, want)
		}
	}
	if got := QuoteString("it's"); got != "'it''s'" {
		t.Errorf("QuoteString = %s", got)
	}
}

func TestPrintNodes(t *testing.T) {
	col := func(n string) Expr { return &ColumnRef{Column: n} }
	cases := []struct {
		n    Node
		want string
	}{
		{&IntegerLit{Value: -5}, "-5"},
		{&IntegerLit{Value: math.MinInt64}, "-9223372036854775808"},
		{&FloatLit{Text: "-1.5", Value: -1.5}, "-1.5"},
		{&FloatLit{Text: "2e3", Value: 2000}, "2e3"},
		{&Unary{Op: "-", X: col("a")}, "- a"},
		{&Unary{Op: "NOT", X: col("a")}, "NOT a"},
		{&Is{X: col("a"), Not: true, Test: IsUnknown}, "a IS NOT UNKNOWN"},
		{&IsDistinct{L: col("a"), R: &NullLit{}}, "a IS DISTINCT FROM NULL"},
		{&Between{X: col("a"), Lo: &IntegerLit{Value: 1}, Hi: &IntegerLit{Value: 2}, Not: true}, "a NOT BETWEEN 1 AND 2"},
		{&In{X: col("a"), List: []Expr{&BoolLit{Value: true}, &BoolLit{}}}, "a IN (TRUE, FALSE)"},
		{&Like{X: col("a"), Pattern: &StringLit{Value: "x"}, CaseInsensitive: true, Not: true}, "a NOT ILIKE 'x'"},
		{&Cast{X: &Param{N: 2}, Type: TypeDouble}, "CAST($2 AS double precision)"},
		{&FuncCall{Name: "Lower", Args: []Expr{col("a")}}, `"Lower"(a)`},
		{&Case{Operand: col("a"), Whens: []*When{{Cond: &IntegerLit{Value: 1}, Result: &Default{}}}, Else: &NullLit{}}, "CASE a WHEN 1 THEN DEFAULT ELSE NULL END"},
		{&ColumnRef{Table: "T", Column: "c"}, `"T".c`},
		{&Binary{Op: "*", L: &Binary{Op: "+", L: col("a"), R: col("b")}, R: &Unary{Op: "NOT", X: col("c")}}, "(a + b) * (NOT c)"},
		{&Binary{Op: "=", L: &Binary{Op: "=", L: col("a"), R: col("b")}, R: col("c")}, "(a = b) = c"},
		{&Unary{Op: "-", X: &Binary{Op: "^", L: col("a"), R: &IntegerLit{Value: -2}}}, "- (a ^ -2)"},
		{&ColumnDef{Name: Name{Name: "d"}, Type: TypeBoolean, Default: &Binary{Op: "OR", L: col("a"), R: col("b")}}, "d boolean DEFAULT (a OR b)"},
		{&Target{Star: true, StarTable: "My"}, `"My".*`},
		{&OrderItem{Expr: col("a"), Desc: true, Nulls: NullsLast}, "a DESC NULLS LAST"},
		{&CreateIndex{Unique: true, IfNotExists: true, Name: Name{Name: "i"}, Table: Name{Name: "t"}, Columns: []Name{{Name: "a"}}}, "CREATE UNIQUE INDEX IF NOT EXISTS i ON t (a)"},
		{&DropIndex{Name: Name{Name: "Idx"}, IfExists: true}, `DROP INDEX IF EXISTS "Idx"`},
		{&Insert{Table: Name{Name: "t"}, Columns: []Name{{Name: "a"}}, DefaultValues: true}, "INSERT INTO t (a) DEFAULT VALUES"},
		{&ColumnDef{Name: Name{Name: "a"}, Type: TypeTimestampTZ, Null: true, Unique: true}, "a timestamptz NULL UNIQUE"},
	}
	for _, c := range cases {
		if got := c.n.String(); got != c.want {
			t.Errorf("got %s, want %s", got, c.want)
		}
	}
	if TypeBoolean.String() != "boolean" || Type(99).String() != "type 99" {
		t.Error("Type.String")
	}
	// Positions.
	if (&Name{P: 3}).Pos() != 3 || (&Assignment{Column: Name{P: 4}}).Pos() != 4 || (&When{Cond: &NullLit{P: 5}}).Pos() != 5 ||
		(&OrderItem{Expr: &NullLit{P: 6}}).Pos() != 6 || (&TableRef{Name: Name{P: 7}}).Pos() != 7 || (&ColumnDef{Name: Name{P: 8}}).Pos() != 8 {
		t.Error("Pos")
	}
}
