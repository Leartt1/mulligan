package change

import (
	"strings"
	"time"
)

// Filter narrows a scan to the events an operator asked about.
//
// The zero Filter matches everything: every field is a narrowing, so leaving
// one unset must widen the scan rather than empty it.
type Filter struct {
	// Tables selects tables by "schema.table", or by bare "table" to match that
	// name in any schema. Matching is case-insensitive, since whether MySQL
	// itself folds table names depends on the host filesystem. Empty means
	// every table.
	Tables []string

	// From and To bound the event timestamp, both inclusive — binlog timestamps
	// have one-second resolution, so an exclusive bound would drop events from
	// the very second the operator asked about. A zero time is unbounded.
	From time.Time
	To   time.Time
}

// Match reports whether ev falls inside the filter.
func (f Filter) Match(ev Event) bool {
	// A schema change is context for every table in the window, not a change to
	// one of them. Which table a DDL statement names cannot be known without
	// parsing SQL, and dropping it because it carries no table name would hide
	// exactly the thing it is kept for.
	if !ev.IsSchemaChange() && !f.matchesTable(ev.Schema, ev.Table) {
		return false
	}
	if !f.From.IsZero() && ev.At.Before(f.From) {
		return false
	}
	if !f.To.IsZero() && ev.At.After(f.To) {
		return false
	}
	return true
}

func (f Filter) matchesTable(schema, table string) bool {
	if len(f.Tables) == 0 {
		return true
	}
	qualified := schema + "." + table
	for _, want := range f.Tables {
		if strings.EqualFold(want, table) || strings.EqualFold(want, qualified) {
			return true
		}
	}
	return false
}
