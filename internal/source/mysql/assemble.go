package mysql

import (
	"fmt"
	"strings"
	"time"

	"github.com/go-mysql-org/go-mysql/replication"

	"github.com/learttytyri/mulligan/internal/binlog"
	"github.com/learttytyri/mulligan/internal/change"
)

// assembler turns the stream of binlog events into whole transactions.
//
// The transaction is the unit because the source applied it as one: reverting
// part of it is incoherent, and holding rows until the commit event is what
// keeps changes that were rolled back — or that a disconnect interrupted — out
// of the store entirely.
type assembler struct {
	logFile string
	flavor  Flavor

	// now supplies the clock for events that carry no timestamp of their own.
	// Overridden in tests.
	now func() time.Time

	// gtid is the identifier of the transaction currently being assembled, when
	// the server issues them.
	gtid string

	// query is the statement whose rows come next. It is cleared as soon as it is
	// attached, because it describes those rows and not whatever follows them.
	query string

	events []change.Event
	at     time.Time
	server uint32
}

func (a *assembler) clock() time.Time {
	if a.now != nil {
		return a.now().UTC()
	}
	return time.Now().UTC()
}

// missed is one change seen but not interpretable.
type missed struct {
	at     time.Time
	reason string
}

// result is what one event did: at most one of these is set.
type result struct {
	txn     *change.Transaction
	miss    *missed
	advance time.Time
}

// handle consumes one binlog event.
//
// Anything it cannot account for is refused rather than skipped. An event this
// does not understand may be the one carrying the change someone will later go
// looking for, and skipping it silently advances past that change while the
// connection looks healthy.
func (a *assembler) handle(e *replication.BinlogEvent) (result, error) {
	hdr := e.Header

	// A heartbeat, and the artificial rotate a stream opens with, carry timestamp
	// zero: they describe the connection rather than a moment in the database. They
	// still have to move coverage, because their whole purpose is to say the
	// collector is attached and current — so they are stamped with the clock
	// instead. Reading them as 1970 would leave an idle server looking like a
	// collector that had stopped.
	when := time.Unix(int64(hdr.Timestamp), 0).UTC()
	if hdr.Timestamp == 0 {
		when = a.clock()
	}

	switch ev := e.Event.(type) {
	case *replication.RotateEvent:
		// Positions after a rotate are relative to the new file, so the name has to
		// change with it or every later identifier points into the wrong log.
		a.logFile = string(ev.NextLogName)
		return result{advance: when}, nil

	case *replication.RowsQueryEvent:
		a.query = string(ev.Query)
		return result{}, nil

	case *replication.MariadbAnnotateRowsEvent:
		a.query = string(ev.Query)
		return result{}, nil

	case *replication.GTIDEvent:
		// An anonymous GTID is what a server with gtid_mode off emits before every
		// transaction, and the library reports it as a GTID whose SID is all zeros.
		// Rendered, that is one string shared by every transaction on the server —
		// so taking it as an identifier makes each transaction look like a
		// re-delivery of the first, and the store's uniqueness constraint discards
		// the rest without a word. It is the absence of an identifier, not one.
		if hdr.EventType == replication.ANONYMOUS_GTID_EVENT {
			return result{}, nil
		}

		next, err := ev.GTIDNext()
		if err != nil {
			return result{}, fmt.Errorf("mysql: reading the GTID at %s:%d: %w", a.logFile, hdr.LogPos, err)
		}
		a.gtid = next.String()
		return result{}, nil

	case *replication.MariadbGTIDEvent:
		a.gtid = ev.GTID.String()
		return result{}, nil

	case *replication.RowsEvent:
		return a.appendRows(hdr, ev, when)

	case *replication.XIDEvent:
		return a.commit(hdr, when), nil

	case *replication.TransactionPayloadEvent:
		return a.unwrap(ev)

	case *replication.QueryEvent:
		return a.query_(ev, hdr, when)
	}

	// XA PREPARE arrives as an event with no dedicated type. Its rows carry no
	// XID, and the XA COMMIT or XA ROLLBACK that resolves them comes later as a
	// separate statement with nothing tying it back — so a prepared transaction
	// that was rolled back would sit in the store as committed fact.
	if hdr.EventType == replication.XA_PREPARE_LOG_EVENT {
		return result{}, fmt.Errorf("mysql: XA transactions cannot be reversed: an XA PREPARE at %s:%d "+
			"writes rows whose commit or rollback is recorded separately, so storing them would record "+
			"changes that may never have been applied", a.logFile, hdr.LogPos)
	}

	// Everything else in a binlog is bookkeeping the assembler has no stake in.
	return result{advance: when}, nil
}

func (a *assembler) appendRows(hdr *replication.EventHeader, rows *replication.RowsEvent, when time.Time) (result, error) {
	// The position of a row event is unreliable on MariaDB, which writes zero for
	// everything inside a transaction. Convert is given the header as-is; the
	// transaction takes its identity from the commit event instead.
	events, err := binlog.Convert(a.logFile, hdr, rows)
	if err != nil {
		// Refusing the whole stream over one unreadable change would make the
		// collector unusable on a busy server; dropping it silently would leave a
		// revert missing exactly the statement someone came looking for. Recorded as
		// absent, any window containing it is refused instead.
		return result{miss: &missed{at: when, reason: err.Error()}}, nil
	}

	for i := range events {
		events[i].Query = a.query
	}
	a.query = ""

	a.events = append(a.events, events...)
	a.at = when
	a.server = hdr.ServerID
	return result{}, nil
}

func (a *assembler) commit(hdr *replication.EventHeader, when time.Time) result {
	defer a.reset()

	if len(a.events) == 0 {
		// A transaction that changed no rows leaves nothing to reverse.
		return result{advance: when}
	}

	if a.at.IsZero() {
		a.at = when
	}
	if a.server == 0 {
		a.server = hdr.ServerID
	}

	return result{txn: &change.Transaction{
		SourceID:    a.sourceID(hdr),
		CommittedAt: a.at,
		ServerID:    a.server,
		Events:      a.events,
	}}
}

// sourceID identifies this transaction in the source's own terms, so a
// re-delivery after a reconnect is recognisable as the same transaction.
//
// Falling back to the commit event's position rather than a row event's is
// deliberate: MariaDB writes zero as the position of everything inside a
// transaction, so keying on a row event would give every MariaDB transaction the
// same identifier and make them all look like copies of one another.
func (a *assembler) sourceID(hdr *replication.EventHeader) string {
	if a.gtid != "" {
		return a.gtid
	}
	return fmt.Sprintf("%s:%d", a.logFile, hdr.LogPos)
}

func (a *assembler) reset() {
	a.events = nil
	a.query = ""
	a.gtid = ""
	a.at = time.Time{}
	a.server = 0
}

// unwrap runs a compressed transaction's inner events through the same handling.
//
// With binlog_transaction_compression on, every DML transaction arrives as one
// of these. Left unhandled it looks like an event of no interest: no rows, no
// commit, a healthy connection, and an empty revert for a window in which a
// great deal happened.
func (a *assembler) unwrap(payload *replication.TransactionPayloadEvent) (result, error) {
	var out result
	for _, inner := range payload.Events {
		res, err := a.handle(inner)
		if err != nil {
			return out, err
		}
		if res.txn != nil {
			out.txn = res.txn
		}
		if res.miss != nil {
			out.miss = res.miss
		}
		if out.advance.Before(res.advance) {
			out.advance = res.advance
		}
	}
	return out, nil
}

func (a *assembler) query_(ev *replication.QueryEvent, hdr *replication.EventHeader, when time.Time) (result, error) {
	stmt := strings.TrimSpace(string(ev.Query))
	upper := strings.ToUpper(stmt)

	switch {
	case strings.HasPrefix(upper, "XA "):
		return result{}, fmt.Errorf("mysql: XA transactions cannot be reversed: %q at %s:%d resolves rows "+
			"recorded separately from it, so storing them would record changes that may never have been applied",
			stmt, a.logFile, hdr.LogPos)

	case upper == "BEGIN":
		// Rows from a previous transaction can only still be buffered if that one
		// never terminated, which nothing should produce; clearing is the safe read.
		a.events = nil
		a.query = ""
		return result{}, nil

	case upper == "COMMIT":
		return a.commit(hdr, when), nil

	case upper == "ROLLBACK":
		// The transaction never happened, so its rows are discarded rather than
		// stored as fact.
		a.reset()
		return result{advance: when}, nil
	}

	// DDL is not reversible from row images and is never rendered as SQL. It is
	// still recorded, because a revert generated across a schema change describes a
	// table in a shape it may no longer have — and a retyped column restores a
	// coerced value with no error at all. It commits on its own: DDL is not part of
	// the surrounding transaction and carries no rows.
	if change.IsDDL(stmt) {
		return result{txn: &change.Transaction{
			SourceID:    a.sourceID(hdr),
			CommittedAt: when,
			ServerID:    hdr.ServerID,
			Events: []change.Event{{
				Schema:   string(ev.Schema),
				Op:       change.SchemaChange,
				Query:    stmt,
				LogFile:  a.logFile,
				LogPos:   hdr.LogPos,
				At:       when,
				ServerID: hdr.ServerID,
			}},
		}}, nil
	}

	// Session bookkeeping. Not reversible, and not worth reporting.
	return result{advance: when}, nil
}
