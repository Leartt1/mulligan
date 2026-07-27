package binlog

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-mysql-org/go-mysql/replication"
)

func rowsEvent() *replication.RowsEvent {
	return &replication.RowsEvent{
		Table:       tableMap(),
		ColumnCount: 2,
		Rows:        [][]any{{int64(7), "new"}},
	}
}

func TestEventsFromReturnsChangesForAMatchingRowEvent(t *testing.T) {
	e := &replication.BinlogEvent{
		Header: header(replication.WRITE_ROWS_EVENTv2),
		Event:  rowsEvent(),
	}

	got, err := eventsFrom("binlog.000004", e, Filter{Tables: []string{"shop.orders"}})
	if err != nil {
		t.Fatalf("eventsFrom returned error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("eventsFrom returned %d events, want 1", len(got))
	}
}

func TestEventsFromDropsRowEventsOutsideTheFilter(t *testing.T) {
	e := &replication.BinlogEvent{
		Header: header(replication.WRITE_ROWS_EVENTv2),
		Event:  rowsEvent(),
	}

	got, err := eventsFrom("binlog.000004", e, Filter{Tables: []string{"shop.customers"}})
	if err != nil {
		t.Fatalf("eventsFrom returned error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("eventsFrom returned %d events for an unselected table, want 0", len(got))
	}
}

// A binlog is mostly not row events — format descriptions, rotates, GTIDs and
// queries all stream past. Treating one as an error would abort every scan on
// the first event.
func TestEventsFromIgnoresNonRowEvents(t *testing.T) {
	e := &replication.BinlogEvent{
		Header: header(replication.QUERY_EVENT),
		Event:  &replication.QueryEvent{Schema: []byte("shop"), Query: []byte("BEGIN")},
	}

	got, err := eventsFrom("binlog.000004", e, Filter{})
	if err != nil {
		t.Fatalf("eventsFrom returned error for a query event: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("eventsFrom returned %d events for a query event, want 0", len(got))
	}
}

// A row event the engine cannot interpret has to stop the scan. Skipping it
// would hand back a revert script missing the statement that caused the damage.
func TestEventsFromFailsOnAnUnreadableRowEvent(t *testing.T) {
	rows := rowsEvent()
	rows.Table.ColumnName = nil

	e := &replication.BinlogEvent{
		Header: header(replication.WRITE_ROWS_EVENTv2),
		Event:  rows,
	}

	if got, err := eventsFrom("binlog.000004", e, Filter{}); err == nil {
		t.Fatalf("eventsFrom returned %d events, want error", len(got))
	}
}

func TestReadFileRejectsAFileThatIsNotABinlog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-binlog")
	if err := os.WriteFile(path, []byte("just some text\n"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if got, err := ReadFile(path, Filter{}); err == nil {
		t.Fatalf("ReadFile returned %d events, want error", len(got))
	}
}

func TestReadFileReportsAMissingFile(t *testing.T) {
	if got, err := ReadFile(filepath.Join(t.TempDir(), "absent.000001"), Filter{}); err == nil {
		t.Fatalf("ReadFile returned %d events, want error", len(got))
	}
}
