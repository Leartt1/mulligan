package reverse

import (
	"strings"
	"testing"

	"github.com/learttytyri/mulligan/internal/change"
)

func col(name string, pk bool) change.Column {
	return change.Column{Name: name, PrimaryKey: pk}
}

// Undoing a sequence of statements means applying their inverses in the
// opposite order. Emitting them chronologically would, for example, re-insert a
// deleted row before undoing the update that ran after the delete.
func TestScriptEmitsReversalsNewestFirst(t *testing.T) {
	cols := []change.Column{col("id", true), col("status", false)}
	events := []change.Event{
		{Schema: "shop", Table: "orders", Op: change.Insert, Columns: cols, After: []any{int64(1), "new"}},
		{Schema: "shop", Table: "orders", Op: change.Update, Columns: cols, Before: []any{int64(1), "new"}, After: []any{int64(1), "shipped"}},
		{Schema: "shop", Table: "orders", Op: change.Delete, Columns: cols, Before: []any{int64(1), "shipped"}},
	}

	got, err := Script(events)
	if err != nil {
		t.Fatalf("Script returned error: %v", err)
	}

	want := []string{
		"INSERT INTO `shop`.`orders` (`id`, `status`) VALUES (1, 'shipped');",
		"UPDATE `shop`.`orders` SET `status` = 'new' WHERE `id` = 1 LIMIT 1;",
		"DELETE FROM `shop`.`orders` WHERE `id` = 1 LIMIT 1;",
	}
	if len(got) != len(want) {
		t.Fatalf("Script() returned %d statements, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("statement %d =\n  %s\nwant\n  %s", i, got[i], want[i])
		}
	}
}

func TestScriptOnNoEventsReturnsNoStatements(t *testing.T) {
	got, err := Script(nil)
	if err != nil {
		t.Fatalf("Script returned error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Script(nil) = %v, want no statements", got)
	}
}

// A partial script is a trap: applying the half that generated cleanly would
// leave the database in a state that is neither before nor after. The position
// is reported so the operator can find the offending event.
func TestScriptFailsWholesaleAndNamesTheBadEvent(t *testing.T) {
	cols := []change.Column{col("id", true)}
	events := []change.Event{
		{Schema: "shop", Table: "orders", Op: change.Insert, Columns: cols, After: []any{int64(1)}},
		{Schema: "shop", Table: "orders", Op: change.Insert, Columns: cols, After: []any{int64(2), "extra"}},
	}

	got, err := Script(events)
	if err == nil {
		t.Fatalf("Script() = %v, want error", got)
	}
	if got != nil {
		t.Errorf("Script() returned %d statements alongside an error, want none", len(got))
	}
	if !strings.Contains(err.Error(), "event 1") {
		t.Errorf("error = %v, want it to identify event 1", err)
	}
}
