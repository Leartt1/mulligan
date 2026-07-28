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

	// ReadOnly marks a column the database computes for itself — a generated
	// column, most commonly. Its value appears in the log like any other, but
	// assigning to it is an error, and it does not need restoring: recomputing
	// follows from restoring the columns it derives from.
	//
	// The binary log carries no flag for this, so it has to be supplied.
	ReadOnly bool
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

	// Query is the statement that produced this row, when the source logged it.
	// MySQL records it only under binlog_rows_query_log_events and MariaDB only
	// when a replica asks for annotated rows, so it is often empty and nothing
	// may depend on it. It belongs to the rows event, not to the transaction: a
	// single transaction routinely contains several statements, and attributing
	// one statement's text to another's rows would misdescribe the change in the
	// artifact a human reads before running it.
	Query string
}

// Transaction is a set of row changes the source database committed atomically.
//
// It is the unit of both storage and reversal. The source applied these rows
// together, so reverting part of one is incoherent, and making it the storage
// unit is what lets a stored position never run ahead of the rows it covers.
type Transaction struct {
	// SourceID identifies this transaction in the source's own terms — its GTID
	// where the server issues them, otherwise the position of the event that
	// committed it. It is what makes re-delivery after a reconnect detectable,
	// so it must come from the source and never be assigned locally.
	SourceID string

	CommittedAt time.Time
	ServerID    uint32
	Events      []Event
}

// Checkpoint is how far a source has been consumed.
//
// GTID is preferred when the server issues them, because a file position is
// only meaningful on the server that wrote it and silently means something else
// after a failover.
type Checkpoint struct {
	LogFile   string
	LogPos    uint32
	GTID      string
	UpdatedAt time.Time
}

// IsZero reports whether nothing has been consumed yet, so a caller knows to
// start from the beginning rather than resuming from position zero.
func (c Checkpoint) IsZero() bool {
	return c.LogFile == "" && c.GTID == ""
}
