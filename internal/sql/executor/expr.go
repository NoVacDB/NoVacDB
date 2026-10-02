package executor

import (
	"unicode/utf8"

	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/types"
)

// evalCtx is what expressions see besides the row: the statement's start
// time, for now().
type evalCtx struct {
	now int64 // microseconds since 2000-01-01 UTC
}

// node is a bound, typed expression. eval returns a value of typ(), or a
// NULL of it; only an untyped literal that no context has resolved has type
// Unknown, and the binder resolves those before evaluation.
type node interface {
	typ() types.Type
	eval(ec *evalCtx, row []types.Value) (types.Value, error)
}

// constNode is a constant.
type constNode struct{ v types.Value }

func (n *constNode) typ() types.Type                                   { return n.v.T }
func (n *constNode) eval(*evalCtx, []types.Value) (types.Value, error) { return n.v, nil }

// colNode reads a column of the current row.
type colNode struct {
	idx int
	t   types.Type
}

func (n *colNode) typ() types.Type { return n.t }
func (n *colNode) eval(_ *evalCtx, row []types.Value) (types.Value, error) {
	return row[n.idx], nil
}

// castNode converts its operand, as an explicit, assignment or implicit
// cast does.
type castNode struct {
	x  node
	to types.Type
}

func (n *castNode) typ() types.Type { return n.to }
func (n *castNode) eval(ec *evalCtx, row []types.Value) (types.Value, error) {
	v, err := n.x.eval(ec, row)
	if err != nil {
		return types.Value{}, err
	}
	return types.Cast(v, n.to)
}

// arithNode is + - * / % ^ on operands already of its type.
type arithNode struct {
	op   string
	l, r node
	t    types.Type
}

func (n *arithNode) typ() types.Type { return n.t }
func (n *arithNode) eval(ec *evalCtx, row []types.Value) (types.Value, error) {
	a, err := n.l.eval(ec, row)
	if err != nil {
		return types.Value{}, err
	}
	b, err := n.r.eval(ec, row)
	if err != nil {
		return types.Value{}, err
	}
	if a.Null || b.Null {
		return types.Null(n.t), nil
	}
	switch n.op {
	case "+":
		return types.Add(a, b)
	case "-":
		return types.Sub(a, b)
	case "*":
		return types.Mul(a, b)
	case "/":
		return types.Div(a, b)
	case "%":
		return types.Mod(a, b)
	}
	return types.Pow(a, b)
}

// negNode is unary minus.
type negNode struct{ x node }

func (n *negNode) typ() types.Type { return n.x.typ() }
func (n *negNode) eval(ec *evalCtx, row []types.Value) (types.Value, error) {
	v, err := n.x.eval(ec, row)
	if err != nil || v.Null {
		return v, err
	}
	return types.Neg(v)
}

// concatNode is || on text operands.
type concatNode struct{ l, r node }

func (n *concatNode) typ() types.Type { return types.Text }
func (n *concatNode) eval(ec *evalCtx, row []types.Value) (types.Value, error) {
	a, err := n.l.eval(ec, row)
	if err != nil {
		return types.Value{}, err
	}
	b, err := n.r.eval(ec, row)
	if err != nil {
		return types.Value{}, err
	}
	if a.Null || b.Null {
		return types.Null(types.Text), nil
	}
	return types.Concat(a, b), nil
}

// cmpNode compares operands of one type.
type cmpNode struct {
	op   string // = <> < <= > >=
	l, r node
}

func (n *cmpNode) typ() types.Type { return types.Bool }
func (n *cmpNode) eval(ec *evalCtx, row []types.Value) (types.Value, error) {
	a, err := n.l.eval(ec, row)
	if err != nil {
		return types.Value{}, err
	}
	b, err := n.r.eval(ec, row)
	if err != nil {
		return types.Value{}, err
	}
	if a.Null || b.Null {
		return types.Null(types.Bool), nil
	}
	return types.NewBool(cmpHolds(n.op, types.Compare(a, b))), nil
}

func cmpHolds(op string, c int) bool {
	switch op {
	case "=":
		return c == 0
	case "<>":
		return c != 0
	case "<":
		return c < 0
	case "<=":
		return c <= 0
	case ">":
		return c > 0
	}
	return c >= 0
}

// distinctNode is IS [NOT] DISTINCT FROM: NULLs are equal to each other.
type distinctNode struct {
	l, r node
	not  bool
}

func (n *distinctNode) typ() types.Type { return types.Bool }
func (n *distinctNode) eval(ec *evalCtx, row []types.Value) (types.Value, error) {
	a, err := n.l.eval(ec, row)
	if err != nil {
		return types.Value{}, err
	}
	b, err := n.r.eval(ec, row)
	if err != nil {
		return types.Value{}, err
	}
	var distinct bool
	switch {
	case a.Null || b.Null:
		distinct = a.Null != b.Null
	default:
		distinct = types.Compare(a, b) != 0
	}
	return types.NewBool(distinct != n.not), nil
}

// andNode, orNode and notNode are three-valued logic.
type andNode struct{ l, r node }

func (n *andNode) typ() types.Type { return types.Bool }
func (n *andNode) eval(ec *evalCtx, row []types.Value) (types.Value, error) {
	a, err := n.l.eval(ec, row)
	if err != nil {
		return types.Value{}, err
	}
	// Left to right, stopping at FALSE, as PostgreSQL's executor does:
	// "x <> 0 AND 10 / x > 1" never divides by zero.
	if !a.Null && !a.Bool() {
		return a, nil
	}
	b, err := n.r.eval(ec, row)
	if err != nil {
		return types.Value{}, err
	}
	return types.And(a, b), nil
}

type orNode struct{ l, r node }

func (n *orNode) typ() types.Type { return types.Bool }
func (n *orNode) eval(ec *evalCtx, row []types.Value) (types.Value, error) {
	a, err := n.l.eval(ec, row)
	if err != nil {
		return types.Value{}, err
	}
	if !a.Null && a.Bool() {
		return a, nil
	}
	b, err := n.r.eval(ec, row)
	if err != nil {
		return types.Value{}, err
	}
	return types.Or(a, b), nil
}

type notNode struct{ x node }

func (n *notNode) typ() types.Type { return types.Bool }
func (n *notNode) eval(ec *evalCtx, row []types.Value) (types.Value, error) {
	v, err := n.x.eval(ec, row)
	if err != nil {
		return types.Value{}, err
	}
	return types.Not(v), nil
}

// isNode is IS [NOT] NULL / TRUE / FALSE / UNKNOWN; never NULL.
type isNode struct {
	x    node
	test int // isNull, isTrue, isFalse
	not  bool
}

const (
	isNull = iota
	isTrue
	isFalse
)

func (n *isNode) typ() types.Type { return types.Bool }
func (n *isNode) eval(ec *evalCtx, row []types.Value) (types.Value, error) {
	v, err := n.x.eval(ec, row)
	if err != nil {
		return types.Value{}, err
	}
	var r bool
	switch n.test {
	case isNull:
		r = v.Null
	case isTrue:
		r = !v.Null && v.Bool()
	default:
		r = !v.Null && !v.Bool()
	}
	return types.NewBool(r != n.not), nil
}

// likeNode is [NOT] LIKE / ILIKE on text.
type likeNode struct {
	x, pat node
	not    bool
	ci     bool
}

func (n *likeNode) typ() types.Type { return types.Bool }
func (n *likeNode) eval(ec *evalCtx, row []types.Value) (types.Value, error) {
	a, err := n.x.eval(ec, row)
	if err != nil {
		return types.Value{}, err
	}
	p, err := n.pat.eval(ec, row)
	if err != nil {
		return types.Value{}, err
	}
	if a.Null || p.Null {
		return types.Null(types.Bool), nil
	}
	ok, err := types.Like(a.S, p.S, n.ci)
	if err != nil {
		return types.Value{}, err
	}
	return types.NewBool(ok != n.not), nil
}

// caseNode is a searched CASE (a simple CASE is bound into one).
type caseNode struct {
	conds, results []node
	els            node // nil: NULL
	t              types.Type
}

func (n *caseNode) typ() types.Type { return n.t }
func (n *caseNode) eval(ec *evalCtx, row []types.Value) (types.Value, error) {
	for i, c := range n.conds {
		v, err := c.eval(ec, row)
		if err != nil {
			return types.Value{}, err
		}
		if !v.Null && v.Bool() {
			return n.results[i].eval(ec, row)
		}
	}
	if n.els == nil {
		return types.Null(n.t), nil
	}
	return n.els.eval(ec, row)
}

// funcNode is a built-in function.
type funcNode struct {
	name string
	args []node
	t    types.Type
}

func (n *funcNode) typ() types.Type { return n.t }
func (n *funcNode) eval(ec *evalCtx, row []types.Value) (types.Value, error) {
	switch n.name {
	case "now":
		return types.NewTimestampTZ(ec.now), nil
	case "coalesce":
		for _, a := range n.args {
			v, err := a.eval(ec, row)
			if err != nil || !v.Null {
				return v, err
			}
		}
		return types.Null(n.t), nil
	}
	vals := make([]types.Value, len(n.args))
	for i, a := range n.args {
		v, err := a.eval(ec, row)
		if err != nil {
			return types.Value{}, err
		}
		vals[i] = v
	}
	switch n.name {
	case "greatest", "least":
		var best types.Value
		found := false
		for _, v := range vals {
			if v.Null {
				continue
			}
			c := 0
			if found {
				c = types.Compare(v, best)
			}
			if !found || (n.name == "greatest" && c > 0) || (n.name == "least" && c < 0) {
				best, found = v, true
			}
		}
		if !found {
			return types.Null(n.t), nil
		}
		return best, nil
	case "nullif":
		a, b := vals[0], vals[1]
		if !a.Null && !b.Null && types.Compare(a, b) == 0 {
			return types.Null(n.t), nil
		}
		return a, nil
	}
	v := vals[0]
	if v.Null {
		return types.Null(n.t), nil
	}
	switch n.name {
	case "lower":
		return types.NewText(types.Lower(v.S)), nil
	case "upper":
		return types.NewText(types.Upper(v.S)), nil
	case "length":
		return types.NewInt4(int32(utf8.RuneCountInString(v.S))), nil
	case "abs":
		return types.Abs(v)
	}
	return types.Value{}, sqlerr.New(sqlerr.InternalError, "unknown function %s", n.name)
}
