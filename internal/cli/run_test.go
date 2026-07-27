package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut strings.Builder
	code = Run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestRunWithNoArgumentsExplainsItself(t *testing.T) {
	code, _, stderr := run(t)

	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "generate") {
		t.Errorf("usage does not mention the generate command:\n%s", stderr)
	}
}

func TestRunRejectsAnUnknownCommand(t *testing.T) {
	code, _, stderr := run(t, "rewind")

	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "rewind") {
		t.Errorf("error does not name the unknown command:\n%s", stderr)
	}
}

// --binlog is the one thing the command cannot default, so its absence has to
// be a usage error rather than a scan of nothing.
func TestRunRequiresABinlogToRead(t *testing.T) {
	code, _, stderr := run(t, "generate")

	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "binlog") {
		t.Errorf("error does not say what is missing:\n%s", stderr)
	}
}

// A time window rarely lines up with a log rotation, so the common case is
// several files. Naming them positionally is what lets a shell glob supply them
// in order.
func TestRunAcceptsBinlogsAsPositionalArguments(t *testing.T) {
	code, _, stderr := run(t, "generate", "first.000001")

	if code != exitFailure {
		t.Errorf("exit code = %d, want %d (the file does not exist)", code, exitFailure)
	}
	if !strings.Contains(stderr, "first.000001") {
		t.Errorf("positional file was not read:\n%s", stderr)
	}
}

func TestRunAcceptsRepeatedBinlogFlags(t *testing.T) {
	code, _, stderr := run(t, "generate", "-binlog", "first.000001", "-binlog", "second.000002")

	if code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	// Reading stops at the first unreadable file rather than skipping it, since a
	// gap in the middle of a window silently drops changes from the revert.
	if !strings.Contains(stderr, "first.000001") {
		t.Errorf("error does not name the file that failed:\n%s", stderr)
	}
}

func TestRunReportsAnUnreadableBinlog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.000001")
	code, stdout, stderr := run(t, "generate", "-binlog", path)

	if code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	if stdout != "" {
		t.Errorf("a failed scan wrote a script to stdout:\n%s", stdout)
	}
	if !strings.Contains(stderr, "absent.000001") {
		t.Errorf("error does not name the file:\n%s", stderr)
	}
}

// A window the operator mistyped must stop the command. Ignoring the bad bound
// would scan a different range than they asked for and look like it worked.
func TestRunRejectsAnUnreadableTimestamp(t *testing.T) {
	code, _, stderr := run(t, "generate", "-binlog", "any.000001", "-from", "yesterday")

	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "yesterday") {
		t.Errorf("error does not quote the bad value:\n%s", stderr)
	}
}

func TestRunRejectsAWindowThatEndsBeforeItStarts(t *testing.T) {
	code, _, stderr := run(t, "generate",
		"-binlog", "any.000001",
		"-from", "2026-07-27T12:00:00Z",
		"-to", "2026-07-27T11:00:00Z")

	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "-to") {
		t.Errorf("error does not explain the window:\n%s", stderr)
	}
}

func TestRunReportsItsVersion(t *testing.T) {
	code, stdout, _ := run(t, "version")

	if code != exitOK {
		t.Errorf("exit code = %d, want %d", code, exitOK)
	}
	if !strings.Contains(stdout, "mulligan") {
		t.Errorf("version output = %q, want it to name the program", stdout)
	}
}

// The file at -out may well be a revert script someone is partway through
// reviewing. Overwriting it silently is the one destructive thing this command
// could do, so it refuses — and refuses before doing the work, not after.
func TestRunRefusesToOverwriteAnExistingFile(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "revert.sql")
	original := "-- a script someone is still reading\n"
	if err := os.WriteFile(outPath, []byte(original), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	code, _, stderr := run(t, "generate", "-binlog", "any.000001", "-out", outPath)

	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, outPath) {
		t.Errorf("error does not name the file it refused to touch:\n%s", stderr)
	}

	kept, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if string(kept) != original {
		t.Errorf("existing file was modified: %q", kept)
	}
}

// Regenerating over yesterday's script is a normal thing to want, so the
// refusal has to be overridable.
func TestRunOverwritesAnExistingFileWhenForced(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "revert.sql")
	if err := os.WriteFile(outPath, []byte("-- stale\n"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// The scan still fails on a bogus binlog; what matters is that -force gets
	// past the overwrite check rather than stopping at it.
	_, _, stderr := run(t, "generate", "-binlog", "any.000001", "-out", outPath, "-force")

	if strings.Contains(stderr, "already exists") {
		t.Errorf("-force did not get past the overwrite check:\n%s", stderr)
	}
}

// A generated script is meant to be reviewed, kept, and maybe replayed, so it
// has to be writable to a file rather than only to a terminal.
func TestRunWritesTheScriptToTheRequestedFile(t *testing.T) {
	binlogPath := filepath.Join(t.TempDir(), "not-a-binlog.000001")
	if err := os.WriteFile(binlogPath, []byte("not a binlog"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	outPath := filepath.Join(t.TempDir(), "revert.sql")

	// The scan fails, and the point is that a failed run leaves no file behind
	// for someone to mistake for a complete revert.
	if code, _, _ := run(t, "generate", "-binlog", binlogPath, "-out", outPath); code != exitFailure {
		t.Fatalf("exit code = %d, want %d", code, exitFailure)
	}
	if _, err := os.Stat(outPath); err == nil {
		t.Error("a failed scan left an output file behind")
	}
}
