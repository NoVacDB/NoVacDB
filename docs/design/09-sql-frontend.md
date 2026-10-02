# 09 — SQL Front End: Lexer, Parser, Errors (`internal/sql/parser`, `internal/sql/sqlerr`)

Status: **Designed for Steps 4.1 (lexer) and 4.2 (parser and AST); each part is implemented in its step. Implemented so far: 4.1.** Written and approved under the standing autonomous-mode instruction.

## 1. Problem

SQL arrives as text: from the wire protocol (Phase 5), from tests, from the sqllogictest runner (Step 4.6). It must become a syntax tree the executor can run, or fail with an error that tells the user exactly what is wrong and where.

The front end must:

- accept the PostgreSQL dialect for the statements NoVacDB supports, with PostgreSQL's lexical rules (identifier case folding, quoting, string and number literals, comments, operators), so that text written for PostgreSQL means the same thing here;
- reject everything else with a PostgreSQL-compatible error: a SQLSTATE code, a message, and the character position of the problem, plus a hint where one helps (problem #44, "cryptic error messages");
- never panic and never loop forever, whatever the input.

## 2. Design

### 2.1 Errors (`internal/sql/sqlerr`)

Every error the SQL layer reports to a client is a `*sqlerr.Error`:

| field | meaning |
|---|---|
| `Code` | the five-character SQLSTATE, e.g. `42601` (syntax error) |
| `Message` | one line, PostgreSQL's wording where PostgreSQL has a message for the case |
| `Detail`, `Hint` | optional; the hint suggests a fix |
| `Position` | 1-based **character** (not byte) position in the query text, 0 if none |

Lower layers produce plain Go errors; the SQL layer wraps them. Anything unexpected becomes `XX000` (internal error), never a wrong code. The wire protocol (Phase 5) sends these fields as they are.

### 2.2 Lexer (Step 4.1)

`parser.Lex(sql) ([]Token, error)` turns the whole text into tokens. A token has a kind, its byte range in the input, and a decoded value:

| kind | examples | value |
|---|---|---|
| `Ident` | `users`, `"Users"` | the name: unquoted names folded to lower case, quoted names as written |
| `Keyword` | `SELECT`, `select` | the keyword (an unquoted identifier that is a known keyword) |
| `String` | `'it''s'`, `E'\n'`, `$$x$$` | the string's contents after escapes |
| `Integer`, `Float` | `42`, `1.5e3`, `0x1F`, `1_000` | the literal's text (converted by the parser) |
| `Param` | `$1` | the parameter number |
| `Op` | `+`, `<=`, `<>`, `!=`, `\|\|`, `::` | the operator text |
| punctuation | `(` `)` `,` `;` `.` `[` `]` `:` | |
| `EOF` | | |

The rules, all PostgreSQL's:

- **Input** must be valid UTF-8 without NUL bytes (`22021`).
- **Whitespace** is space, tab, newline, carriage return and form feed. **Comments** are `--` to the end of the line, and `/* ... */`, which **nest**. An unterminated block comment is an error.
- **Identifiers** start with a letter, `_` or any non-ASCII character, and continue with those, digits and `$`. Unquoted identifiers are folded to lower case (ASCII letters only, as PostgreSQL does for UTF-8). **Quoted identifiers** (`"..."`, with `""` for a quote) keep their case and may hold any character but NUL. An empty quoted identifier is an error.
- **Identifier length.** PostgreSQL silently truncates names longer than 63 bytes, so two different long names can collide. NoVacDB rejects them instead (`42622`, with the limit in the message): a clear error is better than a silent rename.
- **Keywords** are unquoted identifiers matched case-insensitively against the keyword table. Each keyword has PostgreSQL's category (reserved, type/function name, column name, unreserved), which decides where the parser accepts it as a name.
- **Strings.** `'...'` with `''` for a quote; backslashes are ordinary characters (`standard_conforming_strings = on`, PostgreSQL's default). `E'...'` strings take backslash escapes: `\b \f \n \r \t`, `\\`, `\'`, octal `\o`, `\oo`, `\ooo`, hex `\xh`, `\xhh`, and Unicode `\uXXXX`, `\UXXXXXXXX` (a UTF-16 surrogate pair written as two escapes is combined, as PostgreSQL does; a lone surrogate, zero or a value past U+10FFFF is an error; and the result must be valid UTF-8). **Dollar-quoted strings** `$tag$...$tag$` take their contents literally. Two string literals separated only by whitespace **that includes a newline** are one string, as in PostgreSQL. An unterminated string is an error.
- **Numbers.** Decimal integers, decimals (`1.`, `.5`, `1.5`), exponents (`1e10`, `1.5E-3`), and PostgreSQL 16's hexadecimal, octal and binary integers (`0x1F`, `0o17`, `0b101`) and digit separators (`1_000_000`: a single `_` between digits). A number immediately followed by an identifier character is an error ("trailing junk after numeric literal"), as in PostgreSQL 15+.
- **Parameters** `$1`, `$2`, ... for the extended protocol (Phase 5).
- **Operators** are maximal runs of `+ - * / < > = ~ ! @ # % ^ & | \` ?`, cut as PostgreSQL does: a run stops before `--` or `/*`, and a multi-character operator may not end in `+` or `-` unless it also contains one of `~ ! @ # % ^ & | \` ?` (so `a+-1` is `a + -1`). `::` is the cast operator. The parser decides which operators exist; an unknown one is an error there.
- **Positions** are byte offsets in tokens and are converted to character positions only when an error is built.

### 2.3 Parser and AST (Step 4.2)

A hand-written recursive-descent parser over the token list. `parser.Parse(sql) ([]ast.Stmt, error)` parses a semicolon-separated list (empty statements are dropped; a list with none is the empty query).

**Statements:**

```
CREATE TABLE [IF NOT EXISTS] name ( column_def | table_constraint [, ...] )
    column_def: name type [NOT NULL | NULL | PRIMARY KEY | UNIQUE | DEFAULT expr] ...
    table_constraint: PRIMARY KEY ( col [, ...] ) | UNIQUE ( col [, ...] )
DROP TABLE [IF EXISTS] name
CREATE [UNIQUE] INDEX [IF NOT EXISTS] name ON table ( col [, ...] )
DROP INDEX [IF EXISTS] name
INSERT INTO table [( col [, ...] )] VALUES ( expr [, ...] ) [, ...]
SELECT [DISTINCT] target [, ...] [FROM table [[AS] alias]] [WHERE expr]
    [ORDER BY expr [ASC | DESC] [NULLS FIRST | NULLS LAST] [, ...]]
    [LIMIT expr] [OFFSET expr]
    target: * | table.* | expr [[AS] label]
UPDATE table [[AS] alias] SET col = expr [, ...] [WHERE expr]
DELETE FROM table [[AS] alias] [WHERE expr]
```

`ORDER BY` items may also be an output column number or label, as in PostgreSQL. `LIMIT ALL` and `LIMIT NULL` mean no limit.

**Types** (names as PostgreSQL accepts them): `integer`/`int`/`int4`, `bigint`/`int8`, `double precision`/`float8`/`float`, `text`, `boolean`/`bool`, `timestamptz`/`timestamp with time zone`. `timestamp` and `timestamp without time zone` are rejected with a hint to use `timestamptz` (problem #48). Other PostgreSQL types are rejected as unsupported (`0A000`).

**Expressions**, with PostgreSQL's precedence from loosest to tightest:

| level | operators | associativity |
|---|---|---|
| 1 | `OR` | left |
| 2 | `AND` | left |
| 3 | `NOT` | right (prefix) |
| 4 | `IS [NOT] NULL`, `IS [NOT] TRUE/FALSE/UNKNOWN`, `IS [NOT] DISTINCT FROM` | |
| 5 | `<` `>` `=` `<=` `>=` `<>` `!=` | none: `a < b < c` is an error |
| 6 | `[NOT] BETWEEN`, `[NOT] IN (list)`, `[NOT] LIKE`, `[NOT] ILIKE` | |
| 7 | any other operator (`\|\|`) | left |
| 8 | `+` `-` | left |
| 9 | `*` `/` `%` | left |
| 10 | `^` | left |
| 11 | unary `+` `-` | right |
| 12 | `::` (cast) | left |

Primaries: literals (integers, decimals, strings, `TRUE`, `FALSE`, `NULL`), typed literals (`timestamptz '...'`), column references (`col`, `table.col`), parameters, parenthesised expressions, `CAST(expr AS type)`, and function calls `name(args)`. The parser does not check that functions, columns or tables exist; that is the executor's job (Step 4.5), which reports the position the parser recorded.

**AST.** Every node records the byte offset where it starts. Every node prints back to SQL (`String()`), fully parenthesised and with quoted identifiers where needed, so that `Parse(String(Parse(x)))` gives the same tree: the basis of the fuzz test.

**Syntax errors** are `42601` with PostgreSQL's message, `syntax error at or near "tok"` or `syntax error at end of input`, the token's position, and a hint naming what was expected where that is useful (`expected ")"`, `expected a column type`, ...). Integer literals that do not fit in 64 bits are `22003`.

### 2.4 Limits

- Query text: up to 1 MiB (`54000`), so the token list stays small.
- Nesting: expressions nested more than 1000 deep are rejected (`54001`) instead of exhausting the stack.

## 3. Formats

None on disk. The front end is pure functions over strings.

## 4. Concurrency

None: `Lex` and `Parse` have no shared state (the keyword table is read-only after init).

## 5. Failure and crash behaviour

| Input | Result |
|---|---|
| Invalid UTF-8 or a NUL byte | `22021` at the offending character |
| Unterminated string, quoted identifier, dollar quote or comment | `42601` at its start |
| Bad escape in an `E''` string | `22025` (invalid escape sequence) at the escape |
| Name longer than 63 bytes | `42622` |
| Number followed by letters | `42601` "trailing junk after numeric literal" |
| Unknown operator | `42883` "operator does not exist" |
| Any other unexpected token | `42601` with the token's position |
| Text over 1 MiB, nesting over 1000 | `54000`, `54001` |

No input can make the lexer or parser panic or loop: every loop consumes input, and recursion is bounded by the nesting limit.

## 6. Alternatives considered

- **A parser generator (goyacc).** The project allows no dependencies, and goyacc is not part of the standard library. Generated LALR parsers also give poor error messages. A hand-written parser over a small grammar is easy to read and to extend.
- **Lexing on demand instead of a token slice.** Slightly less memory, but a slice makes lookahead trivial and the 1 MiB limit bounds its size.
- **Truncating long names like PostgreSQL.** Silent and collision-prone; an error is clearer (see 2.2).
- **A NUMERIC type for decimal literals.** Not part of Phase 4's types; decimals are `double precision` (section 7).

## 7. Testing plan

- Lexer: a table of inputs and expected token streams for every rule above, including each error; positions; keyword categories; operator cutting; comments nesting; strings joined across newlines; escapes; number forms and junk.
- `FuzzLex`: no panics; tokens are in order, do not overlap, and only whitespace and comments lie between them; re-lexing the text of each identifier, number or operator token gives the same token.
- Parser: every statement form and option; precedence and associativity (checked through the parenthesised printing); every syntax error with its code and position; the limits.
- `FuzzParse`: no panics; whatever parses prints to text that parses to the same printed form.

## 8. Limitations

- Only the statements and types above. Joins, subqueries, aggregates, `GROUP BY` (Phase 7), transactions (Phase 6), `RETURNING`, `INSERT ... SELECT`, `ALTER TABLE` (Phase 10) and arrays are not parsed yet.
- Decimal literals are `double precision`, not `numeric`.
- No `U&'...'` strings, bit strings or `B'...'`/`X'...'` literals.
