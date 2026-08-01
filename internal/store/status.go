package store

import (
	"fmt"
	"time"
)

// Miss is one change the collector saw and could not store.
type Miss struct {
	At     time.Time
	Reason string
}

// Report is everything a store can say about its own health.
//
// It exists because coverage health is otherwise only discoverable by writing
// Go, which means it gets discovered during the incident instead — at the moment
// when the answer "the collector stopped three hours ago" is least useful.
type Report struct {
	// Coverage is the period the store can answer for, and the allowances it
	// judges itself against.
	Coverage Coverage

	// Stale is how far behind the high-water mark is at the instant asked about.
	// Zero on a store that has collected nothing, where "behind" has no meaning.
	Stale time.Duration

	// Gaps and Misses are what the store knows it does not hold. Neither decides
	// the verdict — see Verdict.
	Gaps   []Gap
	Misses []Miss

	// Binding is the source this store follows, and Bound whether it has one yet.
	// A store watch has never connected for has none, and reporting a blank
	// server identity as though it were one would be its own small lie.
	Binding Binding
	Bound   bool

	// Problems is what Integrity found, empty on a sound store.
	Problems []string
}

// Verdict says why this store is not healthy, or "" if it is.
//
// It covers what an operator can act on: a damaged file, a store holding
// nothing, a collector that has stopped. Gaps and misses are deliberately not
// here. They are permanent history — nothing anyone does removes them — so
// failing the verdict on one would leave status red forever, and a check that is
// always red is a check nobody reads.
func (r Report) Verdict() string {
	if len(r.Problems) > 0 {
		return r.Problems[0]
	}
	if r.Coverage.To.IsZero() {
		return "the store " + noCoverageReason
	}
	if r.Stale > r.Coverage.MaxStaleness {
		return "the store " + staleReason(r.Coverage, r.Stale)
	}
	return ""
}

// Healthy reports whether the store is sound and its collector is keeping up.
func (r Report) Healthy() bool { return r.Verdict() == "" }

// Status reports the store's health as of now.
func (s *Store) Status(now time.Time) (Report, error) {
	var r Report

	// Coverage, gaps and misses come out of one transaction: a prune running
	// between two of these reads would produce a report describing a window that
	// never existed, which is the shape of confidently-wrong this store is built
	// to avoid. The binding and the integrity check sit outside it — the binding
	// is written once and never changes, and PRAGMA integrity_check is a
	// whole-file scan that has no business holding a read transaction open.
	tx, err := s.db.Begin()
	if err != nil {
		return r, fmt.Errorf("store: reading the status of %s: %w", s.path, err)
	}
	defer tx.Rollback()

	c, err := readCoverage(tx, s.path)
	if err != nil {
		return r, err
	}
	gaps, err := readGaps(tx)
	if err != nil {
		return r, err
	}
	misses, err := readMisses(tx)
	if err != nil {
		return r, err
	}
	r.Coverage = c
	if !c.To.IsZero() {
		r.Stale = now.Sub(c.To)
	}
	for _, g := range gaps {
		r.Gaps = append(r.Gaps, Gap{From: g.from, To: g.to, Reason: g.reason})
	}
	for _, m := range misses {
		r.Misses = append(r.Misses, Miss{At: m.at, Reason: m.reason})
	}

	if r.Binding, r.Bound, err = s.binding(); err != nil {
		return r, err
	}
	if r.Problems, err = s.Integrity(); err != nil {
		return r, err
	}
	return r, nil
}
