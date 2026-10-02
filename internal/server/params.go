package server

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
)

// maxNameLen is PostgreSQL's NAMEDATALEN-1: application_name is cut to it.
const maxNameLen = 63

// session holds what the startup message settled.
type session struct {
	user, database  string
	applicationName string
	clientEncoding  string // canonical: UTF8 or SQL_ASCII
	ignored         []string
}

// setParam applies one startup parameter (or one -c setting from options)
// to s, with the rules of design doc section 2.3. Names are
// case-insensitive, as PostgreSQL's settings are.
func (s *session) setParam(name, value string) *sqlerr.Error {
	switch strings.ToLower(name) {
	case "application_name":
		s.applicationName = truncate(value, maxNameLen)
	case "client_encoding":
		enc, ok := canonicalEncoding(value)
		if !ok {
			return invalidValue(name, value)
		}
		s.clientEncoding = enc
	case "datestyle":
		if !isoDateStyle(value) {
			return invalidValue(name, value).WithHint("NoVacDB supports only the ISO date style.")
		}
	case "timezone":
		if !isUTC(value) {
			return sqlerr.New(sqlerr.FeatureNotSupported, "time zone %q is not supported yet: the session time zone is always UTC", value)
		}
	case "extra_float_digits":
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return invalidValue(name, value)
		}
		if n < -15 || n > 3 {
			return sqlerr.New(sqlerr.InvalidParameterValue, "%d is outside the valid range for parameter %q (-15 .. 3)", n, name)
		}
	case "search_path", "statement_timeout", "lock_timeout":
		// Accepted and ignored: no schemas or timeouts yet, so nothing a
		// client can observe depends on them.
		s.ignored = append(s.ignored, name)
	default:
		return sqlerr.New(sqlerr.UndefinedObject, "unrecognized configuration parameter %q", name)
	}
	return nil
}

func invalidValue(name, value string) *sqlerr.Error {
	return sqlerr.New(sqlerr.InvalidParameterValue, "invalid value for parameter %q: %q", name, value)
}

// truncate cuts s to at most n bytes without splitting a character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// canonicalEncoding accepts UTF-8 under its PostgreSQL names, and
// SQL_ASCII, which PostgreSQL also accepts with a UTF-8 server: it means
// "no conversion", and libpq sends it from terminals in the C locale.
func canonicalEncoding(v string) (string, bool) {
	norm := strings.NewReplacer("-", "", "_", "").Replace(strings.ToLower(strings.TrimSpace(v)))
	switch norm {
	case "utf8", "unicode":
		return "UTF8", true
	case "sqlascii":
		return "SQL_ASCII", true
	}
	return "", false
}

// isoDateStyle reports whether a DateStyle value keeps ISO output: its
// words are ISO and field orders only (any case, commas or spaces).
func isoDateStyle(v string) bool {
	words := strings.FieldsFunc(strings.ToLower(v), func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
	if len(words) == 0 {
		return false
	}
	for _, w := range words {
		switch w {
		case "iso", "mdy", "dmy", "ymd", "us", "noneuropean", "euro", "european":
		default:
			return false
		}
	}
	return true
}

// isUTC reports whether a TimeZone value names UTC.
func isUTC(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "utc", "etc/utc", "gmt", "etc/gmt", "z", "+00", "0":
		return true
	}
	return false
}

// splitOptions splits the startup "options" parameter as libpq's
// command-line options are split: on unescaped whitespace, with a
// backslash escaping the next character.
func splitOptions(s string) []string {
	var out []string
	var cur strings.Builder
	inWord, escaped := false, false
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped, inWord = false, true
		case r == '\\':
			escaped, inWord = true, true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}

// applyOptions applies the settings in an options parameter: -c
// name=value (or -cname=value) and --name=value, where dashes in a name
// stand for underscores.
func (s *session) applyOptions(options string) *sqlerr.Error {
	args := splitOptions(options)
	for i := 0; i < len(args); i++ {
		a := args[i]
		var setting string
		switch {
		case a == "-c" && i+1 < len(args):
			i++
			setting = args[i]
		case strings.HasPrefix(a, "-c") && len(a) > 2:
			setting = a[2:]
		case strings.HasPrefix(a, "--") && len(a) > 2:
			setting = a[2:]
		default:
			return badOption(a)
		}
		name, value, ok := strings.Cut(setting, "=")
		if !ok || name == "" {
			return badOption(a)
		}
		if strings.HasPrefix(a, "--") {
			name = strings.ReplaceAll(name, "-", "_")
		}
		if err := s.setParam(name, value); err != nil {
			return err
		}
	}
	return nil
}

func badOption(a string) *sqlerr.Error {
	return sqlerr.New(sqlerr.SyntaxError, "invalid command-line argument for server process: %s", a).
		WithHint("Settings are given as -c name=value or --name=value.")
}
