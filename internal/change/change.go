// Package change defines the normalized row-change event that every source
// adapter emits and the reverse engine consumes.
//
// Keeping the model here — rather than in the binlog package — is what lets a
// Postgres WAL adapter reuse the reverse engine unchanged.
package change

import "time"

// Op is the kind of row modification an Event describes.
type Op uint8

const (
	// Insert is a row that came into existence.
	Insert Op = iota + 1
	// Update is a row whose values changed; both images are present.
	Update
	// Delete is a row that ceased to exist.
	Delete
)

// Raw is a column value already rendered in the source database's own syntax,
// emitted into generated SQL verbatim and unquoted.
//
// It exists for values no Go type carries losslessly — DECIMAL is the motivating
// case, where routing through float64 would round an amount and produce a
// reversal that restores the wrong number. A source adapter converts such a
// value once, at the point where it still knows the exact text.
type Raw string

// String names the SQL statement kind the operation corresponds to, for use in
// output an operator reads.
func (o Op) String() string {
	switch o {
	case Insert:
		return "INSERT"
	case Update:
		return "UPDATE"
	case Delete:
		return "DELETE"
	}
	return "UNKNOWN"
}

// Column describes one column of the table the event touched, in the same
// order as the Before and After value slices.
type Column struct {
	Name       string
	PrimaryKey bool
}

// Event is a single row modification, normalized across source databases.
//
// Before holds the row image prior to the statement and is populated for
// Update and Delete. After holds the image following it and is populated for
// Insert and Update. Both slices are indexed in step with Columns.
type Event struct {
	Schema  string
	Table   string
	Op      Op
	Columns []Column
	Before  []any
	After   []any

	// Provenance — where in the source log this event came from, so a
	// generated reversal can be traced back to the statement that caused it.
	LogFile  string
	LogPos   uint32
	At       time.Time
	ServerID uint32
}
