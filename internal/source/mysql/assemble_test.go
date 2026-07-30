package mysql

import (
	"strings"
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"

	"github.com/learttytyri/mulligan/internal/change"
)

func tableMap() *replication.TableMapEvent {
	return &replication.TableMapEvent{
		Schema:      []byte("shop"),
		Table:       []byte("orders"),
		ColumnCount: 2,
		ColumnName:  [][]byte{[]byte("id"), []byte("status")},
		PrimaryKey:  []uint64{0},
	}
}

func event(t replication.EventType, pos uint32, body replication.Event) *replication.BinlogEvent {
	return &replication.BinlogEvent{
		Header: &replication.EventHeader{
			Timestamp: 1785000000,
			EventType: t,
			ServerID:  17,
			LogPos:    pos,
			EventSize: 40,
		},
		Event: body,
	}
}

func rowsEvent() *replication.RowsEvent {
	return &replication.RowsEvent{
		Table:       tableMap(),
		ColumnCount: 2,
		Rows:        [][]any{{int64(7), "new"}},
	}
}

// feed runs a sequence through the assembler and returns whatever transactions
// completed, failing the test on any error.
func feed(t *testing.T, a *assembler, events ...*replication.BinlogEvent) []change.Transaction {
	t.Helper()

	var out []change.Transaction
	for i, e := range events {
		res, err := a.handle(e)
		if err != nil {
			t.Fatalf("event %d (%s) returned error: %v", i, e.Header.EventType, err)
		}
		if res.txn != nil {
			out = append(out, *res.txn)
		}
	}
	return out
}

func newAssembler() *assembler {
	return &assembler{logFile: "binlog.000004", flavor: FlavorMySQL}
}

func TestATransactionIsEmittedOnCommit(t *testing.T) {
	a := newAssembler()

	got := feed(t,
		a,
		event(replication.QUERY_EVENT, 100, &replication.QueryEvent{Query: []byte("BEGIN")}),
		event(replication.TABLE_MAP_EVENT, 200, tableMap()),
		event(replication.WRITE_ROWS_EVENTv2, 300, rowsEvent()),
		event(replication.XID_EVENT, 400, &replication.XIDEvent{XID: 9}),
	)

	if len(got) != 1 {
		t.Fatalf("got %d transactions, want 1", len(got))
	}
	if len(got[0].Events) != 1 {
		t.Errorf("transaction carries %d events, want 1", len(got[0].Events))
	}
	if got[0].ServerID != 17 {
		t.Errorf("server id = %d, want 17", got[0].ServerID)
	}
}

// Nothing is emitted until the source says the transaction committed. Rows are
// written to the binlog before the commit event, so emitting early would store
// changes that may never have been applied.
func TestNoTransactionIsEmittedBeforeCommit(t *testing.T) {
	a := newAssembler()

	got := feed(t,
		a,
		event(replication.QUERY_EVENT, 100, &replication.QueryEvent{Query: []byte("BEGIN")}),
		event(replication.WRITE_ROWS_EVENTv2, 300, rowsEvent()),
	)

	if len(got) != 0 {
		t.Errorf("got %d transactions before any commit, want 0", len(got))
	}
}

// A rolled back transaction never happened, so its buffered rows must be
// discarded rather than stored as fact.
func TestARolledBackTransactionIsDiscarded(t *testing.T) {
	a := newAssembler()

	got := feed(t,
		a,
		event(replication.QUERY_EVENT, 100, &replication.QueryEvent{Query: []byte("BEGIN")}),
		event(replication.WRITE_ROWS_EVENTv2, 300, rowsEvent()),
		event(replication.QUERY_EVENT, 400, &replication.QueryEvent{Query: []byte("ROLLBACK")}),
		event(replication.QUERY_EVENT, 500, &replication.QueryEvent{Query: []byte("BEGIN")}),
		event(replication.WRITE_ROWS_EVENTv2, 600, rowsEvent()),
		event(replication.XID_EVENT, 700, &replication.XIDEvent{XID: 9}),
	)

	if len(got) != 1 {
		t.Fatalf("got %d transactions, want 1", len(got))
	}
	if len(got[0].Events) != 1 {
		t.Errorf("the committed transaction carries %d events, want 1 — the rolled back rows leaked into it", len(got[0].Events))
	}
}

// On MySQL 8.0.20+ with binlog_transaction_compression=ON every DML transaction
// arrives as a single compressed event. An assembler that does not unwrap it
// sees no rows, no commit, and a perfectly healthy connection — and produces an
// empty revert for a window where plenty happened.
func TestACompressedTransactionIsUnwrapped(t *testing.T) {
	a := newAssembler()

	payload := &replication.TransactionPayloadEvent{
		Events: []*replication.BinlogEvent{
			event(replication.QUERY_EVENT, 100, &replication.QueryEvent{Query: []byte("BEGIN")}),
			event(replication.TABLE_MAP_EVENT, 200, tableMap()),
			event(replication.WRITE_ROWS_EVENTv2, 300, rowsEvent()),
			event(replication.XID_EVENT, 400, &replication.XIDEvent{XID: 9}),
		},
	}

	got := feed(t, a, event(replication.TRANSACTION_PAYLOAD_EVENT, 500, payload))

	if len(got) != 1 {
		t.Fatalf("got %d transactions from a compressed payload, want 1", len(got))
	}
	if len(got[0].Events) != 1 {
		t.Errorf("transaction carries %d events, want 1", len(got[0].Events))
	}
}

// XA PREPARE writes row events with no XID, and the later XA COMMIT or XA
// ROLLBACK arrives as a separate statement the assembler cannot tie back. Stored
// as ordinary changes, a prepared-then-rolled-back transaction becomes committed
// fact: the revert would delete a row that was never inserted, or re-insert one
// that still exists, with provenance that looks entirely sound.
func TestXATransactionsAreRefusedRatherThanGuessedAt(t *testing.T) {
	tests := []struct {
		name  string
		event *replication.BinlogEvent
	}{
		{"prepare", event(replication.XA_PREPARE_LOG_EVENT, 100, &replication.GenericEvent{})},
		{"commit", event(replication.QUERY_EVENT, 100, &replication.QueryEvent{Query: []byte("XA COMMIT 'x'")})},
		{"rollback", event(replication.QUERY_EVENT, 100, &replication.QueryEvent{Query: []byte("XA ROLLBACK 'x'")})},
		{"lower case", event(replication.QUERY_EVENT, 100, &replication.QueryEvent{Query: []byte("xa commit 'x'")})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newAssembler()
			_, err := a.handle(tt.event)
			if err == nil {
				t.Fatal("an XA transaction was accepted, want a refusal")
			}
			if !strings.Contains(err.Error(), "XA") {
				t.Errorf("error = %v, want it to name XA", err)
			}
		})
	}
}

// The statement that produced a set of rows arrives just before them. It belongs
// to those rows only: carrying it forward would attribute a later change to a
// statement that did not cause it, in the line an operator reads before running
// a destructive script.
func TestTheStatementIsAttachedOnlyToTheRowsItCaused(t *testing.T) {
	a := newAssembler()

	got := feed(t,
		a,
		event(replication.QUERY_EVENT, 100, &replication.QueryEvent{Query: []byte("BEGIN")}),
		event(replication.ROWS_QUERY_EVENT, 150, &replication.RowsQueryEvent{Query: []byte("UPDATE orders SET status = 'shipped'")}),
		event(replication.WRITE_ROWS_EVENTv2, 300, rowsEvent()),
		event(replication.WRITE_ROWS_EVENTv2, 350, rowsEvent()),
		event(replication.XID_EVENT, 400, &replication.XIDEvent{XID: 9}),
	)

	if len(got) != 1 {
		t.Fatalf("got %d transactions, want 1", len(got))
	}
	evs := got[0].Events
	if len(evs) != 2 {
		t.Fatalf("transaction carries %d events, want 2", len(evs))
	}
	if evs[0].Query != "UPDATE orders SET status = 'shipped'" {
		t.Errorf("first event query = %q, want the statement that preceded it", evs[0].Query)
	}
	if evs[1].Query != "" {
		t.Errorf("second event query = %q, want it empty rather than inherited", evs[1].Query)
	}
}

func TestMariaDBAnnotatedRowsSupplyTheStatement(t *testing.T) {
	a := &assembler{logFile: "binlog.000004", flavor: FlavorMariaDB}

	got := feed(t,
		a,
		event(replication.MARIADB_ANNOTATE_ROWS_EVENT, 150, &replication.MariadbAnnotateRowsEvent{
			Query: []byte("UPDATE orders SET status = 'shipped'"),
		}),
		event(replication.WRITE_ROWS_EVENTv2, 300, rowsEvent()),
		event(replication.XID_EVENT, 400, &replication.XIDEvent{XID: 9}),
	)

	if len(got) != 1 {
		t.Fatalf("got %d transactions, want 1", len(got))
	}
	if q := got[0].Events[0].Query; q != "UPDATE orders SET status = 'shipped'" {
		t.Errorf("query = %q, want the annotated statement", q)
	}
}

// Without a GTID the only identifier available is where the transaction
// committed. It must come from the commit event, never a row event: MariaDB
// writes zero as the position of everything inside a transaction, so keying on a
// row event would collapse every MariaDB transaction onto the same identifier
// and make them look like re-deliveries of one another.
func TestTheSourceIdentifierComesFromTheCommitPosition(t *testing.T) {
	a := newAssembler()

	rows := event(replication.WRITE_ROWS_EVENTv2, 0, rowsEvent())
	got := feed(t,
		a,
		event(replication.QUERY_EVENT, 0, &replication.QueryEvent{Query: []byte("BEGIN")}),
		rows,
		event(replication.XID_EVENT, 700, &replication.XIDEvent{XID: 9}),
	)

	if len(got) != 1 {
		t.Fatalf("got %d transactions, want 1", len(got))
	}
	if want := "binlog.000004:700"; got[0].SourceID != want {
		t.Errorf("source id = %q, want %q", got[0].SourceID, want)
	}
}

func TestAGTIDBecomesTheSourceIdentifier(t *testing.T) {
	a := newAssembler()

	sid := []byte{0x3e, 0x11, 0xfa, 0x47, 0x71, 0xca, 0x11, 0xe1, 0x9e, 0x33, 0xc8, 0x0a, 0xa9, 0x42, 0x95, 0x62}
	got := feed(t,
		a,
		event(replication.GTID_EVENT, 50, &replication.GTIDEvent{SID: sid, GNO: 23}),
		event(replication.QUERY_EVENT, 100, &replication.QueryEvent{Query: []byte("BEGIN")}),
		event(replication.WRITE_ROWS_EVENTv2, 300, rowsEvent()),
		event(replication.XID_EVENT, 400, &replication.XIDEvent{XID: 9}),
	)

	if len(got) != 1 {
		t.Fatalf("got %d transactions, want 1", len(got))
	}
	if want := "3e11fa47-71ca-11e1-9e33-c80aa9429562:23"; got[0].SourceID != want {
		t.Errorf("source id = %q, want the GTID %q", got[0].SourceID, want)
	}
}

func TestAMariaDBGTIDBecomesTheSourceIdentifier(t *testing.T) {
	a := &assembler{logFile: "binlog.000004", flavor: FlavorMariaDB}

	got := feed(t,
		a,
		event(replication.MARIADB_GTID_EVENT, 50, &replication.MariadbGTIDEvent{
			GTID: mysql.MariadbGTID{DomainID: 1, ServerID: 17, SequenceNumber: 42},
		}),
		event(replication.WRITE_ROWS_EVENTv2, 300, rowsEvent()),
		event(replication.XID_EVENT, 400, &replication.XIDEvent{XID: 9}),
	)

	if len(got) != 1 {
		t.Fatalf("got %d transactions, want 1", len(got))
	}
	if want := "1-17-42"; got[0].SourceID != want {
		t.Errorf("source id = %q, want the MariaDB GTID %q", got[0].SourceID, want)
	}
}

// A rotate moves the collector to a new file, and the position of everything
// afterwards is relative to it. Keeping the old name would make every later
// identifier and every provenance line point into the wrong file.
func TestARotateChangesTheLogFileTheAssemblerReports(t *testing.T) {
	a := newAssembler()

	res, err := a.handle(event(replication.ROTATE_EVENT, 0, &replication.RotateEvent{
		NextLogName: []byte("binlog.000005"),
		Position:    4,
	}))
	if err != nil {
		t.Fatalf("rotate returned error: %v", err)
	}
	if res.advance.IsZero() {
		t.Error("a rotate did not advance coverage, so an idle server looks like a stopped collector")
	}

	got := feed(t,
		a,
		event(replication.WRITE_ROWS_EVENTv2, 300, rowsEvent()),
		event(replication.XID_EVENT, 700, &replication.XIDEvent{XID: 9}),
	)
	if len(got) != 1 {
		t.Fatalf("got %d transactions, want 1", len(got))
	}
	if want := "binlog.000005:700"; got[0].SourceID != want {
		t.Errorf("source id = %q, want it to name the rotated file %q", got[0].SourceID, want)
	}
}

// A heartbeat is how an idle source says it is still there. Without it, a
// database nobody is writing to is indistinguishable from a collector that has
// stopped, and the staleness refusal would fire on a perfectly healthy system.
func TestAHeartbeatAdvancesCoverage(t *testing.T) {
	a := newAssembler()

	// A real heartbeat carries timestamp zero: it describes the connection, not a
	// moment in the database. Read literally that is 1970, and a forward-only
	// coverage mark would ignore it.
	hb := event(replication.HEARTBEAT_EVENT, 0, &replication.HeartbeatEvent{Version: 2})
	hb.Header.Timestamp = 0

	res, err := a.handle(hb)
	if err != nil {
		t.Fatalf("heartbeat returned error: %v", err)
	}
	if res.advance.IsZero() {
		t.Fatal("a heartbeat did not advance coverage")
	}
	if res.advance.Year() < 2000 {
		t.Errorf("a heartbeat advanced coverage to %s, which is before anything was collected; "+
			"an idle server would look like a stopped collector", res.advance)
	}
}

// A row event the engine cannot interpret is recorded as a missed change rather
// than dropped or fatal: refusing the whole stream would make the collector
// unusable on a busy server, and dropping it silently would leave a revert
// missing the statement someone is looking for.
func TestAnUninterpretableRowEventIsRecordedAsMissed(t *testing.T) {
	a := newAssembler()

	rows := rowsEvent()
	rows.Table.ColumnName = nil // no column names: Convert refuses

	res, err := a.handle(event(replication.WRITE_ROWS_EVENTv2, 300, rows))
	if err != nil {
		t.Fatalf("an uninterpretable row event stopped the collector: %v", err)
	}
	if res.miss == nil {
		t.Fatal("an uninterpretable row event was dropped without being recorded as missed")
	}
	if !strings.Contains(res.miss.reason, "binlog_row_metadata") {
		t.Errorf("miss reason = %q, want it to carry why the change could not be read", res.miss.reason)
	}
}

// A rollback has to clear the transaction's identity as well as its rows. The
// identifier is what makes a re-delivery recognisable, so if a rolled back
// transaction's GTID were left behind, the next transaction would be stored
// under it — and when that GTID legitimately arrived later it would be taken for
// a duplicate and dropped without a word.
func TestARollbackClearsTheTransactionIdentityAsWellAsItsRows(t *testing.T) {
	a := newAssembler()

	sid := []byte{0x3e, 0x11, 0xfa, 0x47, 0x71, 0xca, 0x11, 0xe1, 0x9e, 0x33, 0xc8, 0x0a, 0xa9, 0x42, 0x95, 0x62}
	got := feed(t,
		a,
		event(replication.GTID_EVENT, 50, &replication.GTIDEvent{SID: sid, GNO: 23}),
		event(replication.QUERY_EVENT, 100, &replication.QueryEvent{Query: []byte("BEGIN")}),
		event(replication.WRITE_ROWS_EVENTv2, 300, rowsEvent()),
		event(replication.QUERY_EVENT, 400, &replication.QueryEvent{Query: []byte("ROLLBACK")}),

		// A second transaction the server issued no GTID for.
		event(replication.QUERY_EVENT, 500, &replication.QueryEvent{Query: []byte("BEGIN")}),
		event(replication.WRITE_ROWS_EVENTv2, 600, rowsEvent()),
		event(replication.XID_EVENT, 700, &replication.XIDEvent{XID: 9}),
	)

	if len(got) != 1 {
		t.Fatalf("got %d transactions, want 1", len(got))
	}
	if want := "binlog.000004:700"; got[0].SourceID != want {
		t.Errorf("source id = %q, want %q — the rolled back transaction's GTID was reused", got[0].SourceID, want)
	}
}

// Rows arriving after a rollback but before any new BEGIN must not be folded
// into whatever commits next.
func TestRowsAreNotCarriedAcrossARollback(t *testing.T) {
	a := newAssembler()

	got := feed(t,
		a,
		event(replication.QUERY_EVENT, 100, &replication.QueryEvent{Query: []byte("BEGIN")}),
		event(replication.WRITE_ROWS_EVENTv2, 300, rowsEvent()),
		event(replication.QUERY_EVENT, 400, &replication.QueryEvent{Query: []byte("ROLLBACK")}),
		event(replication.WRITE_ROWS_EVENTv2, 600, rowsEvent()),
		event(replication.XID_EVENT, 700, &replication.XIDEvent{XID: 9}),
	)

	if len(got) != 1 {
		t.Fatalf("got %d transactions, want 1", len(got))
	}
	if n := len(got[0].Events); n != 1 {
		t.Errorf("transaction carries %d events, want 1 — rows from the rolled back transaction leaked in", n)
	}
}
