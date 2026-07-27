package reverse

import (
	"fmt"

	"github.com/learttytyri/mulligan/internal/change"
)

// validate rejects events the engine cannot faithfully reverse.
//
// A row image out of step with the column list is not recoverable by guessing:
// indexing it would panic, and truncating it would emit a predicate matching
// the wrong rows. Both are worse than refusing the event.
func validate(ev change.Event) error {
	if len(ev.Columns) == 0 {
		return fmt.Errorf("reverse: event for %s carries no columns", qualify(ev.Schema, ev.Table))
	}

	image := func(name string, row []any) error {
		if len(row) != len(ev.Columns) {
			return fmt.Errorf("reverse: %s image for %s has %d values for %d columns",
				name, qualify(ev.Schema, ev.Table), len(row), len(ev.Columns))
		}
		return nil
	}

	switch ev.Op {
	case change.Insert:
		return image("after", ev.After)
	case change.Update:
		if err := image("before", ev.Before); err != nil {
			return err
		}
		return image("after", ev.After)
	case change.Delete:
		return image("before", ev.Before)
	}
	return fmt.Errorf("reverse: unsupported op %d for %s", ev.Op, qualify(ev.Schema, ev.Table))
}
