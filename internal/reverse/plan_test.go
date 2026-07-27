package reverse

import (
	"strings"
	"testing"
	"time"

	"github.com/learttytyri/mulligan/internal/change"
)

func col(name string, pk bool) change.Column {
	return change.Column{Name: name, PrimaryKey: pk}
}

// Undoing a sequence of statements means applying their inverses in the
// opposite order. Emitting them chronologically would, for example, re-insert a
// deleted row before undoing the update that ran after the delete.
func TestPlanEmitsReversalsNewestFirst(t *testing.T) {
	cols := []change.Column{col("id", true), col("status", false)}
	events := []change.Event{
		{Schema: "shop", Table: "orders", Op: change.Insert, Columns: cols, After: []any{int64(1), "new"}},
		{Schema: "shop", Table: "orders", Op: change.Update, Columns: cols, Before: []any{int64(1), "new"}, After: []any{int64(1), "shipped"}},
		{Schema: "shop", Table: "orders", Op: change.Delete, Columns: cols, Before: []any{int64(1), "shipped"}},
	}

	got, err := Plan(events)
	if err != nil {
		t.Fatalf("Plan returned error: %v", err)
	}

	want := []string{
		"INSERT INTO `shop`.`orders` (`id`, `status`) VALUES (1, 'shipped');",
		"UPDATE `shop`.`orders` SET `status` = 'new' WHERE `id` = 1 LIMIT 1;",
		"DELETE FROM `shop`.`orders` WHERE `id` = 1 LIMIT 1;",
	}
	if len(got) != len(want) {
		t.Fatalf("Plan returned %d reversals, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Statement != want[i] {
			t.Errorf("statement %d =\n  %s\nwant\n  %s", i, got[i].Statement, want[i])
		}
	}
}

// The operator reviewing a revert needs to know which logged statement each
// line undoes, so every reversal carries the event it came from.
func TestPlanKeepsEachReversalPairedWithItsEvent(t *testing.T) {
	cols := []change.Column{col("id", true), col("status", false)}
	events := []change.Event{
		{Schema: "shop", Table: "orders", Op: change.Insert, Columns: cols, After: []any{int64(1), "new"},
			LogFile: "binlog.000004", LogPos: 100, At: time.Unix(1785000000, 0).UTC()},
		{Schema: "shop", Table: "customers", Op: change.Insert, Columns: cols, After: []any{int64(2), "new"},
			LogFile: "binlog.000004", LogPos: 200, At: time.Unix(1785000060, 0).UTC()},
	}

	got, err := Plan(events)
	if err != nil {
		t.Fatalf("Plan returned error: %v", err)
	}

	// Newest first, so the customers event at position 200 leads.
	if got[0].Event.Table != "customers" || got[0].Event.LogPos != 200 {
		t.Errorf("reversal 0 came from %s at %d, want customers at 200", got[0].Event.Table, got[0].Event.LogPos)
	}
	if got[1].Event.Table != "orders" || got[1].Event.LogPos != 100 {
		t.Errorf("reversal 1 came from %s at %d, want orders at 100", got[1].Event.Table, got[1].Event.LogPos)
	}
}

func TestPlanOnNoEventsReturnsNoReversals(t *testing.T) {
	got, err := Plan(nil)
	if err != nil {
		t.Fatalf("Plan returned error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Plan(nil) returned %d reversals, want none", len(got))
	}
}

// A partial script is a trap: applying the half that generated cleanly would
// leave the database in a state that is neither before nor after. The position
// is reported so the operator can find the offending event.
func TestPlanFailsWholesaleAndNamesTheBadEvent(t *testing.T) {
	cols := []change.Column{col("id", true)}
	events := []change.Event{
		{Schema: "shop", Table: "orders", Op: change.Insert, Columns: cols, After: []any{int64(1)}},
		{Schema: "shop", Table: "orders", Op: change.Insert, Columns: cols, After: []any{int64(2), "extra"}},
	}

	got, err := Plan(events)
	if err == nil {
		t.Fatalf("Plan returned %d reversals, want error", len(got))
	}
	if got != nil {
		t.Errorf("Plan returned %d reversals alongside an error, want none", len(got))
	}
	if !strings.Contains(err.Error(), "event 1") {
		t.Errorf("error = %v, want it to identify event 1", err)
	}
}
