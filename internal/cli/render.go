// Package cli implements the mulligan command line.
package cli

import (
	"bufio"
	"fmt"
	"io"

	"github.com/learttytyri/mulligan/internal/reverse"
)

// Render writes a revert script for plan, in the order the engine produced it.
//
// Every statement is preceded by the logged change it undoes and where to find
// that change in the source log. Mulligan proposes and the operator commits, so
// the script has to be readable by whoever is deciding whether to run it.
func Render(w io.Writer, source string, plan []reverse.Reversal) error {
	out := bufio.NewWriter(w)

	fmt.Fprintln(out, "-- mulligan revert script")
	fmt.Fprintf(out, "-- source: %s\n", source)

	if len(plan) == 0 {
		fmt.Fprintln(out, "-- no matching changes found")
		return out.Flush()
	}

	fmt.Fprintf(out, "-- %s, newest change first\n", plural(len(plan), "statement"))
	fmt.Fprintln(out, "-- REVIEW BEFORE RUNNING — nothing here has been executed.")

	// The statements below are written in UTC and utf8mb4, so the session has to
	// be put into those terms first. A session in another zone would store
	// TIMESTAMP values shifted by its offset, and one in another charset would
	// convert the text on the way in. Both fail silently.
	fmt.Fprintln(out)
	fmt.Fprintln(out, "SET time_zone = '+00:00';")
	fmt.Fprintln(out, "SET NAMES utf8mb4;")

	for _, r := range plan {
		ev := r.Event
		fmt.Fprintf(out, "\n-- undo %s %s.%s — %s:%d at %s\n",
			ev.Op, ev.Schema, ev.Table, ev.LogFile, ev.LogPos,
			ev.At.UTC().Format("2006-01-02 15:04:05 MST"))
		fmt.Fprintln(out, r.Statement)
	}

	return out.Flush()
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
