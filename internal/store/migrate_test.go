package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// The runner is exercised with a step registered here, because there are no real
// migrations yet. The point of having it before one is needed is that the first
// time a column has to move — on durable data that may be the only record of the
// changes it holds — is the wrong moment to be writing this.
func TestAnOlderStoreIsBroughtForward(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mulligan.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE meta SET value = '1' WHERE key = 'schema_version'`); err != nil {
		t.Fatalf("setting the version: %v", err)
	}
	s.Close()

	// A step that leaves a mark, so the test can tell it ran.
	restore := withMigrations([]migration{{
		Version: 2,
		Why:     "test step",
		Apply: func(tx *sql.Tx) error {
			_, err := tx.Exec(`INSERT INTO meta (key, value) VALUES ('migration_ran', 'yes')`)
			return err
		},
	}})
	defer restore()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("an older store was not brought forward: %v", err)
	}
	defer reopened.Close()

	var mark string
	if err := reopened.db.QueryRow(`SELECT value FROM meta WHERE key = 'migration_ran'`).Scan(&mark); err != nil {
		t.Fatalf("the migration did not run: %v", err)
	}

	var version string
	if err := reopened.db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&version); err != nil {
		t.Fatalf("reading the version: %v", err)
	}
	if version != "2" {
		t.Errorf("schema version = %q after migrating, want 2", version)
	}
}

// A step that fails must leave the store on the version it was already readable
// at, rather than half-converted and claiming to be current.
func TestAFailedMigrationLeavesTheStoreOnItsOldVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mulligan.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE meta SET value = '1' WHERE key = 'schema_version'`); err != nil {
		t.Fatalf("setting the version: %v", err)
	}
	s.Close()

	restore := withMigrations([]migration{{
		Version: 2,
		Why:     "a step that cannot work",
		Apply: func(tx *sql.Tx) error {
			_, err := tx.Exec(`ALTER TABLE nothing_here ADD COLUMN x INT`)
			return err
		},
	}})
	defer restore()

	reopened, err := Open(path)
	if err == nil {
		reopened.Close()
		t.Fatal("a store whose migration failed was opened anyway")
	}
	if !strings.Contains(err.Error(), "a step that cannot work") {
		t.Errorf("error = %v, want it to say which step failed and why it existed", err)
	}

	// Readable at its old version, with nothing half-applied.
	restore()
	back, err := Open(path)
	if err != nil {
		t.Fatalf("the store was left unreadable: %v", err)
	}
	defer back.Close()

	var version string
	if err := back.db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&version); err != nil {
		t.Fatalf("reading the version: %v", err)
	}
	if version != "1" {
		t.Errorf("schema version = %q after a failed migration, want it unchanged at 1", version)
	}
}

// A store already current is left alone.
func TestACurrentStoreIsNotMigrated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mulligan.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	s.Close()

	var ran bool
	restore := withMigrations([]migration{{
		Version: SchemaVersion,
		Why:     "should not run",
		Apply: func(tx *sql.Tx) error {
			ran = true
			return nil
		},
	}})
	defer restore()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer reopened.Close()

	if ran {
		t.Error("a migration ran against a store that was already current")
	}
}
