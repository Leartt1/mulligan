package reverse

import (
	"strings"
	"testing"

	"github.com/learttytyri/mulligan/internal/change"
)

// A source adapter that emits a row image out of step with the column list is
// broken, and indexing into it would panic and take down whatever is streaming
// events. Refusing the event keeps the failure local and legible.
func TestStatementRejectsMalformedEvents(t *testing.T) {
	cols := []change.Column{
		{Name: "id", PrimaryKey: true},
		{Name: "status"},
	}

	tests := []struct {
		name string
		ev   change.Event
	}{
		{
			name: "insert with fewer values than columns",
			ev:   change.Event{Schema: "shop", Table: "orders", Op: change.Insert, Columns: cols, After: []any{int64(7)}},
		},
		{
			name: "insert with more values than columns",
			ev:   change.Event{Schema: "shop", Table: "orders", Op: change.Insert, Columns: cols, After: []any{int64(7), "shipped", "extra"}},
		},
		{
			name: "insert with no after image at all",
			ev:   change.Event{Schema: "shop", Table: "orders", Op: change.Insert, Columns: cols},
		},
		{
			name: "delete with no before image at all",
			ev:   change.Event{Schema: "shop", Table: "orders", Op: change.Delete, Columns: cols},
		},
		{
			name: "update missing its before image",
			ev:   change.Event{Schema: "shop", Table: "orders", Op: change.Update, Columns: cols, After: []any{int64(7), "shipped"}},
		},
		{
			name: "update missing its after image",
			ev:   change.Event{Schema: "shop", Table: "orders", Op: change.Update, Columns: cols, Before: []any{int64(7), "pending"}},
		},
		{
			name: "event with no columns",
			ev:   change.Event{Schema: "shop", Table: "orders", Op: change.Insert, After: []any{int64(7)}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Statement(tt.ev)
			if err == nil {
				t.Fatalf("Statement() = %q, want error", got)
			}
		})
	}
}

// An unset Op is the zero value of change.Op, which is easy to produce by
// accident when building an event field by field.
func TestStatementRejectsUnsetOp(t *testing.T) {
	ev := change.Event{
		Schema:  "shop",
		Table:   "orders",
		Columns: []change.Column{{Name: "id", PrimaryKey: true}},
		After:   []any{int64(7)},
	}

	got, err := Statement(ev)
	if err == nil {
		t.Fatalf("Statement() = %q, want error", got)
	}
	if !strings.Contains(err.Error(), "unsupported op") {
		t.Errorf("error = %v, want it to mention the unsupported op", err)
	}
}
