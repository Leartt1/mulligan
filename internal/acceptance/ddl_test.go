package acceptance

import (
	"strings"
	"testing"

	"github.com/learttytyri/mulligan/internal/binlog"
	"github.com/learttytyri/mulligan/internal/change"
	"github.com/learttytyri/mulligan/internal/reverse"
	"github.com/learttytyri/mulligan/internal/script"
)

// A revert is built from the table as it was when the change happened. If the
// schema moved since, the script describes a table that no longer exists in that
// shape.
//
// Mulligan cannot see that: a binlog event carries the columns as of itself, and
// generate never looks at the live table. So the question these tests answer is
// not "does it handle DDL" — it does not — but "how does it fail", and whether
// any of those failures are silent. A loud failure an operator can read is
// survivable. A script that runs cleanly and writes the wrong thing is not.
//
// The answers are recorded here rather than in prose because they are the kind of
// claim that rots.
func TestRevertingAcrossASchemaChange(t *testing.T) {
	s := startMySQL(t)

	s.exec("CREATE DATABASE shop")

	// applyReverted damages a table, generates the revert, applies whatever DDL the
	// case calls for, and reports what the server said.
	var warned bool
	applyReverted := func(t *testing.T, table, damage, ddl string) (applied bool, message string) {
		t.Helper()
		warned = false
		s := s.with(t)

		s.exec("FLUSH BINARY LOGS")
		logName := s.currentBinlog()

		s.exec(damage)
		if ddl != "" {
			s.exec(ddl)
		}

		events, err := binlog.ReadFile(s.copyBinlog(logName), change.Filter{Tables: []string{"shop." + table}})
		if err != nil {
			t.Fatalf("reading the binlog: %v", err)
		}
		if len(events) == 0 {
			t.Fatal("no changes captured, so the case tests nothing")
		}

		var rows, schemaChanges []change.Event
		for _, ev := range events {
			if ev.IsSchemaChange() {
				schemaChanges = append(schemaChanges, ev)
				continue
			}
			rows = append(rows, ev)
		}

		plan, err := reverse.Plan(rows)
		if err != nil {
			return false, "generation refused: " + err.Error()
		}

		var rendered strings.Builder
		if err := script.Render(&rendered, logName, change.Filter{}, schemaChanges, plan); err != nil {
			t.Fatalf("rendering: %v", err)
		}
		warned = strings.Contains(rendered.String(), "WARNING")
		t.Logf("script warned about a schema change: %v", warned)

		out, err := s.runScript(rendered.String())
		if err != nil {
			return false, strings.TrimSpace(out)
		}
		return true, ""
	}

	t.Run("a column added afterwards", func(t *testing.T) {
		s.with(t).exec(`CREATE TABLE shop.added (id INT PRIMARY KEY, status VARCHAR(16))`)
		s.with(t).exec(`INSERT INTO shop.added VALUES (1, 'pending')`)

		applied, msg := applyReverted(t,
			"added",
			"UPDATE shop.added SET status = 'shipped' WHERE id = 1",
			"ALTER TABLE shop.added ADD COLUMN note VARCHAR(32) NULL")

		got := s.with(t).query("SELECT id, status, note FROM shop.added")
		t.Logf("applied=%v message=%q rows=%v", applied, msg, got)

		// The reversal names only the columns it changed, so a new column it never
		// knew about is not referenced and the restore still lands.
		if !applied {
			t.Errorf("a revert failed because a column was added afterwards: %s", msg)
		}
		if len(got) != 1 || !strings.HasPrefix(got[0], "1\tpending") {
			t.Errorf("status was not restored: %v", got)
		}
	})

	t.Run("a column dropped afterwards", func(t *testing.T) {
		s.with(t).exec(`CREATE TABLE shop.dropped (id INT PRIMARY KEY, status VARCHAR(16), note VARCHAR(32))`)
		s.with(t).exec(`INSERT INTO shop.dropped VALUES (1, 'pending', 'keep')`)

		applied, msg := applyReverted(t,
			"dropped",
			"DELETE FROM shop.dropped WHERE id = 1",
			"ALTER TABLE shop.dropped DROP COLUMN note")

		t.Logf("applied=%v message=%q", applied, msg)

		// Re-inserting names every column of the row as it was, including the one
		// that no longer exists. That has to fail rather than insert a partial row.
		if applied {
			t.Error("a revert naming a dropped column was applied; it should have failed")
		}
		if !strings.Contains(strings.ToLower(msg), "unknown column") {
			t.Errorf("the failure does not name the missing column, so an operator cannot tell why: %s", msg)
		}
	})

	t.Run("a column retyped afterwards", func(t *testing.T) {
		s.with(t).exec(`CREATE TABLE shop.retyped (id INT PRIMARY KEY, amount INT)`)
		s.with(t).exec(`INSERT INTO shop.retyped VALUES (1, 42)`)

		applied, msg := applyReverted(t,
			"retyped",
			"UPDATE shop.retyped SET amount = 99 WHERE id = 1",
			"ALTER TABLE shop.retyped MODIFY COLUMN amount VARCHAR(16)")

		got := s.with(t).query("SELECT id, amount FROM shop.retyped")
		t.Logf("applied=%v message=%q rows=%v", applied, msg, got)

		// This is the case worth knowing about: the value is coerced into the new
		// type without complaint. Whether that is right depends on the change, and
		// nothing tells the operator it happened.
		// The value comes back coerced into the new type and the server says nothing.
		// This is the one schema change that fails silently, so it is the one the
		// script has to warn about — it is the whole reason schema changes are
		// carried through the pipeline at all.
		if !applied {
			t.Fatalf("the revert did not apply: %s", msg)
		}
		if !warned {
			t.Error("a retype restored a coerced value with no warning in the script, " +
				"which is the one schema change an operator gets no other signal about")
		}
	})

	t.Run("a column renamed afterwards", func(t *testing.T) {
		s.with(t).exec(`CREATE TABLE shop.renamed (id INT PRIMARY KEY, status VARCHAR(16))`)
		s.with(t).exec(`INSERT INTO shop.renamed VALUES (1, 'pending')`)

		applied, msg := applyReverted(t,
			"renamed",
			"UPDATE shop.renamed SET status = 'shipped' WHERE id = 1",
			"ALTER TABLE shop.renamed RENAME COLUMN status TO state")

		t.Logf("applied=%v message=%q", applied, msg)

		if applied {
			t.Error("a revert naming a renamed column was applied; it should have failed")
		}
		if !strings.Contains(strings.ToLower(msg), "unknown column") {
			t.Errorf("the failure does not name the missing column: %s", msg)
		}
	})

	t.Run("a column narrowed afterwards", func(t *testing.T) {
		s.with(t).exec(`CREATE TABLE shop.narrowed (id INT PRIMARY KEY, label VARCHAR(64))`)
		s.with(t).exec(`INSERT INTO shop.narrowed VALUES (1, 'a value that is comfortably longer than eight')`)

		applied, msg := applyReverted(t,
			"narrowed",
			"UPDATE shop.narrowed SET label = 'short' WHERE id = 1",
			"ALTER TABLE shop.narrowed MODIFY COLUMN label VARCHAR(8)")

		got := s.with(t).query("SELECT id, label FROM shop.narrowed")
		t.Logf("applied=%v message=%q rows=%v", applied, msg, got)

		// Under strict mode this is refused. Under a permissive sql_mode it would
		// truncate, which is the silent-wrong case; the assertion records which one
		// this server does.
		if applied {
			t.Errorf("RESULT: a value too long for the narrowed column was accepted, giving %v", got)
		}
	})
}

// runScript applies a script and returns the server's complaint rather than
// failing the test, so a case can assert on how a revert fails.
func (s *mysqlServer) runScript(script string) (string, error) {
	cmd := dockerExecScript(s, script)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
