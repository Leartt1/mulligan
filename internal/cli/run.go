package cli

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/learttytyri/mulligan/internal/binlog"
	"github.com/learttytyri/mulligan/internal/change"
	"github.com/learttytyri/mulligan/internal/reverse"
)

// Version is the build's reported version.
const Version = "0.1.0-dev"

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

// Run executes the mulligan command line and returns a process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}

	switch args[0] {
	case "generate":
		return generate(args[1:], stdout, stderr)
	case "version", "-version", "--version":
		fmt.Fprintf(stdout, "mulligan %s\n", Version)
		return exitOK
	case "help", "-h", "-help", "--help":
		usage(stdout)
		return exitOK
	}

	fmt.Fprintf(stderr, "mulligan: unknown command %q\n\n", args[0])
	usage(stderr)
	return exitUsage
}

func usage(w io.Writer) {
	fmt.Fprint(w, `mulligan — Ctrl-Z for the database you already have

Usage:
  mulligan generate -binlog FILE [flags]   generate SQL that undoes logged changes
  mulligan version                         print the version
  mulligan help                            print this message

Generate flags:
  -binlog FILE    MySQL ROW binlog to read (required)
  -tables LIST    comma-separated tables to include, as "table" or "schema.table"
  -from TIME      earliest change to include, inclusive
  -to TIME        latest change to include, inclusive
  -out FILE       write the script here instead of stdout
  -force          overwrite the -out file if it already exists
  -generated LIST generated columns, as "column" or "table.column". The log
                  records their values but not the fact that they are computed,
                  and assigning to one is an error, so they must be named here.
                  Symptom of a missing name: ERROR 3105 when the script is run.

Times accept "2006-01-02 15:04:05" (local) or "2006-01-02T15:04:05Z07:00".

The generated script is a proposal. Nothing is executed — review it first.
`)
}

func generate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { usage(stderr) }

	var (
		binlogPath = fs.String("binlog", "", "MySQL ROW binlog file to read (required)")
		tables     = fs.String("tables", "", `comma-separated tables, as "table" or "schema.table"`)
		from       = fs.String("from", "", "earliest change to include, inclusive")
		to         = fs.String("to", "", "latest change to include, inclusive")
		outPath    = fs.String("out", "", "write the script here instead of stdout")
		force      = fs.Bool("force", false, "overwrite the -out file if it already exists")
		generated  = fs.String("generated", "", "comma-separated generated columns, which are read but never assigned")
	)

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *binlogPath == "" {
		fmt.Fprintln(stderr, "mulligan generate: -binlog is required")
		return exitUsage
	}

	// Check before scanning rather than after. The file may be a revert script
	// someone is partway through reviewing, and finding that out at the end —
	// having already done the work — is the wrong moment.
	if *outPath != "" && !*force {
		if _, err := os.Stat(*outPath); err == nil {
			fmt.Fprintf(stderr, "mulligan generate: %s already exists; pass -force to overwrite it\n", *outPath)
			return exitUsage
		}
	}

	filter, err := buildFilter(*tables, *from, *to)
	if err != nil {
		fmt.Fprintf(stderr, "mulligan generate: %v\n", err)
		return exitUsage
	}

	events, err := binlog.ReadFile(*binlogPath, filter)
	if err != nil {
		fmt.Fprintf(stderr, "mulligan generate: %v\n", err)
		return exitFailure
	}

	change.MarkReadOnly(events, splitList(*generated))

	plan, err := reverse.Plan(events)
	if err != nil {
		fmt.Fprintf(stderr, "mulligan generate: %v\n", err)
		return exitFailure
	}

	// Render into memory first. A half-written script on disk is indistinguishable
	// from a complete one, and the whole point is that a human trusts what they
	// review.
	var script bytes.Buffer
	if err := Render(&script, filepath.Base(*binlogPath), plan); err != nil {
		fmt.Fprintf(stderr, "mulligan generate: %v\n", err)
		return exitFailure
	}

	if *outPath == "" {
		if _, err := stdout.Write(script.Bytes()); err != nil {
			fmt.Fprintf(stderr, "mulligan generate: %v\n", err)
			return exitFailure
		}
		return exitOK
	}

	if err := os.WriteFile(*outPath, script.Bytes(), 0o600); err != nil {
		fmt.Fprintf(stderr, "mulligan generate: %v\n", err)
		return exitFailure
	}
	fmt.Fprintf(stderr, "wrote %s (%s)\n", *outPath, plural(len(plan), "statement"))
	return exitOK
}

func buildFilter(tables, from, to string) (binlog.Filter, error) {
	f := binlog.Filter{Tables: splitList(tables)}

	var err error
	if from != "" {
		if f.From, err = parseTimestamp(from); err != nil {
			return f, fmt.Errorf("-from: %w", err)
		}
	}
	if to != "" {
		if f.To, err = parseTimestamp(to); err != nil {
			return f, fmt.Errorf("-to: %w", err)
		}
	}

	// An inverted window matches nothing, and an empty result reads as "there was
	// nothing to undo" — the most misleading answer this tool can give.
	if !f.From.IsZero() && !f.To.IsZero() && f.To.Before(f.From) {
		return f, fmt.Errorf("-to (%s) is before -from (%s), which can never match a change",
			f.To.Format(time.RFC3339), f.From.Format(time.RFC3339))
	}
	return f, nil
}

// splitList reads a comma-separated flag value, dropping blanks so that a
// trailing comma or an empty value means "none given" rather than one empty name.
func splitList(list string) []string {
	var out []string
	for _, name := range strings.Split(list, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}
