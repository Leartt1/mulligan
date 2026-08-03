package reverse

import (
	"fmt"
	"strings"
	"time"
)

// Display renders a column value as a person should read it: the text the
// generated script would write, without the SQL quoting that makes it parse.
//
// It exists so a diff preview and the statement it previews cannot disagree.
// The preview is what a human looks at before running something destructive, so
// a value that reads as almost-right there — a rounded decimal, bytes shown as
// mojibake — would earn approval for a statement that writes something else.
// Everything the script renders as hex is rendered as hex here for the same
// reason: what is shown is what will be written.
//
// NULL is not accepted. It is the absence of a value rather than one with a
// rendering, and turning it into the text "NULL" would make it indistinguishable
// from a column holding that word. Callers represent it in their own terms — as
// a JSON null, ordinarily.
func Display(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "", fmt.Errorf("reverse: NULL has no display form; the caller represents it")

	case string:
		return displayText(x), nil
	case []byte:
		return displayText(string(x)), nil

	case time.Time:
		return strings.Trim(quoteTime(x), "'"), nil
	}

	// Numbers, booleans and raw values render the same quoted or not, so the SQL
	// rendering is already the readable one — and Raw keeps its validation.
	return literal(v)
}

// displayText shows text as written, or as hex when the script would not quote
// it. The two must agree; see Display.
func displayText(s string) string {
	if !safeToQuote(s) {
		return hexLiteral([]byte(s))
	}
	return s
}
