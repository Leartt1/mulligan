package store

import (
	"strings"
	"testing"
	"time"

	"github.com/learttytyri/mulligan/internal/change"
)

// seeded returns a store holding one change committed at `committed`, with
// coverage opened just before it.
func seeded(t *testing.T, committed int64) *Store {
	t.Helper()
	s := open(t)

	if err := s.OpenCoverage(at(committed - 60)); err != nil {
		t.Fatalf("OpenCoverage returned error: %v", err)
	}
	if err := s.AppendTransaction(txn("gtid:1", committed, update("orders", 576, "pending", "shipped")), checkpoint(576)); err != nil {
		t.Fatalf("AppendTransaction returned error: %v", err)
	}
	return s
}

func TestEventsAreReturnedWhenTheWindowIsCovered(t *testing.T) {
	s := seeded(t, 1785000000)

	got, err := s.Events(change.Filter{From: at(1785000000 - 30), To: at(1785000000 + 30)}, at(1785000000+10))
	if err != nil {
		t.Fatalf("Events returned error: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("Events returned %d events, want 1", len(got))
	}
}

// This is the failure the whole coverage model exists for. A collector that has
// died, wedged, or lost its connection looks exactly like a database nobody is
// writing to: the window is inside coverage, no gap is recorded, and the honest
// answer would be "no matching changes found" with exit 0 — an affirmative
// "nothing happened" delivered during the incident. Afterwards the binlogs
// rotate and the store is the only record left.
func TestEventsRefusesWhenTheCollectorHasFallenBehind(t *testing.T) {
	s := seeded(t, 1785000000)

	if err := s.SetMaxStaleness(5 * time.Minute); err != nil {
		t.Fatalf("SetMaxStaleness returned error: %v", err)
	}

	// An hour after the last thing the collector recorded.
	_, err := s.Events(change.Filter{}, at(1785000000+3600))
	if err == nil {
		t.Fatal("Events answered from a store nothing has written to in an hour")
	}
	if !strings.Contains(err.Error(), "stale") {
		t.Errorf("error = %v, want it to say the store is stale", err)
	}
}

func TestEventsAnswersWhileTheCollectorIsKeepingUp(t *testing.T) {
	s := seeded(t, 1785000000)

	if err := s.SetMaxStaleness(5 * time.Minute); err != nil {
		t.Fatalf("SetMaxStaleness returned error: %v", err)
	}

	if _, err := s.Events(change.Filter{}, at(1785000000+60)); err != nil {
		t.Errorf("Events refused a store only a minute behind: %v", err)
	}
}

// A window reaching back before the store began is not an empty result, it is a
// question the store cannot answer. Returning what it does have would understate
// the damage without saying so.
func TestEventsRefusesAWindowStartingBeforeCoverage(t *testing.T) {
	s := seeded(t, 1785000000)

	_, err := s.Events(change.Filter{From: at(1785000000 - 3600)}, at(1785000000+10))
	if err == nil {
		t.Fatal("Events answered a window starting before the store had any coverage")
	}
	if !strings.Contains(err.Error(), "reaches back") {
		t.Errorf("error = %v, want it to say the window reaches back past coverage", err)
	}
}

// Leaving --from unset asks for as far back as the store goes, which is a
// different request from naming an instant the store cannot answer for.
func TestEventsAcceptsAnUnboundedStart(t *testing.T) {
	s := seeded(t, 1785000000)

	if _, err := s.Events(change.Filter{}, at(1785000000+10)); err != nil {
		t.Errorf("Events refused an unbounded start: %v", err)
	}
}

// A gap is a period the collector knows it did not see. A revert spanning one is
// missing whatever happened inside it, and would look complete.
func TestEventsRefusesAWindowOverlappingAGap(t *testing.T) {
	s := seeded(t, 1785000000)

	from, to := at(1785000100), at(1785000200)
	if err := s.RecordGap(from, to, "watch was not running and those binlogs were purged"); err != nil {
		t.Fatalf("RecordGap returned error: %v", err)
	}
	if err := s.AppendTransaction(txn("gtid:2", 1785000300, update("orders", 900, "x", "y")), checkpoint(900)); err != nil {
		t.Fatalf("AppendTransaction returned error: %v", err)
	}

	tests := []struct {
		name   string
		filter change.Filter
	}{
		{"window contains the gap", change.Filter{From: at(1785000050), To: at(1785000250)}},
		{"window starts inside the gap", change.Filter{From: at(1785000150), To: at(1785000250)}},
		{"window ends inside the gap", change.Filter{From: at(1785000050), To: at(1785000150)}},
		{"window sits entirely inside the gap", change.Filter{From: at(1785000120), To: at(1785000180)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.Events(tt.filter, at(1785000310))
			if err == nil {
				t.Fatal("Events answered a window overlapping a gap")
			}
			if !strings.Contains(err.Error(), "gap") {
				t.Errorf("error = %v, want it to name the gap", err)
			}
			if !strings.Contains(err.Error(), "purged") {
				t.Errorf("error = %v, want it to carry the reason recorded with the gap", err)
			}
		})
	}
}

func TestEventsAnswersAWindowClearOfAGap(t *testing.T) {
	s := seeded(t, 1785000000)

	if err := s.RecordGap(at(1785000100), at(1785000200), "collector restarted"); err != nil {
		t.Fatalf("RecordGap returned error: %v", err)
	}
	if err := s.AppendTransaction(txn("gtid:2", 1785000300, update("orders", 900, "x", "y")), checkpoint(900)); err != nil {
		t.Fatalf("AppendTransaction returned error: %v", err)
	}

	if _, err := s.Events(change.Filter{From: at(1785000250), To: at(1785000310)}, at(1785000310)); err != nil {
		t.Errorf("Events refused a window clear of the gap: %v", err)
	}
}

// A miss is one change the collector saw and could not record. It is a point,
// not a period, but a revert containing that instant is still incomplete.
func TestEventsRefusesAWindowContainingAMiss(t *testing.T) {
	s := seeded(t, 1785000000)

	if err := s.RecordMiss(at(1785000100), "the row image could not be encoded"); err != nil {
		t.Fatalf("RecordMiss returned error: %v", err)
	}
	if err := s.AppendTransaction(txn("gtid:2", 1785000200, update("orders", 900, "x", "y")), checkpoint(900)); err != nil {
		t.Fatalf("AppendTransaction returned error: %v", err)
	}

	_, err := s.Events(change.Filter{From: at(1785000050), To: at(1785000150)}, at(1785000210))
	if err == nil {
		t.Fatal("Events answered a window containing a change that was never recorded")
	}
	if !strings.Contains(err.Error(), "could not be encoded") {
		t.Errorf("error = %v, want it to carry the reason the change was missed", err)
	}
}

func TestEventsAnswersAWindowClearOfAMiss(t *testing.T) {
	s := seeded(t, 1785000000)

	if err := s.RecordMiss(at(1785000100), "the row image could not be encoded"); err != nil {
		t.Fatalf("RecordMiss returned error: %v", err)
	}
	if err := s.AppendTransaction(txn("gtid:2", 1785000200, update("orders", 900, "x", "y")), checkpoint(900)); err != nil {
		t.Fatalf("AppendTransaction returned error: %v", err)
	}

	if _, err := s.Events(change.Filter{From: at(1785000150), To: at(1785000210)}, at(1785000210)); err != nil {
		t.Errorf("Events refused a window clear of the miss: %v", err)
	}
}

// Asking about a period the collector has not reached yet is not an empty
// result. The changes may be happening right now.
func TestEventsRefusesAWindowEndingAfterWhatHasBeenCollected(t *testing.T) {
	s := seeded(t, 1785000000)

	_, err := s.Events(change.Filter{To: at(1785000000 + 600)}, at(1785000000+10))
	if err == nil {
		t.Fatal("Events answered a window ending past what the collector has reached")
	}
	if !strings.Contains(err.Error(), "has not reached") {
		t.Errorf("error = %v, want it to say the collector has not reached that far", err)
	}
}

// A store nothing has ever been written to is the most misleading empty result
// of all: it means the collector never ran, not that nothing happened.
func TestEventsRefusesAStoreThatHasCollectedNothing(t *testing.T) {
	s := open(t)

	_, err := s.Events(change.Filter{}, at(1785000000))
	if err == nil {
		t.Fatal("Events answered from a store that has collected nothing")
	}
	if !strings.Contains(err.Error(), "has not recorded") {
		t.Errorf("error = %v, want it to say nothing has been recorded", err)
	}
}

func TestCoverageReportsWhatTheStoreCanAnswerFor(t *testing.T) {
	s := seeded(t, 1785000000)

	if err := s.SetMaxStaleness(90 * time.Second); err != nil {
		t.Fatalf("SetMaxStaleness returned error: %v", err)
	}

	got, err := s.Coverage()
	if err != nil {
		t.Fatalf("Coverage returned error: %v", err)
	}
	if !got.From.Equal(at(1785000000 - 60)) {
		t.Errorf("from = %s, want %s", got.From, at(1785000000-60))
	}
	if !got.To.Equal(at(1785000000)) {
		t.Errorf("to = %s, want %s", got.To, at(1785000000))
	}
	if got.MaxStaleness != 90*time.Second {
		t.Errorf("max staleness = %s, want 90s", got.MaxStaleness)
	}
}

// Coverage opens once, when the collector first runs. A restart must not move it
// forward, or the period before the restart would stop being answerable even
// though its changes are still stored.
func TestOpeningCoverageASecondTimeDoesNotMoveIt(t *testing.T) {
	s := open(t)

	if err := s.OpenCoverage(at(1785000000)); err != nil {
		t.Fatalf("OpenCoverage returned error: %v", err)
	}
	if err := s.OpenCoverage(at(1785009999)); err != nil {
		t.Fatalf("second OpenCoverage returned error: %v", err)
	}

	got, err := s.Coverage()
	if err != nil {
		t.Fatalf("Coverage returned error: %v", err)
	}
	if !got.From.Equal(at(1785000000)) {
		t.Errorf("from = %s, want it to stay at %s", got.From, at(1785000000))
	}
}

// Staleness is the collector's own setting, recorded where a later generate can
// read it. Two machines otherwise disagree about whether the same store is stale.
func TestMaxStalenessHasADefaultUntilTheCollectorSetsOne(t *testing.T) {
	s := open(t)

	got, err := s.Coverage()
	if err != nil {
		t.Fatalf("Coverage returned error: %v", err)
	}
	if got.MaxStaleness != DefaultMaxStaleness {
		t.Errorf("max staleness = %s, want the default %s", got.MaxStaleness, DefaultMaxStaleness)
	}
}
