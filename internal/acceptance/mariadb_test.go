package acceptance

import (
	"reflect"
	"strings"
	"testing"

	"github.com/learttytyri/mulligan/internal/binlog"
)

// MariaDB is a fork with its own binlog quirks, and the README claims MySQL and
// MariaDB support. This runs the core promise against a real MariaDB rather than
// assuming the formats stayed compatible.
func TestMariaDBReversalRestoresTheRows(t *testing.T) {
	s := startServer(t, mariadb)

	s.exec("CREATE DATABASE shop")
	s.exec(schema)
	s.exec(seed)

	s.exec("FLUSH BINARY LOGS")
	logName := s.currentBinlog()

	before := s.query(snapshot)

	s.exec("UPDATE shop.orders SET status = 'shipped'")

	if damaged := s.query(snapshot); reflect.DeepEqual(before, damaged) {
		t.Fatal("the update changed nothing, so there is nothing to test")
	}

	events, err := binlog.ReadFile(s.copyBinlog(logName), binlog.Filter{Tables: []string{"shop.orders"}})
	if err != nil {
		t.Fatalf("reading the binlog: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("found %d change events, want 2", len(events))
	}

	// MariaDB writes zero as the position of every event inside a transaction,
	// recording the real one only on the event that commits. Provenance is what
	// makes a generated statement reviewable, so a zero here would mean the
	// script cannot say where any of its statements came from.
	for _, ev := range events {
		if ev.LogPos == 0 {
			t.Errorf("event on %s.%s has no position", ev.Schema, ev.Table)
		}
	}

	s.revert(t, events, logName)

	restored := s.query(snapshot)
	if !reflect.DeepEqual(restored, before) {
		t.Errorf("rows were not restored\n got: %q\nwant: %q", restored, before)
	}
}

// The refusals have to hold on MariaDB too, or its users get a plausible-looking
// wrong reversal where a MySQL user gets an error.
func TestMariaDBRefusesALogWithoutColumnNames(t *testing.T) {
	s := startServer(t, mariadb)

	s.exec("CREATE DATABASE shop")
	s.exec(schema)
	s.exec(seed)

	s.exec("SET GLOBAL binlog_row_metadata = MINIMAL")
	s.exec("FLUSH BINARY LOGS")
	logName := s.currentBinlog()
	s.exec("UPDATE shop.orders SET status = 'lost' WHERE id = 1")

	_, err := binlog.ReadFile(s.copyBinlog(logName), binlog.Filter{})
	if err == nil {
		t.Fatal("read a log with no column names without complaint, want an error")
	}
	if !strings.Contains(err.Error(), "binlog_row_metadata") {
		t.Errorf("error = %v, want it to name the setting to fix", err)
	}
}
