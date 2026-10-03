// Package keyword holds the SQL keyword table: every keyword the lexer
// recognises and its PostgreSQL category. See docs/design/09-sql-frontend.md.
package keyword

// Category is PostgreSQL's classification of a keyword, which decides
// where it may be used as a name.
type Category uint8

// Keyword categories, from most to least restricted.
const (
	// Reserved keywords are never names unless quoted.
	Reserved Category = iota + 1
	// TypeFuncName keywords may name functions and types, not columns or
	// tables.
	TypeFuncName
	// ColName keywords may name columns and tables, not functions or types.
	ColName
	// Unreserved keywords may be used as any name.
	Unreserved
)

// keywords maps every keyword the lexer recognises to its category. It
// holds all of PostgreSQL's reserved and type/function-name keywords, so
// that names PostgreSQL rejects are rejected here too, and the column-name
// and unreserved keywords NoVacDB's grammar uses. Any other word is an
// ordinary identifier, which behaves like an unreserved keyword.
var keywords = map[string]Category{
	// Reserved.
	"all": Reserved, "analyse": Reserved, "analyze": Reserved, "and": Reserved,
	"any": Reserved, "array": Reserved, "as": Reserved, "asc": Reserved,
	"asymmetric": Reserved, "both": Reserved, "case": Reserved, "cast": Reserved,
	"check": Reserved, "collate": Reserved, "column": Reserved, "constraint": Reserved,
	"create": Reserved, "current_catalog": Reserved, "current_date": Reserved,
	"current_role": Reserved, "current_time": Reserved, "current_timestamp": Reserved,
	"current_user": Reserved, "default": Reserved, "deferrable": Reserved,
	"desc": Reserved, "distinct": Reserved, "do": Reserved, "else": Reserved,
	"end": Reserved, "except": Reserved, "false": Reserved, "fetch": Reserved,
	"for": Reserved, "foreign": Reserved, "from": Reserved, "grant": Reserved,
	"group": Reserved, "having": Reserved, "in": Reserved, "initially": Reserved,
	"intersect": Reserved, "into": Reserved, "lateral": Reserved, "leading": Reserved,
	"limit": Reserved, "localtime": Reserved, "localtimestamp": Reserved, "not": Reserved,
	"null": Reserved, "offset": Reserved, "on": Reserved, "only": Reserved,
	"or": Reserved, "order": Reserved, "placing": Reserved, "primary": Reserved,
	"references": Reserved, "returning": Reserved, "select": Reserved,
	"session_user": Reserved, "some": Reserved, "symmetric": Reserved,
	"system_user": Reserved, "table": Reserved, "then": Reserved, "to": Reserved,
	"trailing": Reserved, "true": Reserved, "union": Reserved, "unique": Reserved,
	"user": Reserved, "using": Reserved, "variadic": Reserved, "when": Reserved,
	"where": Reserved, "window": Reserved, "with": Reserved,

	// Type and function names.
	"authorization": TypeFuncName, "binary": TypeFuncName, "collation": TypeFuncName,
	"concurrently": TypeFuncName, "cross": TypeFuncName, "current_schema": TypeFuncName,
	"freeze": TypeFuncName, "full": TypeFuncName, "ilike": TypeFuncName,
	"inner": TypeFuncName, "is": TypeFuncName, "isnull": TypeFuncName,
	"join": TypeFuncName, "left": TypeFuncName, "like": TypeFuncName,
	"natural": TypeFuncName, "notnull": TypeFuncName, "outer": TypeFuncName,
	"overlaps": TypeFuncName, "right": TypeFuncName, "similar": TypeFuncName,
	"tablesample": TypeFuncName, "verbose": TypeFuncName,

	// Column names (the ones the grammar uses).
	"between": ColName, "bigint": ColName, "boolean": ColName, "coalesce": ColName,
	"exists": ColName, "float": ColName, "greatest": ColName, "int": ColName,
	"integer": ColName, "least": ColName, "nullif": ColName, "precision": ColName,
	"time": ColName, "timestamp": ColName, "values": ColName,

	// Unreserved (the ones the grammar uses).
	"by": Unreserved, "delete": Unreserved, "double": Unreserved, "drop": Unreserved,
	"first": Unreserved, "if": Unreserved, "index": Unreserved, "insert": Unreserved,
	"key": Unreserved, "last": Unreserved, "nulls": Unreserved, "set": Unreserved,
	"unknown": Unreserved, "update": Unreserved, "without": Unreserved, "zone": Unreserved,
}

// Lookup returns the category of a lower-case word, if it is a keyword.
func Lookup(word string) (Category, bool) {
	c, ok := keywords[word]
	return c, ok
}
