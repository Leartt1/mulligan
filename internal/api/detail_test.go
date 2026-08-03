package api

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/learttytyri/mulligan/internal/change"
	"github.com/learttytyri/mulligan/internal/store"
)

// newestID is the id of the most recent change, which is what a timeline hands
// a reader when they click the top row.
func newestID(t *testing.T, srv *Server) int64 {
	t.Helper()

	rec := get(t, srv, "/api/changes?limit=1")
	var page ChangePage
	decode(t, rec, &page)
	if len(page.Changes) == 0 {
		t.Fatal("the store holds no changes to look at")
	}
	return page.Changes[0].ID
}

func TestDetailRouteShowsBothRowImages(t *testing.T) {
	db := seed(t, 1785000000, 3)
	srv := New(db, "shop.db", clock(1785000060))
	id := newestID(t, srv)

	rec := get(t, srv, "/api/changes/"+strconv.FormatInt(id, 10))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}

	var got ChangeDetail
	decode(t, rec, &got)
	if got.ID != id {
		t.Errorf("id = %d, want %d", got.ID, id)
	}
	if len(got.Columns) != 2 || got.Columns[0].Name != "id" || !got.Columns[0].PrimaryKey {
		t.Fatalf("columns = %+v, want the table's columns with the key marked", got.Columns)
	}
	if len(got.Before) != 2 || got.Before[1] == nil || *got.Before[1] != "pending" {
		t.Errorf("before = %v, want the values the statement overwrote", derefAll(got.Before))
	}
	if len(got.After) != 2 || got.After[1] == nil || *got.After[1] != "shipped" {
		t.Errorf("after = %v, want the values the statement wrote", derefAll(got.After))
	}
}

// Values are JSON strings, never JSON numbers: every browser parses a JSON
// number as a float64, and a DECIMAL or a large BIGINT would be displayed
// almost-right in the diff someone approves.
func TestDetailRouteRendersValuesAsStrings(t *testing.T) {
	db := seedRow(t, 1785000000, change.Update,
		[]any{int64(9007199254740993), change.Raw("19.99")},
		[]any{int64(9007199254740993), change.Raw("24.50")})
	srv := New(db, "shop.db", clock(1785000060))
	id := newestID(t, srv)

	rec := get(t, srv, "/api/changes/"+strconv.FormatInt(id, 10))

	var got ChangeDetail
	decode(t, rec, &got)
	if got.Before[0] == nil || *got.Before[0] != "9007199254740993" {
		t.Errorf("before[0] = %v, want the integer intact past 2^53", derefAll(got.Before))
	}
	if got.Before[1] == nil || *got.Before[1] != "19.99" {
		t.Errorf("before[1] = %v, want the decimal to the cent", derefAll(got.Before))
	}
}

// Values are rendered by the same rules the generated script uses, not by
// whatever Go's default formatting produces. A timestamp shown in Go's format,
// or bytes shown as a list of numbers, would be a preview of something other
// than the statement it is previewing.
func TestDetailRouteRendersValuesTheWayTheScriptDoes(t *testing.T) {
	db := seedRow(t, 1785000000, change.Update,
		[]any{int64(1), time.Date(2026, 7, 25, 17, 20, 0, 0, time.UTC)},
		[]any{int64(1), []byte{0xff, 0xfe}})
	srv := New(db, "shop.db", clock(1785000060))
	id := newestID(t, srv)

	rec := get(t, srv, "/api/changes/"+strconv.FormatInt(id, 10))

	var got ChangeDetail
	decode(t, rec, &got)
	if got.Before[1] == nil || *got.Before[1] != "2026-07-25 17:20:00" {
		t.Errorf("before[1] = %v, want the datetime the script would write", derefAll(got.Before))
	}
	if got.After[1] == nil || *got.After[1] != `X'FFFE'` {
		t.Errorf("after[1] = %v, want the hex literal the script would write", derefAll(got.After))
	}
}

// A NULL column is a null, not the text "NULL" — which would be
// indistinguishable from a column holding that word.
func TestDetailRouteRendersNullAsNull(t *testing.T) {
	db := seedRow(t, 1785000000, change.Update,
		[]any{int64(1), nil},
		[]any{int64(1), "shipped"})
	srv := New(db, "shop.db", clock(1785000060))
	id := newestID(t, srv)

	rec := get(t, srv, "/api/changes/"+strconv.FormatInt(id, 10))

	var got ChangeDetail
	decode(t, rec, &got)
	if got.Before[1] != nil {
		t.Errorf("before[1] = %q, want null", *got.Before[1])
	}
}

// An INSERT has nothing before it and a DELETE nothing after. Reporting an empty
// array would say the row existed with no columns.
func TestDetailRouteOmitsTheImageAnOperationDoesNotHave(t *testing.T) {
	db := seedRow(t, 1785000000, change.Insert, nil, []any{int64(1), "pending"})
	srv := New(db, "shop.db", clock(1785000060))
	id := newestID(t, srv)

	rec := get(t, srv, "/api/changes/"+strconv.FormatInt(id, 10))

	var got ChangeDetail
	decode(t, rec, &got)
	if got.Before != nil {
		t.Errorf("before = %v on an INSERT, want null", derefAll(got.Before))
	}
	if len(got.After) != 2 {
		t.Errorf("after = %v, want the inserted row", derefAll(got.After))
	}
}

func TestDetailRouteReportsAnUnknownID(t *testing.T) {
	db := seed(t, 1785000000, 2)
	srv := New(db, "shop.db", clock(1785000060))

	rec := get(t, srv, "/api/changes/99999")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404\n%s", rec.Code, rec.Body.String())
	}
}

func TestDetailRouteRejectsAnIDThatIsNotANumber(t *testing.T) {
	db := seed(t, 1785000000, 2)
	srv := New(db, "shop.db", clock(1785000060))

	rec := get(t, srv, "/api/changes/newest")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400\n%s", rec.Code, rec.Body.String())
	}
}

// One stored change is complete by definition, so the coverage refusals that
// guard a window do not apply. Withholding it because the collector has since
// stopped would hide the one thing still visible during the incident.
func TestDetailRouteAnswersWhileTheCollectorIsStalled(t *testing.T) {
	db := seed(t, 1785000000, 2)
	fresh := New(db, "shop.db", clock(1785000060))
	id := newestID(t, fresh)

	stalled := New(db, "shop.db", clock(1785000000+3600))
	if rec := get(t, stalled, "/api/changes"); rec.Code != http.StatusConflict {
		t.Fatalf("the timeline answered from a stalled store, so this proves nothing (%d)", rec.Code)
	}

	rec := get(t, stalled, "/api/changes/"+strconv.FormatInt(id, 10))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for one stored change\n%s", rec.Code, rec.Body.String())
	}
}

// seedRow stores one change with the images given, for the cases the general
// seed does not cover.
func seedRow(t *testing.T, committed int64, op change.Op, before, after []any) *store.Store {
	t.Helper()

	db := seed(t, committed-10, 0)
	ev := change.Event{
		Schema:  "shop",
		Table:   "orders",
		Op:      op,
		Columns: []change.Column{{Name: "id", PrimaryKey: true}, {Name: "status"}},
		Before:  before,
		After:   after,
		LogFile: "binlog.000004",
		LogPos:  920,
	}
	tx := change.Transaction{
		SourceID:    "gtid:one",
		CommittedAt: at(committed),
		ServerID:    17,
		Events:      []change.Event{ev},
	}
	if err := db.AppendTransaction(tx,
		change.Checkpoint{LogFile: "binlog.000004", LogPos: 920, UpdatedAt: at(committed)}); err != nil {
		t.Fatalf("setup: appending: %v", err)
	}
	return db
}

func derefAll(values []*string) []string {
	out := make([]string, len(values))
	for i, v := range values {
		if v == nil {
			out[i] = "<null>"
			continue
		}
		out[i] = *v
	}
	return out
}
