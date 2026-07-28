package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	_ "modernc.org/sqlite" // pure-Go driver, so the binary stays static
)

// SchemaVersion is the layout this build writes and understands.
//
// A store from a newer build is refused rather than read. Once the source's
// binlogs rotate the store is the only record of those changes, so misreading
// records written under a layout this build does not know would produce
// plausible values with nothing to indicate they are wrong.
const SchemaVersion = 1

// Store is the rolling window of recent changes, held in SQLite.
type Store struct {
	db   *sql.DB
	path string
}

// Binding is the source a store was captured from.
//
// Every field is something that, if it changed underneath a store, would make
// its existing rows mean something different: a different server has its own
// unrelated positions, a different flavor spells GTIDs differently, and a
// different decode contract turns the same bytes into different values.
type Binding struct {
	Flavor            string
	ServerIdentity    string
	GTIDDialect       string
	DecodeFingerprint string
}

// Open prepares the store at path, creating it if it does not exist.
func Open(path string) (*Store, error) {
	// WAL so a reader is not blocked by the writer: watch holds a write
	// transaction for the length of a source transaction, and a generate that
	// blocked on it would fail at exactly the moment someone needs it.
	//
	// A busy timeout so the writes that do contend wait rather than failing
	// instantly. foreign_keys because SQLite leaves them off, which would make
	// the row_change cascade decorative and let pruning strand rows. NORMAL
	// synchronous is WAL's durable-enough setting: a crash can lose the last
	// commits, which the checkpoint already accounts for, but cannot corrupt.
	dsn := path + "?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(ON)" +
		"&_pragma=synchronous(NORMAL)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: opening %s: %w", path, err)
	}

	s := &Store{db: db, path: path}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the store.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("store: closing %s: %w", s.path, err)
	}
	return nil
}

func (s *Store) init() error {
	if _, err := s.db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("store: preparing %s: %w", s.path, err)
	}

	var recorded string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&recorded)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = s.db.Exec(`INSERT INTO meta (key, value) VALUES ('schema_version', ?)`, SchemaVersion)
		if err != nil {
			return fmt.Errorf("store: recording the schema version in %s: %w", s.path, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("store: reading the schema version of %s: %w", s.path, err)
	}

	version, err := strconv.Atoi(recorded)
	if err != nil {
		return fmt.Errorf("store: %s records an unreadable schema version %q", s.path, recorded)
	}
	if version > SchemaVersion {
		return fmt.Errorf("store: %s was written by a newer build (schema version %d, this build understands %d); "+
			"reading it here could turn records it does not understand into plausible wrong values",
			s.path, version, SchemaVersion)
	}
	return nil
}

// Bind records the source this store was captured from, or refuses if it was
// captured from a different one.
//
// It is called on every connect, not only at creation. A store resumed against
// another server would pick up a position that is valid there and means
// something else, and rows from two unrelated histories would be appended into
// one order — producing a revert assembled from before-images that never
// coexisted, with nothing about it looking wrong.
func (s *Store) Bind(b Binding) error {
	stored, ok, err := s.binding()
	if err != nil {
		return err
	}
	if !ok {
		return s.writeBinding(b)
	}

	for _, m := range []struct {
		what       string
		was, isNow string
	}{
		{"flavor", stored.Flavor, b.Flavor},
		{"server identity", stored.ServerIdentity, b.ServerIdentity},
		{"gtid dialect", stored.GTIDDialect, b.GTIDDialect},
		{"decode contract", stored.DecodeFingerprint, b.DecodeFingerprint},
	} {
		if m.was != m.isNow {
			return fmt.Errorf("store: %s holds changes captured with %s %q, but this connection reports %q; "+
				"a store may only ever follow the source it was created against",
				s.path, m.what, m.was, m.isNow)
		}
	}
	return nil
}

func (s *Store) binding() (Binding, bool, error) {
	var b Binding
	err := s.db.QueryRow(
		`SELECT flavor, server_identity, gtid_dialect, decode_options FROM source WHERE id = 1`,
	).Scan(&b.Flavor, &b.ServerIdentity, &b.GTIDDialect, &b.DecodeFingerprint)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Binding{}, false, nil
	case err != nil:
		return Binding{}, false, fmt.Errorf("store: reading the source binding of %s: %w", s.path, err)
	}
	return b, true, nil
}

func (s *Store) writeBinding(b Binding) error {
	_, err := s.db.Exec(
		`INSERT INTO source (id, flavor, server_identity, gtid_dialect, decode_options, codec_version)
		 VALUES (1, ?, ?, ?, ?, ?)`,
		b.Flavor, b.ServerIdentity, b.GTIDDialect, b.DecodeFingerprint, CodecVersion)
	if err != nil {
		return fmt.Errorf("store: recording the source binding in %s: %w", s.path, err)
	}
	return nil
}
