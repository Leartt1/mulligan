package cli

import (
	"bufio"
	"fmt"
	"io"

	"github.com/learttytyri/mulligan/internal/change"
	"github.com/learttytyri/mulligan/internal/reverse"
)

// scriptWriter emits a revert script as its statements arrive.
//
// Nothing accumulates. Render takes a finished plan and is right for a caller
// that already has one; this exists for the case that cannot afford to build
// one, where the whole matching window would not fit in memory — which is the
// case a run reverting ten million rows is.
//
// The header cannot be written until the statement count is known, so it is not
// written at all: the count moves to the end, where it doubles as the marker
// that says the script is complete.
type scriptWriter struct {
	out    *bufio.Writer
	n      int
	opened bool
}

func newScriptWriter(w io.Writer) *scriptWriter {
	return &scriptWriter{out: bufio.NewWriter(w)}
}

// open writes everything that precedes the first statement.
func (s *scriptWriter) open(source string, window change.Filter, schemaChanges []change.Event) {
	if s.opened {
		return
	}
	s.opened = true

	fmt.Fprintln(s.out, "-- mulligan revert script")
	fmt.Fprintf(s.out, "-- source: %s\n", commentSafe(source))
	if line := describeWindow(window); line != "" {
		fmt.Fprintf(s.out, "-- window: %s\n", line)
	}
	fmt.Fprintln(s.out, "-- newest change first")
	fmt.Fprintln(s.out, "-- REVIEW BEFORE RUNNING — nothing here has been executed.")

	writeSchemaWarning(s.out, schemaChanges)

	fmt.Fprintln(s.out)
	fmt.Fprintln(s.out, "SET time_zone = '+00:00';")
	fmt.Fprintln(s.out, "SET NAMES utf8mb4;")
}

// write emits one reversal.
func (s *scriptWriter) write(r reverse.Reversal) error {
	writeReversal(s.out, r)
	s.n++
	return s.out.Flush()
}

// close writes the trailer.
//
// The count lands here rather than in the header because a streamed script does
// not know it in advance — and putting it at the end is better anyway: a script
// cut short by a crash or a full disk has no trailer, so a reader can tell.
func (s *scriptWriter) close() error {
	if s.n == 0 {
		fmt.Fprintln(s.out, "-- no matching changes found")
		return s.out.Flush()
	}

	fmt.Fprintf(s.out, "\n-- end of script: %s\n", plural(s.n, "statement"))
	return s.out.Flush()
}
