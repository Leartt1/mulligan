package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestWatchRequiresAStore(t *testing.T) {
	code, _, stderr := run(t, "watch", "-server-id", "1001")

	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "-store") {
		t.Errorf("error does not name the missing flag:\n%s", stderr)
	}
}

// A colliding server id disconnects whichever replica claimed it first, which is
// too destructive to pick a default for.
func TestWatchRequiresAServerID(t *testing.T) {
	code, _, stderr := run(t, "watch", "-store", filepath.Join(t.TempDir(), "m.db"))

	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "-server-id") {
		t.Errorf("error does not name the missing flag:\n%s", stderr)
	}
}

func TestWatchRequiresADSN(t *testing.T) {
	t.Setenv("MULLIGAN_DSN", "")

	code, _, stderr := run(t, "watch", "-store", filepath.Join(t.TempDir(), "m.db"), "-server-id", "1001")

	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "MULLIGAN_DSN") {
		t.Errorf("error does not say how to supply the connection:\n%s", stderr)
	}
}

// The password must not reach the terminal, a log file, or a ticket. This is the
// first command that holds one.
func TestWatchNeverPrintsThePassword(t *testing.T) {
	t.Setenv("MULLIGAN_DSN", "repl:hunter2@tcp(127.0.0.1:1)/")

	code, stdout, stderr := run(t, "watch",
		"-store", filepath.Join(t.TempDir(), "m.db"),
		"-server-id", "1001")

	if code == exitOK {
		t.Fatal("watch succeeded against a port nothing is listening on")
	}
	if strings.Contains(stdout+stderr, "hunter2") {
		t.Errorf("the password reached the output:\nstdout: %s\nstderr: %s", stdout, stderr)
	}
	// The address is still useful and should survive redaction.
	if !strings.Contains(stderr, "127.0.0.1:1") {
		t.Errorf("the error does not say which server it failed to reach:\n%s", stderr)
	}
}

// --dsn works but is called out, because an argument is visible in ps to every
// user on the host.
func TestWatchAcceptsADSNFlagAndWarnsAboutIt(t *testing.T) {
	code, _, stderr := run(t, "watch",
		"-store", filepath.Join(t.TempDir(), "m.db"),
		"-server-id", "1001",
		"-dsn", "repl:hunter2@tcp(127.0.0.1:1)/")

	if code == exitOK {
		t.Fatal("watch succeeded against a port nothing is listening on")
	}
	if strings.Contains(stderr, "hunter2") {
		t.Errorf("the password reached the output:\n%s", stderr)
	}
	if !strings.Contains(stderr, "ps") {
		t.Errorf("passing -dsn was not called out as exposing the password:\n%s", stderr)
	}
}

func TestWatchRejectsAnUnreadableRetention(t *testing.T) {
	t.Setenv("MULLIGAN_DSN", "repl:secret@tcp(127.0.0.1:1)/")

	code, _, stderr := run(t, "watch",
		"-store", filepath.Join(t.TempDir(), "m.db"),
		"-server-id", "1001",
		"-retain", "a fortnight")

	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "fortnight") {
		t.Errorf("error does not quote the bad value:\n%s", stderr)
	}
}

func TestUsageMentionsWatch(t *testing.T) {
	_, stdout, _ := run(t, "help")

	if !strings.Contains(stdout, "watch") {
		t.Errorf("help does not mention the watch command:\n%s", stdout)
	}
}

// Reading a store and reading files in one run would order two sets of changes
// against different clocks, so the combination is refused rather than guessed at.
func TestGenerateRefusesBothAStoreAndFiles(t *testing.T) {
	code, _, stderr := run(t, "generate", "-store", filepath.Join(t.TempDir(), "m.db"), "binlog.000001")

	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "not both") {
		t.Errorf("error does not explain the conflict:\n%s", stderr)
	}
}

func TestGenerateRequiresASourceOfChanges(t *testing.T) {
	code, _, stderr := run(t, "generate")

	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "-store") {
		t.Errorf("error does not offer the store as an option:\n%s", stderr)
	}
}

// A store the collector never wrote to must not read as "nothing happened". This
// is the refusal chain reaching the command line.
func TestGenerateFromAnEmptyStoreRefusesRatherThanReportingNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")

	// Opening creates the store without collecting anything.
	code, stdout, stderr := run(t, "generate", "-store", path)
	if code == exitOK {
		t.Fatalf("generate answered from a store that collected nothing:\n%s", stdout)
	}
	if strings.Contains(stdout, "no matching changes") {
		t.Errorf("an uncollected store reported an empty result:\n%s", stdout)
	}
	if !strings.Contains(stderr, "has not recorded") {
		t.Errorf("error does not say nothing was recorded:\n%s", stderr)
	}
}
