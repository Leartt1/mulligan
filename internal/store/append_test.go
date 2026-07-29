package store

import (
	"reflect"
	"testing"
	"time"

	"github.com/learttytyri/mulligan/internal/change"
)

func at(unix int64) time.Time { return time.Unix(unix, 0).UTC() }

func cols() []change.Column {
	return []change.Column{{Name: "id", PrimaryKey: true}, {Name: "status"}}
}

func txn(sourceID string, committed int64, events ...change.Event) change.Transaction {
	return change.Transaction{
		SourceID:    sourceID,
		CommittedAt: at(committed),
		ServerID:    17,
		Events:      events,
	}
}

func update(table string, pos uint32, before, after any) change.Event {
	return change.Event{
		Schema:  "shop",
		Table:   table,
		Op:      change.Update,
		Columns: cols(),
		Before:  []any{int64(1), before},
		After:   []any{int64(1), after},
		LogFile: "binlog.000004",
		LogPos:  pos,
	}
}

func checkpoint(pos uint32) change.Checkpoint {
	return change.Checkpoint{LogFile: "binlog.000004", LogPos: pos, UpdatedAt: at(1785000000)}
}

func TestAppendStoresATransactionAndReadsItBack(t *testing.T) {
	s := open(t)

	want := update("orders", 576, "pending", "shipped")
	if err := s.AppendTransaction(txn("gtid:1", 1785000000, want), checkpoint(576)); err != nil {
		t.Fatalf("AppendTransaction returned error: %v", err)
	}

	got, err := s.Events(change.Filter{}, at(1785000010))
	if err != nil {
		t.Fatalf("Events returned error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Events returned %d events, want 1", len(got))
	}

	ev := got[0]
	if ev.Schema != "shop" || ev.Table != "orders" || ev.Op != change.Update {
		t.Errorf("event = %s.%s %v, want shop.orders Update", ev.Schema, ev.Table, ev.Op)
	}
	if !reflect.DeepEqual(ev.Before, want.Before) {
		t.Errorf("before = %#v, want %#v", ev.Before, want.Before)
	}
	if !reflect.DeepEqual(ev.After, want.After) {
		t.Errorf("after = %#v, want %#v", ev.After, want.After)
	}
	if !reflect.DeepEqual(ev.Columns, want.Columns) {
		t.Errorf("columns = %#v, want %#v", ev.Columns, want.Columns)
	}
	if ev.LogFile != "binlog.000004" || ev.LogPos != 576 {
		t.Errorf("position = %s:%d, want binlog.000004:576", ev.LogFile, ev.LogPos)
	}
	if !ev.At.Equal(at(1785000000)) {
		t.Errorf("at = %s, want %s", ev.At, at(1785000000))
	}
	if ev.ServerID != 17 {
		t.Errorf("server id = %d, want 17", ev.ServerID)
	}
}

// The replication library reconnects on its own and resumes from a position that
// can lag the transaction in flight, so the same transaction arrives twice. Stored
// twice it produces the inverse twice: a script that aborts on a duplicate key
// part way through, or on a table with no primary key silently inserts the row
// again. The coverage model detects changes that are missing and is blind to
// changes that are doubled, so the store itself has to refuse the second copy.
func TestAppendingTheSameTransactionTwiceStoresItOnce(t *testing.T) {
	s := open(t)

	tx := txn("gtid:1", 1785000000, update("orders", 576, "pending", "shipped"))

	if err := s.AppendTransaction(tx, checkpoint(576)); err != nil {
		t.Fatalf("first AppendTransaction returned error: %v", err)
	}
	if err := s.AppendTransaction(tx, checkpoint(576)); err != nil {
		t.Fatalf("re-delivery must be accepted quietly, not refused: %v", err)
	}

	got, err := s.Events(change.Filter{}, at(1785000010))
	if err != nil {
		t.Fatalf("Events returned error: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("Events returned %d events after a re-delivery, want 1", len(got))
	}
}

// Two different transactions that happen to commit in the same second must stay
// distinct: binlog timestamps have one-second resolution, so time cannot tell
// them apart and only the source's own identifier can.
func TestTransactionsWithinTheSameSecondAreKeptApart(t *testing.T) {
	s := open(t)

	if err := s.AppendTransaction(txn("gtid:1", 1785000000, update("orders", 100, "a", "b")), checkpoint(100)); err != nil {
		t.Fatalf("AppendTransaction returned error: %v", err)
	}
	if err := s.AppendTransaction(txn("gtid:2", 1785000000, update("orders", 200, "b", "c")), checkpoint(200)); err != nil {
		t.Fatalf("AppendTransaction returned error: %v", err)
	}

	got, err := s.Events(change.Filter{}, at(1785000010))
	if err != nil {
		t.Fatalf("Events returned error: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("Events returned %d events, want 2", len(got))
	}
}

// Undoing a sequence means applying the inverses in the opposite order, so the
// order rows were applied in has to survive storage exactly. Ordering by time
// would scramble anything committed within the same second.
func TestEventsComeBackInTheOrderTheyWereApplied(t *testing.T) {
	s := open(t)

	first := txn("gtid:1", 1785000000,
		update("orders", 100, "a", "b"),
		update("orders", 100, "c", "d"),
	)
	second := txn("gtid:2", 1785000000, update("orders", 200, "e", "f"))

	if err := s.AppendTransaction(first, checkpoint(100)); err != nil {
		t.Fatalf("AppendTransaction returned error: %v", err)
	}
	if err := s.AppendTransaction(second, checkpoint(200)); err != nil {
		t.Fatalf("AppendTransaction returned error: %v", err)
	}

	got, err := s.Events(change.Filter{}, at(1785000010))
	if err != nil {
		t.Fatalf("Events returned error: %v", err)
	}

	var order []string
	for _, ev := range got {
		order = append(order, ev.Before[1].(string))
	}
	if want := []string{"a", "c", "e"}; !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
}

// The checkpoint says where to resume. If it could advance without the rows it
// covers, a crash between the two would resume past changes that were never
// stored — a hole with nothing recording that it exists.
func TestTheCheckpointAdvancesWithTheTransaction(t *testing.T) {
	s := open(t)

	if err := s.AppendTransaction(txn("gtid:1", 1785000000, update("orders", 576, "a", "b")), checkpoint(576)); err != nil {
		t.Fatalf("AppendTransaction returned error: %v", err)
	}

	got, err := s.Checkpoint()
	if err != nil {
		t.Fatalf("Checkpoint returned error: %v", err)
	}
	if got.LogFile != "binlog.000004" || got.LogPos != 576 {
		t.Errorf("checkpoint = %s:%d, want binlog.000004:576", got.LogFile, got.LogPos)
	}
}

func TestTheCheckpointOfAFreshStoreIsEmpty(t *testing.T) {
	s := open(t)

	got, err := s.Checkpoint()
	if err != nil {
		t.Fatalf("Checkpoint returned error: %v", err)
	}
	if !got.IsZero() {
		t.Errorf("checkpoint = %+v, want the zero value so the caller starts from the beginning", got)
	}
}

// A value the codec cannot encode must take the whole transaction down. Storing
// the rows around it would leave a transaction that is missing one of its
// changes while looking complete, and the checkpoint would move past it.
func TestATransactionWithAnUnstorableValueIsRejectedWhole(t *testing.T) {
	s := open(t)

	bad := update("orders", 576, "pending", "shipped")
	bad.After = []any{int64(1), struct{ Unsupported int }{1}}

	tx := txn("gtid:1", 1785000000, update("orders", 100, "a", "b"), bad)
	if err := s.AppendTransaction(tx, checkpoint(576)); err == nil {
		t.Fatal("AppendTransaction accepted a value it cannot encode")
	}

	var stored int
	if err := s.db.QueryRow(`SELECT count(*) FROM row_change`).Scan(&stored); err != nil {
		t.Fatalf("counting stored rows returned error: %v", err)
	}
	if stored != 0 {
		t.Errorf("%d row changes survived a rejected transaction, want none", stored)
	}

	cp, err := s.Checkpoint()
	if err != nil {
		t.Fatalf("Checkpoint returned error: %v", err)
	}
	if !cp.IsZero() {
		t.Errorf("checkpoint advanced past a transaction that was not stored: %+v", cp)
	}
}

func TestEventsCanBeNarrowedByTableAndTime(t *testing.T) {
	s := open(t)

	if err := s.AppendTransaction(txn("gtid:1", 1785000000, update("orders", 100, "a", "b")), checkpoint(100)); err != nil {
		t.Fatalf("AppendTransaction returned error: %v", err)
	}
	if err := s.AppendTransaction(txn("gtid:2", 1785000060, update("customers", 200, "c", "d")), checkpoint(200)); err != nil {
		t.Fatalf("AppendTransaction returned error: %v", err)
	}

	byTable, err := s.Events(change.Filter{Tables: []string{"shop.customers"}}, at(1785000070))
	if err != nil {
		t.Fatalf("Events returned error: %v", err)
	}
	if len(byTable) != 1 || byTable[0].Table != "customers" {
		t.Errorf("filtering by table returned %d events, want just customers", len(byTable))
	}

	byTime, err := s.Events(change.Filter{From: at(1785000030)}, at(1785000070))
	if err != nil {
		t.Fatalf("Events returned error: %v", err)
	}
	if len(byTime) != 1 || byTime[0].Table != "customers" {
		t.Errorf("filtering by time returned %d events, want just the later one", len(byTime))
	}
}

// The statement that caused a change is optional, and its absence must not be
// filled in from a neighbour — a "caused by" naming the wrong statement is worse
// than none, because it is the line an operator reads before running the script.
func TestTheOriginatingStatementIsStoredPerRowAndNotInherited(t *testing.T) {
	s := open(t)

	withQuery := update("orders", 100, "a", "b")
	withQuery.Query = "UPDATE orders SET status = 'shipped'"
	without := update("orders", 100, "c", "d")

	if err := s.AppendTransaction(txn("gtid:1", 1785000000, withQuery, without), checkpoint(100)); err != nil {
		t.Fatalf("AppendTransaction returned error: %v", err)
	}

	got, err := s.Events(change.Filter{}, at(1785000010))
	if err != nil {
		t.Fatalf("Events returned error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Events returned %d events, want 2", len(got))
	}
	if got[0].Query != withQuery.Query {
		t.Errorf("first query = %q, want %q", got[0].Query, withQuery.Query)
	}
	if got[1].Query != "" {
		t.Errorf("second query = %q, want it empty rather than inherited", got[1].Query)
	}
}
