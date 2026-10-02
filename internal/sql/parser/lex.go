package parser

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/keyword"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
)

// TokenKind is the kind of a token.
type TokenKind uint8

// Token kinds.
const (
	EOF TokenKind = iota
	Ident
	Keyword
	String
	Integer
	Float
	Param
	Op
	LParen
	RParen
	Comma
	Semicolon
	Dot
	LBracket
	RBracket
	Colon
)

var kindNames = [...]string{
	EOF: "end of input", Ident: "identifier", Keyword: "keyword", String: "string",
	Integer: "integer", Float: "number", Param: "parameter", Op: "operator",
	LParen: `"("`, RParen: `")"`, Comma: `","`, Semicolon: `";"`, Dot: `"."`,
	LBracket: `"["`, RBracket: `"]"`, Colon: `":"`,
}

func (k TokenKind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return "token " + strconv.Itoa(int(k))
}

// Token is one lexical token. Pos and End are byte offsets into the query:
// the token is query[Pos:End].
type Token struct {
	Kind     TokenKind
	Pos, End int
	// Str is the identifier (folded to lower case unless quoted), the
	// keyword (lower case), the string's contents, the number's text, or
	// the operator.
	Str string
	// Category is set for keywords.
	Category keyword.Category
	// Quoted is set for quoted identifiers.
	Quoted bool
	// Param is the parameter number of a Param token.
	Param int
}

// Lexer limits.
const (
	// MaxQueryLength is the longest query text accepted, in bytes.
	MaxQueryLength = 1 << 20
	// MaxIdentifierLength is the longest identifier, in bytes, as
	// PostgreSQL's NAMEDATALEN-1.
	MaxIdentifierLength = 63
)

// opChars are the characters operators are made of.
const opChars = "~!@#^&|`?+-*/%<>="

// opNotStrip are the characters whose presence lets a multi-character
// operator end in + or -.
const opNotStrip = "~!@#^&|`?%"

// Lex splits sql into tokens, ending with an EOF token.
func Lex(sql string) ([]Token, error) {
	l := &lexer{s: sql}
	if len(sql) > MaxQueryLength {
		return nil, sqlerr.New(sqlerr.ProgramLimitExceeded, "query text is %d bytes, more than the limit of %d", len(sql), MaxQueryLength)
	}
	if err := l.checkEncoding(); err != nil {
		return nil, err
	}
	var toks []Token
	for {
		t, err := l.next()
		if err != nil {
			return nil, err
		}
		toks = append(toks, t)
		if t.Kind == EOF {
			return toks, nil
		}
	}
}

type lexer struct {
	s string
	i int
}

func (l *lexer) errAt(off int, code, format string, args ...any) *sqlerr.Error {
	return sqlerr.New(code, format, args...).At(l.s, off)
}

// syntaxErrAt reports PostgreSQL's generic syntax error at the text from off.
func (l *lexer) syntaxErrAt(off int, near string) *sqlerr.Error {
	return l.errAt(off, sqlerr.SyntaxError, "syntax error at or near %q", near)
}

func (l *lexer) checkEncoding() error {
	for i := 0; i < len(l.s); {
		r, size := utf8.DecodeRuneInString(l.s[i:])
		if r == utf8.RuneError && size <= 1 {
			return l.errAt(i, sqlerr.CharacterNotInRepertoire, "invalid byte sequence for encoding \"UTF8\": 0x%02x", l.s[i])
		}
		if r == 0 {
			return l.errAt(i, sqlerr.CharacterNotInRepertoire, "invalid byte sequence for encoding \"UTF8\": 0x00")
		}
		i += size
	}
	return nil
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isIdentStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c >= 0x80
}

func isIdentCont(c byte) bool { return isIdentStart(c) || isDigit(c) || c == '$' }

// skipSpace skips whitespace and comments.
func (l *lexer) skipSpace() error {
	for l.i < len(l.s) {
		c := l.s[l.i]
		switch {
		case isSpace(c):
			l.i++
		case strings.HasPrefix(l.s[l.i:], "--"):
			for l.i < len(l.s) && l.s[l.i] != '\n' && l.s[l.i] != '\r' {
				l.i++
			}
		case strings.HasPrefix(l.s[l.i:], "/*"):
			if err := l.skipBlockComment(); err != nil {
				return err
			}
		default:
			return nil
		}
	}
	return nil
}

// skipBlockComment skips a block comment, which may nest.
func (l *lexer) skipBlockComment() error {
	start := l.i
	depth := 0
	for l.i < len(l.s) {
		switch {
		case strings.HasPrefix(l.s[l.i:], "/*"):
			depth++
			l.i += 2
		case strings.HasPrefix(l.s[l.i:], "*/"):
			depth--
			l.i += 2
			if depth == 0 {
				return nil
			}
		default:
			l.i++
		}
	}
	return l.errAt(start, sqlerr.SyntaxError, "unterminated /* comment")
}

func (l *lexer) next() (Token, error) {
	if err := l.skipSpace(); err != nil {
		return Token{}, err
	}
	start := l.i
	if l.i >= len(l.s) {
		return Token{Kind: EOF, Pos: start, End: start}, nil
	}
	c := l.s[l.i]
	single := func(k TokenKind) (Token, error) {
		l.i++
		return Token{Kind: k, Pos: start, End: l.i, Str: l.s[start:l.i]}, nil
	}
	switch {
	case (c == 'e' || c == 'E') && l.i+1 < len(l.s) && l.s[l.i+1] == '\'':
		l.i++
		return l.lexString(start, true)
	case isIdentStart(c):
		return l.lexIdent(start)
	case c == '"':
		return l.lexQuotedIdent(start)
	case c == '\'':
		return l.lexString(start, false)
	case c == '$':
		if l.i+1 < len(l.s) && isDigit(l.s[l.i+1]) {
			return l.lexParam(start)
		}
		return l.lexDollarString(start)
	case isDigit(c) || c == '.' && l.i+1 < len(l.s) && isDigit(l.s[l.i+1]):
		return l.lexNumber(start)
	case c == '(':
		return single(LParen)
	case c == ')':
		return single(RParen)
	case c == ',':
		return single(Comma)
	case c == ';':
		return single(Semicolon)
	case c == '.':
		return single(Dot)
	case c == '[':
		return single(LBracket)
	case c == ']':
		return single(RBracket)
	case c == ':':
		if strings.HasPrefix(l.s[l.i:], "::") {
			l.i += 2
			return Token{Kind: Op, Pos: start, End: l.i, Str: "::"}, nil
		}
		return single(Colon)
	case strings.IndexByte(opChars, c) >= 0:
		return l.lexOp(start), nil
	}
	_, size := utf8.DecodeRuneInString(l.s[l.i:])
	return Token{}, l.syntaxErrAt(start, l.s[start:start+size])
}

func (l *lexer) lexIdent(start int) (Token, error) {
	for l.i < len(l.s) && isIdentCont(l.s[l.i]) {
		l.i++
	}
	raw := l.s[start:l.i]
	name := foldIdent(raw)
	if len(name) > MaxIdentifierLength {
		return Token{}, l.errAt(start, sqlerr.NameTooLong, "identifier %q is %d bytes long, more than the limit of %d", name, len(name), MaxIdentifierLength).
			WithHint("Use a shorter name. NoVacDB does not truncate long names, so two of them cannot silently become the same name.")
	}
	if cat, ok := keyword.Lookup(name); ok {
		return Token{Kind: Keyword, Pos: start, End: l.i, Str: name, Category: cat}, nil
	}
	return Token{Kind: Ident, Pos: start, End: l.i, Str: name}, nil
}

// foldIdent lower-cases ASCII letters, as PostgreSQL does for UTF-8 names.
func foldIdent(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if b[j] >= 'A' && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

func (l *lexer) lexQuotedIdent(start int) (Token, error) {
	l.i++ // opening quote
	var b strings.Builder
	for {
		j := strings.IndexByte(l.s[l.i:], '"')
		if j < 0 {
			return Token{}, l.errAt(start, sqlerr.SyntaxError, "unterminated quoted identifier")
		}
		b.WriteString(l.s[l.i : l.i+j])
		l.i += j + 1
		if l.i < len(l.s) && l.s[l.i] == '"' { // "" is a quote
			b.WriteByte('"')
			l.i++
			continue
		}
		break
	}
	name := b.String()
	if name == "" {
		return Token{}, l.errAt(start, sqlerr.SyntaxError, "zero-length delimited identifier")
	}
	if len(name) > MaxIdentifierLength {
		return Token{}, l.errAt(start, sqlerr.NameTooLong, "identifier %q is %d bytes long, more than the limit of %d", name, len(name), MaxIdentifierLength).
			WithHint("Use a shorter name. NoVacDB does not truncate long names, so two of them cannot silently become the same name.")
	}
	return Token{Kind: Ident, Pos: start, End: l.i, Str: name, Quoted: true}, nil
}

// lexString lexes a quoted string starting at the quote at l.i (after the E
// prefix, if escape). Literals separated by whitespace containing a newline
// are joined.
func (l *lexer) lexString(start int, escape bool) (Token, error) {
	var b strings.Builder
	for {
		if err := l.lexStringPart(start, escape, &b); err != nil {
			return Token{}, err
		}
		// Continuation: whitespace with a newline, then another quote.
		j, sawNewline := l.i, false
		for j < len(l.s) {
			switch {
			case l.s[j] == '\n' || l.s[j] == '\r':
				sawNewline = true
				j++
			case isSpace(l.s[j]):
				j++
			case sawNewline && strings.HasPrefix(l.s[j:], "--"):
				for j < len(l.s) && l.s[j] != '\n' && l.s[j] != '\r' {
					j++
				}
			default:
				goto done
			}
		}
	done:
		if !sawNewline || j >= len(l.s) || l.s[j] != '\'' {
			break
		}
		l.i = j
	}
	if !utf8.ValidString(b.String()) {
		return Token{}, l.errAt(start, sqlerr.CharacterNotInRepertoire, "invalid byte sequence for encoding \"UTF8\"").
			WithDetail("An escape in the string produced bytes that are not valid UTF-8.")
	}
	return Token{Kind: String, Pos: start, End: l.i, Str: b.String()}, nil
}

// lexStringPart lexes one quoted part at l.i into b.
func (l *lexer) lexStringPart(start int, escape bool, b *strings.Builder) error {
	l.i++ // opening quote
	for {
		if l.i >= len(l.s) {
			return l.errAt(start, sqlerr.SyntaxError, "unterminated quoted string")
		}
		c := l.s[l.i]
		switch {
		case c == '\'':
			if l.i+1 < len(l.s) && l.s[l.i+1] == '\'' {
				b.WriteByte('\'')
				l.i += 2
				continue
			}
			l.i++
			return nil
		case c == '\\' && escape:
			if err := l.lexEscape(b); err != nil {
				return err
			}
		default:
			b.WriteByte(c)
			l.i++
		}
	}
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

func hexVal(c byte) (int, bool) {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0'), true
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10, true
	}
	return 0, false
}

// lexEscape decodes the backslash escape at l.i into b.
func (l *lexer) lexEscape(b *strings.Builder) error {
	at := l.i
	l.i++ // backslash
	if l.i >= len(l.s) {
		return nil // the caller reports the unterminated string
	}
	c := l.s[l.i]
	switch c {
	case 'b':
		b.WriteByte('\b')
	case 'f':
		b.WriteByte('\f')
	case 'n':
		b.WriteByte('\n')
	case 'r':
		b.WriteByte('\r')
	case 't':
		b.WriteByte('\t')
	case 'x':
		v, n := 0, 0
		for n < 2 && l.i+1+n < len(l.s) {
			d, ok := hexVal(l.s[l.i+1+n])
			if !ok {
				break
			}
			v = v*16 + d
			n++
		}
		if n == 0 {
			b.WriteByte('x') // \x without digits is just x
			break
		}
		if v == 0 {
			return l.errAt(at, sqlerr.CharacterNotInRepertoire, "invalid byte sequence for encoding \"UTF8\": 0x00")
		}
		b.WriteByte(byte(v))
		l.i += n
	case 'u', 'U':
		r, err := l.lexUnicodeEscape(at, c)
		if err != nil {
			return err
		}
		b.WriteRune(r)
		return nil
	default:
		if isOctal(c) {
			v, n := 0, 0
			for n < 3 && l.i+n < len(l.s) && isOctal(l.s[l.i+n]) {
				v = v*8 + int(l.s[l.i+n]-'0')
				n++
			}
			if v == 0 || v > 0xFF {
				return l.errAt(at, sqlerr.CharacterNotInRepertoire, "invalid byte sequence for encoding \"UTF8\": 0x%02x", v&0xFF)
			}
			b.WriteByte(byte(v))
			l.i += n
			return nil
		}
		// Any other character stands for itself (including \\ and \').
		_, size := utf8.DecodeRuneInString(l.s[l.i:])
		b.WriteString(l.s[l.i : l.i+size])
		l.i += size
		return nil
	}
	l.i++
	return nil
}

// readHex reads exactly n hex digits at l.i.
func (l *lexer) readHex(n int) (rune, bool) {
	if l.i+n > len(l.s) {
		return 0, false
	}
	v := rune(0)
	for k := 0; k < n; k++ {
		d, ok := hexVal(l.s[l.i+k])
		if !ok {
			return 0, false
		}
		v = v*16 + rune(d)
	}
	l.i += n
	return v, true
}

func isHighSurrogate(r rune) bool { return r >= 0xD800 && r <= 0xDBFF }
func isLowSurrogate(r rune) bool  { return r >= 0xDC00 && r <= 0xDFFF }

// lexUnicodeEscape decodes \uXXXX or \UXXXXXXXX at l.i (the u), combining a
// UTF-16 surrogate pair written as two \u escapes.
func (l *lexer) lexUnicodeEscape(at int, c byte) (rune, error) {
	n := 4
	if c == 'U' {
		n = 8
	}
	l.i++
	r, ok := l.readHex(n)
	bad := func() (rune, error) {
		return 0, l.errAt(at, sqlerr.InvalidEscapeSequence, "invalid Unicode escape").
			WithHint(`Unicode escapes must be \uXXXX or \UXXXXXXXX.`)
	}
	if !ok {
		return bad()
	}
	if isHighSurrogate(r) {
		if !strings.HasPrefix(l.s[l.i:], `\u`) && !strings.HasPrefix(l.s[l.i:], `\U`) {
			return 0, l.errAt(at, sqlerr.InvalidEscapeSequence, "invalid Unicode surrogate pair")
		}
		m := 4
		if l.s[l.i+1] == 'U' {
			m = 8
		}
		l.i += 2
		lo, ok := l.readHex(m)
		if !ok || !isLowSurrogate(lo) {
			return 0, l.errAt(at, sqlerr.InvalidEscapeSequence, "invalid Unicode surrogate pair")
		}
		r = 0x10000 + (r-0xD800)<<10 + (lo - 0xDC00)
	} else if isLowSurrogate(r) {
		return 0, l.errAt(at, sqlerr.InvalidEscapeSequence, "invalid Unicode surrogate pair")
	}
	if r == 0 || r > utf8.MaxRune {
		return 0, l.errAt(at, sqlerr.InvalidEscapeSequence, "invalid Unicode escape value")
	}
	return r, nil
}

// lexDollarString lexes $tag$...$tag$ at l.i.
func (l *lexer) lexDollarString(start int) (Token, error) {
	j := l.i + 1
	if j < len(l.s) && isIdentStart(l.s[j]) {
		for j < len(l.s) && (isIdentStart(l.s[j]) || isDigit(l.s[j])) {
			j++
		}
	}
	if j >= len(l.s) || l.s[j] != '$' {
		return Token{}, l.syntaxErrAt(start, "$")
	}
	delim := l.s[l.i : j+1]
	body := j + 1
	end := strings.Index(l.s[body:], delim)
	if end < 0 {
		return Token{}, l.errAt(start, sqlerr.SyntaxError, "unterminated dollar-quoted string at or near %q", delim)
	}
	l.i = body + end + len(delim)
	return Token{Kind: String, Pos: start, End: l.i, Str: l.s[body : body+end]}, nil
}

func (l *lexer) lexParam(start int) (Token, error) {
	l.i++ // $
	for l.i < len(l.s) && isDigit(l.s[l.i]) {
		l.i++
	}
	if l.i < len(l.s) && isIdentCont(l.s[l.i]) {
		return Token{}, l.trailingJunk(start, "parameter")
	}
	n, err := strconv.Atoi(l.s[start+1 : l.i])
	if err != nil || n < 1 || n > 65535 {
		return Token{}, l.errAt(start, sqlerr.UndefinedParameter, "there is no parameter %s", l.s[start:l.i])
	}
	return Token{Kind: Param, Pos: start, End: l.i, Str: l.s[start:l.i], Param: n}, nil
}

// trailingJunk reports a number or parameter run into letters, like 12abc.
func (l *lexer) trailingJunk(start int, what string) *sqlerr.Error {
	end := l.i
	for end < len(l.s) && isIdentCont(l.s[end]) {
		end++
	}
	return l.errAt(start, sqlerr.SyntaxError, "trailing junk after %s at or near %q", what, l.s[start:end])
}

// digits consumes digits accepted by ok, with single underscores between
// them, and reports whether at least one digit was read and the run ended
// cleanly (no trailing or doubled underscore).
func (l *lexer) digits(ok func(byte) bool) (bool, bool) {
	n := 0
	for l.i < len(l.s) {
		c := l.s[l.i]
		if ok(c) {
			n++
			l.i++
			continue
		}
		if c == '_' && n > 0 && l.i+1 < len(l.s) && ok(l.s[l.i+1]) {
			l.i++
			continue
		}
		break
	}
	clean := l.i >= len(l.s) || l.s[l.i] != '_'
	return n > 0, clean
}

func (l *lexer) lexNumber(start int) (Token, error) {
	junk := func() error { return l.trailingJunk(start, "numeric literal") }
	finish := func(k TokenKind) (Token, error) {
		if l.i < len(l.s) && isIdentCont(l.s[l.i]) {
			return Token{}, junk()
		}
		return Token{Kind: k, Pos: start, End: l.i, Str: l.s[start:l.i]}, nil
	}
	// Hexadecimal, octal and binary integers.
	if l.s[l.i] == '0' && l.i+1 < len(l.s) {
		var ok func(byte) bool
		switch l.s[l.i+1] {
		case 'x', 'X':
			ok = func(c byte) bool { _, h := hexVal(c); return h }
		case 'o', 'O':
			ok = isOctal
		case 'b', 'B':
			ok = func(c byte) bool { return c == '0' || c == '1' }
		}
		if ok != nil {
			l.i += 2
			// A separator may follow the prefix: 0x_1F.
			if l.i < len(l.s) && l.s[l.i] == '_' && l.i+1 < len(l.s) && ok(l.s[l.i+1]) {
				l.i++
			}
			got, clean := l.digits(ok)
			if !got || !clean {
				return Token{}, junk()
			}
			return finish(Integer)
		}
	}
	kind := Integer
	if l.s[l.i] != '.' {
		if _, clean := l.digits(isDigit); !clean {
			return Token{}, junk()
		}
	}
	if l.i < len(l.s) && l.s[l.i] == '.' {
		// 1..2 is an integer followed by "..": leave the dots.
		if l.i+1 < len(l.s) && l.s[l.i+1] == '.' {
			return finish(Integer)
		}
		kind = Float
		l.i++
		if l.i < len(l.s) && isDigit(l.s[l.i]) {
			if _, clean := l.digits(isDigit); !clean {
				return Token{}, junk()
			}
		}
	}
	if l.i < len(l.s) && (l.s[l.i] == 'e' || l.s[l.i] == 'E') {
		kind = Float
		l.i++
		if l.i < len(l.s) && (l.s[l.i] == '+' || l.s[l.i] == '-') {
			l.i++
		}
		got, clean := l.digits(isDigit)
		if !got || !clean {
			return Token{}, junk()
		}
	}
	return finish(kind)
}

// lexOp lexes an operator at l.i, cutting it as PostgreSQL does.
func (l *lexer) lexOp(start int) Token {
	end := l.i
	for end < len(l.s) && strings.IndexByte(opChars, l.s[end]) >= 0 {
		end++
	}
	op := l.s[start:end]
	// A comment start ends the operator.
	if k := strings.Index(op, "--"); k >= 0 {
		op = op[:k]
	}
	if k := strings.Index(op, "/*"); k >= 0 {
		op = op[:k]
	}
	if op == "" {
		// Cannot happen (skipSpace consumed any comment), but a zero-length
		// token would never advance: take one character.
		op = l.s[start : start+1]
	}
	if len(op) > 1 && (op[len(op)-1] == '+' || op[len(op)-1] == '-') && !strings.ContainsAny(op[:len(op)-1], opNotStrip) {
		for len(op) > 1 && (op[len(op)-1] == '+' || op[len(op)-1] == '-') {
			op = op[:len(op)-1]
		}
	}
	l.i = start + len(op)
	text := op
	if text == "!=" {
		text = "<>" // PostgreSQL treats != as <>
	}
	return Token{Kind: Op, Pos: start, End: l.i, Str: text}
}
