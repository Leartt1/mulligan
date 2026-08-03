package api

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestChangesRouteListsNewestFirst(t *testing.T) {
	db := seed(t, 1785000000, 4)
	srv := New(db, "shop.db", clock(1785000060))

	rec := get(t, srv, "/api/changes")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}

	var got ChangePage
	decode(t, rec, &got)
	if len(got.Changes) != 4 {
		t.Fatalf("returned %d changes, want 4", len(got.Changes))
	}
	if got.Changes[0].At != "2026-07-25T17:20:03Z" {
		t.Errorf("first change is at %s, want the newest", got.Changes[0].At)
	}
	for i := 1; i < len(got.Changes); i++ {
		if got.Changes[i].ID >= got.Changes[i-1].ID {
			t.Fatalf("changes are not newest first: %d then %d", got.Changes[i-1].ID, got.Changes[i].ID)
		}
	}
	if got.Changes[0].Op != "UPDATE" {
		t.Errorf("op = %q, want the statement kind spelled out", got.Changes[0].Op)
	}
	if got.Changes[0].Table == "" || got.Changes[0].Schema == "" {
		t.Errorf("change does not name its table: %+v", got.Changes[0])
	}
}

// A timeline of ten thousand rows should not carry ten thousand row images. The
// list is what a timeline renders; the images come from the detail route.
func TestChangesRouteOmitsRowImages(t *testing.T) {
	db := seed(t, 1785000000, 2)
	srv := New(db, "shop.db", clock(1785000060))

	rec := get(t, srv, "/api/changes")

	for _, field := range []string{`"before"`, `"after"`, `"columns"`} {
		if strings.Contains(rec.Body.String(), field) {
			t.Errorf("the list carries %s:\n%s", field, rec.Body.String())
		}
	}
}

func TestChangesRoutePagesWithTheCursor(t *testing.T) {
	db := seed(t, 1785000000, 5)
	srv := New(db, "shop.db", clock(1785000060))

	seen := map[int64]bool{}
	target := "/api/changes?limit=2"
	for round := 0; ; round++ {
		if round > 5 {
			t.Fatalf("paging did not finish in %d rounds; last target %s", round, target)
		}

		rec := get(t, srv, target)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d for %s\n%s", rec.Code, target, rec.Body.String())
		}

		var page ChangePage
		decode(t, rec, &page)
		for _, c := range page.Changes {
			if seen[c.ID] {
				t.Fatalf("change %d was returned on two pages", c.ID)
			}
			seen[c.ID] = true
		}
		if page.Next == nil {
			break
		}
		target = "/api/changes?limit=2&before=" + strconv.FormatInt(*page.Next, 10)
	}

	if len(seen) != 5 {
		t.Errorf("paging saw %d changes, want 5", len(seen))
	}
}

// The cursor has to stop being offered, or a client following it walks forever.
func TestChangesRouteOmitsTheCursorOnTheLastPage(t *testing.T) {
	db := seed(t, 1785000000, 2)
	srv := New(db, "shop.db", clock(1785000060))

	rec := get(t, srv, "/api/changes?limit=10")

	var got ChangePage
	decode(t, rec, &got)
	if got.Next != nil {
		t.Errorf("next = %d on a page holding everything", *got.Next)
	}
	if strings.Contains(rec.Body.String(), `"next"`) {
		t.Errorf("next is present on the last page:\n%s", rec.Body.String())
	}
}

func TestChangesRouteFiltersByTable(t *testing.T) {
	db := seed(t, 1785000000, 6)
	srv := New(db, "shop.db", clock(1785000060))

	rec := get(t, srv, "/api/changes?tables=shop.shipments")

	var got ChangePage
	decode(t, rec, &got)
	if len(got.Changes) == 0 {
		t.Fatal("filtering by a table that has changes returned none")
	}
	for _, c := range got.Changes {
		if c.Table != "shipments" {
			t.Errorf("returned a change to %s, which the filter excludes", c.Table)
		}
	}
}

func TestChangesRouteFiltersByTime(t *testing.T) {
	db := seed(t, 1785000000, 6)
	srv := New(db, "shop.db", clock(1785000060))

	rec := get(t, srv, "/api/changes?from=2026-07-25T17:20:02Z&to=2026-07-25T17:20:03Z")

	var got ChangePage
	decode(t, rec, &got)
	if len(got.Changes) != 2 {
		t.Fatalf("returned %d changes, want the 2 inside the window: %+v", len(got.Changes), got.Changes)
	}
}

// The whole reason this reads through the store. An empty array here would be an
// affirmative "nothing happened" delivered during an incident, which is the
// answer this project exists to refuse.
func TestChangesRouteRefusesWhenTheCollectorHasStalled(t *testing.T) {
	db := seed(t, 1785000000, 3)
	srv := New(db, "shop.db", clock(1785000000+3600))

	rec := get(t, srv, "/api/changes")

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409\n%s", rec.Code, rec.Body.String())
	}

	var got struct {
		Error string `json:"error"`
	}
	decode(t, rec, &got)
	if !strings.Contains(got.Error, "stale") {
		t.Errorf("error = %q, want it to name the stalled collector", got.Error)
	}
}

// A window reaching back before the store began is a question it cannot answer,
// not an empty one.
func TestChangesRouteRefusesAWindowBeforeCoverage(t *testing.T) {
	db := seed(t, 1785000000, 3)
	srv := New(db, "shop.db", clock(1785000060))

	rec := get(t, srv, "/api/changes?from=2020-01-01T00:00:00Z")

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409\n%s", rec.Code, rec.Body.String())
	}
}

// A mistyped bound must stop the request. Ignoring it would answer a different
// question than the one asked, and look like it worked.
func TestChangesRouteRejectsAnUnreadableTime(t *testing.T) {
	db := seed(t, 1785000000, 3)
	srv := New(db, "shop.db", clock(1785000060))

	rec := get(t, srv, "/api/changes?from=yesterday")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "yesterday") {
		t.Errorf("error does not quote the bad value:\n%s", rec.Body.String())
	}
}

func TestChangesRouteRejectsUnreadableNumbers(t *testing.T) {
	db := seed(t, 1785000000, 3)
	srv := New(db, "shop.db", clock(1785000060))

	for _, target := range []string{"/api/changes?limit=lots", "/api/changes?before=first"} {
		rec := get(t, srv, target)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400\n%s", target, rec.Code, rec.Body.String())
		}
	}
}

// An inverted window can never match a change, and an empty result reads as
// "there was nothing to undo".
func TestChangesRouteRejectsAWindowThatEndsBeforeItStarts(t *testing.T) {
	db := seed(t, 1785000000, 3)
	srv := New(db, "shop.db", clock(1785000060))

	rec := get(t, srv, "/api/changes?from=2026-07-25T17:20:03Z&to=2026-07-25T17:20:01Z")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400\n%s", rec.Code, rec.Body.String())
	}
}

// An honest empty window is still an empty array rather than null, so a client
// can iterate it without a nil check.
func TestChangesRouteReturnsAnArrayWhenNothingMatches(t *testing.T) {
	db := seed(t, 1785000000, 3)
	srv := New(db, "shop.db", clock(1785000060))

	rec := get(t, srv, "/api/changes?tables=shop.invoices")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"changes": []`) {
		t.Errorf("changes is not an empty array:\n%s", rec.Body.String())
	}
}
