package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Change returns one stored change by the id the store assigned it, reporting
// false if nothing has that id.
//
// Deliberately without the coverage refusals every window read applies. Those
// exist because a window can be silently incomplete — a gap inside it, a
// collector that stopped before its end — and an incomplete window read as
// complete is how a revert comes out wrong. One change cannot be incomplete: it
// is stored, it is whole, and it is precisely what was asked for. Refusing to
// display it because the collector has since stopped would withhold the one
// thing still visible at the moment it is most wanted.
//
// The id is only meaningful within one store. It is not a source identifier and
// says nothing about any other store built from the same server.
func (s *Store) Change(id int64) (Entry, bool, error) {
	var row eventRow
	err := s.db.QueryRow(`SELECT `+eventColumns+`
		   FROM row_change r
		   JOIN txn t ON t.id = r.txn_id
		  WHERE r.id = ?`, id).Scan(row.dest()...)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Entry{}, false, nil
	case err != nil:
		return Entry{}, false, fmt.Errorf("store: reading change %d from %s: %w", id, s.path, err)
	}

	ev, err := row.decode()
	if err != nil {
		return Entry{}, false, err
	}
	return Entry{ID: id, Event: ev}, true, nil
}
