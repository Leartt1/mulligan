package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/learttytyri/mulligan/internal/change"
	"github.com/learttytyri/mulligan/internal/store"
)

// clock returns a fixed now, so a test states the moment it is asking about
// rather than racing the wall clock.
func clock(unix int64) func() time.Time {
	return func() time.Time { return time.Unix(unix, 0).UTC() }
}

func at(unix int64) time.Time { return time.Unix(unix, 0).UTC() }

// seed builds a store holding n changes, one per transaction, a second apart
// from base, alternating between two tables.
func seed(t *testing.T, base int64, n int) *store.Store {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "mulligan.db"))
	if err != nil {
		t.Fatalf("setup: opening store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.OpenCoverage(at(base - 60)); err != nil {
		t.Fatalf("setup: opening coverage: %v", err)
	}
	if err := db.SetMaxStaleness(5 * time.Minute); err != nil {
		t.Fatalf("setup: staleness: %v", err)
	}
	if err := db.Bind(store.Binding{
		Flavor:         "mysql",
		ServerIdentity: "mysql:3e11fa47-71ca-11e1-9e33-c80aa9429562",
		GTIDDialect:    "mysql",
	}); err != nil {
		t.Fatalf("setup: binding: %v", err)
	}

	for i := 0; i < n; i++ {
		committed := base + int64(i)
		table := "orders"
		if i%2 == 1 {
			table = "shipments"
		}
		ev := change.Event{
			Schema:  "shop",
			Table:   table,
			Op:      change.Update,
			Columns: []change.Column{{Name: "id", PrimaryKey: true}, {Name: "status"}},
			Before:  []any{int64(1), "pending"},
			After:   []any{int64(1), "shipped"},
			LogFile: "binlog.000004",
			LogPos:  uint32(576 + i),
		}
		tx := change.Transaction{
			SourceID:    "gtid:" + table + ":" + time.Unix(committed, 0).UTC().Format("150405"),
			CommittedAt: at(committed),
			ServerID:    17,
			Events:      []change.Event{ev},
		}
		cp := change.Checkpoint{LogFile: "binlog.000004", LogPos: uint32(576 + i), UpdatedAt: at(committed)}
		if err := db.AppendTransaction(tx, cp); err != nil {
			t.Fatalf("setup: appending: %v", err)
		}
	}
	return db
}

// get issues a request against the server and returns the response.
func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, into any) {
	t.Helper()

	if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, rec.Body.String())
	}
}

func TestStatusRouteReportsAHealthyStore(t *testing.T) {
	db := seed(t, 1785000000, 3)
	srv := New(db, "shop.db", clock(1785000060))

	rec := get(t, srv, "/api/status")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var got Status
	decode(t, rec, &got)
	if !got.Healthy {
		t.Errorf("healthy = false on a fresh store: %q", got.Verdict)
	}
	if got.Source == nil || got.Source.Flavor != "mysql" {
		t.Errorf("source = %+v, want the bound server", got.Source)
	}
}

// The report is the answer, whatever it says, so an unhealthy store is still a
// 200. Its healthy field carries the verdict — the same information the
// command's exit code carries for a shell.
func TestStatusRouteAnswers200ForAnUnhealthyStore(t *testing.T) {
	db := seed(t, 1785000000, 3)
	srv := New(db, "shop.db", clock(1785000000+3600))

	rec := get(t, srv, "/api/status")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even for a stalled collector\n%s", rec.Code, rec.Body.String())
	}

	var got Status
	decode(t, rec, &got)
	if got.Healthy {
		t.Error("healthy = true an hour after the collector stopped")
	}
	if got.Verdict == "" {
		t.Error("verdict is empty on an unhealthy store")
	}
}

func TestUnknownRouteIs404JSON(t *testing.T) {
	db := seed(t, 1785000000, 1)
	srv := New(db, "shop.db", clock(1785000060))

	rec := get(t, srv, "/api/nothing")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var got struct {
		Error string `json:"error"`
	}
	decode(t, rec, &got)
	if got.Error == "" {
		t.Errorf("404 body carries no error message:\n%s", rec.Body.String())
	}
}

// Nothing here has a side effect, and a surface that accepts writes invites one
// to be added later without the decision being made again.
func TestWritesAreRefused(t *testing.T) {
	db := seed(t, 1785000000, 1)
	srv := New(db, "shop.db", clock(1785000060))

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(method, "/api/status", nil))

		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/status = %d, want 405", method, rec.Code)
		}
	}
}
