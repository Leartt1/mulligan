package change

import "testing"

func invoiceEvent() Event {
	return Event{
		Schema: "shop",
		Table:  "invoices",
		Columns: []Column{
			{Name: "id", PrimaryKey: true},
			{Name: "net"},
			{Name: "tax"},
		},
	}
}

func readOnlyNames(ev Event) []string {
	var out []string
	for _, c := range ev.Columns {
		if c.ReadOnly {
			out = append(out, c.Name)
		}
	}
	return out
}

func TestMarkReadOnlyMatchesColumnsByName(t *testing.T) {
	tests := []struct {
		name  string
		given []string
		want  []string
	}{
		{"bare column name", []string{"tax"}, []string{"tax"}},
		{"qualified by table", []string{"invoices.tax"}, []string{"tax"}},
		{"qualified by schema and table", []string{"shop.invoices.tax"}, []string{"tax"}},
		{"another table's column is left alone", []string{"orders.tax"}, nil},
		{"matched case-insensitively", []string{"Shop.Invoices.TAX"}, []string{"tax"}},
		{"several at once", []string{"net", "tax"}, []string{"net", "tax"}},
		{"nothing given marks nothing", nil, nil},
		{"an unknown name marks nothing", []string{"vat"}, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := []Event{invoiceEvent()}
			MarkReadOnly(events, tt.given)

			got := readOnlyNames(events[0])
			if len(got) != len(tt.want) {
				t.Fatalf("marked %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("marked %v, want %v", got, tt.want)
				}
			}
		})
	}
}

// Events share the column slice they came from, so marking one event must not
// silently mark a column on an event of a different table.
func TestMarkReadOnlyDoesNotLeakAcrossTables(t *testing.T) {
	orders := Event{
		Schema:  "shop",
		Table:   "orders",
		Columns: []Column{{Name: "id", PrimaryKey: true}, {Name: "tax"}},
	}
	events := []Event{invoiceEvent(), orders}

	MarkReadOnly(events, []string{"invoices.tax"})

	if got := readOnlyNames(events[1]); len(got) != 0 {
		t.Errorf("orders had %v marked read-only, want none", got)
	}
	if got := readOnlyNames(events[0]); len(got) != 1 || got[0] != "tax" {
		t.Errorf("invoices had %v marked read-only, want [tax]", got)
	}
}
