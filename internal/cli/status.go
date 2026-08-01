package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

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

// statusJSON is the wire shape of a report.
//
// It is written here rather than as tags on store.Report because it is a
// published contract — v0.3's REST endpoint serves these same fields — and the
// store's own struct should stay free to change without breaking anything
// parsing this. Times are RFC 3339 in UTC and durations are whole seconds, both
// unambiguous to a consumer that is not Go.
type statusJSON struct {
	Store             string          `json:"store"`
	Healthy           bool            `json:"healthy"`
	Verdict           string          `json:"verdict"`
	Source            *statusSource   `json:"source"`
	Coverage          *statusCoverage `json:"coverage"`
	StaleSeconds      int64           `json:"stale_seconds"`
	IntegrityProblems []string        `json:"integrity_problems"`
	Gaps              []statusGap     `json:"gaps"`
	Misses            []statusMiss    `json:"misses"`
}

type statusSource struct {
	Flavor            string `json:"flavor"`
	ServerIdentity    string `json:"server_identity"`
	GTIDDialect       string `json:"gtid_dialect"`
	DecodeFingerprint string `json:"decode_fingerprint"`
}

type statusCoverage struct {
	From                string `json:"from"`
	To                  string `json:"to"`
	MaxStalenessSeconds int64  `json:"max_staleness_seconds"`
	RetentionSeconds    int64  `json:"retention_seconds"`
}

type statusGap struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

type statusMiss struct {
	At     string `json:"at"`
	Reason string `json:"reason"`
}

func writeStatusJSON(w io.Writer, path string, r store.Report) error {
	// Empty collections are [] and absent structures are null. A consumer
	// iterating gaps should not have to special-case "none", and a store that has
	// collected nothing must not report a coverage window beginning in year 1.
	out := statusJSON{
		Store:             path,
		Healthy:           r.Healthy(),
		Verdict:           r.Verdict(),
		IntegrityProblems: []string{},
		Gaps:              []statusGap{},
		Misses:            []statusMiss{},
	}

	if r.Bound {
		out.Source = &statusSource{
			Flavor:            r.Binding.Flavor,
			ServerIdentity:    r.Binding.ServerIdentity,
			GTIDDialect:       r.Binding.GTIDDialect,
			DecodeFingerprint: r.Binding.DecodeFingerprint,
		}
	}
	if !r.Coverage.To.IsZero() {
		out.Coverage = &statusCoverage{
			From:                r.Coverage.From.Format(time.RFC3339),
			To:                  r.Coverage.To.Format(time.RFC3339),
			MaxStalenessSeconds: int64(r.Coverage.MaxStaleness.Seconds()),
			RetentionSeconds:    int64(r.Coverage.Retention.Seconds()),
		}
		out.StaleSeconds = int64(r.Stale.Round(time.Second).Seconds())
	}
	out.IntegrityProblems = append(out.IntegrityProblems, r.Problems...)
	for _, g := range r.Gaps {
		out.Gaps = append(out.Gaps, statusGap{
			From: g.From.Format(time.RFC3339), To: g.To.Format(time.RFC3339), Reason: g.Reason})
	}
	for _, m := range r.Misses {
		out.Misses = append(out.Misses, statusMiss{At: m.At.Format(time.RFC3339), Reason: m.Reason})
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
