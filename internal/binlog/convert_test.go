package binlog

import (
	"reflect"
	"strings"
	"testing"
	"time"

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

func header(t replication.EventType) *replication.EventHeader {
	return &replication.EventHeader{
		Timestamp: 1785000000,
		EventType: t,
		ServerID:  17,
		LogPos:    4242,
	}
}

func TestConvertMapsWriteRowsToInsertPerRow(t *testing.T) {
	rows := &replication.RowsEvent{
		Table:       tableMap(),
		ColumnCount: 2,
		Rows:        [][]any{{int64(7), "new"}, {int64(8), "new"}},
	}

	got, err := Convert("binlog.000004", header(replication.WRITE_ROWS_EVENTv2), rows)
	if err != nil {
		t.Fatalf("Convert returned error: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("Convert returned %d events, want 2", len(got))
	}
	for i, ev := range got {
		if ev.Op != change.Insert {
			t.Errorf("event %d op = %v, want Insert", i, ev.Op)
		}
		if ev.Before != nil {
			t.Errorf("event %d before = %v, want nil for an insert", i, ev.Before)
		}
	}
	if want := []any{int64(7), "new"}; !reflect.DeepEqual(got[0].After, want) {
		t.Errorf("event 0 after = %v, want %v", got[0].After, want)
	}
	if want := []any{int64(8), "new"}; !reflect.DeepEqual(got[1].After, want) {
		t.Errorf("event 1 after = %v, want %v", got[1].After, want)
	}
}

func TestConvertMapsDeleteRowsToDeleteWithBeforeImage(t *testing.T) {
	rows := &replication.RowsEvent{
		Table:       tableMap(),
		ColumnCount: 2,
		Rows:        [][]any{{int64(7), "shipped"}},
	}

	got, err := Convert("binlog.000004", header(replication.DELETE_ROWS_EVENTv2), rows)
	if err != nil {
		t.Fatalf("Convert returned error: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("Convert returned %d events, want 1", len(got))
	}
	if got[0].Op != change.Delete {
		t.Errorf("op = %v, want Delete", got[0].Op)
	}
	if want := []any{int64(7), "shipped"}; !reflect.DeepEqual(got[0].Before, want) {
		t.Errorf("before = %v, want %v", got[0].Before, want)
	}
	if got[0].After != nil {
		t.Errorf("after = %v, want nil for a delete", got[0].After)
	}
}

// An update rows event carries the before and after image of each row as two
// consecutive entries; pairing them wrongly would swap what gets restored with
// what gets overwritten.
func TestConvertPairsUpdateRowsIntoOneEvent(t *testing.T) {
	rows := &replication.RowsEvent{
		Table:       tableMap(),
		ColumnCount: 2,
		Rows: [][]any{
			{int64(7), "new"}, {int64(7), "shipped"},
			{int64(8), "new"}, {int64(8), "shipped"},
		},
	}

	got, err := Convert("binlog.000004", header(replication.UPDATE_ROWS_EVENTv2), rows)
	if err != nil {
		t.Fatalf("Convert returned error: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("Convert returned %d events, want 2", len(got))
	}
	if got[0].Op != change.Update {
		t.Errorf("op = %v, want Update", got[0].Op)
	}
	if want := []any{int64(7), "new"}; !reflect.DeepEqual(got[0].Before, want) {
		t.Errorf("event 0 before = %v, want %v", got[0].Before, want)
	}
	if want := []any{int64(7), "shipped"}; !reflect.DeepEqual(got[0].After, want) {
		t.Errorf("event 0 after = %v, want %v", got[0].After, want)
	}
	if want := []any{int64(8), "new"}; !reflect.DeepEqual(got[1].Before, want) {
		t.Errorf("event 1 before = %v, want %v", got[1].Before, want)
	}
}

func TestConvertRejectsUpdateWithUnpairedRows(t *testing.T) {
	rows := &replication.RowsEvent{
		Table:       tableMap(),
		ColumnCount: 2,
		Rows:        [][]any{{int64(7), "new"}, {int64(7), "shipped"}, {int64(8), "new"}},
	}

	if got, err := Convert("binlog.000004", header(replication.UPDATE_ROWS_EVENTv2), rows); err == nil {
		t.Fatalf("Convert returned %d events, want error for an odd row count", len(got))
	}
}

func TestConvertCarriesProvenanceFromTheLog(t *testing.T) {
	rows := &replication.RowsEvent{
		Table:       tableMap(),
		ColumnCount: 2,
		Rows:        [][]any{{int64(7), "new"}},
	}

	got, err := Convert("binlog.000004", header(replication.WRITE_ROWS_EVENTv2), rows)
	if err != nil {
		t.Fatalf("Convert returned error: %v", err)
	}

	ev := got[0]
	if ev.Schema != "shop" || ev.Table != "orders" {
		t.Errorf("table = %s.%s, want shop.orders", ev.Schema, ev.Table)
	}
	if ev.LogFile != "binlog.000004" || ev.LogPos != 4242 {
		t.Errorf("position = %s:%d, want binlog.000004:4242", ev.LogFile, ev.LogPos)
	}
	if ev.ServerID != 17 {
		t.Errorf("server id = %d, want 17", ev.ServerID)
	}
	if want := time.Unix(1785000000, 0).UTC(); !ev.At.Equal(want) {
		t.Errorf("at = %s, want %s", ev.At, want)
	}
}

func TestConvertMarksPrimaryKeyColumns(t *testing.T) {
	rows := &replication.RowsEvent{
		Table:       tableMap(),
		ColumnCount: 2,
		Rows:        [][]any{{int64(7), "new"}},
	}

	got, err := Convert("binlog.000004", header(replication.WRITE_ROWS_EVENTv2), rows)
	if err != nil {
		t.Fatalf("Convert returned error: %v", err)
	}

	want := []change.Column{{Name: "id", PrimaryKey: true}, {Name: "status"}}
	if !reflect.DeepEqual(got[0].Columns, want) {
		t.Errorf("columns = %+v, want %+v", got[0].Columns, want)
	}
}

// Without binlog_row_metadata=FULL the log carries no column names, and the
// engine would have to guess which value belongs to which column.
func TestConvertRefusesLogWithoutColumnNames(t *testing.T) {
	tm := tableMap()
	tm.ColumnName = nil
	rows := &replication.RowsEvent{Table: tm, ColumnCount: 2, Rows: [][]any{{int64(7), "new"}}}

	got, err := Convert("binlog.000004", header(replication.WRITE_ROWS_EVENTv2), rows)
	if err == nil {
		t.Fatalf("Convert returned %d events, want error", len(got))
	}
	if !strings.Contains(err.Error(), "binlog_row_metadata") {
		t.Errorf("error = %v, want it to name the setting to fix", err)
	}
}

// The decoder records one skip list per row image, empty when nothing was
// skipped. A non-empty outer slice therefore means "there were rows", not
// "columns were omitted" — reading it as the latter refuses every event a
// correctly configured server produces.
func TestConvertAcceptsAFullRowImageThatSkippedNothing(t *testing.T) {
	rows := &replication.RowsEvent{
		Table:          tableMap(),
		ColumnCount:    2,
		Rows:           [][]any{{int64(7), "new"}, {int64(8), "new"}},
		SkippedColumns: [][]int{{}, {}},
	}

	got, err := Convert("binlog.000004", header(replication.WRITE_ROWS_EVENTv2), rows)
	if err != nil {
		t.Fatalf("Convert refused a full row image: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("Convert returned %d events, want 2", len(got))
	}
}

// Without binlog_row_image=FULL some columns are omitted from the row image, so
// a reversal would restore the wrong values for whatever was left out.
func TestConvertRefusesPartialRowImage(t *testing.T) {
	rows := &replication.RowsEvent{
		Table:          tableMap(),
		ColumnCount:    2,
		Rows:           [][]any{{int64(7)}},
		SkippedColumns: [][]int{{1}},
	}

	got, err := Convert("binlog.000004", header(replication.WRITE_ROWS_EVENTv2), rows)
	if err == nil {
		t.Fatalf("Convert returned %d events, want error", len(got))
	}
	if !strings.Contains(err.Error(), "binlog_row_image") {
		t.Errorf("error = %v, want it to name the setting to fix", err)
	}
}

func TestConvertRejectsNonRowEvent(t *testing.T) {
	rows := &replication.RowsEvent{
		Table:       tableMap(),
		ColumnCount: 2,
		Rows:        [][]any{{int64(7), "new"}},
	}

	if got, err := Convert("binlog.000004", header(replication.QUERY_EVENT), rows); err == nil {
		t.Fatalf("Convert returned %d events, want error", len(got))
	}
}

// Every row-event version in the wild has to map, or events from an older
// server or from MariaDB are silently dropped from a revert.
func TestConvertHandlesEveryRowEventVersion(t *testing.T) {
	tests := []struct {
		eventType replication.EventType
		want      change.Op
	}{
		{replication.WRITE_ROWS_EVENTv0, change.Insert},
		{replication.WRITE_ROWS_EVENTv1, change.Insert},
		{replication.WRITE_ROWS_EVENTv2, change.Insert},
		{replication.MARIADB_WRITE_ROWS_COMPRESSED_EVENT_V1, change.Insert},
		{replication.DELETE_ROWS_EVENTv0, change.Delete},
		{replication.DELETE_ROWS_EVENTv1, change.Delete},
		{replication.DELETE_ROWS_EVENTv2, change.Delete},
		{replication.MARIADB_DELETE_ROWS_COMPRESSED_EVENT_V1, change.Delete},
	}

	for _, tt := range tests {
		t.Run(tt.eventType.String(), func(t *testing.T) {
			rows := &replication.RowsEvent{
				Table:       tableMap(),
				ColumnCount: 2,
				Rows:        [][]any{{int64(7), "new"}},
			}

			got, err := Convert("binlog.000004", header(tt.eventType), rows)
			if err != nil {
				t.Fatalf("Convert returned error: %v", err)
			}
			if got[0].Op != tt.want {
				t.Errorf("op = %v, want %v", got[0].Op, tt.want)
			}
		})
	}
}

func TestConvertHandlesEveryUpdateEventVersion(t *testing.T) {
	types := []replication.EventType{
		replication.UPDATE_ROWS_EVENTv0,
		replication.UPDATE_ROWS_EVENTv1,
		replication.UPDATE_ROWS_EVENTv2,
		replication.MARIADB_UPDATE_ROWS_COMPRESSED_EVENT_V1,
	}

	for _, et := range types {
		t.Run(et.String(), func(t *testing.T) {
			rows := &replication.RowsEvent{
				Table:       tableMap(),
				ColumnCount: 2,
				Rows:        [][]any{{int64(7), "new"}, {int64(7), "shipped"}},
			}

			got, err := Convert("binlog.000004", header(et), rows)
			if err != nil {
				t.Fatalf("Convert returned error: %v", err)
			}
			if len(got) != 1 || got[0].Op != change.Update {
				t.Fatalf("got %d events with op %v, want 1 Update", len(got), got[0].Op)
			}
		})
	}
}
