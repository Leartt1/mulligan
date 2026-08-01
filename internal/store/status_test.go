package store

import (
	"strings"
	"testing"
	"time"
)

func TestStatusReportsAKeepingUpCollectorAsHealthy(t *testing.T) {
	s := seeded(t, 1785000000)
	if err := s.SetMaxStaleness(5 * time.Minute); err != nil {
		t.Fatalf("SetMaxStaleness returned error: %v", err)
	}

	r, err := s.Status(at(1785000060))
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if !r.Healthy() {
		t.Errorf("a collector a minute behind reported unhealthy: %q", r.Verdict())
	}
	if r.Stale != time.Minute {
		t.Errorf("Stale = %s, want 1m", r.Stale)
	}
	if !r.Coverage.To.Equal(at(1785000000)) {
		t.Errorf("Coverage.To = %s, want the last committed change", r.Coverage.To)
	}
}

// The failure the whole coverage model exists for: a dead collector and a quiet
// database look identical, so status has to say which one it is looking at.
func TestStatusReportsAStalledCollectorAsUnhealthy(t *testing.T) {
	s := seeded(t, 1785000000)
	if err := s.SetMaxStaleness(5 * time.Minute); err != nil {
		t.Fatalf("SetMaxStaleness returned error: %v", err)
	}

	r, err := s.Status(at(1785000000 + 3600))
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if r.Healthy() {
		t.Fatal("a store nothing has written to in an hour reported healthy")
	}
	if !strings.Contains(r.Verdict(), "stale") {
		t.Errorf("Verdict = %q, want it to say the store is stale", r.Verdict())
	}
}

// An empty store is not a healthy one. Answering "OK" here would tell an
// operator their collector is fine when it has never stored anything.
func TestStatusOfAStoreThatHasCollectedNothingIsUnhealthy(t *testing.T) {
	s := open(t)

	r, err := s.Status(at(1785000000))
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if r.Healthy() {
		t.Fatal("a store holding nothing reported healthy")
	}
	if !r.Coverage.To.IsZero() {
		t.Errorf("Coverage.To = %s, want the zero time on a store that has collected nothing", r.Coverage.To)
	}
}

// Gaps and misses are permanent history: nothing an operator does removes them,
// so latching the verdict on one turns status into a probe that is red forever
// and therefore ignored. They are reported in full and left out of the verdict.
func TestStatusListsGapsAndMissesWithoutFailingTheVerdict(t *testing.T) {
	s := seeded(t, 1785000000)
	if err := s.SetMaxStaleness(5 * time.Minute); err != nil {
		t.Fatalf("SetMaxStaleness returned error: %v", err)
	}
	if err := s.RecordGap(at(1785000010), at(1785000020), "resumed past a purged binlog"); err != nil {
		t.Fatalf("RecordGap returned error: %v", err)
	}
	if err := s.RecordMiss(at(1785000030), "row image larger than the store accepts"); err != nil {
		t.Fatalf("RecordMiss returned error: %v", err)
	}

	r, err := s.Status(at(1785000060))
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if !r.Healthy() {
		t.Errorf("a recorded gap failed the verdict: %q", r.Verdict())
	}
	if len(r.Gaps) != 1 || r.Gaps[0].Reason != "resumed past a purged binlog" {
		t.Errorf("Gaps = %+v, want the one recorded gap", r.Gaps)
	}
	if len(r.Misses) != 1 || r.Misses[0].Reason != "row image larger than the store accepts" {
		t.Errorf("Misses = %+v, want the one recorded miss", r.Misses)
	}
	if !r.Misses[0].At.Equal(at(1785000030)) {
		t.Errorf("Misses[0].At = %s, want the instant the change was seen", r.Misses[0].At)
	}
}

func TestStatusReportsIntegrityProblems(t *testing.T) {
	s := seeded(t, 1785000000)
	if err := s.SetMaxStaleness(5 * time.Minute); err != nil {
		t.Fatalf("SetMaxStaleness returned error: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO txn (source_txn_id, committed_at, server_id) VALUES ('orphan', 1785000001, 7)`); err != nil {
		t.Fatalf("inserting a rowless transaction returned error: %v", err)
	}

	r, err := s.Status(at(1785000060))
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if r.Healthy() {
		t.Fatal("a store with a half-written append reported healthy")
	}
	if !mentions(r.Problems, "no rows") {
		t.Errorf("Problems = %v, want one naming a transaction with no rows", r.Problems)
	}
	if !strings.Contains(r.Verdict(), "no rows") {
		t.Errorf("Verdict = %q, want it to lead with the integrity problem", r.Verdict())
	}
}

// Which server a store follows is the first thing to check when it looks wrong,
// and it is recorded already.
func TestStatusReportsTheSourceBinding(t *testing.T) {
	s := seeded(t, 1785000000)
	if err := s.Bind(binding()); err != nil {
		t.Fatalf("Bind returned error: %v", err)
	}

	r, err := s.Status(at(1785000060))
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if !r.Bound {
		t.Fatal("Bound = false on a store that has been bound")
	}
	if r.Binding != binding() {
		t.Errorf("Binding = %+v, want %+v", r.Binding, binding())
	}
}

// A store watch has not yet connected for has no binding, and reporting a blank
// server identity as though it were one would be worse than saying so.
func TestStatusSaysWhenAStoreHasNoBinding(t *testing.T) {
	s := open(t)

	r, err := s.Status(at(1785000000))
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if r.Bound {
		t.Errorf("Bound = true on a store that was never bound (Binding = %+v)", r.Binding)
	}
}

// The staleness judgement is the collector's, recorded in the store, so status
// run on another machine reaches the same verdict generate would.
func TestStatusAppliesTheStoredStalenessAllowance(t *testing.T) {
	s := seeded(t, 1785000000)
	if err := s.SetMaxStaleness(2 * time.Hour); err != nil {
		t.Fatalf("SetMaxStaleness returned error: %v", err)
	}

	r, err := s.Status(at(1785000000 + 3600))
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if !r.Healthy() {
		t.Errorf("an hour behind failed a two-hour allowance: %q", r.Verdict())
	}
	if r.Coverage.MaxStaleness != 2*time.Hour {
		t.Errorf("Coverage.MaxStaleness = %s, want 2h", r.Coverage.MaxStaleness)
	}
}
