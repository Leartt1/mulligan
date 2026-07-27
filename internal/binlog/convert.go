package binlog

import (
	"fmt"
	"time"

	"github.com/go-mysql-org/go-mysql/replication"
	"github.com/shopspring/decimal"

	"github.com/learttytyri/mulligan/internal/change"
)

// Convert maps one rows event from logFile into normalized change events.
//
// A write or delete event carries one row image per modified row; an update
// event carries two per row, the before image immediately followed by the after
// image.
func Convert(logFile string, hdr *replication.EventHeader, rows *replication.RowsEvent) ([]change.Event, error) {
	op, err := opFor(hdr.EventType)
	if err != nil {
		return nil, err
	}
	if rows.Table == nil {
		return nil, fmt.Errorf("binlog: rows event at %s:%d has no table map", logFile, hdr.LogPos)
	}
	table := fmt.Sprintf("%s.%s", rows.Table.Schema, rows.Table.Table)

	// Anything short of a full row image leaves columns out of the event, and a
	// reversal built from it would restore the wrong values for the rest.
	if skippedColumns(rows) {
		return nil, fmt.Errorf("binlog: row image for %s is partial; set binlog_row_image=FULL on the source server", table)
	}

	cols, err := columnsOf(rows.Table)
	if err != nil {
		return nil, err
	}

	base := change.Event{
		Schema:   string(rows.Table.Schema),
		Table:    string(rows.Table.Table),
		Op:       op,
		Columns:  cols,
		LogFile:  logFile,
		LogPos:   hdr.LogPos,
		At:       time.Unix(int64(hdr.Timestamp), 0).UTC(),
		ServerID: hdr.ServerID,
	}

	if op == change.Update {
		if len(rows.Rows)%2 != 0 {
			return nil, fmt.Errorf("binlog: update event for %s at %s:%d has %d row images, want an even number",
				table, logFile, hdr.LogPos, len(rows.Rows))
		}
		out := make([]change.Event, 0, len(rows.Rows)/2)
		for i := 0; i < len(rows.Rows); i += 2 {
			ev := base
			ev.Before = normalizeRow(rows.Rows[i])
			ev.After = normalizeRow(rows.Rows[i+1])
			out = append(out, ev)
		}
		return out, nil
	}

	out := make([]change.Event, 0, len(rows.Rows))
	for _, row := range rows.Rows {
		ev := base
		if op == change.Insert {
			ev.After = normalizeRow(row)
		} else {
			ev.Before = normalizeRow(row)
		}
		out = append(out, ev)
	}
	return out, nil
}

// normalizeRow converts a decoded row image into values the reverse engine
// renders faithfully, leaving everything it already handles untouched.
func normalizeRow(row []any) []any {
	out := make([]any, len(row))
	for i, v := range row {
		out[i] = normalize(v)
	}
	return out
}

// normalize rewrites values whose Go representation the engine cannot turn back
// into SQL without losing something.
//
// DECIMAL is decoded exactly, as a decimal.Decimal, precisely so that an amount
// is not rounded through float64. Carrying its own text forward keeps that
// exactness all the way into the generated statement, and keeps the engine free
// of any dependency on this source adapter's types.
func normalize(v any) any {
	switch x := v.(type) {
	case decimal.Decimal:
		return change.Raw(x.String())
	case *decimal.Decimal:
		if x == nil {
			return nil
		}
		return change.Raw(x.String())
	}
	return v
}

// skippedColumns reports whether any row image left a column out.
//
// The decoder records one skip list per row image, empty when that row carried
// every column, so the length of the outer slice only counts rows. The omission
// has to be looked for inside it.
func skippedColumns(rows *replication.RowsEvent) bool {
	for _, skipped := range rows.SkippedColumns {
		if len(skipped) > 0 {
			return true
		}
	}
	return false
}

// columnsOf reads the column names and primary key out of the table map event.
//
// Both are present only when the source server logs full row metadata, which is
// what lets Mulligan reverse a binlog file without also connecting to the
// database it came from.
func columnsOf(tm *replication.TableMapEvent) ([]change.Column, error) {
	names := tm.ColumnNameString()
	if len(names) != int(tm.ColumnCount) {
		return nil, fmt.Errorf("binlog: log carries no column names for %s.%s; set binlog_row_metadata=FULL on the source server",
			tm.Schema, tm.Table)
	}

	cols := make([]change.Column, len(names))
	for i, name := range names {
		cols[i] = change.Column{Name: name}
	}
	for _, i := range tm.PrimaryKey {
		if i >= uint64(len(cols)) {
			return nil, fmt.Errorf("binlog: primary key column %d is out of range for %s.%s with %d columns",
				i, tm.Schema, tm.Table, len(cols))
		}
		cols[i].PrimaryKey = true
	}
	return cols, nil
}

// opFor maps a binlog event type onto the row operation it describes, across
// every row-event version MySQL and MariaDB emit.
func opFor(t replication.EventType) (change.Op, error) {
	switch t {
	case replication.WRITE_ROWS_EVENTv0,
		replication.WRITE_ROWS_EVENTv1,
		replication.WRITE_ROWS_EVENTv2,
		replication.MARIADB_WRITE_ROWS_COMPRESSED_EVENT_V1:
		return change.Insert, nil

	case replication.UPDATE_ROWS_EVENTv0,
		replication.UPDATE_ROWS_EVENTv1,
		replication.UPDATE_ROWS_EVENTv2,
		replication.MARIADB_UPDATE_ROWS_COMPRESSED_EVENT_V1:
		return change.Update, nil

	// A partial update event stores a JSON column as a diff against the previous
	// document rather than as its value. Reversed as though it were a full image
	// it would write the diff into the column — valid JSON, and silently the
	// wrong document.
	case replication.PARTIAL_UPDATE_ROWS_EVENT:
		return 0, fmt.Errorf("binlog: partial JSON update cannot be reversed; set binlog_row_value_options='' on the source server")

	case replication.DELETE_ROWS_EVENTv0,
		replication.DELETE_ROWS_EVENTv1,
		replication.DELETE_ROWS_EVENTv2,
		replication.MARIADB_DELETE_ROWS_COMPRESSED_EVENT_V1:
		return change.Delete, nil
	}
	return 0, fmt.Errorf("binlog: %s is not a row event", t)
}
