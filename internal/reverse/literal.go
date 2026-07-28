package reverse

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/learttytyri/mulligan/internal/change"
)

// literal renders a column value as a MySQL literal.
//
// Text is quoted only when it is unambiguous to do so; anything else becomes a
// hex literal. See safeToQuote for why.
func literal(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "NULL", nil

	case bool:
		if x {
			return "1", nil
		}
		return "0", nil

	case int:
		return strconv.FormatInt(int64(x), 10), nil
	case int8:
		return strconv.FormatInt(int64(x), 10), nil
	case int16:
		return strconv.FormatInt(int64(x), 10), nil
	case int32:
		return strconv.FormatInt(int64(x), 10), nil
	case int64:
		return strconv.FormatInt(x, 10), nil

	case uint:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint8:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint16:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint32:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint64:
		return strconv.FormatUint(x, 10), nil

	case float32:
		return strconv.FormatFloat(float64(x), 'g', -1, 32), nil
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64), nil

	case change.Raw:
		// This is the only value that reaches the script without quoting, which
		// makes it the only place a source adapter could put text straight into a
		// statement an operator runs by hand. Restrict it to what it exists for.
		if !change.ValidRaw(string(x)) {
			return "", fmt.Errorf("reverse: raw value %q is not a number; raw values are emitted unquoted and must not carry arbitrary SQL", string(x))
		}
		return string(x), nil

	case string:
		return quoteString(x), nil

	// TEXT and BLOB are one type in the log, distinguishable only by the column
	// charset, so both arrive as bytes. Bytes that read as text are quoted like
	// text: under the SET NAMES the script emits, such a literal stores exactly
	// those bytes in either kind of column, and the reviewer can read what the
	// statement is about to write. Anything else still falls back to hex.
	case []byte:
		return quoteString(string(x)), nil

	case time.Time:
		return quoteTime(x), nil
	}

	return "", fmt.Errorf("reverse: unsupported value type %T", v)
}

// quoteString wraps text in single quotes, doubling any quote it contains.
// Text that cannot be quoted unambiguously becomes a hex literal instead.
func quoteString(s string) string {
	if !safeToQuote(s) {
		return hexLiteral([]byte(s))
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// safeToQuote reports whether s can be rendered as a quoted string that means
// the same thing under every sql_mode and every connection charset.
//
// Doubling a single quote is standard SQL and always safe. A backslash is not:
// MySQL treats it as an escape by default but as a literal character under
// NO_BACKSLASH_ESCAPES, so a quoted string containing one would round-trip
// differently depending on server configuration. Control characters are
// excluded for the same reason — and because a raw newline inside generated SQL
// makes the output unreadable for the human who has to review it.
//
// Bytes that do not form valid UTF-8 came out of a column in some other
// charset. Quoted, they would reach the applying session as text it reads in
// its own charset and silently converts, storing something other than what was
// logged. As hex they are written back byte for byte whatever the session is
// set to. Valid UTF-8 stays quoted so the script remains readable, which is
// what the accompanying SET NAMES in the generated header is for.
func safeToQuote(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' || c < 0x20 || c == 0x7f {
			return false
		}
	}
	return utf8.ValidString(s)
}

// hexLiteral renders bytes as X'...', which every sql_mode reads identically.
func hexLiteral(b []byte) string {
	return fmt.Sprintf("X'%X'", b)
}

// quoteTime renders a timestamp in MySQL's DATETIME syntax, keeping fractional
// seconds only when the value carries them.
//
// The value is normalized to UTC first. A DATETIME arrives already carrying no
// zone, so this changes nothing for it; a TIMESTAMP arrives as an instant in
// whatever zone the machine reading the log happens to sit in, and rendering
// that machine's wall clock would write back a time shifted by its offset. The
// generated script pins the session zone to match.
func quoteTime(t time.Time) string {
	t = t.UTC()

	layout := "2006-01-02 15:04:05"
	if t.Nanosecond() != 0 {
		layout = "2006-01-02 15:04:05.000000"
	}
	return "'" + t.Format(layout) + "'"
}
