package change

import (
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func event(schema, table, when string) Event {
	return Event{Schema: schema, Table: table, At: at(when)}
}

// The zero filter is what a caller gets by leaving every flag unset, and it has
// to mean "everything" rather than "nothing" — the opposite would silently
// produce an empty revert script.
func TestZeroFilterMatchesEveryEvent(t *testing.T) {
	var f Filter
	if !f.Match(event("shop", "orders", "2026-07-27T12:00:00Z")) {
		t.Error("zero filter rejected an event, want it to match everything")
	}
}

func TestFilterMatchesTablesByName(t *testing.T) {
	tests := []struct {
		name   string
		tables []string
		ev     Event
		want   bool
	}{
		{"bare name matches that table", []string{"orders"}, event("shop", "orders", "2026-07-27T12:00:00Z"), true},
		{"bare name matches in any schema", []string{"orders"}, event("archive", "orders", "2026-07-27T12:00:00Z"), true},
		{"bare name rejects a different table", []string{"orders"}, event("shop", "customers", "2026-07-27T12:00:00Z"), false},
		{"qualified name matches its schema", []string{"shop.orders"}, event("shop", "orders", "2026-07-27T12:00:00Z"), true},
		{"qualified name rejects another schema", []string{"shop.orders"}, event("archive", "orders", "2026-07-27T12:00:00Z"), false},
		{"any listed table matches", []string{"customers", "orders"}, event("shop", "orders", "2026-07-27T12:00:00Z"), true},
		{"names are matched case-insensitively", []string{"Shop.Orders"}, event("shop", "orders", "2026-07-27T12:00:00Z"), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := Filter{Tables: tt.tables}
			if got := f.Match(tt.ev); got != tt.want {
				t.Errorf("Match(%s.%s) = %v, want %v", tt.ev.Schema, tt.ev.Table, got, tt.want)
			}
		})
	}
}

// Binlog timestamps have one-second resolution, so an exclusive bound would
// drop events that happened during the very second the operator asked about.
func TestFilterTimeBoundsAreInclusive(t *testing.T) {
	from := at("2026-07-27T12:00:00Z")
	to := at("2026-07-27T13:00:00Z")

	tests := []struct {
		name string
		f    Filter
		ev   Event
		want bool
	}{
		{"before from is rejected", Filter{From: from}, event("shop", "orders", "2026-07-27T11:59:59Z"), false},
		{"exactly from is kept", Filter{From: from}, event("shop", "orders", "2026-07-27T12:00:00Z"), true},
		{"after from is kept", Filter{From: from}, event("shop", "orders", "2026-07-27T12:00:01Z"), true},
		{"after to is rejected", Filter{To: to}, event("shop", "orders", "2026-07-27T13:00:01Z"), false},
		{"exactly to is kept", Filter{To: to}, event("shop", "orders", "2026-07-27T13:00:00Z"), true},
		{"inside the window is kept", Filter{From: from, To: to}, event("shop", "orders", "2026-07-27T12:30:00Z"), true},
		{"outside the window is rejected", Filter{From: from, To: to}, event("shop", "orders", "2026-07-27T14:00:00Z"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.f.Match(tt.ev); got != tt.want {
				t.Errorf("Match(at %s) = %v, want %v", tt.ev.At.Format(time.RFC3339), got, tt.want)
			}
		})
	}
}

// Comparing wall-clock instants rather than formatted strings keeps a window
// given in local time from silently missing UTC-stamped binlog events.
func TestFilterComparesInstantsAcrossTimeZones(t *testing.T) {
	f := Filter{From: at("2026-07-27T14:00:00+02:00")} // 12:00 UTC

	if !f.Match(event("shop", "orders", "2026-07-27T12:00:00Z")) {
		t.Error("event at the same instant in another zone was rejected, want it kept")
	}
	if f.Match(event("shop", "orders", "2026-07-27T11:59:59Z")) {
		t.Error("event before the bound was kept, want it rejected")
	}
}

func TestFilterRequiresBothTableAndTimeToMatch(t *testing.T) {
	f := Filter{Tables: []string{"orders"}, From: at("2026-07-27T12:00:00Z")}

	if f.Match(event("shop", "orders", "2026-07-27T11:00:00Z")) {
		t.Error("right table outside the window matched, want it rejected")
	}
	if f.Match(event("shop", "customers", "2026-07-27T13:00:00Z")) {
		t.Error("wrong table inside the window matched, want it rejected")
	}
	if !f.Match(event("shop", "orders", "2026-07-27T13:00:00Z")) {
		t.Error("right table inside the window was rejected, want it kept")
	}
}
