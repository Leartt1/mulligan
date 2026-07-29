package store

import (
	"fmt"
	"time"
)

// DefaultRetention is how far back the window reaches when the collector does
// not say otherwise. It matches the scale operators already think in for binlog
// retention.
const DefaultRetention = 7 * 24 * time.Hour

// SetRetention records how far back the window should reach.
//
// It lives in the store rather than in a flag read at generate time so that
// every reader applies the collector's decision. Two machines disagreeing about
// how much history exists is the sort of difference that only shows up during an
// incident.
func (s *Store) SetRetention(d time.Duration) error {
	_, err := s.db.Exec(
		`INSERT INTO meta (key, value) VALUES ('retention_seconds', ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		int64(d.Seconds()))
	if err != nil {
		return fmt.Errorf("store: recording retention on %s: %w", s.path, err)
	}
	return nil
}

// Prune discards changes that have fallen out of the retention window and
// returns how many transactions it removed.
//
// The deletions and the coverage boundary move in one transaction. Separately, a
// generate landing in between would judge the window sound against the old
// boundary and then read rows that had just been discarded — an answer missing
// changes, with nothing about it looking wrong.
//
// Coverage moves to the retention edge, never to the oldest surviving change. A
// database quiet for two days has nothing to delete, and following the surviving
// rows would leave the store claiming to answer for a period it has already
// discarded the changes for.
func (s *Store) Prune(now time.Time) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("store: pruning %s: %w", s.path, err)
	}
	defer tx.Rollback()

	retention, ok, err := metaInt(tx, "retention_seconds")
	if err != nil {
		return 0, fmt.Errorf("store: pruning %s: %w", s.path, err)
	}
	if !ok || retention <= 0 {
		// No retention configured means keep everything. Discarding on a guessed
		// default would throw away the only record of changes nobody asked to lose.
		return 0, nil
	}

	edge := now.Add(-time.Duration(retention) * time.Second)

	res, err := tx.Exec(`DELETE FROM txn WHERE committed_at < ?`, edge.Unix())
	if err != nil {
		return 0, fmt.Errorf("store: pruning %s: %w", s.path, err)
	}
	removed, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: pruning %s: %w", s.path, err)
	}

	// A gap or a missed change lying wholly behind the edge can no longer overlap
	// any window the store will answer, so it goes with the rows it described.
	// One straddling the edge stays: a window starting at the edge must still be
	// refused by it.
	if _, err := tx.Exec(`DELETE FROM gap WHERE to_at < ?`, edge.Unix()); err != nil {
		return 0, fmt.Errorf("store: pruning gaps from %s: %w", s.path, err)
	}
	if _, err := tx.Exec(`DELETE FROM miss WHERE at < ?`, edge.Unix()); err != nil {
		return 0, fmt.Errorf("store: pruning missed changes from %s: %w", s.path, err)
	}

	// Coverage only ever narrows: a store opened yesterday does not gain a week of
	// history because a week-long retention was configured.
	if _, err := tx.Exec(
		`INSERT INTO meta (key, value) VALUES ('coverage_from', ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value
		 WHERE CAST(excluded.value AS INTEGER) > CAST(meta.value AS INTEGER)`,
		edge.Unix()); err != nil {
		return 0, fmt.Errorf("store: moving coverage on %s: %w", s.path, err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: pruning %s: %w", s.path, err)
	}
	return int(removed), nil
}
