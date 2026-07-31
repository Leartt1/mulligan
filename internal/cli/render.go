// Package cli implements the mulligan command line.
package cli

import (
	"bufio"
	"fmt"
	"io"
	"unicode"
	"unicode/utf8"

	"github.com/learttytyri/mulligan/internal/change"
	"github.com/learttytyri/mulligan/internal/reverse"
)

// maxCommentBytes caps how much of one value a comment will carry.
//
// The originating statement of a migration or an ORM batch runs to kilobytes,
// and pasting one whole buries every other statement in the script — the
// operator stops reading, which costs more than the truncation does.
const maxCommentBytes = 200

// Render writes a revert script for plan, in the order the engine produced it.
//
// Every statement is preceded by the logged change it undoes and where to find
// that change in the source log. Mulligan proposes and the operator commits, so
// the script has to be readable by whoever is deciding whether to run it.
func Render(w io.Writer, source string, window change.Filter, schemaChanges []change.Event, plan []reverse.Reversal) error {
	out := bufio.NewWriter(w)

	fmt.Fprintln(out, "-- mulligan revert script")
	fmt.Fprintf(out, "-- source: %s\n", commentSafe(source))

	// The window is stated because a bare time on the command line resolves to a
	// date, and a reviewer reading this later cannot otherwise tell which one. It
	// is also the first thing to check when a script contains less than expected.
	if line := describeWindow(window); line != "" {
		fmt.Fprintf(out, "-- window: %s\n", line)
	}

	if len(plan) == 0 {
		fmt.Fprintln(out, "-- no matching changes found")
		return out.Flush()
	}

	fmt.Fprintf(out, "-- %s, newest change first\n", plural(len(plan), "statement"))
	fmt.Fprintln(out, "-- REVIEW BEFORE RUNNING — nothing here has been executed.")

	// A revert is built from each table as it was when the change happened. If the
	// schema moved since, these statements describe a shape the table may no longer
	// have. Dropped, renamed and narrowed columns fail loudly when the script runs;
	// a retyped column does not — it restores a coerced value with no error at all,
	// which is the case this warning exists for.
	writeSchemaWarning(out, schemaChanges)

	// The statements below are written in UTC and utf8mb4, so the session has to
	// be put into those terms first. A session in another zone would store
	// TIMESTAMP values shifted by its offset, and one in another charset would
	// convert the text on the way in. Both fail silently.
	fmt.Fprintln(out)
	fmt.Fprintln(out, "SET time_zone = '+00:00';")
	fmt.Fprintln(out, "SET NAMES utf8mb4;")

	// Everything interpolated below comes from the log rather than from here, so
	// it all goes through commentSafe. See its doc comment for what a value that
	// did not would do to this script.
	for _, r := range plan {
		writeReversal(out, r)
	}

	return out.Flush()
}

// writeReversal emits one statement under its provenance.
func writeReversal(out *bufio.Writer, r reverse.Reversal) {
	ev := r.Event
	fmt.Fprintf(out, "\n-- undo %s %s.%s — %s:%d at %s\n",
		ev.Op, commentSafe(ev.Schema), commentSafe(ev.Table),
		commentSafe(ev.LogFile), ev.LogPos,
		ev.At.UTC().Format("2006-01-02 15:04:05 MST"))

	// Most events carry no query — MySQL logs it only under
	// binlog_rows_query_log_events, MariaDB only for annotated rows. The line is
	// omitted rather than left blank, because a "caused by" that carried over from
	// the previous event would attribute this change to a statement that did not
	// cause it.
	if ev.Query != "" {
		fmt.Fprintf(out, "-- caused by: %s\n", commentSafe(ev.Query))
	}

	fmt.Fprintln(out, r.Statement)
}

// commentSafe renders s for placement on a SQL line comment.
//
// A line comment ends at the first newline, so a value carrying one would close
// its own comment and leave whatever followed as a statement in a script an
// operator runs by hand against production — most likely failing partway
// through and leaving a half-applied revert. Every value interpolated into a
// comment is untrusted: MySQL permits an identifier containing a newline, and a
// logged statement carries them as a matter of course.
//
// The rule mirrors safeToQuote in the reverse package, for the same reasons.
// Control characters are out, which covers the newline and carriage return this
// exists for. Bytes that are not valid UTF-8 came from some other charset, and
// rendering them would mean guessing which one and putting the guess in front of
// a reviewer as if it were what the log said. Anything printable and single-line
// passes through untouched, multi-byte scripts included: the operator is being
// asked to decide whether to run this, and cannot decide about a name they have
// to decode first.
func commentSafe(s string) string {
	if !printableOneLine(s) {
		// Naming the size is what keeps this from reading as "there was nothing
		// here"; a reviewer who sees something was withheld can go look at the log.
		return fmt.Sprintf("<unprintable, %s>", plural(len(s), "byte"))
	}

	if len(s) > maxCommentBytes {
		// Back off to a rune boundary — cutting mid-sequence would put bytes on the
		// line that are no longer valid UTF-8, the very thing checked for above.
		cut := maxCommentBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		// Say that it was cut, and by how much. A silent cut lets a reviewer
		// believe they read the whole statement when they read a prefix of it.
		return fmt.Sprintf("%s… <truncated, %s total>", s[:cut], plural(len(s), "byte"))
	}

	return s
}

// printableOneLine reports whether s can be placed on a comment line as-is.
//
// The test is unicode.IsPrint, which admits letters, marks, numbers,
// punctuation, symbols and the ASCII space, and nothing else. That is a wider
// net than the newline this exists to stop, deliberately:
//
//   - Control characters carry the newline and carriage return that would end
//     the comment and turn what follows into statements.
//   - Line and paragraph separators are not newlines to MySQL, but editors and
//     log viewers break lines on them, so the comment would appear to end
//     somewhere the script does not.
//   - Format characters include the bidirectional overrides. A table name
//     carrying U+202E renders the rest of the line right to left, so a
//     provenance comment can be made to name a different table than the
//     statement beneath it touches. Nothing is executed by it, but the comment
//     is the whole basis on which an operator decides to run the script, and a
//     comment that lies is worse than one that is withheld.
//   - Non-ASCII spaces are indistinguishable from an ordinary space on the line
//     while comparing unequal to one.
//
// Ordinary text in any script still passes untouched, which is the point: an
// operator cannot decide about a name they have to decode first.
func printableOneLine(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// writeSchemaWarning reports the schema changes a window spans.
//
// A revert is built from each table as it was when the change happened, so these
// statements may describe a shape the table no longer has. Dropped, renamed and
// narrowed columns make the script fail loudly when it runs; a retyped one
// restores a converted value with no error at all, which is the case this exists
// for.
func writeSchemaWarning(out *bufio.Writer, schemaChanges []change.Event) {
	if len(schemaChanges) == 0 {
		return
	}

	fmt.Fprintln(out, "--")
	fmt.Fprintf(out, "-- WARNING: %s in this window. The statements below describe\n",
		plural(len(schemaChanges), "schema change"))
	fmt.Fprintln(out, "-- each table as it was at the time, which may not be its shape now. A dropped or")
	fmt.Fprintln(out, "-- renamed column makes the script fail; a retyped one restores a converted value")
	fmt.Fprintln(out, "-- without complaining. Check these against the tables before running:")
	for _, ev := range schemaChanges {
		fmt.Fprintf(out, "--   %s at %s:%d — %s\n",
			ev.At.UTC().Format("2006-01-02 15:04:05 MST"),
			commentSafe(ev.LogFile), ev.LogPos, commentSafe(ev.Query))
	}
}

// describeWindow renders the bounds a script was generated for, or nothing when
// it was unbounded.
func describeWindow(f change.Filter) string {
	const layout = "2006-01-02 15:04:05 MST"

	switch {
	case !f.From.IsZero() && !f.To.IsZero():
		return f.From.UTC().Format(layout) + " to " + f.To.UTC().Format(layout)
	case !f.From.IsZero():
		return "from " + f.From.UTC().Format(layout)
	case !f.To.IsZero():
		return "up to " + f.To.UTC().Format(layout)
	}
	return ""
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
