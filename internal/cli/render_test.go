package cli

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/learttytyri/mulligan/internal/change"
	"github.com/learttytyri/mulligan/internal/reverse"
)

func reversal(op change.Op, table string, pos uint32, stmt string) reverse.Reversal {
	return reverse.Reversal{
		Event: change.Event{
			Schema:  "shop",
			Table:   table,
			Op:      op,
			LogFile: "binlog.000004",
			LogPos:  pos,
			At:      time.Date(2026, 7, 27, 12, 3, 11, 0, time.UTC),
		},
		Statement: stmt,
	}
}

func TestRenderWritesEachStatementUnderItsProvenance(t *testing.T) {
	plan := []reverse.Reversal{
		reversal(change.Update, "orders", 4242, "UPDATE `shop`.`orders` SET `status` = 'pending' WHERE `id` = 7 LIMIT 1;"),
	}

	var out strings.Builder
	if err := Render(&out, "binlog.000004", change.Filter{}, plan); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	got := out.String()

	for _, want := range []string{
		"UPDATE shop.orders",         // which logged statement is being undone
		"binlog.000004:4242",         // where to find it in the log
		"2026-07-27 12:03:11 UTC",    // when it ran
		"UPDATE `shop`.`orders` SET", // the reversal itself
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

// The whole safety model is that a human reads this before running it, so the
// script has to say plainly that nothing has happened yet.
func TestRenderWarnsThatNothingHasBeenExecuted(t *testing.T) {
	plan := []reverse.Reversal{reversal(change.Delete, "orders", 100, "INSERT INTO `shop`.`orders` (`id`) VALUES (7);")}

	var out strings.Builder
	if err := Render(&out, "binlog.000004", change.Filter{}, plan); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}

	if !strings.Contains(out.String(), "REVIEW BEFORE RUNNING") {
		t.Errorf("output carries no review warning:\n%s", out.String())
	}
}

// Timestamps are rendered as UTC and text as utf8mb4, so the script has to put
// the session into those terms before running anything. Without it a session in
// another zone stores TIMESTAMP values shifted by its offset, and one in another
// charset converts the text on the way in — both silently.
func TestRenderPinsSessionTimeZoneAndCharsetBeforeAnyStatement(t *testing.T) {
	plan := []reverse.Reversal{
		reversal(change.Update, "orders", 4242, "UPDATE `shop`.`orders` SET `status` = 'pending' WHERE `id` = 7 LIMIT 1;"),
	}

	var out strings.Builder
	if err := Render(&out, "binlog.000004", change.Filter{}, plan); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	got := out.String()

	for _, want := range []string{"SET time_zone = '+00:00';", "SET NAMES utf8mb4;"} {
		at := strings.Index(got, want)
		if at == -1 {
			t.Errorf("script does not pin the session with %q:\n%s", want, got)
			continue
		}
		if stmt := strings.Index(got, "UPDATE `shop`"); at > stmt {
			t.Errorf("%q comes after the first statement, too late to apply:\n%s", want, got)
		}
	}
}

// An empty result is a real answer — the filter matched nothing — and must not
// look like a script that happens to do nothing.
func TestRenderSaysSoWhenNothingMatched(t *testing.T) {
	var out strings.Builder
	if err := Render(&out, "binlog.000004", change.Filter{}, nil); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	got := out.String()

	if !strings.Contains(got, "no matching changes") {
		t.Errorf("output does not report an empty result:\n%s", got)
	}
	if strings.Contains(got, "REVIEW BEFORE RUNNING") {
		t.Errorf("empty result carries a review warning for a script with no statements:\n%s", got)
	}
}

func TestRenderCountsTheStatements(t *testing.T) {
	plan := []reverse.Reversal{
		reversal(change.Insert, "orders", 100, "DELETE FROM `shop`.`orders` WHERE `id` = 1 LIMIT 1;"),
		reversal(change.Insert, "orders", 200, "DELETE FROM `shop`.`orders` WHERE `id` = 2 LIMIT 1;"),
	}

	var out strings.Builder
	if err := Render(&out, "binlog.000004", change.Filter{}, plan); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}

	if !strings.Contains(out.String(), "2 statements") {
		t.Errorf("output does not state the statement count:\n%s", out.String())
	}
}

// Order carries meaning — reversals run newest first — so rendering must not
// reshuffle what the engine ordered.
func TestRenderPreservesPlanOrder(t *testing.T) {
	plan := []reverse.Reversal{
		reversal(change.Insert, "orders", 200, "DELETE FROM `shop`.`orders` WHERE `id` = 2 LIMIT 1;"),
		reversal(change.Insert, "orders", 100, "DELETE FROM `shop`.`orders` WHERE `id` = 1 LIMIT 1;"),
	}

	var out strings.Builder
	if err := Render(&out, "binlog.000004", change.Filter{}, plan); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	got := out.String()

	first := strings.Index(got, "`id` = 2")
	second := strings.Index(got, "`id` = 1")
	if first == -1 || second == -1 {
		t.Fatalf("output missing one of the statements:\n%s", got)
	}
	if first > second {
		t.Errorf("statements were reordered:\n%s", got)
	}
}

// A SQL line comment ends at the first newline, and everything a provenance
// comment interpolates is untrusted: MySQL permits an identifier containing a
// newline, and a logged statement carries them routinely. A value that ends its
// own comment turns its continuation lines into statements in a script a human
// runs by hand against production — most likely a syntax error partway through,
// leaving a half-applied revert. This is the property the whole fix rests on.
func TestCommentSafeNeverEmitsANewline(t *testing.T) {
	hostile := []string{
		"orders\nDROP TABLE `shop`.`customers`;",
		"orders\r\nDROP TABLE `shop`.`customers`;",
		"orders\rDROP TABLE `shop`.`customers`;",
		"\n",
		"\r",
		"\n\n\n",
		"UPDATE orders\n  SET status = 'shipped'\n  WHERE id = 7",
		"a\x00b",
		"\x1b[2J",
		"tab\tseparated",
		"line\u2028separator",
		"paragraph\u2029separator",
		"h\xe9llo",
		"\xff\xfe\x00\x00",
		strings.Repeat("x", 4096) + "\nDROP TABLE `shop`.`customers`;",
		strings.Repeat("\n", 4096),
	}

	for i, in := range hostile {
		t.Run(fmt.Sprintf("hostile %d", i), func(t *testing.T) {
			got := commentSafe(in)
			if strings.ContainsAny(got, "\n\r") {
				t.Errorf("commentSafe(%q) = %q, which ends the comment it sits on", in, got)
			}
		})
	}
}

// Withholding text is only acceptable if the reviewer can tell it happened and
// how much is missing; a silently empty comment reads like there was nothing to
// say. The byte count is the source string's, not the placeholder's.
func TestCommentSafeWithholdsUnprintableTextAndSaysHowMuch(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"newline", "a\nb", "<unprintable, 3 bytes>"},
		{"carriage return", "a\rb", "<unprintable, 3 bytes>"},
		{"NUL", "\x00", "<unprintable, 1 byte>"},
		{"tab", "a\tb", "<unprintable, 3 bytes>"},
		{"terminal escape sequence", "\x1b[31m", "<unprintable, 5 bytes>"},
		{"DEL", "a\x7fb", "<unprintable, 3 bytes>"},
		{"table name with an embedded newline", "orders\nDROP TABLE users", "<unprintable, 23 bytes>"},

		// Bytes that are not valid UTF-8 came out of a column or an identifier in
		// some other charset. There is no way to render them readably without
		// guessing which one, and a wrong guess puts unreviewed text on the line.
		{"latin1 bytes are not valid utf8", "h\xe9llo", "<unprintable, 5 bytes>"},
		{"lone continuation byte", "a\x80b", "<unprintable, 3 bytes>"},
		{"truncated multi-byte sequence", "a\xf0\x9f", "<unprintable, 3 bytes>"},

		// U+2028 and U+2029 are not newlines to MySQL, but plenty of editors and
		// log viewers break lines on them, so a reviewer would see the comment end
		// where the script does not.
		{"line separator", "a\u2028b", "<unprintable, 5 bytes>"},
		{"paragraph separator", "a\u2029b", "<unprintable, 5 bytes>"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := commentSafe(tt.in); got != tt.want {
				t.Errorf("commentSafe(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// Hex-encoding ordinary text would defeat the point of the comment: the operator
// is being asked to decide whether to run the script, and cannot decide about a
// table name they have to decode first. Anything printable and single-line —
// including scripts that need more than one byte per character — goes through
// untouched.
func TestCommentSafeLeavesReadableTextAlone(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"identifier", "orders", "orders"},
		{"empty", "", ""},
		{"statement text", "UPDATE orders SET status = 'shipped'", "UPDATE orders SET status = 'shipped'"},
		{"log file name", "binlog.000004", "binlog.000004"},
		{"spaces and punctuation", "shop-01, replica #2 (eu-west)", "shop-01, replica #2 (eu-west)"},
		{"accented word", "commandé", "commandé"},
		{"non-latin script", "注文テーブル", "注文テーブル"},
		{"emoji", "orders 🌍", "orders 🌍"},
		{"mixed multi-byte", "héllo → 🌍", "héllo → 🌍"},
		{"comment marker in the text is harmless, it is already a comment", "-- not a new line", "-- not a new line"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := commentSafe(tt.in); got != tt.want {
				t.Errorf("commentSafe(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// A migration or ORM statement can run to megabytes. Pasting one into a comment
// buries every other statement in the script, so it is cut — but a silent cut
// would let a reviewer believe they read the whole statement when they read a
// prefix of it, which is exactly the sort of thing this script exists to avoid.
func TestCommentSafeTruncatesOverlongTextAndSaysItDid(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			"single-byte text is cut at the cap",
			strings.Repeat("a", 500),
			strings.Repeat("a", 200) + "… <truncated, 500 bytes total>",
		},
		{
			// Cutting at byte 200 would land inside the 50th globe (bytes 197-200)
			// and leave the comment holding half a rune, which is no longer valid
			// UTF-8 and renders as a replacement character.
			"cut backs off to a rune boundary",
			"a" + strings.Repeat("🌍", 100),
			"a" + strings.Repeat("🌍", 49) + "… <truncated, 401 bytes total>",
		},
		{
			"text exactly at the cap is left alone",
			strings.Repeat("a", 200),
			strings.Repeat("a", 200),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := commentSafe(tt.in)
			if got != tt.want {
				t.Errorf("commentSafe(%d bytes) = %q, want %q", len(tt.in), got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("commentSafe(%d bytes) is not valid UTF-8: %q", len(tt.in), got)
			}
		})
	}
}

// The failure this file exists to prevent, end to end: a table name carrying a
// newline must not add a line to the script. MySQL accepts such a name, so this
// is reachable without anyone doing anything exotic.
func TestRenderKeepsATableNameWithANewlineOnOneCommentLine(t *testing.T) {
	plan := []reverse.Reversal{
		reversal(change.Update, "orders\nDROP TABLE `shop`.`customers`; -- ", 4242,
			"UPDATE `shop`.`orders` SET `status` = 'pending' WHERE `id` = 7 LIMIT 1;"),
	}

	var out strings.Builder
	if err := Render(&out, "binlog.000004", change.Filter{}, plan); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	got := out.String()

	if strings.Contains(got, "DROP TABLE") {
		t.Errorf("table name escaped its comment and reached the script:\n%s", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "DROP") {
			t.Errorf("script contains a line the operator would run: %q\n%s", line, got)
		}
	}
}

// The originating statement is the one thing that tells a reviewer what the
// change was for. Rendering it is the point of the Query field.
func TestRenderShowsTheStatementThatCausedTheChange(t *testing.T) {
	r := reversal(change.Update, "orders", 576, "UPDATE `shop`.`orders` SET `status` = 'pending' WHERE `id` = 7 LIMIT 1;")
	r.Event.Query = "UPDATE orders SET status = 'shipped'"
	plan := []reverse.Reversal{r}

	var out strings.Builder
	if err := Render(&out, "binlog.000004", change.Filter{}, plan); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	got := out.String()

	if !strings.Contains(got, "-- caused by: UPDATE orders SET status = 'shipped'") {
		t.Errorf("output does not attribute the change to its statement:\n%s", got)
	}
}

// MySQL logs the query only under binlog_rows_query_log_events and MariaDB only
// for annotated rows, so most events carry none. An absent statement must show
// as absent: a "caused by" line borrowed from the neighbouring event would tell
// the operator this change came from a statement that did not cause it.
func TestRenderOmitsCausedByLineWhenTheEventCarriesNoQuery(t *testing.T) {
	withQuery := reversal(change.Update, "orders", 576, "UPDATE `shop`.`orders` SET `status` = 'pending' WHERE `id` = 7 LIMIT 1;")
	withQuery.Event.Query = "UPDATE orders SET status = 'shipped'"
	withoutQuery := reversal(change.Insert, "orders", 800, "DELETE FROM `shop`.`orders` WHERE `id` = 9 LIMIT 1;")

	var out strings.Builder
	if err := Render(&out, "binlog.000004", change.Filter{}, []reverse.Reversal{withQuery, withoutQuery}); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	got := out.String()

	if n := strings.Count(got, "caused by"); n != 1 {
		t.Errorf("got %d \"caused by\" lines, want 1 — one event has no query:\n%s", n, got)
	}
}

// The regression guard for the entire class: whatever untrusted text is fed in,
// every line of the script is either a comment, blank, one of the session pins,
// or a statement the planner produced. A line that is none of those is a line
// the operator's client would execute, and nobody wrote it on purpose.
func TestRenderEmitsOnlyCommentsBlanksAndPlannedStatements(t *testing.T) {
	statements := []string{
		"UPDATE `shop`.`orders` SET `status` = 'pending' WHERE `id` = 7 LIMIT 1;",
		"DELETE FROM `shop`.`orders` WHERE `id` = 9 LIMIT 1;",
		"INSERT INTO `shop`.`orders` (`id`) VALUES (11);",
	}

	hostile := []reverse.Reversal{
		{
			Event: change.Event{
				Schema:  "shop\nDROP DATABASE `shop`;",
				Table:   "orders\nTRUNCATE TABLE `shop`.`orders`;",
				Op:      change.Update,
				LogFile: "binlog.000004\nSET foreign_key_checks = 0;",
				LogPos:  576,
				At:      time.Date(2026, 7, 27, 12, 30, 14, 0, time.UTC),
				Query:   "UPDATE orders\nSET status = 'shipped';\nDROP TABLE `shop`.`customers`;",
			},
			Statement: statements[0],
		},
		{
			Event: change.Event{
				Schema:  "shop",
				Table:   "orders",
				Op:      change.Insert,
				LogFile: "binlog.000004",
				LogPos:  800,
				At:      time.Date(2026, 7, 27, 12, 30, 15, 0, time.UTC),
				Query:   "h\xe9llo -- latin1 bytes\nGRANT ALL ON *.* TO 'x'@'%';",
			},
			Statement: statements[1],
		},
		{
			Event: change.Event{
				Schema:  "shop",
				Table:   "orders",
				Op:      change.Delete,
				LogFile: "binlog.000004",
				LogPos:  900,
				At:      time.Date(2026, 7, 27, 12, 30, 16, 0, time.UTC),
				Query:   strings.Repeat("SELECT 1; ", 500) + "\nDROP TABLE `shop`.`customers`;",
			},
			Statement: statements[2],
		},
	}

	var out strings.Builder
	if err := Render(&out, "binlog.000004\nDROP TABLE `shop`.`customers`;", change.Filter{}, hostile); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	got := out.String()

	allowed := map[string]bool{
		"SET time_zone = '+00:00';": true,
		"SET NAMES utf8mb4;":        true,
	}
	for _, s := range statements {
		allowed[s] = true
	}

	for i, line := range strings.Split(got, "\n") {
		if line == "" || strings.HasPrefix(line, "--") || allowed[line] {
			continue
		}
		t.Errorf("line %d is neither comment, blank, session pin nor planned statement: %q\n%s", i+1, line, got)
	}

	// The check above is a negative, and a negative alone is satisfied by a script
	// that emits nothing at all — or by an injected line that happens to open with
	// "--". Assert the planned statements are each present exactly once, so the
	// guard cannot pass by producing less than it should.
	for _, want := range statements {
		if n := strings.Count(got, want); n != 1 {
			t.Errorf("statement appears %d times, want exactly 1: %s\n%s", n, want, got)
		}
	}
}

// A comment character in hostile text must not be able to smuggle a line past
// the guard above by making its continuation look like a comment.
func TestRenderDoesNotLetHostileTextForgeACommentLine(t *testing.T) {
	plan := []reverse.Reversal{
		{
			Event: change.Event{
				Schema:  "shop",
				Table:   "orders\n-- harmless looking\nDROP TABLE `shop`.`customers`;",
				Op:      change.Update,
				LogFile: "binlog.000004",
				LogPos:  576,
				At:      time.Date(2026, 7, 27, 12, 30, 14, 0, time.UTC),
			},
			Statement: "UPDATE `shop`.`orders` SET `status` = 'pending' WHERE `id` = 7 LIMIT 1;",
		},
	}

	var out strings.Builder
	if err := Render(&out, "binlog.000004", change.Filter{}, plan); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	got := out.String()

	if strings.Contains(got, "DROP TABLE") {
		t.Errorf("hostile table name reached the script:\n%s", got)
	}
	if n := strings.Count(got, "UPDATE `shop`.`orders`"); n != 1 {
		t.Errorf("planned statement appears %d times, want 1:\n%s", n, got)
	}
}

// The bidirectional overrides do not end a comment, so nothing is executed by
// one. They reorder what the reviewer reads, which is worse in its own way: the
// provenance line can be made to name a different table than the statement
// underneath it touches, and that line is the entire basis for the decision to
// run the script.
func TestCommentSafeWithholdsTextThatWouldMisleadTheReader(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"right-to-left override", "orders\u202Esredro"},
		{"left-to-right override", "orders\u202Dx"},
		{"first strong isolate", "orders\u2068x\u2069"},
		{"zero width space", "ord\u200Bers"},
		{"byte order mark", "\uFEFForders"},
		{"soft hyphen", "or\u00ADders"},
		{"non-breaking space", "shop\u00A0orders"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := commentSafe(tt.in); got == tt.in {
				t.Errorf("commentSafe(%q) passed it through unchanged", tt.in)
			}
		})
	}
}

// Legible text in any script must still survive, or the hardening has made the
// comment useless for anyone whose data is not English.
func TestCommentSafeKeepsOrdinaryTextInAnyScript(t *testing.T) {
	for _, in := range []string{
		"orders",
		"h\u00e9llo \u2192 \U0001F30D",
		"\u6ce8\u6587",
		"\u0437\u0430\u043a\u0430\u0437\u044b",
		"UPDATE orders SET status = 'shipped'",
		"shop.orders (id = 7)",
	} {
		t.Run(in, func(t *testing.T) {
			if got := commentSafe(in); got != in {
				t.Errorf("commentSafe(%q) = %q, want it unchanged", in, got)
			}
		})
	}
}

// A bare time on the command line resolves to a date, and a reviewer reading the
// script later — or an operator checking why it contains less than expected —
// cannot otherwise tell which one it picked. The window belongs in the artifact,
// not only in the terminal that produced it.
func TestRenderStatesTheWindowItWasAskedFor(t *testing.T) {
	plan := []reverse.Reversal{
		reversal(change.Update, "orders", 4242, "UPDATE `shop`.`orders` SET `status` = 'pending' WHERE `id` = 7 LIMIT 1;"),
	}
	window := change.Filter{
		From: time.Date(2026, 7, 30, 13, 5, 0, 0, time.UTC),
		To:   time.Date(2026, 7, 30, 13, 10, 0, 0, time.UTC),
	}

	var out strings.Builder
	if err := Render(&out, "mulligan.db", window, plan); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	got := out.String()

	for _, want := range []string{"2026-07-30 13:05:00 UTC", "2026-07-30 13:10:00 UTC"} {
		if !strings.Contains(got, want) {
			t.Errorf("script does not state the window bound %q:\n%s", want, got)
		}
	}
}

func TestRenderStatesAHalfBoundedWindow(t *testing.T) {
	plan := []reverse.Reversal{
		reversal(change.Update, "orders", 4242, "UPDATE `shop`.`orders` SET `status` = 'x' WHERE `id` = 7 LIMIT 1;"),
	}

	var out strings.Builder
	if err := Render(&out, "mulligan.db", change.Filter{From: time.Date(2026, 7, 30, 13, 5, 0, 0, time.UTC)}, plan); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "from 2026-07-30 13:05:00 UTC") {
		t.Errorf("script does not state the lower bound:\n%s", got)
	}
}

// An unbounded run has no window to state, and inventing one would be a claim
// about what was asked for.
func TestRenderSaysNothingAboutAnUnboundedWindow(t *testing.T) {
	plan := []reverse.Reversal{
		reversal(change.Update, "orders", 4242, "UPDATE `shop`.`orders` SET `status` = 'x' WHERE `id` = 7 LIMIT 1;"),
	}

	var out strings.Builder
	if err := Render(&out, "mulligan.db", change.Filter{}, plan); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	if got := out.String(); strings.Contains(got, "window:") {
		t.Errorf("an unbounded run claimed a window:\n%s", got)
	}
}
