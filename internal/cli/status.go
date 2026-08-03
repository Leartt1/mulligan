package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/learttytyri/mulligan/internal/api"
	"github.com/learttytyri/mulligan/internal/store"
)

// statusTime is how an instant is written in the human report: seconds, UTC, and
// no zone abbreviation, matching the timestamps in a generated script.
const statusTime = "2006-01-02 15:04:05"

// status reports what a store can and cannot answer for.
//
// The exit code is the point. A dead collector and a quiet database look
// identical from the outside, and the difference is only visible in the store —
// so this has to be answerable by a cron entry, not only by a human reading
// prose. Zero means sound and keeping up; one means something an operator should
// go and look at.
func status(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { usage(stderr) }

	var (
		storePath = fs.String("store", "", "window store to report on (required)")
		asJSON    = fs.Bool("json", false, "write the report as one JSON object")
	)

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *storePath == "" {
		fmt.Fprintln(stderr, "mulligan status: -store is required")
		return exitUsage
	}

	// Opening a store creates it. Without this check a mistyped path would be
	// reported as a store whose collector has never run, which is a true
	// statement about a file that did not exist until this command ran.
	if _, err := os.Stat(*storePath); err != nil {
		fmt.Fprintf(stderr, "mulligan status: no store at %s\n", *storePath)
		return exitFailure
	}

	db, err := store.Open(*storePath)
	if err != nil {
		fmt.Fprintf(stderr, "mulligan status: %v\n", err)
		return exitFailure
	}
	defer db.Close()

	report, err := db.Status(time.Now().UTC())
	if err != nil {
		fmt.Fprintf(stderr, "mulligan status: %v\n", err)
		return exitFailure
	}

	if *asJSON {
		err = writeStatusJSON(stdout, *storePath, report)
	} else {
		err = writeStatusText(stdout, *storePath, report)
	}
	if err != nil {
		fmt.Fprintf(stderr, "mulligan status: %v\n", err)
		return exitFailure
	}

	if !report.Healthy() {
		return exitFailure
	}
	return exitOK
}

func writeStatusText(w io.Writer, path string, r store.Report) error {
	var b []byte
	line := func(label, value string) {
		b = append(b, fmt.Sprintf("%-9s %s\n", label, value)...)
	}

	line("store", path)

	if r.Bound {
		// An empty dialect is how a source records that the server issues no
		// GTIDs, which is MySQL 8.0's default. Printed bare it looks like a field
		// that failed to load rather than an answer.
		gtid := r.Binding.GTIDDialect
		if gtid == "" {
			gtid = "none — the server issues no GTIDs"
		}
		line("source", fmt.Sprintf("%s · server %s · gtid %s",
			r.Binding.Flavor, r.Binding.ServerIdentity, gtid))
	} else {
		line("source", "not bound yet")
	}

	if r.Coverage.To.IsZero() {
		line("coverage", "nothing recorded yet")
	} else {
		line("coverage", fmt.Sprintf("%s → %s UTC",
			r.Coverage.From.Format(statusTime), r.Coverage.To.Format(statusTime)))
	}

	if r.Coverage.Retention == 0 {
		line("retention", "unset, so nothing is discarded")
	} else {
		line("retention", r.Coverage.Retention.String())
	}

	if r.Coverage.To.IsZero() {
		line("freshness", "no changes have been collected")
	} else {
		line("freshness", fmt.Sprintf("last change %s ago (allowed %s)",
			r.Stale.Round(time.Second), r.Coverage.MaxStaleness))
	}

	if len(r.Problems) == 0 {
		line("integrity", "ok")
	} else {
		line("integrity", plural(len(r.Problems), "problem"))
		for _, p := range r.Problems {
			b = append(b, "  "+p+"\n"...)
		}
	}

	// Gaps and misses are listed whatever the verdict says. They are what the
	// store knows it does not hold, and an operator planning a revert needs to
	// see them even on a store that is otherwise perfectly healthy.
	if len(r.Gaps) == 0 {
		line("gaps", "none")
	} else {
		line("gaps", plural(len(r.Gaps), "gap"))
		for _, g := range r.Gaps {
			b = append(b, fmt.Sprintf("  %s → %s UTC  %s\n",
				g.From.Format(statusTime), g.To.Format(statusTime), g.Reason)...)
		}
	}

	if len(r.Misses) == 0 {
		line("misses", "none")
	} else {
		line("misses", plural(len(r.Misses), "missed change"))
		for _, m := range r.Misses {
			b = append(b, fmt.Sprintf("  %s UTC  %s\n", m.At.Format(statusTime), m.Reason)...)
		}
	}

	if r.Healthy() {
		b = append(b, "\nOK\n"...)
	} else {
		b = append(b, fmt.Sprintf("\nNOT OK: %s\n", r.Verdict())...)
	}

	_, err := w.Write(b)
	return err
}

// writeStatusJSON writes the report in the shape internal/api publishes, so the
// command and GET /api/status cannot come to describe a store differently.
func writeStatusJSON(w io.Writer, path string, r store.Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(api.NewStatus(path, r))
}
