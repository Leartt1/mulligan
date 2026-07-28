package acceptance

import (
	"reflect"
	"testing"

	"github.com/learttytyri/mulligan/internal/binlog"
	"github.com/learttytyri/mulligan/internal/change"
)

// wideSchema covers the column types a real table actually uses. Each one is a
// separate way for a reversal to be quietly wrong: a value the engine renders
// with the wrong syntax, the wrong precision, or the wrong time zone restores
// something that is not what was there before.
const wideSchema = `CREATE TABLE shop.wide (
	id          INT PRIMARY KEY,
	big         BIGINT UNSIGNED  NOT NULL,
	negative    INT              NOT NULL,
	amount      DECIMAL(12,4)    NOT NULL,
	ratio       DOUBLE           NOT NULL,
	flag        BIT(8)           NOT NULL,
	tiny        TINYINT          NOT NULL,
	label       VARCHAR(64)      NOT NULL,
	quoted      VARCHAR(64)      NOT NULL,
	backslashed VARCHAR(64)      NOT NULL,
	unicode     VARCHAR(64)      NOT NULL,
	body        TEXT             NOT NULL,
	prose       TEXT             NOT NULL,
	blob_col    BLOB             NOT NULL,
	place       POINT            NOT NULL,
	state       ENUM('new','packed','shipped') NOT NULL,
	tags        SET('fragile','gift','rush')   NOT NULL,
	when_date   DATE             NOT NULL,
	when_time   TIME             NOT NULL,
	when_dt     DATETIME(6)      NOT NULL,
	when_ts     TIMESTAMP(6)     NOT NULL,
	when_year   YEAR             NOT NULL,
	doc         JSON             NOT NULL,
	maybe       VARCHAR(32)      NULL
)`

const wideSeed = "INSERT INTO shop.wide VALUES (" +
	"1," +
	"18446744073709551615," + // the largest unsigned value, negative if read as signed
	"-2147483648," +
	"-1234.5678," + // rounds if it goes through float
	"0.1," +
	"b'10110011'," +
	"-128," +
	"'ordinary'," +
	`"it's quoted",` + // a single quote inside the value
	`'back\\slash',` + // a backslash, whose meaning depends on sql_mode
	"'héllo → 🌍'," + // multi-byte utf8mb4
	"'line one\nline two'," + // a control character
	"'A plain sentence, readable in review.'," + // TEXT with nothing needing escape
	"0xDEADBEEF," +
	"ST_PointFromText('POINT(12.5 -3.25)')," +
	"'packed'," +
	"'fragile,rush'," +
	"'2026-07-27'," +
	"'13:45:05'," +
	"'2026-07-27 13:45:05.123456'," +
	"'2026-07-27 13:45:05.123456'," +
	"2026," +
	`'{"a": 1, "b": [true, null, "x"]}',` +
	"NULL)"

const wideSnapshot = `SELECT id, big, negative, amount, ratio, flag+0, tiny, label, quoted,
	backslashed, unicode, body, prose, HEX(blob_col), ST_AsText(place), state, tags,
	when_date, when_time, when_dt, when_ts, when_year, doc, maybe FROM shop.wide ORDER BY id`

// Every column type has to survive a round trip through the engine: damage the
// row, generate the reversal, apply it, and land back on exactly what was there.
func TestReversalRestoresEveryColumnType(t *testing.T) {
	s := startMySQL(t)

	s.exec("CREATE DATABASE shop")
	s.exec(wideSchema)
	s.exec(wideSeed)

	s.exec("FLUSH BINARY LOGS")
	logName := s.currentBinlog()

	before := s.query(wideSnapshot)

	// Overwrite every column, so reversing has to restore every one of them.
	s.exec(`UPDATE shop.wide SET
		big = 1, negative = 0, amount = 0.0001, ratio = 9.5, flag = b'00000001',
		tiny = 1, label = 'changed', quoted = 'changed', backslashed = 'changed',
		unicode = 'changed', body = 'changed', prose = 'changed', blob_col = 0x00,
		place = ST_PointFromText('POINT(0 0)'),
		state = 'new', tags = 'gift', when_date = '2000-01-01',
		when_time = '00:00:00', when_dt = '2000-01-01 00:00:00',
		when_ts = '2000-01-01 00:00:00', when_year = 2000,
		doc = '{"changed": true}', maybe = 'changed'`)

	if damaged := s.query(wideSnapshot); reflect.DeepEqual(before, damaged) {
		t.Fatal("the update changed nothing, so there is nothing to test")
	}

	events, err := binlog.ReadFile(s.copyBinlog(logName), change.Filter{Tables: []string{"shop.wide"}})
	if err != nil {
		t.Fatalf("reading the binlog: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("found %d change events, want 1", len(events))
	}

	s.revert(t, events, logName)

	restored := s.query(wideSnapshot)
	if !reflect.DeepEqual(restored, before) {
		t.Errorf("row was not restored\n got: %q\nwant: %q", restored, before)
	}
}

// A DELETE is reversed by re-inserting the row, which exercises literal
// rendering for every column at once rather than only the ones an update
// touched.
func TestReinsertingADeletedRowRestoresEveryColumnType(t *testing.T) {
	s := startMySQL(t)

	s.exec("CREATE DATABASE shop")
	s.exec(wideSchema)
	s.exec(wideSeed)

	s.exec("FLUSH BINARY LOGS")
	logName := s.currentBinlog()

	before := s.query(wideSnapshot)

	s.exec("DELETE FROM shop.wide WHERE id = 1")

	events, err := binlog.ReadFile(s.copyBinlog(logName), change.Filter{Tables: []string{"shop.wide"}})
	if err != nil {
		t.Fatalf("reading the binlog: %v", err)
	}

	s.revert(t, events, logName)

	restored := s.query(wideSnapshot)
	if !reflect.DeepEqual(restored, before) {
		t.Errorf("row was not restored\n got: %q\nwant: %q", restored, before)
	}
}
