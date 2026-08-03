package reverse

import (
	"strings"
	"testing"
	"time"

	"github.com/learttytyri/mulligan/internal/change"
)

func TestDisplayShowsTextWithoutSQLQuoting(t *testing.T) {
	got, err := Display("pending")
	if err != nil {
		t.Fatalf("Display returned error: %v", err)
	}
	if got != "pending" {
		t.Errorf("Display = %q, want the text as written", got)
	}
}

// A quote inside text is doubled in SQL so the statement parses. Doubling it in
// a preview would show a value that is not the one stored, and someone
// comparing the two would be comparing against a rendering artifact.
func TestDisplayDoesNotDoubleQuotesTheWaySQLDoes(t *testing.T) {
	got, err := Display("o'brien")
	if err != nil {
		t.Fatalf("Display returned error: %v", err)
	}
	if got != "o'brien" {
		t.Errorf("Display = %q, want o'brien", got)
	}

	// The SQL rendering still doubles it; these are deliberately different.
	lit, err := literal("o'brien")
	if err != nil {
		t.Fatalf("literal returned error: %v", err)
	}
	if lit != "'o''brien'" {
		t.Errorf("literal = %q, want the doubled form", lit)
	}
}

// Values the script writes as hex are shown as hex. The preview and the
// statement have to agree about what is going to be written, and a byte string
// rendered as mojibake in one and hex in the other invites approving a change
// on the strength of a rendering that is not what runs.
func TestDisplayShowsUnquotableBytesAsHex(t *testing.T) {
	for name, value := range map[string]any{
		"invalid utf-8":     []byte{0xff, 0xfe},
		"control character": "line\nbreak",
		"backslash":         `C:\tmp`,
	} {
		got, err := Display(value)
		if err != nil {
			t.Fatalf("%s: Display returned error: %v", name, err)
		}
		if !strings.HasPrefix(got, "X'") {
			t.Errorf("%s: Display = %q, want a hex literal", name, got)
		}

		lit, err := literal(value)
		if err != nil {
			t.Fatalf("%s: literal returned error: %v", name, err)
		}
		if got != lit {
			t.Errorf("%s: Display = %q but the script writes %q", name, got, lit)
		}
	}
}

// The motivating case for Raw: routing a DECIMAL through a float rounds it, and
// a preview showing a rounded amount is how someone approves restoring the
// wrong number.
func TestDisplayShowsADecimalExactly(t *testing.T) {
	got, err := Display(change.Raw("19.99"))
	if err != nil {
		t.Fatalf("Display returned error: %v", err)
	}
	if got != "19.99" {
		t.Errorf("Display = %q, want 19.99", got)
	}
}

func TestDisplayShowsATimeInUTCWithoutQuotes(t *testing.T) {
	got, err := Display(time.Date(2026, 7, 25, 17, 20, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Display returned error: %v", err)
	}
	if got != "2026-07-25 17:20:00" {
		t.Errorf("Display = %q, want the datetime unquoted", got)
	}
}

func TestDisplayShowsNumbersAsDigits(t *testing.T) {
	got, err := Display(int64(-42))
	if err != nil {
		t.Fatalf("Display returned error: %v", err)
	}
	if got != "-42" {
		t.Errorf("Display = %q, want -42", got)
	}
}

// NULL is not a value with a rendering; it is the absence of one, and a caller
// that turned it into the text "NULL" would be indistinguishable from a column
// holding that word.
func TestDisplayRefusesNull(t *testing.T) {
	if _, err := Display(nil); err == nil {
		t.Error("Display rendered NULL as though it were a value")
	}
}
