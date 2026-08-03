package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/learttytyri/mulligan/internal/change"
)

func TestRevertRouteStreamsAScript(t *testing.T) {
	db := seed(t, 1785000000, 3)
	srv := New(db, "shop.db", clock(1785000060))

	rec := get(t, srv, "/api/revert.sql")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", ct)
	}
	// A browser should offer to save it, not render it: the script is a file
	// someone reviews and then runs, not a page.
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want an attachment", cd)
	}

	body := rec.Body.String()
	if !strings.HasPrefix(body, "-- mulligan revert script") {
		t.Errorf("body does not start with the script header:\n%s", body)
	}
	if !strings.Contains(body, "REVIEW BEFORE RUNNING") {
		t.Errorf("body does not carry the review warning:\n%s", body)
	}
	// The trailer is what says the script is complete rather than cut short.
	if !strings.Contains(body, "-- end of script: 3 statements") {
		t.Errorf("body has no completion trailer:\n%s", body)
	}
}

func TestRevertRouteUndoesTheLoggedChange(t *testing.T) {
	db := seed(t, 1785000000, 1)
	srv := New(db, "shop.db", clock(1785000060))

	rec := get(t, srv, "/api/revert.sql")

	body := rec.Body.String()
	if !strings.Contains(body, "UPDATE `shop`.`orders` SET `status` = 'pending'") {
		t.Errorf("script does not restore the overwritten value:\n%s", body)
	}
}

func TestRevertRouteFiltersByTable(t *testing.T) {
	db := seed(t, 1785000000, 6)
	srv := New(db, "shop.db", clock(1785000060))

	rec := get(t, srv, "/api/revert.sql?tables=shop.shipments")

	body := rec.Body.String()
	if strings.Contains(body, "`orders`") {
		t.Errorf("script touches a table the filter excludes:\n%s", body)
	}
	if !strings.Contains(body, "`shipments`") {
		t.Errorf("script does not touch the table asked for:\n%s", body)
	}
}

// A revert built across a schema change describes each table as it was at the
// time, which may not be its shape now — and a retyped column restores a coerced
// value with no error at all. The warning is the only thing standing between
// that and a script someone runs believing it is sound.
func TestRevertRouteWarnsAboutASchemaChangeInTheWindow(t *testing.T) {
	db := seed(t, 1785000000, 2)

	ddl := change.Event{
		Schema:  "shop",
		Table:   "orders",
		Op:      change.SchemaChange,
		Query:   "ALTER TABLE shop.orders MODIFY status VARCHAR(32)",
		LogFile: "binlog.000004",
		LogPos:  700,
	}
	if err := db.AppendTransaction(change.Transaction{
		SourceID:    "gtid:ddl",
		CommittedAt: at(1785000005),
		ServerID:    17,
		Events:      []change.Event{ddl},
	}, change.Checkpoint{LogFile: "binlog.000004", LogPos: 700, UpdatedAt: at(1785000005)}); err != nil {
		t.Fatalf("setup: appending the schema change: %v", err)
	}

	srv := New(db, "shop.db", clock(1785000060))
	rec := get(t, srv, "/api/revert.sql")

	body := rec.Body.String()
	if !strings.Contains(body, "WARNING") || !strings.Contains(body, "schema change") {
		t.Errorf("script does not warn about the schema change in the window:\n%s", body)
	}
	if !strings.Contains(body, "ALTER TABLE shop.orders MODIFY status VARCHAR(32)") {
		t.Errorf("warning does not name the statement:\n%s", body)
	}
	// It is reported, never reversed: a row log cannot undo DDL.
	if strings.Contains(body, "\nALTER TABLE") {
		t.Errorf("script tried to reverse a schema change:\n%s", body)
	}
}

// The refusal has to land before the first byte. Once a response body has
// started there is no way to retract it, and a script that begins and then stops
// looks like a script.
func TestRevertRouteRefusesAStalledCollectorBeforeWritingAnything(t *testing.T) {
	db := seed(t, 1785000000, 3)
	srv := New(db, "shop.db", clock(1785000000+3600))

	rec := get(t, srv, "/api/revert.sql")

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409\n%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "mulligan revert script") {
		t.Errorf("a refusal still emitted script text:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "stale") {
		t.Errorf("refusal does not say why:\n%s", rec.Body.String())
	}
}

func TestRevertRouteRefusesAWindowBeforeCoverage(t *testing.T) {
	db := seed(t, 1785000000, 3)
	srv := New(db, "shop.db", clock(1785000060))

	rec := get(t, srv, "/api/revert.sql?from=2020-01-01T00:00:00Z")

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409\n%s", rec.Code, rec.Body.String())
	}
}

func TestRevertRouteRejectsAnUnreadableTime(t *testing.T) {
	db := seed(t, 1785000000, 3)
	srv := New(db, "shop.db", clock(1785000060))

	rec := get(t, srv, "/api/revert.sql?to=someday")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400\n%s", rec.Code, rec.Body.String())
	}
}

// An honest empty window says so in the script, having already established that
// the store could answer for it.
func TestRevertRouteSaysWhenNothingMatches(t *testing.T) {
	db := seed(t, 1785000000, 3)
	srv := New(db, "shop.db", clock(1785000060))

	rec := get(t, srv, "/api/revert.sql?tables=shop.invoices")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no matching changes found") {
		t.Errorf("script does not say the window was empty:\n%s", rec.Body.String())
	}
}
