// Package parser turns SQL text into a syntax tree: a lexer with
// PostgreSQL's lexical rules (lex.go) and a recursive-descent parser
// (parse.go) producing the trees of package ast. See docs/design/09-sql-frontend.md.
package parser
