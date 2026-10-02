package executor

import (
	"sort"
	"strconv"

	"github.com/vikrant-choudhary06/NoVacDB/internal/catalog"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/ast"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/types"
	"github.com/vikrant-choudhary06/NoVacDB/internal/storage"
)

// MaxSelectColumns is PostgreSQL's limit on a SELECT's output columns
// (MaxTupleAttributeNumber). It also keeps a result's column count inside
// the protocol's 16-bit field.
const MaxSelectColumns = 1664

// table looks up a table named in a statement.
func (st *stmt) table(name ast.Name) (*catalog.Table, error) {
	if t, ok := st.db.cat.Table(name.Name); ok {
		return t, nil
	}
	if _, ok := st.db.cat.Index(name.Name); ok {
		return nil, sqlerr.New(sqlerr.WrongObjectType, "%q is an index", name.Name).At(st.sql, name.P)
	}
	return nil, sqlerr.New(sqlerr.UndefinedTable, "relation %q does not exist", name.Name).At(st.sql, name.P)
}

// rowBinder returns a binder over a table's columns, under its alias if
// it has one.
func (st *stmt) rowBinder(tbl *catalog.Table, ref *ast.TableRef) *binder {
	b := &binder{sql: st.sql, table: tbl, name: ref.Name.Name, params: st.params}
	if ref.Alias.Name != "" {
		b.name = ref.Alias.Name
	}
	return b
}

// where binds a WHERE clause; nil means none.
func (b *binder) where(e ast.Expr) (node, error) {
	if e == nil {
		return nil, nil
	}
	n, err := b.bind(e)
	if err != nil {
		return nil, err
	}
	return b.boolean(n, e.Pos(), "WHERE")
}

// sortKey is one ORDER BY item: an output column (out >= 0) or an
// expression over the input row.
type sortKey struct {
	out        int
	n          node
	desc       bool
	nullsFirst bool
}

func (st *stmt) selectStmt(s *ast.Select) (*Result, error) {
	b := st.binder()
	var tbl *catalog.Table
	if s.From != nil {
		var err error
		if tbl, err = st.table(s.From.Name); err != nil {
			return nil, err
		}
		b = st.rowBinder(tbl, s.From)
	}

	// Output columns, at most MaxSelectColumns of them.
	var outs []node
	cols := []Column{} // non-nil even for an empty select list
	var exprs []string // each output's expression, to match ORDER BY under DISTINCT
	for _, t := range s.Targets {
		if len(cols) >= MaxSelectColumns || t.Star && tbl != nil && len(cols)+len(tbl.Columns) > MaxSelectColumns {
			return nil, sqlerr.New(sqlerr.TooManyColumns, "target lists can have at most %d entries", MaxSelectColumns).At(st.sql, t.P)
		}
		if t.Star {
			if tbl == nil {
				return nil, sqlerr.New(sqlerr.SyntaxError, "SELECT * with no tables specified is not valid").At(st.sql, t.P)
			}
			if t.StarTable != "" && t.StarTable != b.name {
				return nil, b.badQualifier(t.StarTable).At(st.sql, t.P)
			}
			for i, c := range tbl.Columns {
				outs = append(outs, &colNode{idx: i, t: c.Type})
				cols = append(cols, Column{c.Name, c.Type})
				exprs = append(exprs, (&ast.ColumnRef{Column: c.Name}).String())
			}
			continue
		}
		n, err := b.bind(t.Expr)
		if err != nil {
			return nil, err
		}
		if n, err = b.resolved(n, t.Expr.Pos()); err != nil {
			return nil, err
		}
		name := outputName(t.Expr)
		if t.Alias.Name != "" {
			name = t.Alias.Name
		}
		outs = append(outs, n)
		cols = append(cols, Column{name, n.typ()})
		exprs = append(exprs, t.Expr.String())
	}

	where, err := b.where(s.Where)
	if err != nil {
		return nil, err
	}

	// ORDER BY: an output column number, an output column name, or an
	// expression over the input (which DISTINCT requires to be an output).
	var keys []sortKey
	for _, o := range s.OrderBy {
		k := sortKey{out: -1, desc: o.Desc, nullsFirst: o.Desc}
		switch o.Nulls {
		case ast.NullsFirst:
			k.nullsFirst = true
		case ast.NullsLast:
			k.nullsFirst = false
		}
		out, err := st.orderTarget(o, s, cols, exprs)
		if err != nil {
			return nil, err
		}
		if out >= 0 {
			k.out = out
		} else {
			if s.Distinct {
				return nil, sqlerr.New(sqlerr.InvalidColumnReference, "for SELECT DISTINCT, ORDER BY expressions must appear in select list").At(st.sql, o.Pos())
			}
			n, err := b.bind(o.Expr)
			if err != nil {
				return nil, err
			}
			if k.n, err = b.resolved(n, o.Expr.Pos()); err != nil {
				return nil, err
			}
		}
		keys = append(keys, k)
	}

	limit, offset, err := st.limits(s)
	if err != nil {
		return nil, err
	}
	if st.describe {
		return &Result{Columns: cols}, nil
	}

	// Run: scan and filter, project and compute sort keys, then DISTINCT,
	// sort, OFFSET and LIMIT. Without ORDER BY or DISTINCT the scan stops
	// as soon as LIMIT is satisfied.
	type outRow struct {
		vals, keys []types.Value
	}
	var rows []outRow
	var seen map[string]bool
	if s.Distinct {
		seen = map[string]bool{}
	}
	// Without ORDER BY the scan can stop once LIMIT rows are out; DISTINCT
	// drops duplicates before they count.
	streaming := len(keys) == 0
	emit := func(_ storage.RID, in []types.Value) (bool, error) {
		r := outRow{vals: make([]types.Value, len(outs))}
		for i, n := range outs {
			v, err := n.eval(st.ec, in)
			if err != nil {
				return false, err
			}
			r.vals[i] = v
		}
		if seen != nil {
			k := distinctKey(r.vals)
			if seen[k] {
				return true, nil
			}
			seen[k] = true
		}
		if len(keys) > 0 {
			r.keys = make([]types.Value, len(keys))
			for i, k := range keys {
				var v types.Value
				var err error
				if k.out >= 0 {
					v = r.vals[k.out]
				} else if v, err = k.n.eval(st.ec, in); err != nil {
					return false, err
				}
				r.keys[i] = v
			}
		}
		rows = append(rows, r)
		return !streaming || limit < 0 || int64(len(rows))-offset < limit, nil
	}
	if tbl == nil {
		if where != nil {
			v, err := where.eval(st.ec, nil)
			if err != nil {
				return nil, err
			}
			if v.Null || !v.Bool() {
				return &Result{Tag: "SELECT 0", Columns: cols}, nil
			}
		}
		if _, err := emit(storage.RID{}, nil); err != nil {
			return nil, err
		}
	} else {
		a := st.access(tbl, where)
		if limit != 0 || !streaming {
			if err := st.scan(tbl, a, where, emit); err != nil {
				return nil, err
			}
		}
	}
	if len(keys) > 0 {
		sort.SliceStable(rows, func(i, j int) bool {
			for k, key := range keys {
				if c := compareSort(rows[i].keys[k], rows[j].keys[k], key); c != 0 {
					return c < 0
				}
			}
			return false
		})
	}
	res := &Result{Columns: cols}
	for i, r := range rows {
		if int64(i) < offset {
			continue
		}
		if limit >= 0 && int64(len(res.Rows)) >= limit {
			break
		}
		res.Rows = append(res.Rows, r.vals)
	}
	res.Tag = "SELECT " + strconv.Itoa(len(res.Rows))
	return res, nil
}

// orderTarget finds the output column an ORDER BY item names, by number or
// by name, or, under DISTINCT, by being the same expression; -1 if none.
func (st *stmt) orderTarget(o *ast.OrderItem, s *ast.Select, cols []Column, exprs []string) (int, error) {
	switch e := o.Expr.(type) {
	case *ast.IntegerLit:
		if e.Value < 1 || e.Value > int64(len(cols)) {
			return 0, sqlerr.New(sqlerr.InvalidColumnReference, "ORDER BY position %d is not in select list", e.Value).At(st.sql, e.P)
		}
		return int(e.Value - 1), nil
	case *ast.ColumnRef:
		if e.Table == "" {
			found := -1
			for i, c := range cols {
				if c.Name != e.Column {
					continue
				}
				if found >= 0 && exprs[found] != exprs[i] {
					return 0, sqlerr.New(sqlerr.AmbiguousColumn, "ORDER BY %q is ambiguous", e.Column).At(st.sql, e.P)
				}
				if found < 0 {
					found = i
				}
			}
			if found >= 0 {
				return found, nil
			}
		}
	}
	if s.Distinct {
		text := o.Expr.String()
		for i, x := range exprs {
			if x == text {
				return i, nil
			}
		}
	}
	return -1, nil
}

// compareSort orders two sort-key values: NULLs first or last as asked,
// then by value, reversed for DESC.
func compareSort(a, b types.Value, k sortKey) int {
	switch {
	case a.Null && b.Null:
		return 0
	case a.Null:
		if k.nullsFirst {
			return -1
		}
		return 1
	case b.Null:
		if k.nullsFirst {
			return 1
		}
		return -1
	}
	c := types.Compare(a, b)
	if k.desc {
		c = -c
	}
	return c
}

// distinctKey encodes a row so that rows DISTINCT treats as equal (NULLs
// equal, -0 = +0, NaN = NaN) have equal keys.
func distinctKey(vals []types.Value) string {
	var k []byte
	for _, v := range vals {
		k = types.AppendKey(k, v)
	}
	return string(k)
}

// limits evaluates LIMIT and OFFSET: -1 means no limit.
func (st *stmt) limits(s *ast.Select) (limit, offset int64, err error) {
	limit = -1
	eval := func(e ast.Expr, what string, code string) (int64, bool, error) {
		b := &binder{sql: st.sql, params: st.params, noVars: func() *sqlerr.Error {
			return sqlerr.New(sqlerr.InvalidColumnReference, "argument of %s must not contain variables", what)
		}}
		n, err := b.bind(e)
		if err != nil {
			return 0, false, err
		}
		if n, err = b.coerce(n, e.Pos(), types.Int8); err != nil {
			return 0, false, err
		}
		if !n.typ().IsNumeric() {
			return 0, false, sqlerr.New(sqlerr.DatatypeMismatch, "argument of %s must be type bigint, not type %s", what, n.typ()).At(st.sql, e.Pos())
		}
		if st.describe {
			return 0, false, nil // a parameter has no value yet
		}
		v, err := (&castNode{x: n, to: types.Int8}).eval(st.ec, nil)
		if err != nil || v.Null {
			return 0, false, err
		}
		if v.I < 0 {
			return 0, false, sqlerr.New(code, "%s must not be negative", what)
		}
		return v.I, true, nil
	}
	if s.Limit != nil {
		v, ok, err := eval(s.Limit, "LIMIT", sqlerr.InvalidRowCountInLimit)
		if err != nil {
			return 0, 0, err
		}
		if ok {
			limit = v
		}
	}
	if s.Offset != nil {
		v, _, err := eval(s.Offset, "OFFSET", sqlerr.InvalidRowCountInOffset)
		if err != nil {
			return 0, 0, err
		}
		offset = v
	}
	return limit, offset, nil
}
