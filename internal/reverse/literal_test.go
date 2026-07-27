package reverse

import (
	"testing"
	"time"

	"github.com/learttytyri/mulligan/internal/change"
)

// Values arriving from a binlog are arbitrary user data. Encoding them wrongly
// either produces SQL that will not parse or, worse, SQL that parses into a
// different statement than intended.
func TestLiteralEncodesValuesForMySQL(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"nil is NULL, not the string 'nil'", nil, "NULL"},
		{"signed integer", int64(-42), "-42"},
		{"unsigned integer", uint64(18446744073709551615), "18446744073709551615"},
		{"float keeps full precision", float64(1.5), "1.5"},
		{"float without fraction is not truncated to int", float64(2), "2"},
		{"bool true is 1", true, "1"},
		{"bool false is 0", false, "0"},
		{"plain string", "pending", "'pending'"},
		{"empty string", "", "''"},
		{"single quote is doubled, valid in every sql_mode", "it's", "'it''s'"},

		// A backslash means two different things depending on
		// NO_BACKSLASH_ESCAPES, so the value is emitted as a hex literal, which
		// is unambiguous in both modes.
		{"backslash falls back to hex", `C:\tmp`, "X'433A5C746D70'"},
		{"newline falls back to hex", "a\nb", "X'610A62'"},
		{"NUL byte falls back to hex", "a\x00b", "X'610062'"},

		{"binary column is hex", []byte{0xde, 0xad, 0xbe, 0xef}, "X'DEADBEEF'"},
		{"empty binary column", []byte{}, "X''"},

		// A DECIMAL amount is carried as pre-rendered text and emitted as-is;
		// quoting it would compare a string against a numeric column, and
		// routing it through float64 would round it.
		{"raw value is emitted verbatim", change.Raw("1234.56"), "1234.56"},
		{"raw negative value", change.Raw("-0.001"), "-0.001"},
		{"raw value keeps trailing zeros of its scale", change.Raw("10.00"), "10.00"},

		{"datetime", time.Date(2026, 7, 27, 13, 45, 5, 0, time.UTC), "'2026-07-27 13:45:05'"},
		{"datetime keeps microseconds", time.Date(2026, 7, 27, 13, 45, 5, 123456000, time.UTC), "'2026-07-27 13:45:05.123456'"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := literal(tt.in)
			if err != nil {
				t.Fatalf("literal(%#v) returned error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("literal(%#v) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}

// An unknown type must not be silently coerced into a literal that happens to
// look plausible — a wrong value in generated SQL is worse than a refusal.
func TestLiteralRejectsUnknownType(t *testing.T) {
	if got, err := literal(struct{ A int }{1}); err == nil {
		t.Fatalf("literal() = %q, want error for unsupported type", got)
	}
}
