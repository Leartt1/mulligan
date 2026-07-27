package reverse

import (
	"fmt"

	"github.com/learttytyri/mulligan/internal/change"
)

// Reversal is one generated statement together with the logged change it
// undoes. The pairing is what lets a reviewer see which statement in the source
// log each line of a revert corresponds to.
type Reversal struct {
	Event     change.Event
	Statement string
}

// Plan returns the reversals that undo events, ordered newest first.
//
// events is expected in the order the source database applied them. Undoing a
// sequence means applying the inverses in the opposite order — otherwise a
// re-inserted row can be undone by a statement that ran after the delete, and
// the result matches neither the before nor the after state.
//
// Generation is all-or-nothing. Half a revert script is more dangerous than
// none, so a single unreversible event fails the whole batch.
func Plan(events []change.Event) ([]Reversal, error) {
	if len(events) == 0 {
		return nil, nil
	}

	out := make([]Reversal, 0, len(events))
	for i := len(events) - 1; i >= 0; i-- {
		stmt, err := Statement(events[i])
		if err != nil {
			return nil, fmt.Errorf("event %d: %w", i, err)
		}
		out = append(out, Reversal{Event: events[i], Statement: stmt})
	}
	return out, nil
}
