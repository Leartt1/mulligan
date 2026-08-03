package store

import (
	"testing"
	"time"

	"github.com/learttytyri/mulligan/internal/change"
)

func TestChangeReturnsOneStoredChangeWithBothImages(t *testing.T) {
	s := seeded(t, 1785000000)

	page, err := s.Page(change.Filter{}, 0, 10, at(1785000010))
	if err != nil {
		t.Fatalf("Page returned error: %v", err)
	}
	if len(page) != 1 {
		t.Fatalf("Page returned %d entries, want 1", len(page))
	}

	got, ok, err := s.Change(page[0].ID)
	if err != nil {
		t.Fatalf("Change returned error: %v", err)
	}
	if !ok {
		t.Fatal("Change did not find an id Page had just handed over")
	}
	if got.ID != page[0].ID {
		t.Errorf("Change returned id %d, want %d", got.ID, page[0].ID)
	}
	if got.Event.Table != "orders" || got.Event.Op != change.Update {
		t.Errorf("Change returned %s on %s, want an UPDATE on orders", got.Event.Op, got.Event.Table)
	}
	// Both images are the diff. A detail view without them shows nothing a
	// timeline row did not already say.
	if len(got.Event.Before) != 2 || got.Event.Before[1] != "pending" {
		t.Errorf("before image = %v, want the values the statement overwrote", got.Event.Before)
	}
	if len(got.Event.After) != 2 || got.Event.After[1] != "shipped" {
		t.Errorf("after image = %v, want the values the statement wrote", got.Event.After)
	}
}

// Asking for one id has to return that change and not a neighbouring one. With
// a single stored change any lookup looks correct, so this seeds several and
// asks for one in the middle.
func TestChangeReturnsTheChangeAsked(t *testing.T) {
	s := seedPages(t, 1785000000, 5)

	page, err := s.Page(change.Filter{}, 0, 10, at(1785000010))
	if err != nil {
		t.Fatalf("Page returned error: %v", err)
	}
	want := page[2]

	got, ok, err := s.Change(want.ID)
	if err != nil || !ok {
		t.Fatalf("Change(%d): ok=%v err=%v", want.ID, ok, err)
	}
	if got.ID != want.ID {
		t.Errorf("Change returned id %d, want %d", got.ID, want.ID)
	}
	if got.Event.LogPos != want.Event.LogPos {
		t.Errorf("Change returned the change at %s:%d, want the one at %s:%d",
			got.Event.LogFile, got.Event.LogPos, want.Event.LogFile, want.Event.LogPos)
	}
	if !got.Event.At.Equal(want.Event.At) || got.Event.Table != want.Event.Table {
		t.Errorf("Change returned %s at %s, want %s at %s",
			got.Event.Table, got.Event.At, want.Event.Table, want.Event.At)
	}
}

// Ids that were never assigned must come back as absent — including the ones
// below the range. A client that omits the id, or parses it into a zero, must
// not be handed the oldest change as though it had asked for it.
func TestChangeReportsAnUnknownID(t *testing.T) {
	s := seedPages(t, 1785000000, 3)

	for _, id := range []int64{99999, 0, -1} {
		_, ok, err := s.Change(id)
		if err != nil {
			t.Fatalf("Change(%d) returned error: %v", id, err)
		}
		if ok {
			t.Errorf("Change(%d) claimed to find a change that was never stored", id)
		}
	}
}

// Looking at one stored change is not a window, so the coverage refusals do not
// apply to it: the change is there, it is complete, and it is exactly what was
// asked for. Refusing to show it because the collector has since stopped would
// withhold the one thing the operator can still see during the incident.
func TestChangeAnswersEvenWhileTheCollectorIsStalled(t *testing.T) {
	s := seeded(t, 1785000000)
	if err := s.SetMaxStaleness(5 * time.Minute); err != nil {
		t.Fatalf("SetMaxStaleness returned error: %v", err)
	}

	page, err := s.Page(change.Filter{}, 0, 10, at(1785000010))
	if err != nil {
		t.Fatalf("Page returned error: %v", err)
	}

	// An hour past anything the collector recorded — a window read would refuse.
	if _, err := s.Page(change.Filter{}, 0, 10, at(1785000000+3600)); err == nil {
		t.Fatal("a paged read answered from a stalled store, so this proves nothing")
	}

	if _, ok, err := s.Change(page[0].ID); err != nil || !ok {
		t.Errorf("Change refused a stored change while the collector was stalled: ok=%v err=%v", ok, err)
	}
}
