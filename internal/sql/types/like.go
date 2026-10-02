package types

import (
	"unicode/utf8"

	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
)

// Lower lower-cases ASCII letters only, as PostgreSQL does in the C
// collation (the only one in Phase 4).
func Lower(s string) string { return mapASCII(s, 'A', 'Z', 'a'-'A') }

// Upper upper-cases ASCII letters only.
func Upper(s string) string { return mapASCII(s, 'a', 'z', 'A'-'a') }

func mapASCII(s string, lo, hi byte, delta int) string {
	for i := 0; i < len(s); i++ {
		if s[i] >= lo && s[i] <= hi {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if b[j] >= lo && b[j] <= hi {
					b[j] = byte(int(b[j]) + delta)
				}
			}
			return string(b)
		}
	}
	return s
}

// likeItem is one element of a compiled LIKE pattern.
type likeItem struct {
	kind byte // '%' any run, '_' one character, 'c' a literal character
	r    rune
}

// compileLike parses a LIKE pattern: % matches any run of characters, _
// any one character, and a backslash makes the next character literal.
func compileLike(pattern string) ([]likeItem, error) {
	var items []likeItem
	for i := 0; i < len(pattern); {
		r, size := utf8.DecodeRuneInString(pattern[i:])
		i += size
		switch r {
		case '%':
			if len(items) > 0 && items[len(items)-1].kind == '%' {
				continue // %% is the same as %
			}
			items = append(items, likeItem{kind: '%'})
		case '_':
			items = append(items, likeItem{kind: '_'})
		case '\\':
			if i >= len(pattern) {
				return nil, sqlerr.New(sqlerr.InvalidEscapeSequence, "LIKE pattern must not end with escape character")
			}
			r, size = utf8.DecodeRuneInString(pattern[i:])
			i += size
			items = append(items, likeItem{kind: 'c', r: r})
		default:
			items = append(items, likeItem{kind: 'c', r: r})
		}
	}
	return items, nil
}

// Like reports whether s matches the LIKE pattern; with caseInsensitive
// (ILIKE) ASCII letters match regardless of case.
func Like(s, pattern string, caseInsensitive bool) (bool, error) {
	if caseInsensitive {
		s, pattern = Lower(s), Lower(pattern)
	}
	items, err := compileLike(pattern)
	if err != nil {
		return false, err
	}
	runes := []rune(s)
	// Classic wildcard matching: on a mismatch, backtrack to the last %
	// and let it absorb one more character. O(len(s) * len(pattern)).
	si, pi := 0, 0
	starP, starS := -1, 0
	for si < len(runes) {
		switch {
		case pi < len(items) && items[pi].kind == '%':
			starP, starS = pi, si
			pi++
		case pi < len(items) && (items[pi].kind == '_' || items[pi].r == runes[si]):
			si++
			pi++
		case starP >= 0:
			starS++
			si, pi = starS, starP+1
		default:
			return false, nil
		}
	}
	for pi < len(items) && items[pi].kind == '%' {
		pi++
	}
	return pi == len(items), nil
}
