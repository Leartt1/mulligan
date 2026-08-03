package store

import (
	"fmt"
	"time"

	"github.com/learttytyri/mulligan/internal/change"
)

// DefaultPageSize is how many changes a page holds when the caller names no
// limit, and MaxPageSize the most it will hold whatever they ask for.
//
// The cap is not politeness. Everything else that reads a large window streams
// it, precisely so a revert covering ten million rows does not have to fit in
// memory; an unclamped page would be the one place that quietly materializes it.
const (
	DefaultPageSize = 100
	MaxPageSize     = 1000
)

// Entry is a stored change together with the identity the store assigned it.
//
// The id lives here rather than on change.Event because it is the store's own —
// a binlog source has no notion of it, and neither would a Postgres WAL adapter.
// change.Event is the seam those share, and putting a storage detail on it would
// make every source pretend to have one.
type Entry struct {
	ID    int64
	Event change.Event
}

// Page returns up to limit changes matching f, newest first, continuing after
// the cursor. A zero cursor starts at the newest change, and a zero limit means
// DefaultPageSize.
//
// The cursor is the store's own row id rather than an offset because a collector
// is appending while someone pages. Under an offset, each new change shifts the
// window: the reader sees one change twice and never sees another, with nothing
// about the result looking wrong. Ids do not move.
//
// Coverage is checked exactly as it is for a whole-window read. A page is a
// window like any other, and one that quietly omitted a gap or a stalled
// collector would be this project's central failure wearing a friendlier shape —
// an empty list that reads as "nothing happened".
func (s *Store) Page(f change.Filter, before int64, limit int, now time.Time) ([]Entry, error) {
	switch {
	case limit <= 0:
		limit = DefaultPageSize
	case limit > MaxPageSize:
		limit = MaxPageSize
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: reading a page of changes from %s: %w", s.path, err)
	}
	defer tx.Rollback()

	coverage, err := readCoverage(tx, s.path)
	if err != nil {
		return nil, err
	}
	gaps, err := readGaps(tx)
	if err != nil {
		return nil, err
	}
	misses, err := readMisses(tx)
	if err != nil {
		return nil, err
	}
	if err := checkCoverage(coverage, f, now, gaps, misses); err != nil {
		return nil, err
	}

	// Only the cursor is pushed into SQL. Which changes a filter matches is
	// decided by change.Filter, in Go, as every other read here decides it — two
	// definitions of matching, one in Go and one in SQL, would eventually disagree
	// about which changes a revert covers.
	//
	// The cost is rows read and discarded when the filter is narrow. That is
	// recorded as a known limit rather than paid for with a second set of rules.
	rows, err := tx.Query(`SELECT r.id, `+eventColumns+`
		   FROM row_change r
		   JOIN txn t ON t.id = r.txn_id
		  WHERE (? = 0 OR r.id < ?)
		  ORDER BY r.id DESC`, before, before)
	if err != nil {
		return nil, fmt.Errorf("store: reading a page of changes from %s: %w", s.path, err)
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() && len(out) < limit {
		var (
			id  int64
			row eventRow
		)
		if err := rows.Scan(append([]any{&id}, row.dest()...)...); err != nil {
			return nil, fmt.Errorf("store: reading a change: %w", err)
		}
		ev, err := row.decode()
		if err != nil {
			return nil, err
		}
		if f.Match(ev) {
			out = append(out, Entry{ID: id, Event: ev})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading a page of changes from %s: %w", s.path, err)
	}
	return out, nil
}
