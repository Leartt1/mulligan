package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/learttytyri/mulligan/internal/change"
)

// DefaultMaxStaleness is how far behind the collector may fall before the store
// stops answering.
//
// It is deliberately short. The cost of being wrong in one direction is a
// refusal an operator can override by looking at why the collector stopped; in
// the other it is an authoritative "nothing happened" during an incident.
const DefaultMaxStaleness = 5 * time.Minute

// Coverage is the period a store can answer for.
type Coverage struct {
	// From is the earliest instant the store can speak to. Earlier than this it
	// has no opinion — not "nothing happened".
	From time.Time

	// To is the high-water mark: the furthest point the collector has reached.
	// It advances on every committed transaction and on the idle signals from the
	// source, so a quiet database keeps it moving while a stopped collector does
	// not.
	To time.Time

	// MaxStaleness is how far behind To may fall before the store refuses. It is
	// the collector's setting, recorded here so a generate run on another machine
	// applies the same judgement rather than its own.
	MaxStaleness time.Duration

	// Retention is how far back the window is meant to reach. Zero means nothing
	// is discarded.
	Retention time.Duration
}

// OpenCoverage marks the instant from which this store can answer, if it has not
// been marked already.
//
// A restart must not move it forward: the changes collected before the restart
// are still stored, and moving the boundary would stop them being answerable
// while they sat there.
func (s *Store) OpenCoverage(when time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO meta (key, value) VALUES ('coverage_from', ?) ON CONFLICT (key) DO NOTHING`,
		when.Unix())
	if err != nil {
		return fmt.Errorf("store: opening coverage on %s: %w", s.path, err)
	}
	return nil
}

// SetMaxStaleness records how far behind the collector may fall.
func (s *Store) SetMaxStaleness(d time.Duration) error {
	_, err := s.db.Exec(
		`INSERT INTO meta (key, value) VALUES ('max_staleness_seconds', ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		int64(d.Seconds()))
	if err != nil {
		return fmt.Errorf("store: recording the staleness allowance on %s: %w", s.path, err)
	}
	return nil
}

// RecordGap marks a period the collector knows it did not see.
//
// The bounds are the last checkpoint and the first event received after
// resuming: both are known exactly, where "the earliest event the server still
// holds" is not, and an approximate boundary is the kind of nearly-right that
// this design exists to avoid.
func (s *Store) RecordGap(from, to time.Time, reason string) error {
	_, err := s.db.Exec(
		`INSERT INTO gap (from_at, to_at, reason) VALUES (?, ?, ?)`,
		from.Unix(), to.Unix(), reason)
	if err != nil {
		return fmt.Errorf("store: recording a gap on %s: %w", s.path, err)
	}
	return nil
}

// RecordMiss marks one change the collector saw and could not store.
//
// Refusing the whole stream over a single unstorable change would make the tool
// unusable on a busy server, but continuing silently would leave a revert
// missing exactly the statement someone is looking for. The change is recorded
// as absent so any window containing it is refused instead.
func (s *Store) RecordMiss(when time.Time, reason string) error {
	_, err := s.db.Exec(
		`INSERT INTO miss (at, reason) VALUES (?, ?)`,
		when.Unix(), reason)
	if err != nil {
		return fmt.Errorf("store: recording a missed change on %s: %w", s.path, err)
	}
	return nil
}

// Coverage reports the period this store can answer for.
func (s *Store) Coverage() (Coverage, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Coverage{}, fmt.Errorf("store: reading coverage of %s: %w", s.path, err)
	}
	defer tx.Rollback()
	return readCoverage(tx, s.path)
}

func readCoverage(tx *sql.Tx, path string) (Coverage, error) {
	c := Coverage{MaxStaleness: DefaultMaxStaleness}

	from, hasFrom, err := metaInt(tx, "coverage_from")
	if err != nil {
		return c, fmt.Errorf("store: reading coverage of %s: %w", path, err)
	}
	if !hasFrom {
		// A store the collector never explicitly opened still covers whatever it
		// holds; the earliest change it stored is the honest boundary. An empty
		// store has none, which the caller reports as "nothing recorded yet".
		var earliest sql.NullInt64
		if err := tx.QueryRow(`SELECT MIN(committed_at) FROM txn`).Scan(&earliest); err != nil {
			return c, fmt.Errorf("store: reading coverage of %s: %w", path, err)
		}
		from = earliest.Int64
	}
	if from > 0 {
		c.From = time.Unix(from, 0).UTC()
	}

	to, hasTo, err := metaInt(tx, "coverage_to")
	if err != nil {
		return c, fmt.Errorf("store: reading coverage of %s: %w", path, err)
	}
	if hasTo {
		c.To = time.Unix(to, 0).UTC()
	}

	stale, hasStale, err := metaInt(tx, "max_staleness_seconds")
	if err != nil {
		return c, fmt.Errorf("store: reading coverage of %s: %w", path, err)
	}
	if hasStale {
		c.MaxStaleness = time.Duration(stale) * time.Second
	}

	retention, hasRetention, err := metaInt(tx, "retention_seconds")
	if err != nil {
		return c, fmt.Errorf("store: reading coverage of %s: %w", path, err)
	}
	if hasRetention {
		c.Retention = time.Duration(retention) * time.Second
	}

	return c, nil
}

func metaInt(tx *sql.Tx, key string) (int64, bool, error) {
	var raw string
	err := tx.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&raw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, err
	}

	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("meta key %q holds %q, which is not a number", key, raw)
	}
	return n, true, nil
}

// noCoverageReason and staleReason word the two conditions under which a store
// cannot answer for anything at all, whatever window is asked for.
//
// They live here once because two callers reach them: generate refuses on them,
// and status reports on them. Worded separately they would drift, and a status
// that says "OK" while generate refuses is worse than no status at all — it
// would send an operator into an incident believing a window they cannot have.
const noCoverageReason = "has not recorded any changes yet; " +
	"start mulligan watch against the source before generating a revert"

func staleReason(c Coverage, behind time.Duration) string {
	return fmt.Sprintf("is stale — nothing has been collected for %s (allowed %s, last change at %s UTC); "+
		"the collector may have stopped, and an empty result here would read as though nothing happened",
		behind.Round(time.Second), c.MaxStaleness, c.To.Format("2006-01-02 15:04:05"))
}

// checkCoverage decides whether the store can answer for the window in f.
//
// Every branch here exists because the alternative is an empty or partial result
// that reads as an answer. A revert that silently omits changes is the failure
// this whole design is arranged around, so each condition is refused by name and
// with the reason, rather than quietly returning fewer rows.
func checkCoverage(c Coverage, f change.Filter, now time.Time, gaps []gapRow, misses []missRow) error {
	if c.To.IsZero() {
		return fmt.Errorf("store: %s", noCoverageReason)
	}

	if behind := now.Sub(c.To); behind > c.MaxStaleness {
		return fmt.Errorf("store: %s", staleReason(c, behind))
	}

	// An unset From asks for as far back as the store goes, which is a different
	// request from naming an instant the store cannot answer for.
	if !f.From.IsZero() && f.From.Before(c.From) {
		return fmt.Errorf("store: the window reaches back to %s UTC but the store only covers from %s UTC; "+
			"changes before that were never recorded",
			f.From.UTC().Format("2006-01-02 15:04:05"), c.From.Format("2006-01-02 15:04:05"))
	}

	// Asking about a period the collector has not reached is refused, but only
	// once it is further ahead than the staleness allowance. Inside that
	// allowance the check above has already established the collector is live, and
	// refusing a --to a few seconds past its last commit would fire on almost
	// every honest request.
	if !f.To.IsZero() && f.To.After(c.To.Add(c.MaxStaleness)) {
		return fmt.Errorf("store: the window ends at %s UTC but the collector has not reached past %s UTC; "+
			"changes in between may still be arriving",
			f.To.UTC().Format("2006-01-02 15:04:05"), c.To.Format("2006-01-02 15:04:05"))
	}

	from, to := windowBounds(c, f)

	for _, g := range gaps {
		if !from.After(g.to) && !g.from.After(to) {
			return fmt.Errorf("store: the window overlaps a gap from %s to %s UTC (%s); "+
				"changes in that period were never recorded",
				g.from.Format("2006-01-02 15:04:05"), g.to.Format("2006-01-02 15:04:05"), g.reason)
		}
	}

	for _, m := range misses {
		if !from.After(m.at) && !m.at.After(to) {
			return fmt.Errorf("store: a change at %s UTC inside the window was seen but could not be recorded (%s); "+
				"a revert built from this window would be missing it",
				m.at.Format("2006-01-02 15:04:05"), m.reason)
		}
	}

	return nil
}

// windowBounds resolves an unbounded filter against what the store actually
// covers, so an unset bound is compared as the store's own edge rather than as
// the zero time or the far future.
func windowBounds(c Coverage, f change.Filter) (time.Time, time.Time) {
	from, to := f.From, f.To
	if from.IsZero() {
		from = c.From
	}
	if to.IsZero() {
		to = c.To
	}
	return from.UTC(), to.UTC()
}

type gapRow struct {
	from, to time.Time
	reason   string
}

type missRow struct {
	at     time.Time
	reason string
}

func readGaps(tx *sql.Tx) ([]gapRow, error) {
	rows, err := tx.Query(`SELECT from_at, to_at, reason FROM gap ORDER BY from_at`)
	if err != nil {
		return nil, fmt.Errorf("store: reading recorded gaps: %w", err)
	}
	defer rows.Close()

	var out []gapRow
	for rows.Next() {
		var from, to int64
		var reason string
		if err := rows.Scan(&from, &to, &reason); err != nil {
			return nil, fmt.Errorf("store: reading recorded gaps: %w", err)
		}
		out = append(out, gapRow{from: time.Unix(from, 0).UTC(), to: time.Unix(to, 0).UTC(), reason: reason})
	}
	return out, rows.Err()
}

func readMisses(tx *sql.Tx) ([]missRow, error) {
	rows, err := tx.Query(`SELECT at, reason FROM miss ORDER BY at`)
	if err != nil {
		return nil, fmt.Errorf("store: reading missed changes: %w", err)
	}
	defer rows.Close()

	var out []missRow
	for rows.Next() {
		var when int64
		var reason string
		if err := rows.Scan(&when, &reason); err != nil {
			return nil, fmt.Errorf("store: reading missed changes: %w", err)
		}
		out = append(out, missRow{at: time.Unix(when, 0).UTC(), reason: reason})
	}
	return out, rows.Err()
}

// AdvanceCoverage moves the high-water mark without storing a change.
//
// It is how an idle source keeps the window current: heartbeats and rotates say
// the collector is still attached and has seen everything up to that instant.
// Without it a database nobody is writing to is indistinguishable from a
// collector that has stopped, and the staleness refusal would fire on a
// perfectly healthy system.
func (s *Store) AdvanceCoverage(to time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: advancing coverage on %s: %w", s.path, err)
	}
	defer tx.Rollback()

	if err := advanceCoverageTo(tx, to); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: advancing coverage on %s: %w", s.path, err)
	}
	return nil
}

// Gap is a period the store knows it did not record.
type Gap struct {
	From, To time.Time
	Reason   string
}

// Gaps returns the periods this store knows are missing, oldest first.
//
// Exported because coverage health is something an operator needs to be able to
// look at deliberately, rather than discovering during an incident that a window
// they were counting on has a hole in it.
func (s *Store) Gaps() ([]Gap, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: reading gaps from %s: %w", s.path, err)
	}
	defer tx.Rollback()

	rows, err := readGaps(tx)
	if err != nil {
		return nil, err
	}

	out := make([]Gap, len(rows))
	for i, r := range rows {
		out[i] = Gap{From: r.from, To: r.to, Reason: r.reason}
	}
	return out, nil
}
