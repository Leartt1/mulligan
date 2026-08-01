package acceptance

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The claim status makes is that its exit code distinguishes a live collector
// from one that has stopped. Both look identical from outside the store, and the
// difference only exists against a real server that is really being followed —
// so it is measured here rather than asserted from a fixture.
func TestStatusNoticesWhenAWatchingCollectorStops(t *testing.T) {
	s := startMySQL(t)
	bin := buildMulligan(t)

	s.exec("CREATE DATABASE shop")
	s.exec(schema)
	s.exec(seed)

	path := filepath.Join(t.TempDir(), "mulligan.db")

	// A short allowance so the stall is observable without a long sleep. The
	// collector records it in the store, which is what status then judges by.
	cmd := exec.Command(bin, "watch",
		"-store", path,
		"-server-id", "4200",
		"-max-staleness", "5s")
	cmd.Env = append(os.Environ(), "MULLIGAN_DSN="+s.dsn())

	var watchOut strings.Builder
	cmd.Stdout = &watchOut
	cmd.Stderr = &watchOut
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting watch: %v", err)
	}
	killed := false
	t.Cleanup(func() {
		if !killed {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_ = cmd.Wait()
		}
	})

	time.Sleep(2 * time.Second)
	s.exec("UPDATE shop.orders SET status = 'watched' WHERE id = 1")
	time.Sleep(2 * time.Second)

	code, out := runStatus(t, bin, path)
	if code != 0 {
		t.Fatalf("status exited %d while the collector was running:\n%s\nwatch said:\n%s", code, out, watchOut.String())
	}
	if !strings.Contains(out, "\nOK\n") {
		t.Errorf("a live collector was not reported as OK:\n%s", out)
	}
	if !strings.Contains(out, "integrity ok") {
		t.Errorf("status did not report on integrity:\n%s", out)
	}
	// The store is following a real server, and which one is the first thing to
	// check when it looks wrong.
	if !strings.Contains(out, "source    mysql") {
		t.Errorf("status did not name the source it is bound to:\n%s", out)
	}

	// No cleanup, no flush — the ordinary way a collector dies.
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("killing watch: %v", err)
	}
	_ = cmd.Wait()
	killed = true

	// Past the allowance the collector recorded, with nothing writing to the
	// store. This is the moment a quiet database and a dead collector look the
	// same, and the only difference is that status says so.
	time.Sleep(7 * time.Second)

	code, out = runStatus(t, bin, path)
	if code != 1 {
		t.Fatalf("status exited %d after the collector was killed, want 1:\n%s", code, out)
	}
	if !strings.Contains(out, "NOT OK") || !strings.Contains(out, "stale") {
		t.Errorf("status did not report the stopped collector:\n%s", out)
	}
}

func runStatus(t *testing.T, bin, path string, extra ...string) (code int, out string) {
	t.Helper()

	args := append([]string{"status", "-store", path}, extra...)
	cmd := exec.Command(bin, args...)

	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		code = exit.ExitCode()
	default:
		t.Fatalf("running status: %v\n%s", err, buf.String())
	}
	return code, buf.String()
}
