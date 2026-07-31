package store

import (
	"strings"
	"testing"

	"github.com/learttytyri/mulligan/internal/change"
)

func TestIntegrityReportsNothingWrongWithAHealthyStore(t *testing.T) {
	s := seeded(t, 1785000000)

	problems, err := s.Integrity()
	if err != nil {
		t.Fatalf("Integrity returned error: %v", err)
	}
	if len(problems) != 0 {
		t.Errorf("a healthy store reported problems: %v", problems)
	}
}

func TestIntegrityOfAnEmptyStoreIsClean(t *testing.T) {
	s := open(t)

	problems, err := s.Integrity()
	if err != nil {
		t.Fatalf("Integrity returned error: %v", err)
	}
	if len(problems) != 0 {
		t.Errorf("a new store reported problems: %v", problems)
	}
}

// A transaction with no rows is what a half-written append would leave behind.
// It cannot happen while appends are atomic, which is exactly why something has
// to check: the value of the invariant is in noticing if it ever stops holding.
func TestIntegrityNoticesATransactionWithNoRows(t *testing.T) {
	s := seeded(t, 1785000000)

	if _, err := s.db.Exec(
		`INSERT INTO txn (source_txn_id, committed_at, server_id) VALUES ('orphan', 1785000001, 7)`); err != nil {
		t.Fatalf("inserting a rowless transaction returned error: %v", err)
	}

	problems, err := s.Integrity()
	if err != nil {
		t.Fatalf("Integrity returned error: %v", err)
	}
	if !mentions(problems, "no rows") {
		t.Errorf("problems = %v, want one naming a transaction with no rows", problems)
	}
}

// The checkpoint says where collection will resume. Ahead of the newest stored
// change, it would resume past changes that were never recorded — a hole with
// nothing describing it.
func TestIntegrityNoticesACheckpointAheadOfTheData(t *testing.T) {
	s := seeded(t, 1785000000)

	if _, err := s.db.Exec(
		`UPDATE checkpoint SET updated_at = ? WHERE id = 1`, 1785009999); err != nil {
		t.Fatalf("moving the checkpoint returned error: %v", err)
	}

	problems, err := s.Integrity()
	if err != nil {
		t.Fatalf("Integrity returned error: %v", err)
	}
	if !mentions(problems, "checkpoint") {
		t.Errorf("problems = %v, want one naming the checkpoint", problems)
	}
}

// A checkpoint at the newest change is the ordinary state after every append and
// must not be reported.
func TestIntegrityAcceptsACheckpointAtTheNewestChange(t *testing.T) {
	s := open(t)

	if err := s.AppendTransaction(
		txn("gtid:1", 1785000000, update("orders", 576, "a", "b")),
		change.Checkpoint{LogFile: "binlog.000004", LogPos: 576, UpdatedAt: at(1785000000)}); err != nil {
		t.Fatalf("AppendTransaction returned error: %v", err)
	}

	problems, err := s.Integrity()
	if err != nil {
		t.Fatalf("Integrity returned error: %v", err)
	}
	if len(problems) != 0 {
		t.Errorf("an ordinary store reported problems: %v", problems)
	}
}

func mentions(problems []string, substr string) bool {
	for _, p := range problems {
		if strings.Contains(p, substr) {
			return true
		}
	}
	return false
}
