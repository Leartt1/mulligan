package acceptance

import (
	"reflect"
	"strings"
	"testing"

	"github.com/learttytyri/mulligan/internal/binlog"
)

// Anyone who can write a row can choose what ends up in a generated script, and
// that script is run by hand against production. So row data and identifiers are
// both untrusted input to a code generator, and the only thing standing between
// a hostile value and arbitrary execution is how the engine quotes it.
//
// The table and column names carry backticks; the values carry quote characters,
// backslashes, comment openers and statement terminators.
const injectionSchema = "CREATE TABLE shop.`ord``ers` (" +
	"`id` INT PRIMARY KEY," +
	"`sta``tus` VARCHAR(255) NOT NULL," +
	"payload VARCHAR(255) NOT NULL," +
	"note VARCHAR(255) NULL)"

const injectionSeed = "INSERT INTO shop.`ord``ers` VALUES " +
	// A closing quote followed by its own statement.
	"(1, '''; DROP TABLE shop.canary; -- ', 'one', NULL)," +
	// A backslash, whose meaning depends on sql_mode, ahead of a terminator.
	`(2, 'a\\b; DROP TABLE shop.canary', 'two', NULL),` +
	// Backticks, trying to break out of an identifier.
	"(3, '`; DROP TABLE shop.canary; -- ', 'three', NULL)," +
	// A comment opener that would swallow the rest of the line.
	"(4, '-- ', 'four', NULL)," +
	// A terminator plus a second statement, no quote involved.
	"(5, '; DROP TABLE shop.canary', 'five', NULL)"

const injectionSnapshot = "SELECT `id`, `sta``tus`, payload, note FROM shop.`ord``ers` ORDER BY `id`"

func TestHostileRowDataCannotInjectIntoTheGeneratedScript(t *testing.T) {
	s := startMySQL(t)

	s.exec("CREATE DATABASE shop")
	s.exec(injectionSchema)

	// If any payload ever escapes its literal, the script drops this table. Its
	// survival is the assertion that nothing broke out.
	s.exec("CREATE TABLE shop.canary (id INT PRIMARY KEY)")
	s.exec("INSERT INTO shop.canary VALUES (1)")

	s.exec(injectionSeed)
	s.exec("FLUSH BINARY LOGS")
	logName := s.currentBinlog()

	before := s.query(injectionSnapshot)

	// Damage every row, so the reversal has to render each hostile value back.
	s.exec("UPDATE shop.`ord``ers` SET `sta``tus` = 'wiped', note = 'wiped'")

	if damaged := s.query(injectionSnapshot); reflect.DeepEqual(before, damaged) {
		t.Fatal("the update changed nothing, so there is nothing to test")
	}

	events, err := binlog.ReadFile(s.copyBinlog(logName), binlog.Filter{Tables: []string{"shop.ord`ers"}})
	if err != nil {
		t.Fatalf("reading the binlog: %v", err)
	}
	if len(events) != 5 {
		t.Fatalf("found %d change events, want 5", len(events))
	}

	s.revert(t, events, logName)

	// The canary is the security assertion; the snapshot is the correctness one.
	if got := s.query("SELECT id FROM shop.canary"); len(got) != 1 || got[0] != "1" {
		t.Errorf("canary table lost its row (%v) — a payload escaped its literal", got)
	}

	restored := s.query(injectionSnapshot)
	if !reflect.DeepEqual(restored, before) {
		t.Errorf("rows were not restored\n got: %q\nwant: %q", restored, before)
	}
}

// A backtick in an identifier is the identifier-side equivalent of a quote in a
// value: unescaped, it ends the quoted name early and the rest becomes syntax.
func TestBacktickedIdentifiersAreEscapedInGeneratedSQL(t *testing.T) {
	s := startMySQL(t)

	s.exec("CREATE DATABASE shop")
	s.exec(injectionSchema)
	s.exec("INSERT INTO shop.`ord``ers` VALUES (1, 'a', 'b', NULL)")
	s.exec("FLUSH BINARY LOGS")
	logName := s.currentBinlog()

	s.exec("DELETE FROM shop.`ord``ers` WHERE `id` = 1")

	events, err := binlog.ReadFile(s.copyBinlog(logName), binlog.Filter{})
	if err != nil {
		t.Fatalf("reading the binlog: %v", err)
	}

	plan := s.revert(t, events, logName)

	// Doubling is what keeps the name a name.
	for _, r := range plan {
		if !strings.Contains(r.Statement, "`ord``ers`") {
			t.Errorf("table name is not backtick-escaped:\n%s", r.Statement)
		}
		if !strings.Contains(r.Statement, "`sta``tus`") {
			t.Errorf("column name is not backtick-escaped:\n%s", r.Statement)
		}
	}

	if got := s.query(injectionSnapshot); len(got) != 1 {
		t.Errorf("row was not re-inserted: %v", got)
	}
}
