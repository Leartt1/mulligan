package acceptance

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/learttytyri/mulligan/internal/cli"
)

// A time window rarely lines up with a log rotation, so the damage an operator
// wants undone is often spread across files. This drives the command itself
// rather than the packages under it, because the ordering that matters —
// reversing the later file's changes before the earlier one's — is decided
// where the files are read.
func TestGenerateSpansARotatedLog(t *testing.T) {
	s := startMySQL(t)

	s.exec("CREATE DATABASE shop")
	s.exec(schema)
	s.exec(seed)

	s.exec("FLUSH BINARY LOGS")
	first := s.currentBinlog()

	before := s.query(snapshot)

	// Two accidents, either side of a rotation.
	s.exec("UPDATE shop.orders SET status = 'shipped' WHERE id IN (1, 2)")
	s.exec("FLUSH BINARY LOGS")
	second := s.currentBinlog()
	s.exec("UPDATE shop.orders SET status = 'lost' WHERE id = 3")

	firstPath := s.copyBinlog(first)
	secondPath := s.copyBinlog(second)

	outPath := filepath.Join(t.TempDir(), "revert.sql")
	var stdout, stderr strings.Builder
	code := cli.Run([]string{
		"generate", "-tables", "shop.orders", "-out", outPath, firstPath, secondPath,
	}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("generate exited %d: %s", code, stderr.String())
	}

	script, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading the script: %v", err)
	}
	t.Logf("generated script:\n%s", script)

	// The header has to account for every log the script was built from, or a
	// reviewer cannot tell what window they are looking at.
	for _, name := range []string{first, second} {
		if !strings.Contains(string(script), name) {
			t.Errorf("script header does not name %s:\n%s", name, script)
		}
	}

	s.applyScript(string(script))

	restored := s.query(snapshot)
	if !reflect.DeepEqual(restored, before) {
		t.Errorf("rows were not restored\n got: %q\nwant: %q", restored, before)
	}
}

// Skipping an unreadable file in the middle of a window would drop the changes
// it held, and the resulting script would look complete.
func TestGenerateStopsWhenOneLogInTheRunIsUnreadable(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "binlog.000001")
	if err := os.WriteFile(good, []byte("not a binlog"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	var stdout, stderr strings.Builder
	code := cli.Run([]string{"generate", good, filepath.Join(dir, "binlog.000002")}, &stdout, &stderr)

	if code == 0 {
		t.Errorf("generate succeeded with an unreadable log in the run:\n%s", stdout.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("a failed run still wrote a script:\n%s", stdout.String())
	}
}
