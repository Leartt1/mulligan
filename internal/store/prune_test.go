package store

import (
	"strings"
	"testing"
	"time"

	"github.com/learttytyri/mulligan/internal/change"
)

const day = 24 * time.Hour

// stocked returns a store holding one transaction per day for the last seven
// days, counting back from `now`.
func stocked(t *testing.T, now int64) *Store {
	t.Helper()
	s := open(t)

	if err := s.OpenCoverage(at(now - 7*86400)); err != nil {
		t.Fatalf("OpenCoverage returned error: %v", err)
	}
	for d := 7; d >= 0; d-- {
		committed := now - int64(d)*86400
		tx := txn("gtid:"+time.Unix(committed, 0).UTC().Format("20060102"), committed,
			update("orders", uint32(100+d), "before", "after"))
		if err := s.AppendTransaction(tx, checkpoint(uint32(100+d))); err != nil {
			t.Fatalf("AppendTransaction returned error: %v", err)
		}
	}
	return s
}

func TestPruneRemovesChangesOlderThanTheRetentionWindow(t *testing.T) {
	now := int64(1785000000)
	s := stocked(t, now)

	if err := s.SetRetention(3 * day); err != nil {
		t.Fatalf("SetRetention returned error: %v", err)
	}
	if _, err := s.Prune(at(now)); err != nil {
		t.Fatalf("Prune returned error: %v", err)
	}

	var remaining int
	if err := s.db.QueryRow(`SELECT count(*) FROM txn`).Scan(&remaining); err != nil {
		t.Fatalf("counting transactions returned error: %v", err)
	}
	// Days 0 through 3 are inside a three-day window; days 4 through 7 are not.
	if remaining != 4 {
		t.Errorf("%d transactions remain, want 4", remaining)
	}
}

func TestPruneRemovesTheRowsOfThePrunedTransactions(t *testing.T) {
	now := int64(1785000000)
	s := stocked(t, now)

	if err := s.SetRetention(3 * day); err != nil {
		t.Fatalf("SetRetention returned error: %v", err)
	}
	if _, err := s.Prune(at(now)); err != nil {
		t.Fatalf("Prune returned error: %v", err)
	}

	var orphans int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM row_change WHERE txn_id NOT IN (SELECT id FROM txn)`).Scan(&orphans); err != nil {
		t.Fatalf("counting orphaned rows returned error: %v", err)
	}
	if orphans != 0 {
		t.Errorf("%d rows outlived their transaction", orphans)
	}
}

// This is the point of pruning being a store operation rather than a DELETE.
// Coverage must move to the retention edge, not to the oldest surviving change:
// a database quiet for two days would otherwise leave coverage_from two days
// back, and the store would claim to answer for a period whose changes it had
// already discarded.
func TestPruneMovesCoverageToTheRetentionEdgeNotTheOldestSurvivingChange(t *testing.T) {
	now := int64(1785000000)
	s := open(t)

	if err := s.OpenCoverage(at(now - 10*86400)); err != nil {
		t.Fatalf("OpenCoverage returned error: %v", err)
	}
	// Nothing was written for the last two days: the newest change is two days old.
	for _, ago := range []int64{9, 8, 7, 2} {
		committed := now - ago*86400
		tx := txn("gtid:"+time.Unix(committed, 0).UTC().Format("20060102"), committed,
			update("orders", uint32(ago), "before", "after"))
		if err := s.AppendTransaction(tx, checkpoint(uint32(ago))); err != nil {
			t.Fatalf("AppendTransaction returned error: %v", err)
		}
	}

	if err := s.SetRetention(3 * day); err != nil {
		t.Fatalf("SetRetention returned error: %v", err)
	}
	if _, err := s.Prune(at(now)); err != nil {
		t.Fatalf("Prune returned error: %v", err)
	}

	got, err := s.Coverage()
	if err != nil {
		t.Fatalf("Coverage returned error: %v", err)
	}

	wantEdge := at(now - 3*86400)
	if !got.From.Equal(wantEdge) {
		t.Errorf("coverage from = %s, want the retention edge %s", got.From, wantEdge)
	}
	// The oldest surviving change is two days old. Coverage must not follow it.
	if got.From.Equal(at(now - 2*86400)) {
		t.Error("coverage followed the oldest surviving change, so a quiet period reads as covered")
	}
}

// Coverage only ever narrows. A store opened yesterday does not gain a week of
// history the moment a week-long retention is configured.
func TestPruneDoesNotWidenCoverageBackwards(t *testing.T) {
	now := int64(1785000000)
	s := open(t)

	opened := at(now - 86400)
	if err := s.OpenCoverage(opened); err != nil {
		t.Fatalf("OpenCoverage returned error: %v", err)
	}
	if err := s.AppendTransaction(txn("gtid:1", now, update("orders", 100, "a", "b")), checkpoint(100)); err != nil {
		t.Fatalf("AppendTransaction returned error: %v", err)
	}

	if err := s.SetRetention(7 * day); err != nil {
		t.Fatalf("SetRetention returned error: %v", err)
	}
	if _, err := s.Prune(at(now)); err != nil {
		t.Fatalf("Prune returned error: %v", err)
	}

	got, err := s.Coverage()
	if err != nil {
		t.Fatalf("Coverage returned error: %v", err)
	}
	if !got.From.Equal(opened) {
		t.Errorf("coverage from = %s, want it to stay at %s", got.From, opened)
	}
}

// Once the window has moved past a period, a revert reaching into it has to be
// refused rather than answered from whatever survived.
func TestAWindowReachingIntoPrunedTimeIsRefused(t *testing.T) {
	now := int64(1785000000)
	s := stocked(t, now)

	if err := s.SetRetention(3 * day); err != nil {
		t.Fatalf("SetRetention returned error: %v", err)
	}
	if _, err := s.Prune(at(now)); err != nil {
		t.Fatalf("Prune returned error: %v", err)
	}

	_, err := s.Events(change.Filter{From: at(now - 5*86400)}, at(now))
	if err == nil {
		t.Fatal("Events answered a window reaching into pruned time")
	}
	if !strings.Contains(err.Error(), "reaches back") {
		t.Errorf("error = %v, want it to say the window reaches back past coverage", err)
	}
}

// A gap wholly inside discarded time can no longer overlap any answerable
// window, so it is discarded with it rather than accumulating forever.
func TestPruneDiscardsGapsAndMissesBehindTheEdge(t *testing.T) {
	now := int64(1785000000)
	s := stocked(t, now)

	if err := s.RecordGap(at(now-6*86400), at(now-5*86400), "old outage"); err != nil {
		t.Fatalf("RecordGap returned error: %v", err)
	}
	if err := s.RecordMiss(at(now-6*86400), "old miss"); err != nil {
		t.Fatalf("RecordMiss returned error: %v", err)
	}
	// This one straddles the edge and must survive, or a window starting at the
	// edge would stop being refused.
	if err := s.RecordGap(at(now-4*86400), at(now-2*86400), "recent outage"); err != nil {
		t.Fatalf("RecordGap returned error: %v", err)
	}

	if err := s.SetRetention(3 * day); err != nil {
		t.Fatalf("SetRetention returned error: %v", err)
	}
	if _, err := s.Prune(at(now)); err != nil {
		t.Fatalf("Prune returned error: %v", err)
	}

	var gaps, misses int
	if err := s.db.QueryRow(`SELECT count(*) FROM gap`).Scan(&gaps); err != nil {
		t.Fatalf("counting gaps returned error: %v", err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM miss`).Scan(&misses); err != nil {
		t.Fatalf("counting misses returned error: %v", err)
	}
	if gaps != 1 {
		t.Errorf("%d gaps remain, want only the one straddling the edge", gaps)
	}
	if misses != 0 {
		t.Errorf("%d misses remain, want none behind the edge", misses)
	}

	// The straddling gap still refuses a window that touches it.
	if _, err := s.Events(change.Filter{From: at(now - 3*86400), To: at(now)}, at(now)); err == nil {
		t.Error("a window touching the surviving gap was answered")
	}
}

func TestPruneReportsHowMuchItRemoved(t *testing.T) {
	now := int64(1785000000)
	s := stocked(t, now)

	if err := s.SetRetention(3 * day); err != nil {
		t.Fatalf("SetRetention returned error: %v", err)
	}
	removed, err := s.Prune(at(now))
	if err != nil {
		t.Fatalf("Prune returned error: %v", err)
	}
	if removed != 4 {
		t.Errorf("Prune reported %d transactions removed, want 4", removed)
	}
}

func TestPruningAStoreWithoutRetentionConfiguredKeepsEverything(t *testing.T) {
	now := int64(1785000000)
	s := stocked(t, now)

	removed, err := s.Prune(at(now))
	if err != nil {
		t.Fatalf("Prune returned error: %v", err)
	}
	if removed != 0 {
		t.Errorf("Prune removed %d transactions with no retention set, want 0", removed)
	}
}

func TestRetentionIsReportedWithCoverage(t *testing.T) {
	s := open(t)

	if err := s.SetRetention(36 * time.Hour); err != nil {
		t.Fatalf("SetRetention returned error: %v", err)
	}
	got, err := s.Coverage()
	if err != nil {
		t.Fatalf("Coverage returned error: %v", err)
	}
	if got.Retention != 36*time.Hour {
		t.Errorf("retention = %s, want 36h", got.Retention)
	}
}
