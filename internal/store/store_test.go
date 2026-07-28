package store

import (
	"path/filepath"
	"strings"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "mulligan.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func binding() Binding {
	return Binding{
		Flavor:            "mysql",
		ServerIdentity:    "3e11fa47-71ca-11e1-9e33-c80aa9429562",
		GTIDDialect:       "mysql",
		DecodeFingerprint: "v1;parse_time=true;use_decimal=true;verify_checksum=true;tz=UTC",
	}
}

func TestOpenCreatesAStoreThatCanBeReopened(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mulligan.db")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := first.Bind(binding()); err != nil {
		t.Fatalf("Bind returned error: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopening returned error: %v", err)
	}
	defer second.Close()

	// The binding survives, or a restart would look like a different server.
	if err := second.Bind(binding()); err != nil {
		t.Errorf("reopened store rejected its own binding: %v", err)
	}
}

// watch holds a write transaction for the length of a source transaction, which
// on a large UPDATE is a long time. Under SQLite's default rollback journal a
// concurrent generate is blocked out at commit time, and the operator is told the
// store is busy at the exact moment they are trying to undo something.
//
// This asserts the setting rather than the concurrency it buys. The blocking only
// appears in a narrow window around commit, so a behavioural test for it is a
// race dressed up as an assertion — it passed just as happily with the journal
// mode set to DELETE, which is worse than no test at all. Read this as: the
// decision is deliberate, and changing it should require changing this line.
func TestTheStoreIsConfiguredForConcurrentReadAndWrite(t *testing.T) {
	s := open(t)

	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("reading the journal mode returned error: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Errorf("journal_mode = %q, want wal so a read is not blocked by the collector's open write", mode)
	}

	var timeout int
	if err := s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		t.Fatalf("reading the busy timeout returned error: %v", err)
	}
	if timeout <= 0 {
		t.Errorf("busy_timeout = %d, want a wait rather than an instant failure under contention", timeout)
	}
}

// The row_change to txn cascade is what keeps pruning from leaving rows behind
// that belong to no transaction. SQLite disables foreign keys by default, so
// without the pragma the constraint is decorative.
func TestDeletingATransactionRemovesItsRows(t *testing.T) {
	s := open(t)

	if _, err := s.db.Exec(
		`INSERT INTO txn (id, source_txn_id, committed_at, server_id) VALUES (1, 'gtid:1', 100, 7)`); err != nil {
		t.Fatalf("inserting a transaction returned error: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO row_change (txn_id, schema_name, table_name, op, log_file, log_pos, columns, before, after)
		 VALUES (1, 'shop', 'orders', 2, 'binlog.000004', 576, x'01', NULL, NULL)`); err != nil {
		t.Fatalf("inserting a row change returned error: %v", err)
	}

	if _, err := s.db.Exec(`DELETE FROM txn WHERE id = 1`); err != nil {
		t.Fatalf("deleting the transaction returned error: %v", err)
	}

	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM row_change`).Scan(&n); err != nil {
		t.Fatalf("counting rows returned error: %v", err)
	}
	if n != 0 {
		t.Errorf("%d row changes outlived their transaction, so foreign keys are not enforced", n)
	}
}

// A row change that names no transaction cannot be ordered against anything, so
// the constraint must reject it rather than accept an orphan.
func TestARowChangeWithoutATransactionIsRejected(t *testing.T) {
	s := open(t)

	_, err := s.db.Exec(
		`INSERT INTO row_change (txn_id, schema_name, table_name, op, log_file, log_pos, columns, before, after)
		 VALUES (999, 'shop', 'orders', 2, 'binlog.000004', 576, x'01', NULL, NULL)`)
	if err == nil {
		t.Error("an orphan row change was accepted, so foreign keys are not enforced")
	}
}

// A store is bound to the server it was captured from. Without that, pointing
// watch at a different server resumes on a position that is valid there and
// means something else, and rows from two unrelated histories are appended into
// one order — a revert built from before-images that never coexisted.
func TestBindRefusesAStoreCapturedFromADifferentSource(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(b *Binding)
		wantsIn string
	}{
		{"another server", func(b *Binding) { b.ServerIdentity = "00000000-0000-0000-0000-000000000000" }, "server"},
		{"another flavor", func(b *Binding) { b.Flavor = "mariadb" }, "flavor"},
		{"another gtid dialect", func(b *Binding) { b.GTIDDialect = "mariadb" }, "gtid"},
		{"another decode contract", func(b *Binding) { b.DecodeFingerprint = "v1;parse_time=false" }, "decode"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := open(t)
			if err := s.Bind(binding()); err != nil {
				t.Fatalf("first Bind returned error: %v", err)
			}

			other := binding()
			tt.mutate(&other)

			err := s.Bind(other)
			if err == nil {
				t.Fatalf("Bind accepted a store captured from a different source")
			}
			if !strings.Contains(strings.ToLower(err.Error()), tt.wantsIn) {
				t.Errorf("error = %v, want it to say which part differs (%q)", err, tt.wantsIn)
			}
		})
	}
}

func TestBindIsIdempotentForTheSameSource(t *testing.T) {
	s := open(t)

	for i := 0; i < 3; i++ {
		if err := s.Bind(binding()); err != nil {
			t.Fatalf("Bind %d returned error: %v", i+1, err)
		}
	}
}

// A store written by a newer build may use a format this one would misread. It
// is not a rebuildable cache, so refusing is the only safe answer — silently
// reading it would produce plausible values from records this build does not
// understand.
func TestOpenRefusesAStoreFromANewerBuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mulligan.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE meta SET value = ? WHERE key = 'schema_version'`, SchemaVersion+1); err != nil {
		t.Fatalf("bumping the schema version returned error: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	reopened, err := Open(path)
	if err == nil {
		reopened.Close()
		t.Fatal("Open accepted a store from a newer build, want a refusal")
	}
	if !strings.Contains(err.Error(), "newer") {
		t.Errorf("error = %v, want it to say the store is from a newer build", err)
	}
}

func TestOpenReportsAPathItCannotUse(t *testing.T) {
	// A directory is not a database, and the failure has to name the path so an
	// operator can see which one they got wrong.
	dir := t.TempDir()
	if s, err := Open(dir); err == nil {
		s.Close()
		t.Fatal("Open accepted a directory as a store")
	}
}
