package acceptance

import (
	"reflect"
	"strings"
	"testing"

	"github.com/learttytyri/mulligan/internal/binlog"
	"github.com/learttytyri/mulligan/internal/change"
	"github.com/learttytyri/mulligan/internal/reverse"
)

// A generated column is computed from the others rather than written. If the log
// were to name one without carrying a value for it, every value after it would
// line up against the wrong column, and the reversal would be confidently wrong
// rather than merely broken. And a reversal that assigns to a generated column
// is rejected by the server outright.
const generatedSchema = `CREATE TABLE shop.invoices (
	id       INT PRIMARY KEY,
	net      DECIMAL(10,2) NOT NULL,
	rate     DECIMAL(4,3)  NOT NULL,
	tax      DECIMAL(10,2) AS (net * rate) VIRTUAL,
	gross    DECIMAL(10,2) AS (net + net * rate) STORED,
	label    VARCHAR(32)   NOT NULL
)`

const generatedSnapshot = `SELECT id, net, rate, tax, gross, label FROM shop.invoices ORDER BY id`

func TestReversalHandlesGeneratedColumns(t *testing.T) {
	s := startMySQL(t)

	s.exec("CREATE DATABASE shop")
	s.exec(generatedSchema)
	s.exec("INSERT INTO shop.invoices (id, net, rate, label) VALUES (1, 100.00, 0.200, 'first')")

	s.exec("FLUSH BINARY LOGS")
	logName := s.currentBinlog()

	before := s.query(generatedSnapshot)

	s.exec("UPDATE shop.invoices SET net = 999.00, label = 'wrong'")

	if damaged := s.query(generatedSnapshot); reflect.DeepEqual(before, damaged) {
		t.Fatal("the update changed nothing, so there is nothing to test")
	}

	events, err := binlog.ReadFile(s.copyBinlog(logName), binlog.Filter{Tables: []string{"shop.invoices"}})
	if err != nil {
		t.Fatalf("reading the binlog: %v", err)
	}

	// The log carries no flag saying these are computed, so they are named.
	change.MarkReadOnly(events, []string{"invoices.tax", "invoices.gross"})

	plan, err := reverse.Plan(events)
	if err != nil {
		t.Fatalf("generating the reversal: %v", err)
	}
	for _, r := range plan {
		t.Logf("generated: %s", r.Statement)

		// Assigning to a generated column is an error the server raises at apply
		// time; the statement must not contain one at all.
		if strings.Contains(r.Statement, "`tax` =") {
			t.Errorf("reversal assigns to the virtual column tax:\n%s", r.Statement)
		}
		if strings.Contains(r.Statement, "`gross` =") {
			t.Errorf("reversal assigns to the stored generated column gross:\n%s", r.Statement)
		}
	}

	s.revert(t, events, logName)

	restored := s.query(generatedSnapshot)
	if !reflect.DeepEqual(restored, before) {
		t.Errorf("rows were not restored\n got: %q\nwant: %q", restored, before)
	}
}
