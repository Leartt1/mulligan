package binlog

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/go-mysql-org/go-mysql/replication"

	"github.com/learttytyri/mulligan/internal/change"
)

// ReadFile scans a binlog file and returns the change events matching f, in the
// order the source database applied them.
func ReadFile(path string, f Filter) ([]change.Event, error) {
	p := newParser()
	logFile := filepath.Base(path)

	var out []change.Event
	err := p.ParseFile(path, 0, func(e *replication.BinlogEvent) error {
		events, err := eventsFrom(logFile, e, f)
		if err != nil {
			return err
		}
		out = append(out, events...)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("binlog: reading %s: %w", path, err)
	}
	return out, nil
}

// newParser configures value decoding so that what the engine renders back into
// SQL means the same thing it meant in the source row.
func newParser() *replication.BinlogParser {
	p := replication.NewBinlogParser()

	// Decode temporal columns into time.Time rather than pre-formatted strings,
	// so the engine controls the literal syntax it emits.
	p.SetParseTime(true)
	p.SetTimestampStringLocation(time.UTC)

	// Keep DECIMAL exact. Decoded as a float it would round, and a reversal that
	// restores a rounded amount is worse than one that refuses to run.
	p.SetUseDecimal(true)

	// A binlog whose checksum does not match has been truncated or corrupted,
	// and rows decoded out of it cannot be trusted to reverse anything.
	p.SetVerifyChecksum(true)

	return p
}

// eventsFrom turns one parsed binlog event into the change events it describes.
//
// Most of a binlog is not row data — format descriptions, rotates, GTIDs and
// queries all stream past — so anything else is skipped rather than refused. A
// row event that cannot be interpreted does stop the scan: silently dropping it
// would return a revert script missing the statement that caused the damage.
func eventsFrom(logFile string, e *replication.BinlogEvent, f Filter) ([]change.Event, error) {
	rows, ok := e.Event.(*replication.RowsEvent)
	if !ok {
		return nil, nil
	}

	events, err := Convert(logFile, e.Header, rows)
	if err != nil {
		return nil, err
	}

	kept := events[:0]
	for _, ev := range events {
		if f.Match(ev) {
			kept = append(kept, ev)
		}
	}
	return kept, nil
}
