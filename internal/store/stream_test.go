package store

import (
	"errors"
	"testing"

	"github.com/learttytyri/mulligan/internal/change"
)

// Reversals are emitted newest first, so the store hands them over that way
// rather than collecting the window and reversing it in memory. The whole
// matching set is exactly what will not fit when it matters: a run reverting ten
// million rows is the case this tool exists for.
func TestEachEventVisitsNewestFirst(t *testing.T) {
	s := open(t)

	if err := s.OpenCoverage(at(1785000000 - 60)); err != nil {
		t.Fatalf("OpenCoverage: %v", err)
	}
	for i, when := range []int64{1785000000, 1785000010, 1785000020} {
		tx := txn("gtid:"+string(rune('a'+i)), when, update("orders", uint32(100+i), "before", "after"))
		if err := s.AppendTransaction(tx, checkpoint(uint32(100+i))); err != nil {
			t.Fatalf("AppendTransaction: %v", err)
		}
	}

	var positions []uint32
	err := s.EachEvent(change.Filter{}, at(1785000030), func(ev change.Event) error {
		positions = append(positions, ev.LogPos)
		return nil
	})
	if err != nil {
		t.Fatalf("EachEvent returned error: %v", err)
	}

	want := []uint32{102, 101, 100}
	if len(positions) != len(want) {
		t.Fatalf("visited %d events, want %d", len(positions), len(want))
	}
	for i := range want {
		if positions[i] != want[i] {
			t.Errorf("event %d was at %d, want %d — the order a revert applies in", i, positions[i], want[i])
		}
	}
}

// The refusals are the point of reading through the store at all, so they have
// to fire before any event is handed over — not partway through, with output
// already written.
func TestEachEventRefusesBeforeVisitingAnything(t *testing.T) {
	s := seeded(t, 1785000000)

	if err := s.RecordGap(at(1785000010), at(1785000020), "collector was down"); err != nil {
		t.Fatalf("RecordGap: %v", err)
	}
	if err := s.AppendTransaction(
		txn("gtid:2", 1785000030, update("orders", 900, "x", "y")), checkpoint(900)); err != nil {
		t.Fatalf("AppendTransaction: %v", err)
	}

	var visited int
	err := s.EachEvent(change.Filter{From: at(1785000000)}, at(1785000040), func(change.Event) error {
		visited++
		return nil
	})
	if err == nil {
		t.Fatal("a window spanning a gap was streamed")
	}
	if visited != 0 {
		t.Errorf("%d events were handed over before the refusal", visited)
	}
}

// A caller that stops reading stops the scan, and its reason is what comes back.
func TestEachEventStopsWhenTheCallerFails(t *testing.T) {
	s := seeded(t, 1785000000)
	for i := 0; i < 5; i++ {
		tx := txn("gtid:more"+string(rune('a'+i)), 1785000001+int64(i), update("orders", uint32(200+i), "a", "b"))
		if err := s.AppendTransaction(tx, checkpoint(uint32(200+i))); err != nil {
			t.Fatalf("AppendTransaction: %v", err)
		}
	}

	stop := errors.New("enough")
	var visited int
	err := s.EachEvent(change.Filter{}, at(1785000010), func(change.Event) error {
		visited++
		return stop
	})

	if !errors.Is(err, stop) {
		t.Errorf("error = %v, want the caller's own reason", err)
	}
	if visited != 1 {
		t.Errorf("%d events were visited after the caller stopped, want 1", visited)
	}
}

func TestEachEventAppliesTheFilter(t *testing.T) {
	s := open(t)

	if err := s.OpenCoverage(at(1785000000 - 60)); err != nil {
		t.Fatalf("OpenCoverage: %v", err)
	}
	if err := s.AppendTransaction(
		txn("gtid:1", 1785000000, update("orders", 100, "a", "b")), checkpoint(100)); err != nil {
		t.Fatalf("AppendTransaction: %v", err)
	}
	if err := s.AppendTransaction(
		txn("gtid:2", 1785000010, update("customers", 200, "c", "d")), checkpoint(200)); err != nil {
		t.Fatalf("AppendTransaction: %v", err)
	}

	var tables []string
	err := s.EachEvent(change.Filter{Tables: []string{"shop.customers"}}, at(1785000020),
		func(ev change.Event) error {
			tables = append(tables, ev.Table)
			return nil
		})
	if err != nil {
		t.Fatalf("EachEvent returned error: %v", err)
	}
	if len(tables) != 1 || tables[0] != "customers" {
		t.Errorf("visited %v, want only customers", tables)
	}
}
