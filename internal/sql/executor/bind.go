package executor

import (
	"strings"

	"github.com/vikrant-choudhary06/NoVacDB/internal/catalog"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/ast"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/types"
)

// binder turns syntax trees into typed nodes. See
// docs/design/10-executor.md sections 2.1 and 2.6.
type binder struct {
	sql string // the query text, for error positions; "" for none
	// The columns in scope: a table, under its name or alias. table is
	// nil where no columns are visible.
	table *catalog.Table
	name  string
	// noVars, if set, makes a column reference this error (LIMIT,
	// DEFAULT expressions).
	noVars func() *sqlerr.Error
	// params are the statement's parameters; nil where there are none
	// (stored defaults, DDL).
	params *params
}

// at positions err at a byte offset of the query.
func (b *binder) at(err *sqlerr.Error, pos int) *sqlerr.Error {
	if b.sql == "" {
		return err
	}
	return err.At(b.sql, pos)
}

const castHint = "No operator matches the given name and argument types. You might need to add explicit type casts."

func (b *binder) noOperator(pos int, op string, l, r types.Type) *sqlerr.Error {
	return b.at(sqlerr.New(sqlerr.UndefinedFunction, "operator does not exist: %s %s %s", l, op, r).WithHint(castHint), pos)
}

// literal reports whether n is an untyped literal ('...' or NULL).
func unknownLit(n node) bool { return n.typ() == types.Unknown }

// coerce resolves an untyped literal to type t: NULL becomes t's NULL, a
// string is read with t's input function, now, as PostgreSQL does.
func (b *binder) coerce(n node, pos int, t types.Type) (node, error) {
	if !unknownLit(n) {
		return n, nil
	}
	if p, ok := n.(*paramNode); ok {
		p.p.types[p.n-1] = t // inferred from the context, as PostgreSQL does
		return p, nil
	}
	c := n.(*constNode)
	if c.v.Null {
		return &constNode{types.Null(t)}, nil
	}
	v, err := types.Parse(c.v.S, t)
	if err != nil {
		return nil, b.at(sqlerr.From(err), pos)
	}
	return &constNode{v}, nil
}

// widen converts n to the numeric type t if it is not already.
func widen(n node, t types.Type) node {
	if n.typ() == t {
		return n
	}
	return &castNode{x: n, to: t}
}

func numericMax(a, b types.Type) types.Type {
	if a == types.Float8 || b == types.Float8 {
		return types.Float8
	}
	if a == types.Int8 || b == types.Int8 {
		return types.Int8
	}
	return types.Int4
}

// pair resolves two operands to one type for a comparison-like operator:
// untyped literals take the other side's type (text if both are untyped),
// and integers and doubles widen. Anything else has no operator.
func (b *binder) pair(l, r node, lpos, rpos, pos int, op string) (node, node, types.Type, error) {
	lt, rt := l.typ(), r.typ()
	var t types.Type
	switch {
	case lt == types.Unknown && rt == types.Unknown:
		t = types.Text
	case lt == types.Unknown:
		t = rt
	case rt == types.Unknown:
		t = lt
	case lt == rt:
		t = lt
	case lt.IsNumeric() && rt.IsNumeric():
		t = numericMax(lt, rt)
	default:
		return nil, nil, 0, b.noOperator(pos, op, lt, rt)
	}
	var err error
	if l, err = b.coerce(l, lpos, t); err != nil {
		return nil, nil, 0, err
	}
	if r, err = b.coerce(r, rpos, t); err != nil {
		return nil, nil, 0, err
	}
	if t.IsNumeric() {
		l, r = widen(l, t), widen(r, t)
	}
	return l, r, t, nil
}

// common resolves several expressions to one type (CASE results,
// COALESCE, GREATEST, LEAST): the type of the typed ones, all equal or all
// numeric (widened), text if all are untyped.
func (b *binder) common(ns []node, poss []int, what string) ([]node, types.Type, error) {
	t := types.Unknown
	for i, n := range ns {
		nt := n.typ()
		switch {
		case nt == types.Unknown:
		case t == types.Unknown || t == nt:
			t = nt
		case t.IsNumeric() && nt.IsNumeric():
			t = numericMax(t, nt)
		default:
			return nil, 0, b.at(sqlerr.New(sqlerr.DatatypeMismatch, "%s types %s and %s cannot be matched", what, t, nt), poss[i])
		}
	}
	if t == types.Unknown {
		t = types.Text
	}
	out := make([]node, len(ns))
	for i, n := range ns {
		c, err := b.coerce(n, poss[i], t)
		if err != nil {
			return nil, 0, err
		}
		if t.IsNumeric() {
			c = widen(c, t)
		}
		out[i] = c
	}
	return out, t, nil
}

// boolean requires a boolean operand for what (AND, WHERE, ...).
func (b *binder) boolean(n node, pos int, what string) (node, error) {
	n, err := b.coerce(n, pos, types.Bool)
	if err != nil {
		return nil, err
	}
	if n.typ() != types.Bool {
		return nil, b.at(sqlerr.New(sqlerr.DatatypeMismatch, "argument of %s must be type boolean, not type %s", what, n.typ()), pos)
	}
	return n, nil
}

// resolved gives an untyped literal that nothing constrained type text,
// as PostgreSQL does for output columns and sort keys.
func (b *binder) resolved(n node, pos int) (node, error) {
	return b.coerce(n, pos, types.Text)
}

// assign converts n for storing into column col, as PostgreSQL's
// assignment casts do: untyped literals by their input function, numbers
// between numeric types (checked at run time), and anything to text.
func (b *binder) assign(n node, pos int, col *catalog.Column) (node, error) {
	t := n.typ()
	switch {
	case t == col.Type:
		return n, nil
	case t == types.Unknown:
		return b.coerce(n, pos, col.Type)
	case t.IsNumeric() && col.Type.IsNumeric(), col.Type == types.Text:
		return &castNode{x: n, to: col.Type}, nil
	}
	return nil, b.at(sqlerr.New(sqlerr.DatatypeMismatch, "column %q is of type %s but expression is of type %s", col.Name, col.Type, t).
		WithHint("You will need to rewrite or cast the expression."), pos)
}

// bind binds an expression.
func (b *binder) bind(e ast.Expr) (node, error) {
	switch e := e.(type) {
	case *ast.IntegerLit:
		if e.Value >= -1<<31 && e.Value < 1<<31 {
			return &constNode{types.NewInt4(int32(e.Value))}, nil
		}
		return &constNode{types.NewInt8(e.Value)}, nil
	case *ast.FloatLit:
		return &constNode{types.NewFloat8(e.Value)}, nil
	case *ast.StringLit:
		return &constNode{types.NewUnknown(e.Value)}, nil
	case *ast.BoolLit:
		return &constNode{types.NewBool(e.Value)}, nil
	case *ast.NullLit:
		return &constNode{types.Null(types.Unknown)}, nil
	case *ast.Default:
		return nil, b.at(sqlerr.New(sqlerr.SyntaxError, "DEFAULT is not allowed in this context"), e.P)
	case *ast.Param:
		return b.param(e)
	case *ast.ColumnRef:
		return b.column(e)
	case *ast.Unary:
		return b.unary(e)
	case *ast.Binary:
		return b.binary(e)
	case *ast.Is:
		return b.is(e)
	case *ast.IsDistinct:
		l, r, err := b.operands(e.L, e.R)
		if err != nil {
			return nil, err
		}
		l, r, _, err = b.pair(l, r, e.L.Pos(), e.R.Pos(), e.P, "=")
		if err != nil {
			return nil, err
		}
		return &distinctNode{l: l, r: r, not: e.Not}, nil
	case *ast.Between:
		return b.between(e)
	case *ast.In:
		return b.in(e)
	case *ast.Like:
		return b.like(e)
	case *ast.Cast:
		return b.cast(e)
	case *ast.FuncCall:
		return b.call(e)
	case *ast.Case:
		return b.caseExpr(e)
	}
	return nil, sqlerr.New(sqlerr.FeatureNotSupported, "unsupported expression %s", e)
}

func (b *binder) operands(l, r ast.Expr) (node, node, error) {
	ln, err := b.bind(l)
	if err != nil {
		return nil, nil, err
	}
	rn, err := b.bind(r)
	if err != nil {
		return nil, nil, err
	}
	return ln, rn, nil
}

func (b *binder) column(e *ast.ColumnRef) (node, error) {
	if b.noVars != nil {
		return nil, b.at(b.noVars(), e.P)
	}
	if e.Table != "" && (b.table == nil || e.Table != b.name) {
		return nil, b.at(b.badQualifier(e.Table), e.P)
	}
	if b.table != nil {
		if i := b.table.ColumnIndex(e.Column); i >= 0 {
			return &colNode{idx: i, t: b.table.Columns[i].Type}, nil
		}
	}
	name := ast.QuoteIdent(e.Column)
	if e.Table != "" {
		name = ast.QuoteIdent(e.Table) + "." + name
		return nil, b.at(sqlerr.New(sqlerr.UndefinedColumn, "column %s does not exist", name), e.P)
	}
	return nil, b.at(sqlerr.New(sqlerr.UndefinedColumn, "column %q does not exist", e.Column), e.P)
}

// badQualifier is the error for a column qualified by a name that is not
// the table's name in this query: PostgreSQL points out an alias that
// hides the table's own name.
func (b *binder) badQualifier(name string) *sqlerr.Error {
	if b.table != nil && name == b.table.Name {
		return sqlerr.New(sqlerr.UndefinedTable, "invalid reference to FROM-clause entry for table %q", name).
			WithHint("Perhaps you meant to reference the table alias %q.", b.name)
	}
	return sqlerr.New(sqlerr.UndefinedTable, "missing FROM-clause entry for table %q", name)
}

func (b *binder) unary(e *ast.Unary) (node, error) {
	x, err := b.bind(e.X)
	if err != nil {
		return nil, err
	}
	if e.Op == "NOT" {
		x, err := b.boolean(x, e.X.Pos(), "NOT")
		if err != nil {
			return nil, err
		}
		return &notNode{x}, nil
	}
	switch t := x.typ(); {
	case t == types.Unknown:
		return nil, b.at(sqlerr.New(sqlerr.AmbiguousFunction, "operator is not unique: %s unknown", e.Op).WithHint(castHint), e.P)
	case !t.IsNumeric():
		return nil, b.at(sqlerr.New(sqlerr.UndefinedFunction, "operator does not exist: %s %s", e.Op, t).WithHint(castHint), e.P)
	}
	if e.Op == "+" {
		return x, nil
	}
	return &negNode{x}, nil
}

func (b *binder) binary(e *ast.Binary) (node, error) {
	l, r, err := b.operands(e.L, e.R)
	if err != nil {
		return nil, err
	}
	lpos, rpos := e.L.Pos(), e.R.Pos()
	switch e.Op {
	case "AND", "OR":
		if l, err = b.boolean(l, lpos, e.Op); err != nil {
			return nil, err
		}
		if r, err = b.boolean(r, rpos, e.Op); err != nil {
			return nil, err
		}
		if e.Op == "AND" {
			return &andNode{l, r}, nil
		}
		return &orNode{l, r}, nil
	case "=", "<>", "<", "<=", ">", ">=":
		l, r, _, err := b.pair(l, r, lpos, rpos, e.P, e.Op)
		if err != nil {
			return nil, err
		}
		return &cmpNode{op: e.Op, l: l, r: r}, nil
	case "||":
		// An untyped literal is text; at least one side must be text, and
		// the other is converted to its text form.
		if l, err = b.coerce(l, lpos, types.Text); err != nil {
			return nil, err
		}
		if r, err = b.coerce(r, rpos, types.Text); err != nil {
			return nil, err
		}
		if l.typ() != types.Text && r.typ() != types.Text {
			return nil, b.noOperator(e.P, e.Op, l.typ(), r.typ())
		}
		if l.typ() != types.Text {
			l = &castNode{x: l, to: types.Text}
		}
		if r.typ() != types.Text {
			r = &castNode{x: r, to: types.Text}
		}
		return &concatNode{l, r}, nil
	}
	// Arithmetic.
	if unknownLit(l) && unknownLit(r) {
		return nil, b.at(sqlerr.New(sqlerr.AmbiguousFunction, "operator is not unique: unknown %s unknown", e.Op).WithHint(castHint), e.P)
	}
	l, r, t, err := b.pair(l, r, lpos, rpos, e.P, e.Op)
	if err != nil {
		return nil, err
	}
	if !t.IsNumeric() || (e.Op == "%" && !t.IsInteger()) {
		return nil, b.noOperator(e.P, e.Op, t, t)
	}
	if e.Op == "^" {
		t = types.Float8
		l, r = widen(l, t), widen(r, t)
	}
	return &arithNode{op: e.Op, l: l, r: r, t: t}, nil
}

func (b *binder) is(e *ast.Is) (node, error) {
	x, err := b.bind(e.X)
	if err != nil {
		return nil, err
	}
	switch e.Test {
	case ast.IsNull:
		return &isNode{x: x, test: isNull, not: e.Not}, nil
	case ast.IsUnknown:
		if x, err = b.boolean(x, e.X.Pos(), "IS UNKNOWN"); err != nil {
			return nil, err
		}
		return &isNode{x: x, test: isNull, not: e.Not}, nil
	case ast.IsTrue:
		if x, err = b.boolean(x, e.X.Pos(), "IS TRUE"); err != nil {
			return nil, err
		}
		return &isNode{x: x, test: isTrue, not: e.Not}, nil
	}
	if x, err = b.boolean(x, e.X.Pos(), "IS FALSE"); err != nil {
		return nil, err
	}
	return &isNode{x: x, test: isFalse, not: e.Not}, nil
}

// between is x >= lo AND x <= hi; NOT BETWEEN is its negation, which in
// three-valued logic equals x < lo OR x > hi.
func (b *binder) between(e *ast.Between) (node, error) {
	ge, err := b.binary(&ast.Binary{P: e.P, Op: ">=", L: e.X, R: e.Lo})
	if err != nil {
		return nil, err
	}
	le, err := b.binary(&ast.Binary{P: e.P, Op: "<=", L: e.X, R: e.Hi})
	if err != nil {
		return nil, err
	}
	var n node = &andNode{ge, le}
	if e.Not {
		n = &notNode{n}
	}
	return n, nil
}

// in is x = a OR x = b ...; NOT IN its negation. Three-valued OR gives
// exactly SQL's IN: TRUE if any matches, else NULL if any comparison is
// NULL, else FALSE.
func (b *binder) in(e *ast.In) (node, error) {
	var n node
	for _, item := range e.List {
		c, err := b.binary(&ast.Binary{P: e.P, Op: "=", L: e.X, R: item})
		if err != nil {
			return nil, err
		}
		if n == nil {
			n = c
		} else {
			n = &orNode{n, c}
		}
	}
	if e.Not {
		n = &notNode{n}
	}
	return n, nil
}

func (b *binder) like(e *ast.Like) (node, error) {
	x, p, err := b.operands(e.X, e.Pattern)
	if err != nil {
		return nil, err
	}
	if x, err = b.coerce(x, e.X.Pos(), types.Text); err != nil {
		return nil, err
	}
	if p, err = b.coerce(p, e.Pattern.Pos(), types.Text); err != nil {
		return nil, err
	}
	if x.typ() != types.Text || p.typ() != types.Text {
		op := "~~"
		if e.CaseInsensitive {
			op += "*"
		}
		if e.Not {
			op = "!" + op
		}
		return nil, b.noOperator(e.P, op, x.typ(), p.typ())
	}
	return &likeNode{x: x, pat: p, not: e.Not, ci: e.CaseInsensitive}, nil
}

func (b *binder) cast(e *ast.Cast) (node, error) {
	x, err := b.bind(e.X)
	if err != nil {
		return nil, err
	}
	to := types.FromAST(e.Type)
	if unknownLit(x) {
		return b.coerce(x, e.X.Pos(), to)
	}
	if !types.CanCast(x.typ(), to) {
		return nil, b.at(types.CannotCast(x.typ(), to), e.P)
	}
	if x.typ() == to {
		return x, nil
	}
	return &castNode{x: x, to: to}, nil
}

func (b *binder) noFunction(e *ast.FuncCall, args []node) *sqlerr.Error {
	names := make([]string, len(args))
	for i, a := range args {
		names[i] = a.typ().String()
	}
	return b.at(sqlerr.New(sqlerr.UndefinedFunction, "function %s(%s) does not exist", e.Name, strings.Join(names, ", ")).
		WithHint("No function matches the given name and argument types. You might need to add explicit type casts."), e.P)
}

func (b *binder) call(e *ast.FuncCall) (node, error) {
	args := make([]node, len(e.Args))
	poss := make([]int, len(e.Args))
	for i, a := range e.Args {
		n, err := b.bind(a)
		if err != nil {
			return nil, err
		}
		args[i], poss[i] = n, a.Pos()
	}
	name := strings.ToLower(e.Name)
	switch name {
	case "now":
		if len(args) != 0 {
			return nil, b.noFunction(e, args)
		}
		return &funcNode{name: name, t: types.TimestampTZ}, nil
	case "lower", "upper", "length":
		if len(args) != 1 {
			return nil, b.noFunction(e, args)
		}
		a, err := b.coerce(args[0], poss[0], types.Text)
		if err != nil {
			return nil, err
		}
		if a.typ() != types.Text {
			return nil, b.noFunction(e, args)
		}
		t := types.Text
		if name == "length" {
			t = types.Int4
		}
		return &funcNode{name: name, args: []node{a}, t: t}, nil
	case "abs":
		if len(args) != 1 {
			return nil, b.noFunction(e, args)
		}
		switch t := args[0].typ(); {
		case t == types.Unknown:
			return nil, b.at(sqlerr.New(sqlerr.AmbiguousFunction, "function abs(unknown) is not unique").
				WithHint("Could not choose a best candidate function. You might need to add explicit type casts."), e.P)
		case !t.IsNumeric():
			return nil, b.noFunction(e, args)
		}
		return &funcNode{name: name, args: args, t: args[0].typ()}, nil
	case "coalesce", "greatest", "least":
		if len(args) == 0 {
			return nil, b.noFunction(e, args)
		}
		ns, t, err := b.common(args, poss, strings.ToUpper(name))
		if err != nil {
			return nil, err
		}
		return &funcNode{name: name, args: ns, t: t}, nil
	case "nullif":
		if len(args) != 2 {
			return nil, b.noFunction(e, args)
		}
		l, r, t, err := b.pair(args[0], args[1], poss[0], poss[1], e.P, "=")
		if err != nil {
			return nil, err
		}
		return &funcNode{name: name, args: []node{l, r}, t: t}, nil
	}
	return nil, b.noFunction(e, args)
}

func (b *binder) caseExpr(e *ast.Case) (node, error) {
	var operand node
	if e.Operand != nil {
		var err error
		if operand, err = b.bind(e.Operand); err != nil {
			return nil, err
		}
	}
	n := &caseNode{}
	var results []node
	var poss []int
	for _, w := range e.Whens {
		var cond node
		var err error
		if operand != nil {
			// A simple CASE compares the operand with each value.
			v, err := b.bind(w.Cond)
			if err != nil {
				return nil, err
			}
			l, r, _, err := b.pair(operand, v, e.Operand.Pos(), w.Cond.Pos(), w.Cond.Pos(), "=")
			if err != nil {
				return nil, err
			}
			cond = &cmpNode{op: "=", l: l, r: r}
		} else {
			if cond, err = b.bind(w.Cond); err != nil {
				return nil, err
			}
			if cond, err = b.boolean(cond, w.Cond.Pos(), "CASE/WHEN"); err != nil {
				return nil, err
			}
		}
		res, err := b.bind(w.Result)
		if err != nil {
			return nil, err
		}
		n.conds = append(n.conds, cond)
		results = append(results, res)
		poss = append(poss, w.Result.Pos())
	}
	if e.Else != nil {
		els, err := b.bind(e.Else)
		if err != nil {
			return nil, err
		}
		results = append(results, els)
		poss = append(poss, e.Else.Pos())
	}
	ns, t, err := b.common(results, poss, "CASE")
	if err != nil {
		return nil, err
	}
	n.t = t
	n.results = ns[:len(e.Whens)]
	if e.Else != nil {
		n.els = ns[len(e.Whens)]
	}
	return n, nil
}

// outputName is PostgreSQL's name for an output column without an alias.
func outputName(e ast.Expr) string {
	if n := figureName(e); n != "" {
		return n
	}
	return "?column?"
}

func figureName(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.ColumnRef:
		return e.Column
	case *ast.FuncCall:
		return strings.ToLower(e.Name)
	case *ast.Case:
		return "case"
	case *ast.BoolLit:
		return "bool" // TRUE is the constant 'true'::boolean
	case *ast.Cast:
		if n := figureName(e.X); n != "" {
			return n
		}
		return pgTypeName[e.Type]
	}
	return ""
}

var pgTypeName = map[ast.Type]string{
	ast.TypeInteger: "int4", ast.TypeBigInt: "int8", ast.TypeDouble: "float8",
	ast.TypeText: "text", ast.TypeBoolean: "bool", ast.TypeTimestampTZ: "timestamptz",
}
