package acceptance

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/learttytyri/mulligan/internal/change"
	"github.com/learttytyri/mulligan/internal/store"
)

// buildMulligan compiles the command once per test binary run.
func buildMulligan(t *testing.T) string {
	t.Helper()

	bin := filepath.Join(t.TempDir(), "mulligan")
	cmd := exec.Command("go", "build", "-o", bin, "../../cmd/mulligan")

	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("building mulligan: %v\n%s", err, stderr.String())
	}
	return bin
}

// A collector is killed, not asked to stop. It is the ordinary way a process
// ends: an OOM kill, a container stopped past its grace period, a machine losing
// power. What must survive is that the store is readable, holds no half-written
// transaction, and has a checkpoint that has not moved past the changes it
// covers — because a checkpoint ahead of the data resumes past changes nobody
// recorded, leaving a hole with nothing to describe it.
func TestAKilledCollectorLeavesTheStoreConsistent(t *testing.T) {
	s := startMySQL(t)
	bin := buildMulligan(t)

	s.exec("CREATE DATABASE shop")
	s.exec(schema)
	s.exec(seed)

	dir := t.TempDir()
	path := filepath.Join(dir, "mulligan.db")

	// Several rounds, because where the kill lands relative to a write is a race
	// and one attempt proves little.
	for round := 0; round < 4; round++ {
		t.Run(fmt.Sprintf("round %d", round), func(t *testing.T) {
			cmd := exec.Command(bin, "watch",
				"-store", path,
				"-server-id", "4100",
				"-max-staleness", "1h")
			cmd.Env = append(os.Environ(), "MULLIGAN_DSN="+s.dsn())

			var out strings.Builder
			cmd.Stdout = &out
			cmd.Stderr = &out

			if err := cmd.Start(); err != nil {
				t.Fatalf("starting watch: %v", err)
			}

			// Let it attach before there is anything to collect, so the workload lands
			// while it is streaming rather than before it connects.
			time.Sleep(2 * time.Second)

			// A steady stream of committed transactions for the kill to interrupt.
			stop := make(chan struct{})
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				for i := 0; ; i++ {
					select {
					case <-stop:
						return
					default:
					}
					if _, err := s.run(fmt.Sprintf(
						"UPDATE shop.orders SET status = 'r%d-%d' WHERE id = %d", round, i, i%3+1)); err != nil {
						return
					}
				}
			}()

			time.Sleep(1500 * time.Millisecond)

			// SIGKILL: no cleanup, no flush, no chance to finish a write.
			if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
				t.Fatalf("killing watch: %v", err)
			}
			_ = cmd.Wait()

			// Stop writing before inspecting, so the store is not moving underneath.
			close(stop)
			<-finished

			db, err := store.Open(path)
			if err != nil {
				t.Fatalf("the store could not be opened after a kill: %v\nwatch said:\n%s", err, out.String())
			}
			defer db.Close()

			problems, err := db.Integrity()
			if err != nil {
				t.Fatalf("checking the store: %v", err)
			}
			if len(problems) > 0 {
				t.Errorf("the store is inconsistent after a kill:\n  %s", strings.Join(problems, "\n  "))
			}

			// Whatever it did record has to be readable, not merely present.
			cov, err := db.Coverage()
			if err != nil {
				t.Fatalf("reading coverage: %v", err)
			}
			if !cov.To.IsZero() {
				if _, err := db.Events(change.Filter{}, cov.To); err != nil {
					t.Errorf("the store holds changes it cannot read back: %v", err)
				}
			}
		})
	}
}

// After the kill, a new collector has to pick up and carry on rather than
// refusing the store it was writing a moment ago.
func TestACollectorResumesAfterBeingKilled(t *testing.T) {
	s := startMySQL(t)
	bin := buildMulligan(t)

	s.exec("CREATE DATABASE shop")
	s.exec(schema)
	s.exec(seed)

	path := filepath.Join(t.TempDir(), "mulligan.db")

	start := func() *exec.Cmd {
		cmd := exec.Command(bin, "watch", "-store", path, "-server-id", "4101", "-max-staleness", "1h")
		cmd.Env = append(os.Environ(), "MULLIGAN_DSN="+s.dsn())
		if err := cmd.Start(); err != nil {
			t.Fatalf("starting watch: %v", err)
		}
		return cmd
	}

	first := start()
	time.Sleep(2 * time.Second)
	s.exec("UPDATE shop.orders SET status = 'before-kill' WHERE id = 1")
	time.Sleep(time.Second)

	_ = first.Process.Signal(syscall.SIGKILL)
	_ = first.Wait()

	before := countEvents(t, path)
	if before == 0 {
		t.Fatal("nothing was collected before the kill, so the resume proves nothing")
	}

	second := start()
	t.Cleanup(func() {
		_ = second.Process.Signal(syscall.SIGTERM)
		_ = second.Wait()
	})
	time.Sleep(2 * time.Second)

	s.exec("UPDATE shop.orders SET status = 'after-restart' WHERE id = 2")

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if countEvents(t, path) > before {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Errorf("the collector did not resume after being killed: still %d changes", before)
}

func countEvents(t *testing.T, path string) int {
	t.Helper()

	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	defer db.Close()

	cov, err := db.Coverage()
	if err != nil {
		t.Fatalf("reading coverage: %v", err)
	}
	if cov.To.IsZero() {
		return 0
	}

	events, err := db.Events(change.Filter{}, cov.To)
	if err != nil {
		t.Fatalf("reading changes: %v", err)
	}
	return len(events)
}
