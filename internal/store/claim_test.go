package store

import (
	"path/filepath"
	"strings"
	"testing"
)

// Two collectors writing one store interleave their transactions into a single
// order that reflects neither source. The file is not corrupted — it opens and
// reads perfectly well — but a revert built from it applies changes in an order
// that never happened.
func TestASecondCollectorCannotClaimTheSameStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mulligan.db")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer first.Close()

	if err := first.Claim(); err != nil {
		t.Fatalf("the first collector could not claim the store: %v", err)
	}
	if first.LockUnsupported() {
		t.Skip("advisory locking is unavailable on this platform")
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("opening a second handle returned error: %v", err)
	}
	defer second.Close()

	err = second.Claim()
	if err == nil {
		t.Fatal("a second collector claimed a store another was already following")
	}
	if !strings.Contains(err.Error(), "another collector") {
		t.Errorf("error = %v, want it to say another collector holds the store", err)
	}
}

// Releasing hands the store to whoever wants it next, so an orderly restart does
// not have to wait for anything to time out.
func TestReleasingLetsAnotherCollectorTakeOver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mulligan.db")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := first.Claim(); err != nil {
		t.Fatalf("claiming: %v", err)
	}
	if first.LockUnsupported() {
		t.Skip("advisory locking is unavailable on this platform")
	}
	if err := first.Release(); err != nil {
		t.Fatalf("Release returned error: %v", err)
	}
	first.Close()

	second, err := Open(path)
	if err != nil {
		t.Fatalf("opening a second handle returned error: %v", err)
	}
	defer second.Close()

	if err := second.Claim(); err != nil {
		t.Errorf("a released store could not be claimed: %v", err)
	}
}

// Reading a store is not owning it. Generate must never be blocked by the
// collector that is filling it, which is the ordinary arrangement: one process
// writing, another asking what it has.
func TestClaimingDoesNotStopAnotherProcessReading(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mulligan.db")

	writer, err := Open(path)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer writer.Close()
	if err := writer.Claim(); err != nil {
		t.Fatalf("claiming: %v", err)
	}

	reader, err := Open(path)
	if err != nil {
		t.Fatalf("a reader could not open a claimed store: %v", err)
	}
	defer reader.Close()

	if _, err := reader.Coverage(); err != nil {
		t.Errorf("a reader could not read a claimed store: %v", err)
	}
}
