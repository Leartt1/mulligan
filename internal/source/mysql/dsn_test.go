package mysql

import (
	"strings"
	"testing"
)

func TestParseDSNReadsTheFormsAnOperatorWrites(t *testing.T) {
	tests := []struct {
		in       string
		wantAddr string
		wantUser string
		wantPass string
	}{
		{"repl:secret@tcp(db.internal:3306)/", "db.internal:3306", "repl", "secret"},
		{"repl:secret@tcp(db.internal:3306)/mysql", "db.internal:3306", "repl", "secret"},
		{"repl:secret@db.internal:3306", "db.internal:3306", "repl", "secret"},
		{"repl:secret@tcp(db.internal)/", "db.internal:3306", "repl", "secret"},
		{"repl@tcp(db.internal:3306)/", "db.internal:3306", "repl", ""},
		{"repl:p@ss:w0rd@tcp(db.internal:3306)/", "db.internal:3306", "repl", "p@ss:w0rd"},
		{"repl:secret@tcp(127.0.0.1:13306)/", "127.0.0.1:13306", "repl", "secret"},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseDSN(tt.in)
			if err != nil {
				t.Fatalf("parseDSN returned error: %v", err)
			}
			if got.addr != tt.wantAddr {
				t.Errorf("addr = %q, want %q", got.addr, tt.wantAddr)
			}
			if got.user != tt.wantUser {
				t.Errorf("user = %q, want %q", got.user, tt.wantUser)
			}
			if got.password != tt.wantPass {
				t.Errorf("password = %q, want %q", got.password, tt.wantPass)
			}
		})
	}
}

// A DSN that cannot be parsed is reported to the operator, and the password is
// the one thing that must not travel with the complaint. Error text ends up in
// logs, terminals, tickets and screenshots.
func TestParseDSNNeverPutsThePasswordInItsError(t *testing.T) {
	hostile := []string{
		"",
		"not a dsn",
		"repl:hunter2@",
		"repl:hunter2@tcp(",
		"repl:hunter2@tcp()/",
		":hunter2@tcp(db:3306)/",
		"@tcp(db:3306)/",
	}

	for _, in := range hostile {
		t.Run(in, func(t *testing.T) {
			_, err := parseDSN(in)
			if err == nil {
				t.Fatalf("parseDSN(%q) was accepted, want an error", in)
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("error leaks the password: %v", err)
			}
		})
	}
}

// Redact is what every other message about this connection goes through, so it
// has to hold for a password containing the delimiters too.
func TestRedactRemovesThePassword(t *testing.T) {
	tests := []string{
		"repl:secret@tcp(db:3306)/",
		"repl:p@ss:w0rd@tcp(db:3306)/",
		"repl:secret@db:3306",
	}

	for _, in := range tests {
		t.Run(in, func(t *testing.T) {
			got := Redact(in)
			if strings.Contains(got, "secret") || strings.Contains(got, "p@ss") || strings.Contains(got, "w0rd") {
				t.Errorf("Redact(%q) = %q, which still carries the password", in, got)
			}
			if !strings.Contains(got, "db:3306") {
				t.Errorf("Redact(%q) = %q, want it to keep the address so the message is still useful", in, got)
			}
		})
	}
}

// A string with no credentials in it should come back readable rather than
// blanked, or every log line about a connection becomes useless.
func TestRedactLeavesSomethingUseful(t *testing.T) {
	if got := Redact("tcp(db:3306)/"); !strings.Contains(got, "db:3306") {
		t.Errorf("Redact() = %q, want the address preserved", got)
	}
}
