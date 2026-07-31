package store

import (
	"database/sql"
	"fmt"
)

// migration is one step from the layout numbered Version-1 to Version.
//
// Steps are applied in order and only ever forward. There is no down step: a
// store is not a development database that can be rebuilt from a schema file —
// once the source's binlogs rotate it is the only record of the changes it
// holds, so undoing a migration would mean discarding data with nothing to
// restore it from. A build that cannot read a store refuses it instead.
type migration struct {
	Version int
	Why     string
	Apply   func(tx *sql.Tx) error
}

// migrations lists every step from version 1 to SchemaVersion.
//
// Empty because version 1 is the first published layout and nothing has needed
// changing yet. The runner exists regardless: the store is durable data, so the
// first time a column has to move is exactly the wrong time to be inventing the
// machinery for it.
var migrations []migration

// migrate brings a store from the layout it was written under up to this one.
//
// Every step runs in one transaction with the version bump that records it, so a
// crash midway leaves the store on the version it was already readable at rather
// than half-converted and claiming otherwise.
func migrate(db *sql.DB, path string, from int) error {
	for _, m := range migrations {
		if m.Version <= from {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("store: migrating %s to version %d: %w", path, m.Version, err)
		}

		if err := m.Apply(tx); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migrating %s to version %d (%s): %w", path, m.Version, m.Why, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO meta (key, value) VALUES ('schema_version', ?)
			 ON CONFLICT (key) DO UPDATE SET value = excluded.value`, m.Version); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: recording version %d on %s: %w", m.Version, path, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: migrating %s to version %d: %w", path, m.Version, err)
		}
	}
	return nil
}

// withMigrations swaps the migration list, for tests that exercise the runner
// before any real step exists. It returns a function restoring the original, and
// is safe to call more than once.
func withMigrations(steps []migration) func() {
	old := migrations
	migrations = steps
	return func() { migrations = old }
}
