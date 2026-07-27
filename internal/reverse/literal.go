package reverse

import (
	"fmt"
	"strconv"
	"strings"
	"time"

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
		return string(x), nil

	case string:
		return quoteString(x), nil
	case []byte:
		return hexLiteral(x), nil

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
// the same thing under every sql_mode.
//
// Doubling a single quote is standard SQL and always safe. A backslash is not:
// MySQL treats it as an escape by default but as a literal character under
// NO_BACKSLASH_ESCAPES, so a quoted string containing one would round-trip
// differently depending on server configuration. Control characters are
// excluded for the same reason — and because a raw newline inside generated SQL
// makes the output unreadable for the human who has to review it.
func safeToQuote(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' || c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// hexLiteral renders bytes as X'...', which every sql_mode reads identically.
func hexLiteral(b []byte) string {
	return fmt.Sprintf("X'%X'", b)
}

// quoteTime renders a timestamp in MySQL's DATETIME syntax, keeping fractional
// seconds only when the value carries them.
func quoteTime(t time.Time) string {
	layout := "2006-01-02 15:04:05"
	if t.Nanosecond() != 0 {
		layout = "2006-01-02 15:04:05.000000"
	}
	return "'" + t.Format(layout) + "'"
}
