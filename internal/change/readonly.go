package change

import "strings"

// MarkReadOnly flags the named columns as computed by the database, so the
// engine reads them but never assigns to them.
//
// The binary log records a generated column's value like any other and carries
// no flag saying it is generated, so the names have to come from the operator.
// A name matches as bare "column", or qualified as "table.column" or
// "schema.table.column", and is compared case-insensitively.
func MarkReadOnly(events []Event, names []string) {
	want := make(map[string]bool, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			want[strings.ToLower(name)] = true
		}
	}
	if len(want) == 0 {
		return
	}

	for i := range events {
		ev := &events[i]

		// Events decoded from one table map share a column slice. Copy before
		// marking, so a change never reaches an event this call did not inspect.
		var marked []Column

		for j, c := range ev.Columns {
			if !want[strings.ToLower(c.Name)] &&
				!want[strings.ToLower(ev.Table+"."+c.Name)] &&
				!want[strings.ToLower(ev.Schema+"."+ev.Table+"."+c.Name)] {
				continue
			}
			if marked == nil {
				marked = make([]Column, len(ev.Columns))
				copy(marked, ev.Columns)
			}
			marked[j].ReadOnly = true
		}

		if marked != nil {
			ev.Columns = marked
		}
	}
}
