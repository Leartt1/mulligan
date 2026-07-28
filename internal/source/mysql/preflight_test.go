package mysql

import (
	"strings"
	"testing"
)

// The library defaults its flavor to "mysql" and only logs a mismatch rather than
// failing. Under that default MariaDB substitutes dummy events for its own, so
// GTID tracking never engages and every row event arrives with position zero —
// leaving the store, which becomes the only record once binlogs rotate, unable to
// say where any change came from.
func TestFlavorFromVersionDistinguishesMariaDB(t *testing.T) {
	tests := []struct {
		version string
		want    Flavor
	}{
		{"8.0.46", FlavorMySQL},
		{"8.4.0", FlavorMySQL},
		{"9.1.0-commercial", FlavorMySQL},
		{"11.4.12-MariaDB-ubu2404-log", FlavorMariaDB},
		{"10.5.9-MariaDB", FlavorMariaDB},
		{"10.11.2-mariadb", FlavorMariaDB},
	}

	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			if got := flavorFromVersion(tt.version); got != tt.want {
				t.Errorf("flavorFromVersion(%q) = %q, want %q", tt.version, got, tt.want)
			}
		})
	}
}

// Discovering a wrong setting halfway through a week of collection is too late:
// the changes logged under it are already unrecoverable. Every required setting
// is checked before streaming starts, and refused by the name an operator has to
// go and change.
func TestCheckSettingsRefusesEachRequiredSettingByName(t *testing.T) {
	ok := map[string]string{
		"binlog_format":       "ROW",
		"binlog_row_image":    "FULL",
		"binlog_row_metadata": "FULL",
	}

	tests := []struct {
		name    string
		mutate  func(map[string]string)
		wantVar string
	}{
		{"statement format", func(m map[string]string) { m["binlog_format"] = "STATEMENT" }, "binlog_format"},
		{"mixed format", func(m map[string]string) { m["binlog_format"] = "MIXED" }, "binlog_format"},
		{"minimal row image", func(m map[string]string) { m["binlog_row_image"] = "MINIMAL" }, "binlog_row_image"},
		{"noblob row image", func(m map[string]string) { m["binlog_row_image"] = "NOBLOB" }, "binlog_row_image"},
		{"minimal metadata", func(m map[string]string) { m["binlog_row_metadata"] = "MINIMAL" }, "binlog_row_metadata"},
		{"format missing entirely", func(m map[string]string) { delete(m, "binlog_format") }, "binlog_format"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vars := map[string]string{}
			for k, v := range ok {
				vars[k] = v
			}
			tt.mutate(vars)

			err := checkSettings(vars)
			if err == nil {
				t.Fatalf("checkSettings accepted %v, want a refusal", vars)
			}
			if !strings.Contains(err.Error(), tt.wantVar) {
				t.Errorf("error = %v, want it to name %s", err, tt.wantVar)
			}
		})
	}
}

func TestCheckSettingsAcceptsACorrectlyConfiguredServer(t *testing.T) {
	err := checkSettings(map[string]string{
		"binlog_format":       "ROW",
		"binlog_row_image":    "FULL",
		"binlog_row_metadata": "FULL",
	})
	if err != nil {
		t.Errorf("checkSettings refused a correct configuration: %v", err)
	}
}

// Settings values arrive from the server in whatever case it stores them.
func TestCheckSettingsIgnoresCase(t *testing.T) {
	err := checkSettings(map[string]string{
		"binlog_format":       "row",
		"binlog_row_image":    "full",
		"binlog_row_metadata": "Full",
	})
	if err != nil {
		t.Errorf("checkSettings refused a correct configuration in lower case: %v", err)
	}
}

// With binlog_row_value_options=PARTIAL_JSON a JSON column is logged as a diff
// rather than a value. Reversing from a diff writes a syntactically valid JSON
// document that is not what the column held, with nothing to indicate it.
func TestCheckSettingsRefusesPartialJSON(t *testing.T) {
	err := checkSettings(map[string]string{
		"binlog_format":            "ROW",
		"binlog_row_image":         "FULL",
		"binlog_row_metadata":      "FULL",
		"binlog_row_value_options": "PARTIAL_JSON",
	})
	if err == nil {
		t.Fatal("checkSettings accepted PARTIAL_JSON, want a refusal")
	}
	if !strings.Contains(err.Error(), "binlog_row_value_options") {
		t.Errorf("error = %v, want it to name the setting to fix", err)
	}
}

// MariaDB has no such variable, so its absence must not be read as a problem.
func TestCheckSettingsAcceptsAnEmptyValueOptions(t *testing.T) {
	err := checkSettings(map[string]string{
		"binlog_format":            "ROW",
		"binlog_row_image":         "FULL",
		"binlog_row_metadata":      "FULL",
		"binlog_row_value_options": "",
	})
	if err != nil {
		t.Errorf("checkSettings refused a server without value options: %v", err)
	}
}

// Statement capture is optional and each flavor spells it differently, so the
// answer must not be a refusal — only a record of whether the timeline will be
// able to show what caused a change.
func TestStatementCaptureIsDetectedPerFlavor(t *testing.T) {
	tests := []struct {
		name   string
		flavor Flavor
		vars   map[string]string
		want   bool
	}{
		{"mysql on", FlavorMySQL, map[string]string{"binlog_rows_query_log_events": "ON"}, true},
		{"mysql off", FlavorMySQL, map[string]string{"binlog_rows_query_log_events": "OFF"}, false},
		{"mysql absent", FlavorMySQL, map[string]string{}, false},
		{"mariadb on", FlavorMariaDB, map[string]string{"binlog_annotate_row_events": "ON"}, true},
		{"mariadb off", FlavorMariaDB, map[string]string{"binlog_annotate_row_events": "OFF"}, false},
		{"mysql variable ignored on mariadb", FlavorMariaDB, map[string]string{"binlog_rows_query_log_events": "ON"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := statementCapture(tt.flavor, tt.vars); got != tt.want {
				t.Errorf("statementCapture(%q, %v) = %v, want %v", tt.flavor, tt.vars, got, tt.want)
			}
		})
	}
}
