// Package acceptance holds the end-to-end checks that a generated reversal
// actually restores a real MySQL database.
//
// These run against a throwaway server in a container. They are skipped when
// docker is unavailable or when -short is set.
package acceptance

import (
	"reflect"
	"strings"
	"testing"

	"github.com/learttytyri/mulligan/internal/binlog"
)

const schema = `CREATE TABLE shop.orders (
	id       INT PRIMARY KEY,
	customer VARCHAR(64)   NOT NULL,
	status   VARCHAR(16)   NOT NULL,
	total    DECIMAL(10,2) NOT NULL,
	note     VARCHAR(64)   NULL
)`

const seed = `INSERT INTO shop.orders (id, customer, status, total, note) VALUES
	(1, 'ada',   'pending', 19.99,  NULL),
	(2, 'grace', 'packed',  250.00, 'gift wrap'),
	(3, 'alan',  'shipped', 3.50,   'fragile')`

const snapshot = `SELECT id, customer, status, total, note FROM shop.orders ORDER BY id`

func TestV01Acceptance(t *testing.T) {
	s := startMySQL(t)

	s.exec("CREATE DATABASE shop")
	s.exec(schema)
	s.exec(seed)

	t.Run("reverting an update that forgot its WHERE restores every row", func(t *testing.T) {
		s := s.with(t)

		// Rotate first so the accident lands in a log file of its own, which is
		// what an operator narrowing down a window ends up with anyway.
		s.exec("FLUSH BINARY LOGS")
		logName := s.currentBinlog()

		before := s.query(snapshot)

		// The accident this whole project exists for.
		s.exec("UPDATE shop.orders SET status = 'shipped'")

		damaged := s.query(snapshot)
		if reflect.DeepEqual(before, damaged) {
			t.Fatal("the bad update changed nothing, so there is nothing to test")
		}

		events, err := binlog.ReadFile(s.copyBinlog(logName), binlog.Filter{Tables: []string{"shop.orders"}})
		if err != nil {
			t.Fatalf("reading the binlog: %v", err)
		}

		// Rows 1 and 2 changed; row 3 was already 'shipped' and MySQL logs no row
		// image for a row an update left alone.
		if len(events) != 2 {
			t.Fatalf("found %d change events, want 2:\n%+v", len(events), events)
		}

		s.revert(t, events, logName)

		restored := s.query(snapshot)
		if !reflect.DeepEqual(restored, before) {
			t.Errorf("rows were not restored\n got: %q\nwant: %q", restored, before)
		}
	})

	// A reversal built from a partial row image would restore whatever columns
	// the log happened to carry and silently invent the rest.
	t.Run("refuses a log written without a full row image", func(t *testing.T) {
		s := s.with(t)

		s.exec("SET GLOBAL binlog_row_image = MINIMAL")
		defer s.exec("SET GLOBAL binlog_row_image = FULL")

		s.exec("FLUSH BINARY LOGS")
		logName := s.currentBinlog()
		s.exec("UPDATE shop.orders SET status = 'lost' WHERE id = 1")

		_, err := binlog.ReadFile(s.copyBinlog(logName), binlog.Filter{})
		if err == nil {
			t.Fatal("read a partial row image without complaint, want an error")
		}
		if !strings.Contains(err.Error(), "binlog_row_image") {
			t.Errorf("error = %v, want it to name the setting to fix", err)
		}
	})

	// Without column names in the log there is no way to know which value belongs
	// to which column, and a reversal would be a guess.
	t.Run("refuses a log written without column names", func(t *testing.T) {
		s := s.with(t)

		s.exec("SET GLOBAL binlog_row_metadata = MINIMAL")
		defer s.exec("SET GLOBAL binlog_row_metadata = FULL")

		s.exec("FLUSH BINARY LOGS")
		logName := s.currentBinlog()
		s.exec("UPDATE shop.orders SET status = 'lost' WHERE id = 2")

		_, err := binlog.ReadFile(s.copyBinlog(logName), binlog.Filter{})
		if err == nil {
			t.Fatal("read a log with no column names without complaint, want an error")
		}
		if !strings.Contains(err.Error(), "binlog_row_metadata") {
			t.Errorf("error = %v, want it to name the setting to fix", err)
		}
	})
}
