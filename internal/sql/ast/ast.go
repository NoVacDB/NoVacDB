// Package ast defines the syntax tree the SQL parser produces. Every node
// records the byte offset in the query where it starts (for error
// positions) and prints back to SQL, with identifiers quoted where needed
// and only the parentheses operator precedence requires, so that parsing
// the printed form gives the same tree and is never nested more deeply than
// the original text. See docs/design/09-sql-frontend.md.
package ast

import (
	"strconv"
	"strings"

	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/keyword"
)

// Node is any syntax tree node.
type Node interface {
	// Pos is the byte offset in the query where the node starts.
	Pos() int
	// String prints the node as SQL.
	String() string
}

// Stmt is a statement.
type Stmt interface {
	Node
	stmt()
}

// Expr is an expression.
type Expr interface {
	Node
	expr()
}

// Type is a column or cast type.
type Type uint8

// Types.
const (
	TypeInvalid Type = iota
	TypeInteger
	TypeBigInt
	TypeDouble
	TypeText
	TypeBoolean
	TypeTimestampTZ
)

var typeNames = [...]string{
	TypeInvalid: "invalid", TypeInteger: "integer", TypeBigInt: "bigint",
	TypeDouble: "double precision", TypeText: "text", TypeBoolean: "boolean",
	TypeTimestampTZ: "timestamptz",
}

// String prints the Type as SQL.
func (t Type) String() string {
	if int(t) < len(typeNames) {
		return typeNames[t]
	}
	return "type " + strconv.Itoa(int(t))
}

// Name is an identifier with its position.
type Name struct {
	P    int
	Name string
}

// Pos returns the name's position.
func (n Name) Pos() int { return n.P }

// String prints the name, quoted if needed.
func (n Name) String() string { return QuoteIdent(n.Name) }

// QuoteIdent returns name as it must be written in SQL: bare if it is a
// lower-case identifier that is not a keyword, otherwise double-quoted.
func QuoteIdent(name string) string {
	_, isKeyword := keyword.Lookup(name)
	bare := name != "" && !isKeyword
	for i := 0; i < len(name) && bare; i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c == '_', c >= 0x80:
		case (c >= '0' && c <= '9' || c == '$') && i > 0:
		default:
			bare = false
		}
	}
	if bare {
		return name
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// QuoteString returns s as a standard SQL string literal.
func QuoteString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// Precedence levels for printing, from loosest to tightest, matching the
// parser (docs/design/09-sql-frontend.md section 2.3).
const (
	precOr = iota + 1
	precAnd
	precNot
	precIs
	precCmp
	precPredicate // BETWEEN, IN, LIKE
	precOther     // ||
	precAdd
	precMul
	precExp
	precUnary
	precPrimary
)

// binaryPrec returns the precedence of a binary operator.
func binaryPrec(op string) int {
	switch op {
	case "OR":
		return precOr
	case "AND":
		return precAnd
	case "=", "<>", "<", ">", "<=", ">=":
		return precCmp
	case "+", "-":
		return precAdd
	case "*", "/", "%":
		return precMul
	case "^":
		return precExp
	}
	return precOther
}

// prec returns how tightly e binds as printed.
func prec(e Expr) int {
	switch e := e.(type) {
	case *Binary:
		return binaryPrec(e.Op)
	case *Unary:
		if e.Op == "NOT" {
			return precNot
		}
		return precUnary
	case *Is, *IsDistinct:
		return precIs
	case *Between, *In, *Like:
		return precPredicate
	}
	// Literals bind tightly even when negative: casts print as CAST(...),
	// so no postfix operator can follow a printed "-5".
	return precPrimary
}

// operand prints e where an expression binding at least as tightly as min
// is needed, adding parentheses if e binds more loosely.
func operand(e Expr, min int) string {
	if prec(e) < min {
		return "(" + e.String() + ")"
	}
	return e.String()
}

func join[T Node](items []T, sep string) string {
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = it.String()
	}
	return strings.Join(parts, sep)
}

// --- statements ----------------------------------------------------------

// ColumnDef is one column of CREATE TABLE.
type ColumnDef struct {
	Name       Name
	Type       Type
	TypeP      int
	NotNull    bool
	Null       bool // explicitly NULL
	PrimaryKey bool
	Unique     bool
	Default    Expr // nil if none
}

// Pos returns the position of the ColumnDef.
func (c *ColumnDef) Pos() int { return c.Name.P }

// String prints the ColumnDef as SQL.
func (c *ColumnDef) String() string {
	s := c.Name.String() + " " + c.Type.String()
	if c.NotNull {
		s += " NOT NULL"
	}
	if c.Null {
		s += " NULL"
	}
	if c.PrimaryKey {
		s += " PRIMARY KEY"
	}
	if c.Unique {
		s += " UNIQUE"
	}
	if c.Default != nil {
		s += " DEFAULT " + operand(c.Default, precCmp)
	}
	return s
}

// TableConstraint is a PRIMARY KEY or UNIQUE constraint over columns.
type TableConstraint struct {
	P          int
	PrimaryKey bool // else UNIQUE
	Columns    []Name
}

// Pos returns the position of the TableConstraint.
func (c *TableConstraint) Pos() int { return c.P }

// String prints the TableConstraint as SQL.
func (c *TableConstraint) String() string {
	kind := "UNIQUE"
	if c.PrimaryKey {
		kind = "PRIMARY KEY"
	}
	return kind + " (" + join(c.Columns, ", ") + ")"
}

// CreateTable is CREATE TABLE.
type CreateTable struct {
	P           int
	Name        Name
	IfNotExists bool
	Columns     []*ColumnDef
	Constraints []*TableConstraint
}

// Pos returns the position of the CreateTable.
func (s *CreateTable) Pos() int { return s.P }
func (*CreateTable) stmt()      {}

// String prints the CreateTable as SQL.
func (s *CreateTable) String() string {
	out := "CREATE TABLE "
	if s.IfNotExists {
		out += "IF NOT EXISTS "
	}
	parts := make([]string, 0, len(s.Columns)+len(s.Constraints))
	for _, c := range s.Columns {
		parts = append(parts, c.String())
	}
	for _, c := range s.Constraints {
		parts = append(parts, c.String())
	}
	return out + s.Name.String() + " (" + strings.Join(parts, ", ") + ")"
}

// DropTable is DROP TABLE.
type DropTable struct {
	P        int
	Name     Name
	IfExists bool
}

// Pos returns the position of the DropTable.
func (s *DropTable) Pos() int { return s.P }
func (*DropTable) stmt()      {}

// String prints the DropTable as SQL.
func (s *DropTable) String() string {
	out := "DROP TABLE "
	if s.IfExists {
		out += "IF EXISTS "
	}
	return out + s.Name.String()
}

// CreateIndex is CREATE INDEX.
type CreateIndex struct {
	P           int
	Name        Name // Name.Name is "" if no name was given
	Unique      bool
	IfNotExists bool
	Table       Name
	Columns     []Name
}

// Pos returns the position of the CreateIndex.
func (s *CreateIndex) Pos() int { return s.P }
func (*CreateIndex) stmt()      {}

// String prints the CreateIndex as SQL.
func (s *CreateIndex) String() string {
	out := "CREATE "
	if s.Unique {
		out += "UNIQUE "
	}
	out += "INDEX "
	if s.IfNotExists {
		out += "IF NOT EXISTS "
	}
	if s.Name.Name != "" {
		out += s.Name.String() + " "
	}
	return out + "ON " + s.Table.String() + " (" + join(s.Columns, ", ") + ")"
}

// DropIndex is DROP INDEX.
type DropIndex struct {
	P        int
	Name     Name
	IfExists bool
}

// Pos returns the position of the DropIndex.
func (s *DropIndex) Pos() int { return s.P }
func (*DropIndex) stmt()      {}

// String prints the DropIndex as SQL.
func (s *DropIndex) String() string {
	out := "DROP INDEX "
	if s.IfExists {
		out += "IF EXISTS "
	}
	return out + s.Name.String()
}

// Insert is INSERT INTO ... VALUES or DEFAULT VALUES.
type Insert struct {
	P             int
	Table         Name
	Columns       []Name   // nil: all columns in order
	Rows          [][]Expr // nil with DefaultValues
	DefaultValues bool
}

// Pos returns the position of the Insert.
func (s *Insert) Pos() int { return s.P }
func (*Insert) stmt()      {}

// String prints the Insert as SQL.
func (s *Insert) String() string {
	out := "INSERT INTO " + s.Table.String()
	if s.Columns != nil {
		out += " (" + join(s.Columns, ", ") + ")"
	}
	if s.DefaultValues {
		return out + " DEFAULT VALUES"
	}
	rows := make([]string, len(s.Rows))
	for i, r := range s.Rows {
		rows[i] = "(" + join(r, ", ") + ")"
	}
	return out + " VALUES " + strings.Join(rows, ", ")
}

// TableRef is a table in FROM, UPDATE or DELETE, with an optional alias.
type TableRef struct {
	Name  Name
	Alias Name // Alias.Name is "" if none
}

// Pos returns the position of the TableRef.
func (t *TableRef) Pos() int { return t.Name.P }

// String prints the TableRef as SQL.
func (t *TableRef) String() string {
	if t.Alias.Name == "" {
		return t.Name.String()
	}
	return t.Name.String() + " AS " + t.Alias.String()
}

// Target is one item of a SELECT list.
type Target struct {
	P         int
	Star      bool   // * or table.*
	StarTable string // the table of table.*, "" for *
	Expr      Expr   // nil for a star
	Alias     Name   // Alias.Name is "" if none
}

// Pos returns the position of the Target.
func (t *Target) Pos() int { return t.P }

// String prints the Target as SQL.
func (t *Target) String() string {
	if t.Star {
		if t.StarTable != "" {
			return QuoteIdent(t.StarTable) + ".*"
		}
		return "*"
	}
	if t.Alias.Name != "" {
		return t.Expr.String() + " AS " + t.Alias.String()
	}
	return t.Expr.String()
}

// NullsOrder is the NULLS FIRST/LAST option of an ORDER BY item.
type NullsOrder uint8

// NULL orderings; the default is NULLS LAST for ascending and NULLS FIRST
// for descending order, as in PostgreSQL.
const (
	NullsDefault NullsOrder = iota
	NullsFirst
	NullsLast
)

// OrderItem is one ORDER BY item.
type OrderItem struct {
	Expr  Expr
	Desc  bool
	Nulls NullsOrder
}

// Pos returns the position of the OrderItem.
func (o *OrderItem) Pos() int { return o.Expr.Pos() }

// String prints the OrderItem as SQL.
func (o *OrderItem) String() string {
	s := o.Expr.String()
	if o.Desc {
		s += " DESC"
	}
	switch o.Nulls {
	case NullsFirst:
		s += " NULLS FIRST"
	case NullsLast:
		s += " NULLS LAST"
	}
	return s
}

// Select is SELECT.
type Select struct {
	P        int
	Distinct bool
	Targets  []*Target
	From     *TableRef // nil without FROM
	Where    Expr
	OrderBy  []*OrderItem
	Limit    Expr // nil for no limit
	Offset   Expr
}

// Pos returns the position of the Select.
func (s *Select) Pos() int { return s.P }
func (*Select) stmt()      {}

// String prints the Select as SQL.
func (s *Select) String() string {
	out := "SELECT "
	if s.Distinct {
		out += "DISTINCT "
	}
	out += join(s.Targets, ", ")
	if s.From != nil {
		out += " FROM " + s.From.String()
	}
	if s.Where != nil {
		out += " WHERE " + s.Where.String()
	}
	if len(s.OrderBy) > 0 {
		out += " ORDER BY " + join(s.OrderBy, ", ")
	}
	if s.Limit != nil {
		out += " LIMIT " + s.Limit.String()
	}
	if s.Offset != nil {
		out += " OFFSET " + s.Offset.String()
	}
	return out
}

// Assignment is one col = expr of UPDATE.
type Assignment struct {
	Column Name
	Value  Expr // *Default for DEFAULT
}

// Pos returns the position of the Assignment.
func (a *Assignment) Pos() int { return a.Column.P }

// String prints the Assignment as SQL.
func (a *Assignment) String() string { return a.Column.String() + " = " + a.Value.String() }

// Update is UPDATE.
type Update struct {
	P     int
	Table *TableRef
	Sets  []*Assignment
	Where Expr
}

// Pos returns the position of the Update.
func (s *Update) Pos() int { return s.P }
func (*Update) stmt()      {}

// String prints the Update as SQL.
func (s *Update) String() string {
	out := "UPDATE " + s.Table.String() + " SET " + join(s.Sets, ", ")
	if s.Where != nil {
		out += " WHERE " + s.Where.String()
	}
	return out
}

// Delete is DELETE.
type Delete struct {
	P     int
	Table *TableRef
	Where Expr
}

// Pos returns the position of the Delete.
func (s *Delete) Pos() int { return s.P }
func (*Delete) stmt()      {}

// String prints the Delete as SQL.
func (s *Delete) String() string {
	out := "DELETE FROM " + s.Table.String()
	if s.Where != nil {
		out += " WHERE " + s.Where.String()
	}
	return out
}

// --- expressions ---------------------------------------------------------

// IntegerLit is an integer constant.
type IntegerLit struct {
	P     int
	Value int64
}

// Pos returns the position of the IntegerLit.
func (e *IntegerLit) Pos() int { return e.P }
func (*IntegerLit) expr()      {}

// String prints the IntegerLit as SQL.
func (e *IntegerLit) String() string { return strconv.FormatInt(e.Value, 10) }

// FloatLit is a decimal or exponent constant. Text is as written (with a
// leading "-" once negated), so it prints exactly.
type FloatLit struct {
	P     int
	Text  string
	Value float64
}

// Pos returns the position of the FloatLit.
func (e *FloatLit) Pos() int { return e.P }
func (*FloatLit) expr()      {}

// String prints the FloatLit as SQL.
func (e *FloatLit) String() string { return e.Text }

// StringLit is a string constant (of unknown type until used).
type StringLit struct {
	P     int
	Value string
}

// Pos returns the position of the StringLit.
func (e *StringLit) Pos() int { return e.P }
func (*StringLit) expr()      {}

// String prints the StringLit as SQL.
func (e *StringLit) String() string { return QuoteString(e.Value) }

// BoolLit is TRUE or FALSE.
type BoolLit struct {
	P     int
	Value bool
}

// Pos returns the position of the BoolLit.
func (e *BoolLit) Pos() int { return e.P }
func (*BoolLit) expr()      {}

// String prints the BoolLit as SQL.
func (e *BoolLit) String() string {
	if e.Value {
		return "TRUE"
	}
	return "FALSE"
}

// NullLit is NULL.
type NullLit struct{ P int }

// Pos returns the position of the NullLit.
func (e *NullLit) Pos() int     { return e.P }
func (*NullLit) expr()          {}
func (*NullLit) String() string { return "NULL" }

// Default is DEFAULT in VALUES or SET.
type Default struct{ P int }

// Pos returns the position of the Default.
func (e *Default) Pos() int     { return e.P }
func (*Default) expr()          {}
func (*Default) String() string { return "DEFAULT" }

// ColumnRef is a column, optionally qualified by its table.
type ColumnRef struct {
	P      int
	Table  string // "" if unqualified
	Column string
}

// Pos returns the position of the ColumnRef.
func (e *ColumnRef) Pos() int { return e.P }
func (*ColumnRef) expr()      {}

// String prints the ColumnRef as SQL.
func (e *ColumnRef) String() string {
	if e.Table != "" {
		return QuoteIdent(e.Table) + "." + QuoteIdent(e.Column)
	}
	return QuoteIdent(e.Column)
}

// Param is a parameter $N.
type Param struct {
	P int
	N int
}

// Pos returns the position of the Param.
func (e *Param) Pos() int { return e.P }
func (*Param) expr()      {}

// String prints the Param as SQL.
func (e *Param) String() string { return "$" + strconv.Itoa(e.N) }

// Unary is a prefix operator: "-", "+" or "NOT".
type Unary struct {
	P  int
	Op string
	X  Expr
}

// Pos returns the position of the Unary.
func (e *Unary) Pos() int { return e.P }
func (*Unary) expr()      {}

// String prints the Unary as SQL.
func (e *Unary) String() string {
	if e.Op == "NOT" {
		return "NOT " + operand(e.X, precNot)
	}
	// The space keeps "- -x" from lexing as a comment.
	return e.Op + " " + operand(e.X, precUnary)
}

// Binary is an infix operator: OR, AND, comparisons, arithmetic, ||.
type Binary struct {
	P    int
	Op   string
	L, R Expr
}

// Pos returns the position of the Binary.
func (e *Binary) Pos() int { return e.P }
func (*Binary) expr()      {}

// String prints the Binary as SQL.
func (e *Binary) String() string {
	p := binaryPrec(e.Op)
	left := p // left-associative: the left operand may be the same operator
	if p == precCmp {
		left = p + 1 // comparisons do not associate
	}
	return operand(e.L, left) + " " + e.Op + " " + operand(e.R, p+1)
}

// IsTest is what an IS test checks.
type IsTest uint8

// IS tests.
const (
	IsNull IsTest = iota
	IsTrue
	IsFalse
	IsUnknown
)

var isNames = [...]string{IsNull: "NULL", IsTrue: "TRUE", IsFalse: "FALSE", IsUnknown: "UNKNOWN"}

// Is is x IS [NOT] NULL/TRUE/FALSE/UNKNOWN.
type Is struct {
	P    int
	X    Expr
	Not  bool
	Test IsTest
}

// Pos returns the position of the Is.
func (e *Is) Pos() int { return e.P }
func (*Is) expr()      {}

// String prints the Is as SQL.
func (e *Is) String() string {
	not := ""
	if e.Not {
		not = "NOT "
	}
	return operand(e.X, precIs) + " IS " + not + isNames[e.Test]
}

// IsDistinct is l IS [NOT] DISTINCT FROM r.
type IsDistinct struct {
	P    int
	L, R Expr
	Not  bool
}

// Pos returns the position of the IsDistinct.
func (e *IsDistinct) Pos() int { return e.P }
func (*IsDistinct) expr()      {}

// String prints the IsDistinct as SQL.
func (e *IsDistinct) String() string {
	not := ""
	if e.Not {
		not = "NOT "
	}
	return operand(e.L, precIs) + " IS " + not + "DISTINCT FROM " + operand(e.R, precCmp)
}

// Between is x [NOT] BETWEEN lo AND hi.
type Between struct {
	P         int
	X, Lo, Hi Expr
	Not       bool
}

// Pos returns the position of the Between.
func (e *Between) Pos() int { return e.P }
func (*Between) expr()      {}

// String prints the Between as SQL.
func (e *Between) String() string {
	not := ""
	if e.Not {
		not = "NOT "
	}
	return operand(e.X, precOther) + " " + not + "BETWEEN " + operand(e.Lo, precOther) + " AND " + operand(e.Hi, precOther)
}

// In is x [NOT] IN (list).
type In struct {
	P    int
	X    Expr
	List []Expr
	Not  bool
}

// Pos returns the position of the In.
func (e *In) Pos() int { return e.P }
func (*In) expr()      {}

// String prints the In as SQL.
func (e *In) String() string {
	not := ""
	if e.Not {
		not = "NOT "
	}
	return operand(e.X, precOther) + " " + not + "IN (" + join(e.List, ", ") + ")"
}

// Like is x [NOT] LIKE/ILIKE pattern.
type Like struct {
	P               int
	X, Pattern      Expr
	Not             bool
	CaseInsensitive bool
}

// Pos returns the position of the Like.
func (e *Like) Pos() int { return e.P }
func (*Like) expr()      {}

// String prints the Like as SQL.
func (e *Like) String() string {
	op := "LIKE"
	if e.CaseInsensitive {
		op = "ILIKE"
	}
	if e.Not {
		op = "NOT " + op
	}
	return operand(e.X, precOther) + " " + op + " " + operand(e.Pattern, precOther)
}

// Cast is CAST(x AS type), also written x::type or type 'literal'.
type Cast struct {
	P    int
	X    Expr
	Type Type
}

// Pos returns the position of the Cast.
func (e *Cast) Pos() int { return e.P }
func (*Cast) expr()      {}

// String prints the Cast as SQL.
func (e *Cast) String() string { return "CAST(" + e.X.String() + " AS " + e.Type.String() + ")" }

// FuncCall is name(args).
type FuncCall struct {
	P    int
	Name string
	Args []Expr
}

// Pos returns the position of the FuncCall.
func (e *FuncCall) Pos() int { return e.P }
func (*FuncCall) expr()      {}

// String prints the FuncCall as SQL.
func (e *FuncCall) String() string {
	return QuoteIdent(e.Name) + "(" + join(e.Args, ", ") + ")"
}

// When is one WHEN ... THEN ... of a CASE.
type When struct {
	Cond, Result Expr
}

// Pos returns the position of the When.
func (w *When) Pos() int { return w.Cond.Pos() }

// String prints the When as SQL.
func (w *When) String() string { return "WHEN " + w.Cond.String() + " THEN " + w.Result.String() }

// Case is CASE [operand] WHEN ... THEN ... [ELSE ...] END.
type Case struct {
	P       int
	Operand Expr // nil for a searched CASE
	Whens   []*When
	Else    Expr // nil if none
}

// Pos returns the position of the Case.
func (e *Case) Pos() int { return e.P }
func (*Case) expr()      {}

// String prints the Case as SQL.
func (e *Case) String() string {
	s := "CASE "
	if e.Operand != nil {
		s += e.Operand.String() + " "
	}
	s += join(e.Whens, " ")
	if e.Else != nil {
		s += " ELSE " + e.Else.String()
	}
	return s + " END"
}
