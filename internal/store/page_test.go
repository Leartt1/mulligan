package store

import (
	"strings"
	"testing"
	"time"

	"github.com/learttytyri/mulligan/internal/change"
)

// seedPages fills a store with n transactions, one change each, committed one
// second apart from base.
func seedPages(t *testing.T, base int64, n int) *Store {
	t.Helper()
	s := open(t)

	if err := s.OpenCoverage(at(base - 60)); err != nil {
		t.Fatalf("OpenCoverage returned error: %v", err)
	}
	for i := 0; i < n; i++ {
		committed := base + int64(i)
		ev := update("orders", uint32(576+i), "pending", "shipped")
		if i%2 == 1 {
			ev = update("shipments", uint32(576+i), "queued", "sent")
		}
		if err := s.AppendTransaction(
			txn("gtid:"+time.Unix(committed, 0).UTC().Format("150405"), committed, ev),
			change.Checkpoint{LogFile: "binlog.000004", LogPos: uint32(576 + i), UpdatedAt: at(committed)},
		); err != nil {
			t.Fatalf("AppendTransaction returned error: %v", err)
		}
	}
	return s
}

// Newest first, because that is the order a revert applies in and the order a
// timeline reads in. A page that arrived oldest-first would have to be reversed
// by every caller.
func TestPageReturnsTheNewestChangesFirst(t *testing.T) {
	s := seedPages(t, 1785000000, 5)

	got, err := s.Page(change.Filter{}, 0, 10, at(1785000010))
	if err != nil {
		t.Fatalf("Page returned error: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("Page returned %d entries, want 5", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].ID >= got[i-1].ID {
			t.Fatalf("entry %d has id %d, not below the previous %d", i, got[i].ID, got[i-1].ID)
		}
		if got[i].Event.At.After(got[i-1].Event.At) {
			t.Errorf("entry %d is newer than the one before it", i)
		}
	}
	if !got[0].Event.At.Equal(at(1785000004)) {
		t.Errorf("first entry is at %s, want the newest change", got[0].Event.At)
	}
}

func TestPageStopsAtTheLimit(t *testing.T) {
	s := seedPages(t, 1785000000, 5)

	got, err := s.Page(change.Filter{}, 0, 2, at(1785000010))
	if err != nil {
		t.Fatalf("Page returned error: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("Page returned %d entries, want 2", len(got))
	}
}

// The cursor is the store's own row id rather than an offset: a collector is
// appending while someone pages, and an offset would shift underneath them —
// repeating one change and skipping another, with nothing looking wrong.
func TestPageWalksTheWholeSetWithTheCursor(t *testing.T) {
	s := seedPages(t, 1785000000, 5)

	var seen []int64
	cursor := int64(0)
	// Bounded, so a cursor that fails to advance fails the test rather than
	// spinning until the whole package times out with nothing to read.
	for round := 0; ; round++ {
		if round > 5 {
			t.Fatalf("paging did not finish in %d rounds; the cursor is not advancing past %d", round, cursor)
		}
		got, err := s.Page(change.Filter{}, cursor, 2, at(1785000010))
		if err != nil {
			t.Fatalf("Page returned error: %v", err)
		}
		if len(got) == 0 {
			break
		}
		for _, e := range got {
			seen = append(seen, e.ID)
		}
		cursor = got[len(got)-1].ID
	}

	if len(seen) != 5 {
		t.Fatalf("paging saw %d changes, want 5: %v", len(seen), seen)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] >= seen[i-1] {
			t.Fatalf("paging repeated or went backwards: %v", seen)
		}
	}
}

// A change appended while someone is paging is newer than the cursor, so it
// belongs to a page they have already read past — not inserted into the middle
// of the one they are about to ask for.
func TestPageIsNotDisturbedByAChangeAppendedMidWalk(t *testing.T) {
	s := seedPages(t, 1785000000, 4)

	first, err := s.Page(change.Filter{}, 0, 2, at(1785000010))
	if err != nil {
		t.Fatalf("Page returned error: %v", err)
	}

	if err := s.AppendTransaction(
		txn("gtid:later", 1785000009, update("orders", 999, "shipped", "delivered")),
		checkpoint(999)); err != nil {
		t.Fatalf("AppendTransaction returned error: %v", err)
	}

	second, err := s.Page(change.Filter{}, first[len(first)-1].ID, 10, at(1785000010))
	if err != nil {
		t.Fatalf("Page returned error: %v", err)
	}
	if len(second) != 2 {
		t.Fatalf("second page holds %d entries, want the 2 remaining", len(second))
	}
	for _, e := range second {
		for _, f := range first {
			if e.ID == f.ID {
				t.Errorf("id %d appeared on both pages", e.ID)
			}
		}
	}
}

func TestPageAppliesTheTableFilter(t *testing.T) {
	s := seedPages(t, 1785000000, 6)

	got, err := s.Page(change.Filter{Tables: []string{"shop.shipments"}}, 0, 10, at(1785000010))
	if err != nil {
		t.Fatalf("Page returned error: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("Page returned nothing for a table that has changes")
	}
	for _, e := range got {
		if e.Event.Table != "shipments" {
			t.Errorf("Page returned a change to %s, which the filter excludes", e.Event.Table)
		}
	}
}

// A page is a window like any other. One that quietly omitted a stalled
// collector would be the failure this store is arranged around, dressed up as a
// friendlier-looking empty list.
func TestPageRefusesWhenTheCollectorHasFallenBehind(t *testing.T) {
	s := seedPages(t, 1785000000, 3)
	if err := s.SetMaxStaleness(5 * time.Minute); err != nil {
		t.Fatalf("SetMaxStaleness returned error: %v", err)
	}

	_, err := s.Page(change.Filter{}, 0, 10, at(1785000000+3600))
	if err == nil {
		t.Fatal("Page answered from a store nothing has written to in an hour")
	}
	if !strings.Contains(err.Error(), "stale") {
		t.Errorf("error = %v, want it to say the store is stale", err)
	}
}

// A client asking for everything gets a page, not the store. Left unclamped,
// one request would materialize a window that was deliberately never
// materialized anywhere else in this code.
func TestPageClampsAnUnreasonableLimit(t *testing.T) {
	s := open(t)
	if err := s.OpenCoverage(at(1785000000 - 60)); err != nil {
		t.Fatalf("OpenCoverage returned error: %v", err)
	}

	// More changes than the cap, in one transaction: the cap counts row changes,
	// and a thousand separate appends would only be a slower way to prove it.
	events := make([]change.Event, MaxPageSize+5)
	for i := range events {
		events[i] = update("orders", uint32(i), "pending", "shipped")
	}
	if err := s.AppendTransaction(txn("gtid:big", 1785000000, events...), checkpoint(1)); err != nil {
		t.Fatalf("AppendTransaction returned error: %v", err)
	}

	got, err := s.Page(change.Filter{}, 0, 1_000_000, at(1785000010))
	if err != nil {
		t.Fatalf("Page returned error: %v", err)
	}
	if len(got) != MaxPageSize {
		t.Errorf("Page returned %d entries, want the %d cap", len(got), MaxPageSize)
	}
}

func TestPageWithNoLimitUsesADefault(t *testing.T) {
	s := seedPages(t, 1785000000, 3)

	got, err := s.Page(change.Filter{}, 0, 0, at(1785000010))
	if err != nil {
		t.Fatalf("Page returned error: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("Page returned %d entries, want all 3 under the default limit", len(got))
	}
}
