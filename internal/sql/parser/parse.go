package parser

import (
	"math"
	"strconv"
	"strings"

	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/ast"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/keyword"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
)

// MaxDepth is the deepest expression nesting accepted.
const MaxDepth = 1000

// Parse parses a semicolon-separated list of statements. Empty statements
// are dropped, so blank or comment-only text gives an empty list.
func Parse(sql string) ([]ast.Stmt, error) {
	toks, err := Lex(sql)
	if err != nil {
		return nil, err
	}
	p := &parser{sql: sql, toks: toks}
	var stmts []ast.Stmt
	for {
		for p.peek().Kind == Semicolon {
			p.next()
		}
		if p.peek().Kind == EOF {
			return stmts, nil
		}
		s, err := p.parseStmt()
		if err != nil {
			return nil, err
		}
		stmts = append(stmts, s)
		if t := p.peek(); t.Kind != Semicolon && t.Kind != EOF {
			return nil, p.syntaxErr(t)
		}
	}
}

// ParseExpr parses a single expression, for tests and tools.
func ParseExpr(sql string) (ast.Expr, error) {
	toks, err := Lex(sql)
	if err != nil {
		return nil, err
	}
	p := &parser{sql: sql, toks: toks}
	e, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.Kind != EOF {
		return nil, p.syntaxErr(t)
	}
	return e, nil
}

type parser struct {
	sql   string
	toks  []Token
	p     int
	depth int
}

func (p *parser) peek() Token { return p.toks[p.p] }

func (p *parser) peekAt(n int) Token {
	if p.p+n < len(p.toks) {
		return p.toks[p.p+n]
	}
	return p.toks[len(p.toks)-1] // EOF
}

func (p *parser) next() Token {
	t := p.toks[p.p]
	if t.Kind != EOF {
		p.p++
	}
	return t
}

// isKw reports whether t is the keyword kw.
func isKw(t Token, kw string) bool { return t.Kind == Keyword && t.Str == kw }

// acceptKw consumes the next token if it is the keyword kw.
func (p *parser) acceptKw(kw string) bool {
	if isKw(p.peek(), kw) {
		p.next()
		return true
	}
	return false
}

func (p *parser) accept(k TokenKind) bool {
	if p.peek().Kind == k {
		p.next()
		return true
	}
	return false
}

// syntaxErr is PostgreSQL's syntax error at token t.
func (p *parser) syntaxErr(t Token) *sqlerr.Error {
	if t.Kind == EOF {
		return sqlerr.New(sqlerr.SyntaxError, "syntax error at end of input").At(p.sql, t.Pos)
	}
	return sqlerr.New(sqlerr.SyntaxError, "syntax error at or near %q", p.sql[t.Pos:t.End]).At(p.sql, t.Pos)
}

// expected is a syntax error at the next token with a hint.
func (p *parser) expected(what string) *sqlerr.Error {
	return p.syntaxErr(p.peek()).WithHint("Expected %s.", what)
}

func (p *parser) expectKw(kw string) error {
	if !p.acceptKw(kw) {
		return p.expected(strings.ToUpper(kw))
	}
	return nil
}

func (p *parser) expect(k TokenKind) error {
	if !p.accept(k) {
		return p.expected(k.String())
	}
	return nil
}

func (p *parser) unsupported(t Token, format string, args ...any) *sqlerr.Error {
	return sqlerr.New(sqlerr.FeatureNotSupported, format, args...).At(p.sql, t.Pos)
}

// --- names ---------------------------------------------------------------

// isColID reports whether t can be a column or table name (PostgreSQL's
// ColId): an identifier, or an unreserved or column-name keyword.
func isColID(t Token) bool {
	return t.Kind == Ident || t.Kind == Keyword && (t.Category == keyword.Unreserved || t.Category == keyword.ColName)
}

// isFuncName reports whether t can name a function.
func isFuncName(t Token) bool {
	return t.Kind == Ident || t.Kind == Keyword && (t.Category == keyword.Unreserved || t.Category == keyword.TypeFuncName)
}

// colID parses a column or table name; what names it in the error.
func (p *parser) colID(what string) (ast.Name, error) {
	t := p.peek()
	if !isColID(t) {
		e := p.expected(what)
		if t.Kind == Keyword {
			e = e.WithHint("%q is a reserved word; to use it as a name, write it in double quotes.", t.Str)
		}
		return ast.Name{}, e
	}
	p.next()
	return ast.Name{P: t.Pos, Name: t.Str}, nil
}

// colLabel parses a name after AS, where any keyword is allowed.
func (p *parser) colLabel() (ast.Name, error) {
	t := p.peek()
	if t.Kind != Ident && t.Kind != Keyword {
		return ast.Name{}, p.expected("a name")
	}
	p.next()
	return ast.Name{P: t.Pos, Name: t.Str}, nil
}

// optAlias parses [AS] alias. Without AS only identifiers and unreserved
// keywords other than stop are aliases, so a following clause keyword (such
// as UPDATE's SET) is not mistaken for one.
func (p *parser) optAlias(stop string) (ast.Name, error) {
	if p.acceptKw("as") {
		return p.colLabel()
	}
	if t := p.peek(); t.Kind == Ident || t.Kind == Keyword && t.Category == keyword.Unreserved && t.Str != stop {
		p.next()
		return ast.Name{P: t.Pos, Name: t.Str}, nil
	}
	return ast.Name{}, nil
}

func (p *parser) nameList() ([]ast.Name, error) {
	if err := p.expect(LParen); err != nil {
		return nil, err
	}
	var names []ast.Name
	for {
		n, err := p.colID("a column name")
		if err != nil {
			return nil, err
		}
		names = append(names, n)
		if !p.accept(Comma) {
			break
		}
	}
	return names, p.expect(RParen)
}

// --- types ---------------------------------------------------------------

// typeWord returns the word of t if it can start a type name.
func typeWord(t Token) (string, bool) {
	if t.Kind == Ident || t.Kind == Keyword && t.Category != keyword.Reserved {
		return t.Str, true
	}
	return "", false
}

// grammarTypeNames are type names that exist only as grammar keywords; as
// in PostgreSQL, quoting them ("integer") names a type that does not exist.
var grammarTypeNames = map[string]bool{
	"integer": true, "int": true, "bigint": true, "double": true, "float": true,
	"boolean": true, "timestamp": true,
}

// unsupportedTypes maps PostgreSQL type names NoVacDB does not have yet to
// a hint.
var unsupportedTypes = map[string]string{
	"smallint": "Use integer.", "int2": "Use integer.",
	"real": "Use double precision.", "float4": "Use double precision.",
	"numeric": "Use bigint or double precision.", "decimal": "Use bigint or double precision.",
	"varchar": "Use text.", "char": "Use text.", "character": "Use text.", "bpchar": "Use text.", "name": "Use text.",
	"date": "Use timestamptz.", "time": "Use timestamptz.", "timetz": "Use timestamptz.", "interval": "",
	"json": "", "jsonb": "", "uuid": "", "bytea": "", "serial": "", "bigserial": "", "money": "",
}

// parseType parses a type name.
func (p *parser) parseType() (ast.Type, int, error) {
	t := p.peek()
	w, ok := typeWord(t)
	if !ok {
		return 0, 0, p.expected("a type name")
	}
	p.next()
	if t.Quoted && grammarTypeNames[w] {
		return 0, 0, sqlerr.New(sqlerr.UndefinedObject, "type %q does not exist", w).At(p.sql, t.Pos).
			WithHint("Write the type name without quotes.")
	}
	var typ ast.Type
	switch w {
	case "integer", "int", "int4":
		typ = ast.TypeInteger
	case "bigint", "int8":
		typ = ast.TypeBigInt
	case "float8":
		typ = ast.TypeDouble
	case "double":
		if err := p.expectKw("precision"); err != nil {
			return 0, 0, err
		}
		typ = ast.TypeDouble
	case "float":
		typ = ast.TypeDouble
		if p.accept(LParen) {
			pt := p.peek()
			n, err := strconv.Atoi(pt.Str)
			if pt.Kind != Integer || err != nil {
				return 0, 0, p.expected("a precision")
			}
			p.next()
			if n < 1 || n > 53 {
				return 0, 0, sqlerr.New(sqlerr.InvalidParameterValue, "precision for type float must be between 1 and 53 bits").At(p.sql, pt.Pos)
			}
			if n <= 24 {
				return 0, 0, p.unsupported(t, "type real (float(%d)) is not supported yet", n).WithHint("Use double precision.")
			}
			if err := p.expect(RParen); err != nil {
				return 0, 0, err
			}
		}
	case "text":
		typ = ast.TypeText
	case "boolean", "bool":
		typ = ast.TypeBoolean
	case "timestamptz":
		typ = ast.TypeTimestampTZ
	case "timestamp":
		if p.acceptKw("with") {
			if err := p.expectKw("time"); err != nil {
				return 0, 0, err
			}
			if err := p.expectKw("zone"); err != nil {
				return 0, 0, err
			}
			typ = ast.TypeTimestampTZ
			break
		}
		if p.acceptKw("without") {
			if err := p.expectKw("time"); err != nil {
				return 0, 0, err
			}
			if err := p.expectKw("zone"); err != nil {
				return 0, 0, err
			}
		}
		return 0, 0, p.unsupported(t, "type timestamp without time zone is not supported").
			WithHint("Use timestamptz: it stores an exact moment, so values do not change meaning when the session time zone differs.")
	default:
		if hint, ok := unsupportedTypes[w]; ok {
			e := p.unsupported(t, "type %s is not supported yet", w)
			if hint != "" {
				e = e.WithHint("%s", hint)
			}
			return 0, 0, e
		}
		return 0, 0, sqlerr.New(sqlerr.UndefinedObject, "type %q does not exist", w).At(p.sql, t.Pos)
	}
	if p.peek().Kind == LBracket {
		return 0, 0, p.unsupported(p.peek(), "array types are not supported yet")
	}
	return typ, t.Pos, nil
}

// --- statements ----------------------------------------------------------

func (p *parser) parseStmt() (ast.Stmt, error) {
	t := p.peek()
	switch {
	case isKw(t, "select"):
		return p.parseSelect()
	case isKw(t, "insert"):
		return p.parseInsert()
	case isKw(t, "update"):
		return p.parseUpdate()
	case isKw(t, "delete"):
		return p.parseDelete()
	case isKw(t, "create"):
		switch next := p.peekAt(1); {
		case isKw(next, "table"):
			return p.parseCreateTable()
		case isKw(next, "index") || isKw(next, "unique"):
			return p.parseCreateIndex()
		}
		p.next()
		return nil, p.expected("TABLE or INDEX")
	case isKw(t, "drop"):
		switch next := p.peekAt(1); {
		case isKw(next, "table"):
			return p.parseDropTable()
		case isKw(next, "index"):
			return p.parseDropIndex()
		}
		p.next()
		return nil, p.expected("TABLE or INDEX")
	}
	return nil, p.syntaxErr(t)
}

func (p *parser) ifNotExists() (bool, error) {
	if !p.acceptKw("if") {
		return false, nil
	}
	if err := p.expectKw("not"); err != nil {
		return false, err
	}
	return true, p.expectKw("exists")
}

func (p *parser) ifExists() (bool, error) {
	if !p.acceptKw("if") {
		return false, nil
	}
	return true, p.expectKw("exists")
}

func (p *parser) parseCreateTable() (ast.Stmt, error) {
	s := &ast.CreateTable{P: p.next().Pos}
	p.next() // TABLE
	var err error
	if s.IfNotExists, err = p.ifNotExists(); err != nil {
		return nil, err
	}
	if s.Name, err = p.colID("a table name"); err != nil {
		return nil, err
	}
	if err := p.expect(LParen); err != nil {
		return nil, err
	}
	if p.accept(RParen) {
		return s, nil // a table with no columns, as PostgreSQL allows
	}
	for {
		t := p.peek()
		switch {
		case isKw(t, "primary") || isKw(t, "unique"):
			c, err := p.parseTableConstraint()
			if err != nil {
				return nil, err
			}
			s.Constraints = append(s.Constraints, c)
		case isKw(t, "check") || isKw(t, "foreign") || isKw(t, "constraint") || isKw(t, "references"):
			return nil, p.unsupported(t, "%s constraints are not supported yet", strings.ToUpper(t.Str))
		default:
			c, err := p.parseColumnDef()
			if err != nil {
				return nil, err
			}
			s.Columns = append(s.Columns, c)
		}
		if !p.accept(Comma) {
			break
		}
	}
	return s, p.expect(RParen)
}

func (p *parser) parseTableConstraint() (*ast.TableConstraint, error) {
	t := p.next()
	c := &ast.TableConstraint{P: t.Pos, PrimaryKey: isKw(t, "primary")}
	if c.PrimaryKey {
		if err := p.expectKw("key"); err != nil {
			return nil, err
		}
	}
	var err error
	c.Columns, err = p.nameList()
	return c, err
}

func (p *parser) parseColumnDef() (*ast.ColumnDef, error) {
	name, err := p.colID("a column name or table constraint")
	if err != nil {
		return nil, err
	}
	c := &ast.ColumnDef{Name: name}
	if c.Type, c.TypeP, err = p.parseType(); err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		switch {
		case isKw(t, "not"):
			p.next()
			if err := p.expectKw("null"); err != nil {
				return nil, err
			}
			c.NotNull = true
		case isKw(t, "null"):
			p.next()
			c.Null = true
		case isKw(t, "primary"):
			p.next()
			if err := p.expectKw("key"); err != nil {
				return nil, err
			}
			c.PrimaryKey = true
		case isKw(t, "unique"):
			p.next()
			c.Unique = true
		case isKw(t, "default"):
			p.next()
			if c.Default, err = p.parseBExpr(); err != nil {
				return nil, err
			}
		case isKw(t, "check") || isKw(t, "references") || isKw(t, "constraint") || isKw(t, "collate"):
			return nil, p.unsupported(t, "%s in a column definition is not supported yet", strings.ToUpper(t.Str))
		default:
			if c.NotNull && c.Null {
				return nil, sqlerr.New(sqlerr.SyntaxError, "conflicting NULL/NOT NULL declarations for column %q", c.Name.Name).At(p.sql, c.Name.P)
			}
			return c, nil
		}
	}
}

func (p *parser) parseDropTable() (ast.Stmt, error) {
	s := &ast.DropTable{P: p.next().Pos}
	p.next() // TABLE
	var err error
	if s.IfExists, err = p.ifExists(); err != nil {
		return nil, err
	}
	if s.Name, err = p.colID("a table name"); err != nil {
		return nil, err
	}
	if p.peek().Kind == Comma {
		return nil, p.unsupported(p.peek(), "dropping several tables in one statement is not supported yet")
	}
	return s, nil
}

func (p *parser) parseCreateIndex() (ast.Stmt, error) {
	s := &ast.CreateIndex{P: p.next().Pos}
	s.Unique = p.acceptKw("unique")
	if err := p.expectKw("index"); err != nil {
		return nil, err
	}
	if t := p.peek(); isKw(t, "concurrently") {
		return nil, p.unsupported(t, "CREATE INDEX CONCURRENTLY is not supported yet")
	}
	var err error
	if s.IfNotExists, err = p.ifNotExists(); err != nil {
		return nil, err
	}
	if !isKw(p.peek(), "on") || s.IfNotExists {
		if s.Name, err = p.colID("an index name"); err != nil {
			return nil, err
		}
	}
	if err := p.expectKw("on"); err != nil {
		return nil, err
	}
	if s.Table, err = p.colID("a table name"); err != nil {
		return nil, err
	}
	if t := p.peek(); isKw(t, "using") {
		return nil, p.unsupported(t, "index access methods other than the default are not supported yet")
	}
	s.Columns, err = p.nameList()
	return s, err
}

func (p *parser) parseDropIndex() (ast.Stmt, error) {
	s := &ast.DropIndex{P: p.next().Pos}
	p.next() // INDEX
	var err error
	if s.IfExists, err = p.ifExists(); err != nil {
		return nil, err
	}
	if s.Name, err = p.colID("an index name"); err != nil {
		return nil, err
	}
	if p.peek().Kind == Comma {
		return nil, p.unsupported(p.peek(), "dropping several indexes in one statement is not supported yet")
	}
	return s, nil
}

func (p *parser) parseInsert() (ast.Stmt, error) {
	s := &ast.Insert{P: p.next().Pos}
	if err := p.expectKw("into"); err != nil {
		return nil, err
	}
	var err error
	if s.Table, err = p.colID("a table name"); err != nil {
		return nil, err
	}
	if p.peek().Kind == LParen {
		if s.Columns, err = p.nameList(); err != nil {
			return nil, err
		}
	}
	t := p.peek()
	switch {
	case isKw(t, "default"):
		p.next()
		if err := p.expectKw("values"); err != nil {
			return nil, err
		}
		s.DefaultValues = true
		return s, nil
	case isKw(t, "select"):
		return nil, p.unsupported(t, "INSERT ... SELECT is not supported yet")
	}
	if err := p.expectKw("values"); err != nil {
		return nil, err
	}
	for {
		if err := p.expect(LParen); err != nil {
			return nil, err
		}
		var row []ast.Expr
		for {
			e, err := p.parseExprOrDefault()
			if err != nil {
				return nil, err
			}
			row = append(row, e)
			if !p.accept(Comma) {
				break
			}
		}
		if err := p.expect(RParen); err != nil {
			return nil, err
		}
		s.Rows = append(s.Rows, row)
		if !p.accept(Comma) {
			break
		}
	}
	if t := p.peek(); isKw(t, "returning") || isKw(t, "on") {
		return nil, p.unsupported(t, "%s is not supported yet", strings.ToUpper(t.Str))
	}
	return s, nil
}

func (p *parser) parseExprOrDefault() (ast.Expr, error) {
	if t := p.peek(); isKw(t, "default") {
		p.next()
		return &ast.Default{P: t.Pos}, nil
	}
	return p.parseExpr()
}

// parseTableRef parses a table name and optional alias; stop is a keyword
// that ends the reference instead of being an alias.
func (p *parser) parseTableRef(stop string) (*ast.TableRef, error) {
	name, err := p.colID("a table name")
	if err != nil {
		return nil, err
	}
	if p.peek().Kind == Dot {
		return nil, p.unsupported(p.peek(), "schema-qualified table names are not supported yet")
	}
	r := &ast.TableRef{Name: name}
	r.Alias, err = p.optAlias(stop)
	return r, err
}

func (p *parser) parseSelect() (ast.Stmt, error) {
	s := &ast.Select{P: p.next().Pos}
	if p.acceptKw("distinct") {
		if t := p.peek(); isKw(t, "on") {
			return nil, p.unsupported(t, "DISTINCT ON is not supported yet")
		}
		s.Distinct = true
	} else {
		p.acceptKw("all")
	}
	// PostgreSQL allows an empty select list: SELECT FROM t.
	for empty := p.endOfTargets(); !empty; {
		tg, err := p.parseTarget()
		if err != nil {
			return nil, err
		}
		s.Targets = append(s.Targets, tg)
		if !p.accept(Comma) {
			break
		}
	}
	var err error
	if p.acceptKw("from") {
		if s.From, err = p.parseTableRef(""); err != nil {
			return nil, err
		}
		if t := p.peek(); t.Kind == Comma || isKw(t, "join") || isKw(t, "inner") || isKw(t, "left") ||
			isKw(t, "right") || isKw(t, "full") || isKw(t, "cross") || isKw(t, "natural") {
			return nil, p.unsupported(t, "joins are not supported yet")
		}
	}
	if p.acceptKw("where") {
		if s.Where, err = p.parseExpr(); err != nil {
			return nil, err
		}
	}
	if t := p.peek(); isKw(t, "group") || isKw(t, "having") || isKw(t, "window") {
		return nil, p.unsupported(t, "%s is not supported yet", strings.ToUpper(t.Str))
	}
	if p.acceptKw("order") {
		if err := p.expectKw("by"); err != nil {
			return nil, err
		}
		for {
			item, err := p.parseOrderItem()
			if err != nil {
				return nil, err
			}
			s.OrderBy = append(s.OrderBy, item)
			if !p.accept(Comma) {
				break
			}
		}
	}
	// LIMIT and OFFSET in either order, each at most once.
	for {
		t := p.peek()
		switch {
		case isKw(t, "limit") && s.Limit == nil:
			p.next()
			if p.acceptKw("all") {
				s.Limit = &ast.NullLit{P: t.Pos}
			} else if s.Limit, err = p.parseExpr(); err != nil {
				return nil, err
			}
		case isKw(t, "offset") && s.Offset == nil:
			p.next()
			if s.Offset, err = p.parseExpr(); err != nil {
				return nil, err
			}
			if w := p.peek(); w.Kind == Ident && (w.Str == "row" || w.Str == "rows") {
				p.next()
			}
		case isKw(t, "union") || isKw(t, "intersect") || isKw(t, "except") || isKw(t, "fetch") || isKw(t, "for"):
			return nil, p.unsupported(t, "%s is not supported yet", strings.ToUpper(t.Str))
		default:
			return s, nil
		}
	}
}

// endOfTargets reports whether the select list has ended (or is empty).
func (p *parser) endOfTargets() bool {
	t := p.peek()
	if t.Kind == EOF || t.Kind == Semicolon {
		return true
	}
	for _, kw := range []string{"from", "where", "order", "limit", "offset", "group", "having", "union", "intersect", "except", "window", "fetch", "for"} {
		if isKw(t, kw) {
			return true
		}
	}
	return false
}

func (p *parser) parseTarget() (*ast.Target, error) {
	t := p.peek()
	if t.Kind == Op && t.Str == "*" {
		p.next()
		return &ast.Target{P: t.Pos, Star: true}, nil
	}
	// table.*
	if isColID(t) && p.peekAt(1).Kind == Dot && p.peekAt(2).Kind == Op && p.peekAt(2).Str == "*" {
		p.next()
		p.next()
		p.next()
		return &ast.Target{P: t.Pos, Star: true, StarTable: t.Str}, nil
	}
	e, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	tg := &ast.Target{P: t.Pos, Expr: e}
	tg.Alias, err = p.optAlias("")
	return tg, err
}

func (p *parser) parseOrderItem() (*ast.OrderItem, error) {
	e, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	o := &ast.OrderItem{Expr: e}
	if p.acceptKw("desc") {
		o.Desc = true
	} else {
		p.acceptKw("asc")
	}
	if t := p.peek(); isKw(t, "using") {
		return nil, p.unsupported(t, "ORDER BY ... USING is not supported yet")
	}
	if p.acceptKw("nulls") {
		switch {
		case p.acceptKw("first"):
			o.Nulls = ast.NullsFirst
		case p.acceptKw("last"):
			o.Nulls = ast.NullsLast
		default:
			return nil, p.expected("FIRST or LAST")
		}
	}
	return o, nil
}

func (p *parser) parseUpdate() (ast.Stmt, error) {
	s := &ast.Update{P: p.next().Pos}
	var err error
	if s.Table, err = p.parseTableRef("set"); err != nil {
		return nil, err
	}
	if err := p.expectKw("set"); err != nil {
		return nil, err
	}
	for {
		if t := p.peek(); t.Kind == LParen {
			return nil, p.unsupported(t, "multiple-column assignment is not supported yet")
		}
		col, err := p.colID("a column name")
		if err != nil {
			return nil, err
		}
		if t := p.peek(); t.Kind == Dot || t.Kind == LBracket {
			return nil, p.unsupported(t, "assigning to a field or element is not supported")
		}
		if t := p.peek(); t.Kind != Op || t.Str != "=" {
			return nil, p.expected(`"="`)
		}
		p.next()
		v, err := p.parseExprOrDefault()
		if err != nil {
			return nil, err
		}
		s.Sets = append(s.Sets, &ast.Assignment{Column: col, Value: v})
		if !p.accept(Comma) {
			break
		}
	}
	if t := p.peek(); isKw(t, "from") {
		return nil, p.unsupported(t, "UPDATE ... FROM is not supported yet")
	}
	if p.acceptKw("where") {
		if s.Where, err = p.parseExpr(); err != nil {
			return nil, err
		}
	}
	if t := p.peek(); isKw(t, "returning") {
		return nil, p.unsupported(t, "RETURNING is not supported yet")
	}
	return s, nil
}

func (p *parser) parseDelete() (ast.Stmt, error) {
	s := &ast.Delete{P: p.next().Pos}
	if err := p.expectKw("from"); err != nil {
		return nil, err
	}
	var err error
	if s.Table, err = p.parseTableRef(""); err != nil {
		return nil, err
	}
	if t := p.peek(); isKw(t, "using") {
		return nil, p.unsupported(t, "DELETE ... USING is not supported yet")
	}
	if p.acceptKw("where") {
		if s.Where, err = p.parseExpr(); err != nil {
			return nil, err
		}
	}
	if t := p.peek(); isKw(t, "returning") {
		return nil, p.unsupported(t, "RETURNING is not supported yet")
	}
	return s, nil
}

// --- expressions ---------------------------------------------------------

func (p *parser) enter() error {
	p.depth++
	if p.depth > MaxDepth {
		return sqlerr.New(sqlerr.StatementTooComplex, "expression nested more than %d levels deep", MaxDepth).At(p.sql, p.peek().Pos)
	}
	return nil
}

func (p *parser) leave() { p.depth-- }

func (p *parser) parseExpr() (ast.Expr, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()
	return p.parseOr()
}

// parseBExpr parses an expression without the boolean connectives and IS
// tests (like PostgreSQL's b_expr), as DEFAULT takes: `DEFAULT 0 NOT NULL`
// is a default and a constraint.
func (p *parser) parseBExpr() (ast.Expr, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()
	return p.parseCmp()
}

func (p *parser) parseOr() (ast.Expr, error) {
	l, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if !isKw(t, "or") {
			return l, nil
		}
		p.next()
		r, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		l = &ast.Binary{P: l.Pos(), Op: "OR", L: l, R: r}
	}
}

func (p *parser) parseAnd() (ast.Expr, error) {
	l, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if !isKw(t, "and") {
			return l, nil
		}
		p.next()
		r, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		l = &ast.Binary{P: l.Pos(), Op: "AND", L: l, R: r}
	}
}

func (p *parser) parseNot() (ast.Expr, error) {
	t := p.peek()
	if !isKw(t, "not") {
		return p.parseIs()
	}
	p.next()
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()
	x, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	return &ast.Unary{P: t.Pos, Op: "NOT", X: x}, nil
}

func (p *parser) parseIs() (ast.Expr, error) {
	x, err := p.parseCmp()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		switch {
		case isKw(t, "isnull"):
			p.next()
			x = &ast.Is{P: x.Pos(), X: x, Test: ast.IsNull}
			continue
		case isKw(t, "notnull"):
			p.next()
			x = &ast.Is{P: x.Pos(), X: x, Not: true, Test: ast.IsNull}
			continue
		case !isKw(t, "is"):
			return x, nil
		}
		p.next()
		not := p.acceptKw("not")
		switch u := p.peek(); {
		case isKw(u, "null"):
			p.next()
			x = &ast.Is{P: x.Pos(), X: x, Not: not, Test: ast.IsNull}
		case isKw(u, "true"):
			p.next()
			x = &ast.Is{P: x.Pos(), X: x, Not: not, Test: ast.IsTrue}
		case isKw(u, "false"):
			p.next()
			x = &ast.Is{P: x.Pos(), X: x, Not: not, Test: ast.IsFalse}
		case isKw(u, "unknown"):
			p.next()
			x = &ast.Is{P: x.Pos(), X: x, Not: not, Test: ast.IsUnknown}
		case isKw(u, "distinct"):
			p.next()
			if err := p.expectKw("from"); err != nil {
				return nil, err
			}
			r, err := p.parseCmp()
			if err != nil {
				return nil, err
			}
			x = &ast.IsDistinct{P: x.Pos(), L: x, R: r, Not: not}
		default:
			return nil, p.expected("NULL, TRUE, FALSE, UNKNOWN or DISTINCT FROM")
		}
	}
}

func isCmpOp(t Token) bool {
	if t.Kind != Op {
		return false
	}
	switch t.Str {
	case "<", ">", "=", "<=", ">=", "<>":
		return true
	}
	return false
}

func (p *parser) parseCmp() (ast.Expr, error) {
	l, err := p.parsePredicate()
	if err != nil {
		return nil, err
	}
	t := p.peek()
	if !isCmpOp(t) {
		return l, nil
	}
	p.next()
	// Comparisons do not associate: in a < b < c the second < is left
	// over, which the caller reports as a syntax error.
	r, err := p.parsePredicate()
	if err != nil {
		return nil, err
	}
	return &ast.Binary{P: l.Pos(), Op: t.Str, L: l, R: r}, nil
}

// parsePredicate parses [NOT] BETWEEN, IN, LIKE and ILIKE, which do not
// associate either.
func (p *parser) parsePredicate() (ast.Expr, error) {
	x, err := p.parseOther()
	if err != nil {
		return nil, err
	}
	not := false
	t := p.peek()
	if isKw(t, "not") {
		switch u := p.peekAt(1); {
		case isKw(u, "between"), isKw(u, "in"), isKw(u, "like"), isKw(u, "ilike"):
			p.next()
			not = true
			t = p.peek()
		default:
			return x, nil
		}
	}
	var e ast.Expr
	switch {
	case isKw(t, "between"):
		p.next()
		if u := p.peek(); isKw(u, "symmetric") || isKw(u, "asymmetric") {
			return nil, p.unsupported(u, "BETWEEN %s is not supported yet", strings.ToUpper(u.Str))
		}
		lo, err := p.parseOther()
		if err != nil {
			return nil, err
		}
		if err := p.expectKw("and"); err != nil {
			return nil, err
		}
		hi, err := p.parseOther()
		if err != nil {
			return nil, err
		}
		e = &ast.Between{P: x.Pos(), X: x, Lo: lo, Hi: hi, Not: not}
	case isKw(t, "in"):
		p.next()
		if err := p.expect(LParen); err != nil {
			return nil, err
		}
		if u := p.peek(); isKw(u, "select") {
			return nil, p.unsupported(u, "subqueries are not supported yet")
		}
		in := &ast.In{P: x.Pos(), X: x, Not: not}
		for {
			v, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			in.List = append(in.List, v)
			if !p.accept(Comma) {
				break
			}
		}
		if err := p.expect(RParen); err != nil {
			return nil, err
		}
		e = in
	case isKw(t, "like"), isKw(t, "ilike"):
		p.next()
		pat, err := p.parseOther()
		if err != nil {
			return nil, err
		}
		if u := p.peek(); u.Kind == Ident && u.Str == "escape" {
			return nil, p.unsupported(u, "LIKE ... ESCAPE is not supported yet")
		}
		e = &ast.Like{P: x.Pos(), X: x, Pattern: pat, Not: not, CaseInsensitive: isKw(t, "ilike")}
	default:
		return x, nil
	}
	// A second predicate is left over and reported by the caller.
	return e, nil
}

// Operators NoVacDB implements, all and by precedence level.
var (
	knownOps = map[string]bool{
		"+": true, "-": true, "*": true, "/": true, "%": true, "^": true, "||": true, "::": true,
		"<": true, ">": true, "=": true, "<=": true, ">=": true, "<>": true,
	}
	addOps = map[string]bool{"+": true, "-": true}
	mulOps = map[string]bool{"*": true, "/": true, "%": true}
)

// otherOpLevel reports whether op belongs to the "any other operator" level.
func otherOpLevel(t Token) bool {
	if t.Kind != Op {
		return false
	}
	switch t.Str {
	case "+", "-", "*", "/", "%", "^", "::", "<", ">", "=", "<=", ">=", "<>":
		return false
	}
	return true
}

func (p *parser) undefinedOp(t Token) *sqlerr.Error {
	return sqlerr.New(sqlerr.UndefinedFunction, "operator does not exist: %s", t.Str).At(p.sql, t.Pos).
		WithHint("Supported operators are + - * / %% ^ || and the comparisons = <> != < > <= >=.")
}

func (p *parser) parseOther() (ast.Expr, error) {
	l, err := p.parseAdd()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if !otherOpLevel(t) {
			return l, nil
		}
		if t.Str != "||" {
			return nil, p.undefinedOp(t)
		}
		p.next()
		r, err := p.parseAdd()
		if err != nil {
			return nil, err
		}
		l = &ast.Binary{P: l.Pos(), Op: t.Str, L: l, R: r}
	}
}

func (p *parser) parseAdd() (ast.Expr, error) {
	l, err := p.parseMul()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.Kind != Op || !addOps[t.Str] {
			return l, nil
		}
		p.next()
		r, err := p.parseMul()
		if err != nil {
			return nil, err
		}
		l = &ast.Binary{P: l.Pos(), Op: t.Str, L: l, R: r}
	}
}

func (p *parser) parseMul() (ast.Expr, error) {
	l, err := p.parseExp()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.Kind != Op || !mulOps[t.Str] {
			return l, nil
		}
		p.next()
		r, err := p.parseExp()
		if err != nil {
			return nil, err
		}
		l = &ast.Binary{P: l.Pos(), Op: t.Str, L: l, R: r}
	}
}

func (p *parser) parseExp() (ast.Expr, error) {
	l, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.Kind != Op || t.Str != "^" {
			return l, nil
		}
		p.next()
		r, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		l = &ast.Binary{P: l.Pos(), Op: "^", L: l, R: r}
	}
}

func (p *parser) parseUnary() (ast.Expr, error) {
	t := p.peek()
	if t.Kind != Op || t.Str != "-" && t.Str != "+" {
		// An operator NoVacDB does not know may be a prefix operator in
		// PostgreSQL (@, ~, ...); a known infix one here is a syntax error.
		if t.Kind == Op && !knownOps[t.Str] {
			return nil, p.undefinedOp(t)
		}
		return p.parseCast()
	}
	p.next()
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()
	// -9223372036854775808 is the one integer literal whose magnitude does
	// not fit in an int64.
	if u := p.peek(); t.Str == "-" && u.Kind == Integer && integerMagnitude(u.Str) == 1<<63 && !isCastNext(p.peekAt(1)) {
		p.next()
		return &ast.IntegerLit{P: t.Pos, Value: math.MinInt64}, nil
	}
	x, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	if t.Str == "-" {
		// As PostgreSQL does, a negated numeric constant is a constant.
		switch lit := x.(type) {
		case *ast.IntegerLit:
			if lit.Value != math.MinInt64 {
				return &ast.IntegerLit{P: t.Pos, Value: -lit.Value}, nil
			}
		case *ast.FloatLit:
			text := "-" + lit.Text
			if strings.HasPrefix(lit.Text, "-") {
				text = lit.Text[1:]
			}
			return &ast.FloatLit{P: t.Pos, Text: text, Value: -lit.Value}, nil
		}
	}
	return &ast.Unary{P: t.Pos, Op: t.Str, X: x}, nil
}

func isCastNext(t Token) bool { return t.Kind == Op && t.Str == "::" }

func (p *parser) parseCast() (ast.Expr, error) {
	x, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.Kind != Op || t.Str != "::" {
			return x, nil
		}
		p.next()
		typ, _, err := p.parseType()
		if err != nil {
			return nil, err
		}
		x = &ast.Cast{P: x.Pos(), X: x, Type: typ}
	}
}

// normalizeNumber drops digit separators.
func normalizeNumber(s string) string { return strings.ReplaceAll(s, "_", "") }

// integerMagnitudeOK returns the value of an Integer token's text, and
// false if it does not fit in 64 unsigned bits.
func integerMagnitudeOK(text string) (uint64, bool) {
	s := normalizeNumber(text)
	base := 10
	if len(s) > 2 && s[0] == '0' {
		switch s[1] {
		case 'x', 'X':
			base, s = 16, s[2:]
		case 'o', 'O':
			base, s = 8, s[2:]
		case 'b', 'B':
			base, s = 2, s[2:]
		}
	}
	v, err := strconv.ParseUint(s, base, 64)
	return v, err == nil
}

// integerMagnitude is integerMagnitudeOK's value (0 when it does not fit).
func integerMagnitude(text string) uint64 {
	v, _ := integerMagnitudeOK(text)
	return v
}

// integerValue converts an Integer token's text.
func (p *parser) integerValue(t Token) (int64, error) {
	v, ok := integerMagnitudeOK(t.Str)
	if !ok || v > math.MaxInt64 {
		return 0, sqlerr.New(sqlerr.NumericValueOutOfRange, "integer constant %s is out of range for type bigint", t.Str).At(p.sql, t.Pos).
			WithHint("NoVacDB has no numeric type yet; integers must fit in 64 bits.")
	}
	return int64(v), nil
}

func (p *parser) floatValue(t Token) (float64, error) {
	s := normalizeNumber(t.Str)
	v, err := strconv.ParseFloat(s, 64)
	underflow := err == nil && v == 0 && strings.ContainsAny(strings.SplitN(strings.ToLower(s), "e", 2)[0], "123456789")
	if err != nil || underflow {
		return 0, sqlerr.New(sqlerr.NumericValueOutOfRange, "%q is out of range for type double precision", t.Str).At(p.sql, t.Pos)
	}
	return v, nil
}

// funcLikeColNames are column-name keywords that are also called like
// functions.
var funcLikeColNames = map[string]bool{"coalesce": true, "greatest": true, "least": true, "nullif": true}

func (p *parser) parsePrimary() (ast.Expr, error) {
	t := p.peek()
	switch t.Kind {
	case Integer:
		p.next()
		v, err := p.integerValue(t)
		if err != nil {
			return nil, err
		}
		return &ast.IntegerLit{P: t.Pos, Value: v}, nil
	case Float:
		p.next()
		v, err := p.floatValue(t)
		if err != nil {
			return nil, err
		}
		return &ast.FloatLit{P: t.Pos, Text: normalizeNumber(t.Str), Value: v}, nil
	case String:
		p.next()
		return &ast.StringLit{P: t.Pos, Value: t.Str}, nil
	case Param:
		p.next()
		return &ast.Param{P: t.Pos, N: t.Param}, nil
	case LParen:
		p.next()
		if u := p.peek(); isKw(u, "select") {
			return nil, p.unsupported(u, "subqueries are not supported yet")
		}
		x, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if u := p.peek(); u.Kind == Comma {
			return nil, p.unsupported(u, "row constructors are not supported yet")
		}
		return x, p.expect(RParen)
	case Keyword:
		switch t.Str {
		case "true", "false":
			p.next()
			return &ast.BoolLit{P: t.Pos, Value: t.Str == "true"}, nil
		case "null":
			p.next()
			return &ast.NullLit{P: t.Pos}, nil
		case "cast":
			return p.parseCastCall()
		case "case":
			return p.parseCase()
		case "exists", "array", "any", "some", "all":
			return nil, p.unsupported(t, "%s is not supported yet", strings.ToUpper(t.Str))
		case "current_timestamp", "current_date", "current_time", "localtimestamp", "localtime",
			"current_user", "session_user", "current_role", "user", "current_catalog", "current_schema", "system_user":
			return nil, p.unsupported(t, "%s is not supported yet", strings.ToUpper(t.Str))
		}
	}
	// A type name followed by a string is a typed literal: timestamptz '...'.
	if w, ok := typeWord(t); ok && p.isTypedLiteral(w) {
		typ, _, err := p.parseType()
		if err != nil {
			return nil, err
		}
		s := p.peek()
		if s.Kind != String {
			return nil, p.expected("a string literal")
		}
		p.next()
		return &ast.Cast{P: t.Pos, X: &ast.StringLit{P: s.Pos, Value: s.Str}, Type: typ}, nil
	}
	if p.peekAt(1).Kind == LParen && (isFuncName(t) || t.Kind == Keyword && funcLikeColNames[t.Str]) {
		return p.parseFuncCall()
	}
	if isColID(t) {
		p.next()
		if p.accept(Dot) {
			c := p.peek()
			if c.Kind == Op && c.Str == "*" {
				return nil, p.syntaxErr(c).WithHint("table.* is allowed only in the SELECT list.")
			}
			if c.Kind != Ident && c.Kind != Keyword {
				return nil, p.expected("a column name")
			}
			p.next()
			if p.peek().Kind == Dot {
				return nil, p.unsupported(p.peek(), "schema-qualified names are not supported yet")
			}
			return &ast.ColumnRef{P: t.Pos, Table: t.Str, Column: c.Str}, nil
		}
		return &ast.ColumnRef{P: t.Pos, Column: t.Str}, nil
	}
	return nil, p.syntaxErr(t)
}

// isTypedLiteral reports whether the type name starting at the next token
// (word w) is followed by a string literal.
func (p *parser) isTypedLiteral(w string) bool {
	n := 1
	switch w {
	case "double":
		n = 2 // double precision
	case "timestamp":
		if isKw(p.peekAt(1), "with") || isKw(p.peekAt(1), "without") {
			n = 4 // timestamp with[out] time zone
		}
	}
	return p.peekAt(n).Kind == String
}

func (p *parser) parseCastCall() (ast.Expr, error) {
	t := p.next() // CAST
	if err := p.expect(LParen); err != nil {
		return nil, err
	}
	x, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if err := p.expectKw("as"); err != nil {
		return nil, err
	}
	typ, _, err := p.parseType()
	if err != nil {
		return nil, err
	}
	return &ast.Cast{P: t.Pos, X: x, Type: typ}, p.expect(RParen)
}

func (p *parser) parseFuncCall() (ast.Expr, error) {
	t := p.next()
	p.next() // (
	f := &ast.FuncCall{P: t.Pos, Name: t.Str}
	if u := p.peek(); u.Kind == Op && u.Str == "*" || isKw(u, "distinct") {
		return nil, p.unsupported(u, "aggregate functions are not supported yet")
	}
	if p.accept(RParen) {
		return f, nil
	}
	for {
		a, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		f.Args = append(f.Args, a)
		if !p.accept(Comma) {
			break
		}
	}
	if u := p.peek(); isKw(u, "order") {
		return nil, p.unsupported(u, "aggregate functions are not supported yet")
	}
	return f, p.expect(RParen)
}

func (p *parser) parseCase() (ast.Expr, error) {
	t := p.next() // CASE
	c := &ast.Case{P: t.Pos}
	var err error
	if !isKw(p.peek(), "when") {
		if c.Operand, err = p.parseExpr(); err != nil {
			return nil, err
		}
	}
	for p.acceptKw("when") {
		w := &ast.When{}
		if w.Cond, err = p.parseExpr(); err != nil {
			return nil, err
		}
		if err := p.expectKw("then"); err != nil {
			return nil, err
		}
		if w.Result, err = p.parseExpr(); err != nil {
			return nil, err
		}
		c.Whens = append(c.Whens, w)
	}
	if len(c.Whens) == 0 {
		return nil, p.expected("WHEN")
	}
	if p.acceptKw("else") {
		if c.Else, err = p.parseExpr(); err != nil {
			return nil, err
		}
	}
	return c, p.expectKw("end")
}
