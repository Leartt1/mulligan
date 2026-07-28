// Package mysql streams row changes from a live MySQL or MariaDB server by
// connecting as a replica.
//
// It emits the same normalized change events the file reader does, so the
// reverse engine is unaware of which source a change arrived from.
package mysql

import (
	"fmt"
	"strings"
)

// Flavor is which of the two servers is on the other end. They diverge enough —
// in GTID dialect, in whether positions are recorded on events inside a
// transaction, and in how statement capture is spelled — that guessing wrong
// produces a store that cannot say where its changes came from.
type Flavor string

const (
	FlavorMySQL   Flavor = "mysql"
	FlavorMariaDB Flavor = "mariadb"
)

// requiredSettings are the server settings without which a reversal would be
// wrong rather than merely unavailable, each with the reason an operator needs
// in order to decide whether to change it.
var requiredSettings = []struct {
	name   string
	want   string
	reason string
}{
	{
		name:   "binlog_format",
		want:   "ROW",
		reason: "statement-format logs record the statement, not the rows it changed, so there is nothing to reverse",
	},
	{
		name:   "binlog_row_image",
		want:   "FULL",
		reason: "a partial image omits the values an UPDATE overwrote, which are exactly what a reversal restores",
	},
	{
		name:   "binlog_row_metadata",
		want:   "FULL",
		reason: "without column names the log identifies columns only by position, and a reversal would be a guess",
	},
}

// checkSettings refuses a server whose configuration cannot produce reversible
// changes.
//
// This runs before streaming begins. Discovering a wrong setting partway through
// a week of collection is too late — the changes logged under it are already
// unrecoverable, and the store is the only record once binlogs rotate.
func checkSettings(vars map[string]string) error {
	for _, s := range requiredSettings {
		got, ok := vars[s.name]
		if !ok {
			return fmt.Errorf("mysql: the server does not report %s; Mulligan needs it set to %s because %s",
				s.name, s.want, s.reason)
		}
		if !strings.EqualFold(got, s.want) {
			return fmt.Errorf("mysql: %s is %s, must be %s: %s", s.name, got, s.want, s.reason)
		}
	}

	// A JSON column logged as a diff decodes into a valid document that is not
	// what the column held. The absence of the variable is not a problem —
	// MariaDB has no such setting.
	if v := vars["binlog_row_value_options"]; strings.Contains(strings.ToUpper(v), "PARTIAL_JSON") {
		return fmt.Errorf("mysql: binlog_row_value_options is %s, which logs JSON columns as diffs rather than values; "+
			"a reversal built from a diff writes a document that is valid but wrong", v)
	}

	return nil
}

// statementCapture reports whether the server will tell us which statement
// caused a set of rows.
//
// It is never required. The timeline is better with it and correct without it,
// so a server that does not offer it is not refused — but which variable to
// consult depends on the flavor, and consulting the wrong one would report the
// feature as permanently unavailable on a server that has it enabled.
func statementCapture(flavor Flavor, vars map[string]string) bool {
	name := "binlog_rows_query_log_events"
	if flavor == FlavorMariaDB {
		name = "binlog_annotate_row_events"
	}
	return strings.EqualFold(vars[name], "ON")
}

// flavorFromVersion reads the flavor out of the server's own version string.
//
// The replication library defaults to MySQL and only logs a mismatch rather than
// failing, and under that default MariaDB substitutes dummy events for its own —
// so GTID tracking never engages and row events arrive with position zero.
func flavorFromVersion(version string) Flavor {
	if strings.Contains(strings.ToLower(version), "mariadb") {
		return FlavorMariaDB
	}
	return FlavorMySQL
}
