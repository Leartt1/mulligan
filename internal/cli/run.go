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
	"github.com/learttytyri/mulligan/internal/store"
)

// Version is the build's reported version.
const Version = "0.2.0-dev"

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
	case "watch":
		return watch(args[1:], stdout, stderr)
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
  mulligan watch [flags]              follow a live server into a window store
  mulligan generate [flags] FILE...   generate SQL that undoes logged changes
  mulligan version                    print the version
  mulligan help                       print this message

Watch flags:
  -store FILE          window store to write (required)
  -server-id N         replication server id, unique in the topology (required)
  -dsn DSN             connection string; prefer the MULLIGAN_DSN environment
                       variable, since an argument is visible in ps output to
                       every user on the host
  -retain DURATION     how far back to keep changes (default 168h)
  -max-staleness DUR   how far behind the collector may fall before generate
                       refuses to answer (default 5m)

Binlog files are named positionally or with -binlog, and are read in the order
given — oldest first, which is what a glob over binlog.0000* already produces.

Generate flags:
  -binlog FILE    a binlog to read; may be given more than once
  -tables LIST    comma-separated tables to include, as "table" or "schema.table"
  -from TIME      earliest change to include, inclusive
  -to TIME        latest change to include, inclusive
  -out FILE       write the script here instead of stdout
  -force          overwrite the -out file if it already exists
  -store FILE     read changes from a window store instead of binlog files
  -generated LIST generated columns, as "column" or "table.column". The log
                  records their values but not the fact that they are computed,
                  and assigning to one is an error, so they must be named here.
                  Symptom of a missing name: ERROR 3105 when the script is run.

Times accept a bare "15:04" — meaning its most recent occurrence — as well as
"2006-01-02 15:04:05" (local) or "2006-01-02T15:04:05Z07:00". The generated
script states the window it resolved to.

The generated script is a proposal. Nothing is executed — review it first.
`)
}

func generate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { usage(stderr) }

	var binlogs stringList
	fs.Var(&binlogs, "binlog", "MySQL ROW binlog file to read; repeatable")

	var (
		tables    = fs.String("tables", "", `comma-separated tables, as "table" or "schema.table"`)
		from      = fs.String("from", "", "earliest change to include, inclusive")
		to        = fs.String("to", "", "latest change to include, inclusive")
		outPath   = fs.String("out", "", "write the script here instead of stdout")
		force     = fs.Bool("force", false, "overwrite the -out file if it already exists")
		storePath = fs.String("store", "", "read changes from a window store instead of binlog files")
		generated = fs.String("generated", "", "comma-separated generated columns, which are read but never assigned")
	)

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	// Files may arrive as -binlog flags, as positional arguments, or both. The
	// positional form is what lets a shell glob expand a rotation into order.
	files := append([]string(binlogs), fs.Args()...)

	switch {
	case *storePath != "" && len(files) > 0:
		fmt.Fprintln(stderr, "mulligan generate: read either a store or binlog files, not both; "+
			"the two would order their changes against different clocks")
		return exitUsage
	case *storePath == "" && len(files) == 0:
		fmt.Fprintln(stderr, "mulligan generate: name a window store with -store, "+
			"or at least one binlog file positionally or with -binlog")
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

	var (
		events []change.Event
		label  string
	)
	if *storePath != "" {
		events, err = eventsFromStore(*storePath, filter)
		label = filepath.Base(*storePath)
	} else {
		events, err = eventsFromFiles(files, filter)
		label = sourceLabel(files)
	}
	if err != nil {
		fmt.Fprintf(stderr, "mulligan generate: %v\n", err)
		return exitFailure
	}

	// Schema changes are carried alongside the row changes so the script can warn
	// about them; they are never reversed.
	rowChanges, schemaChanges := splitSchemaChanges(events)

	change.MarkReadOnly(rowChanges, splitList(*generated))

	plan, err := reverse.Plan(rowChanges)
	if err != nil {
		fmt.Fprintf(stderr, "mulligan generate: %v\n", err)
		return exitFailure
	}

	// Render into memory first. A half-written script on disk is indistinguishable
	// from a complete one, and the whole point is that a human trusts what they
	// review.
	var script bytes.Buffer
	if err := Render(&script, label, filter, schemaChanges, plan); err != nil {
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

func buildFilter(tables, from, to string) (change.Filter, error) {
	f := change.Filter{Tables: splitList(tables)}

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

// stringList collects a flag given more than once, so several binlogs can be
// named without a separator that a filename might itself contain.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ", ") }

func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
}

// sourceLabel names the logs a script was built from, for its header.
func sourceLabel(files []string) string {
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = filepath.Base(f)
	}
	return strings.Join(names, ", ")
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

// eventsFromStore reads a window the collector already gathered.
//
// The store refuses a window it cannot answer for rather than returning fewer
// rows, so an error here is usually a statement about coverage — a gap, a
// stalled collector, a window reaching past retention — and is passed through
// intact for the operator to read.
func eventsFromStore(path string, filter change.Filter) ([]change.Event, error) {
	db, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	return db.Events(filter, time.Now().UTC())
}

// eventsFromFiles reads the named binlogs in the order given.
//
// A rotation is a sequence, and undoing a sequence depends on knowing what came
// after what; a shell glob over binlog.0000* already hands them over oldest
// first. A file that cannot be read stops everything rather than being skipped:
// a gap in the middle of a window drops changes from the revert without anything
// looking wrong.
func eventsFromFiles(files []string, filter change.Filter) ([]change.Event, error) {
	var out []change.Event
	for _, path := range files {
		found, err := binlog.ReadFile(path, filter)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	return out, nil
}

// splitSchemaChanges separates DDL from the row changes it sits among.
//
// A schema change is not reversible and never becomes SQL, but a revert
// generated across one describes a table in a shape it may no longer have — so
// it travels this far in order to be reported.
func splitSchemaChanges(events []change.Event) (rows, schema []change.Event) {
	for _, ev := range events {
		if ev.IsSchemaChange() {
			schema = append(schema, ev)
			continue
		}
		rows = append(rows, ev)
	}
	return rows, schema
}
