package acceptance

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/learttytyri/mulligan/internal/change"
	"github.com/learttytyri/mulligan/internal/store"
)

// This is the failure the coverage model was built for, driven all the way
// through a real server.
//
// The collector stops. Changes happen while it is down. The binlogs holding them
// are purged before it comes back, so those changes are gone from the source and
// were never stored — nothing anywhere can reconstruct them. A revert spanning
// that period has to say so, because the alternative is a script that looks
// complete and silently omits whatever happened in the hole.
func TestChangesMissedWhileTheCollectorWasDownAreRefusedNotSkipped(t *testing.T) {
	s := startMySQL(t)

	s.exec("CREATE DATABASE shop")
	s.exec(schema)

	path := filepath.Join(t.TempDir(), "mulligan.db")

	// Collect a first batch, so there is a checkpoint to become unresumable.
	c := startCollector(t, s, path)
	s.exec(seed)
	c.waitForEvents(t, 3)
	c.stop()

	// Changes the collector does not see, in logs that are then thrown away.
	s.exec("FLUSH BINARY LOGS")
	s.exec("UPDATE shop.orders SET status = 'lost' WHERE id = 1")
	s.exec("FLUSH BINARY LOGS")
	s.exec("UPDATE shop.orders SET status = 'lost' WHERE id = 2")
	s.exec("FLUSH BINARY LOGS")

	// Purge everything before the current file, which is where the checkpoint
	// pointed. The changes above are now unrecoverable from any source.
	s.exec("PURGE BINARY LOGS TO '" + s.currentBinlog() + "'")

	// The collector comes back to a resume point the server can no longer honour.
	back := startCollectorLogging(t, s, path)

	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopening the store: %v", err)
	}
	defer db.Close()

	gaps := waitForGap(t, db)
	for _, g := range gaps {
		t.Logf("recorded gap %s to %s: %s", g.From, g.To, g.Reason)
	}

	// A window spanning the hole is refused, by name and with the reason. It starts
	// at the coverage boundary so the reach-back check passes and the gap is what
	// the refusal is actually about.
	cov, err := db.Coverage()
	if err != nil {
		t.Fatalf("reading coverage: %v", err)
	}
	_, err = db.Events(change.Filter{From: cov.From}, time.Now().UTC())
	if err == nil {
		t.Fatal("a window spanning the gap was answered, so a revert would omit the missed changes silently")
	}
	if !strings.Contains(err.Error(), "gap") {
		t.Errorf("error = %v, want it to name the gap", err)
	}
	t.Logf("refusal: %v", err)

	// A gap must not make the store useless: the collector is running again, and a
	// window entirely after the hole is still answerable. Refusing everything would
	// be safe and worthless.
	after := gaps[len(gaps)-1].To.Add(time.Second)
	deadline := time.Now().Add(25 * time.Second)
	for attempt := 0; ; attempt++ {
		// A distinct value each time: an UPDATE that changes nothing logs no row
		// event, so repeating the same one would never produce a change with a late
		// enough timestamp to fall inside the window.
		s.exec(fmt.Sprintf("UPDATE shop.orders SET status = 'after-%d' WHERE id = 3", attempt))

		got, err := db.Events(change.Filter{From: after}, time.Now().UTC())
		if err == nil && len(got) > 0 {
			t.Logf("the store still answers for %d changes after the gap", len(got))
			return
		}
		select {
		case rerr := <-back.done:
			t.Fatalf("the collector returned early: %v", rerr)
		default:
		}

		if time.Now().After(deadline) {
			t.Fatalf("the store never answered for a window after the gap (%d changes, err %v); "+
				"one hole must not make the rest unreadable", len(got), err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// waitForGap blocks until the collector has recorded a gap.
func waitForGap(t *testing.T, db *store.Store) []store.Gap {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		gaps, err := db.Gaps()
		if err != nil {
			t.Fatalf("reading gaps: %v", err)
		}
		if len(gaps) > 0 {
			return gaps
		}
		time.Sleep(300 * time.Millisecond)
	}

	t.Fatal("no gap was recorded for the period the collector was down, " +
		"so a revert spanning it would look complete")
	return nil
}
