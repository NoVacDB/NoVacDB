// Package parser turns SQL text into a syntax tree: a lexer with
// PostgreSQL's lexical rules (this file's neighbours lex.go and keywords.go)
// and a recursive-descent parser. See docs/design/09-sql-frontend.md.
package parser
