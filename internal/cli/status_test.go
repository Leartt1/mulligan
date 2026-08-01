package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/learttytyri/mulligan/internal/change"
	"github.com/learttytyri/mulligan/internal/store"
)

// seedStore writes one change committed `ago` before now, so a test can place a
// store either inside or outside the staleness allowance.
func seedStore(t *testing.T, ago time.Duration) (path string, db *store.Store) {
	t.Helper()

	path = filepath.Join(t.TempDir(), "mulligan.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("setup: opening store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	committed := time.Now().UTC().Add(-ago).Truncate(time.Second)
	if err := db.OpenCoverage(committed.Add(-time.Hour)); err != nil {
		t.Fatalf("setup: opening coverage: %v", err)
	}
	if err := db.Bind(store.Binding{
		Flavor:            "mysql",
		ServerIdentity:    "3e11fa47-71ca-11e1-9e33-c80aa9429562",
		GTIDDialect:       "mysql",
		DecodeFingerprint: "v1;tz=UTC",
	}); err != nil {
		t.Fatalf("setup: binding: %v", err)
	}

	ev := change.Event{
		Schema:  "shop",
		Table:   "orders",
		Op:      change.Update,
		Columns: []change.Column{{Name: "id", PrimaryKey: true}, {Name: "status"}},
		Before:  []any{int64(1), "pending"},
		After:   []any{int64(1), "shipped"},
		LogFile: "binlog.000004",
		LogPos:  576,
	}
	tx := change.Transaction{SourceID: "gtid:1", CommittedAt: committed, ServerID: 17, Events: []change.Event{ev}}
	cp := change.Checkpoint{LogFile: "binlog.000004", LogPos: 576, UpdatedAt: committed}
	if err := db.AppendTransaction(tx, cp); err != nil {
		t.Fatalf("setup: appending: %v", err)
	}
	return path, db
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestStatusRequiresAStore(t *testing.T) {
	code, _, stderr := run(t, "status")

	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "-store") {
		t.Errorf("error does not name the missing flag:\n%s", stderr)
	}
}

// Opening a store creates it, so a mistyped path would otherwise produce an
// empty store and report it as one whose collector has never run — an answer
// about a file the operator did not mean and that did not exist a moment ago.
func TestStatusRefusesAStoreThatDoesNotExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.db")

	code, _, stderr := run(t, "status", "-store", path)

	if code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, path) {
		t.Errorf("error does not name the store:\n%s", stderr)
	}
	if fileExists(path) {
		t.Error("status created the store file it was asked about")
	}
}

// An empty GTID dialect is not a missing field: it is how a source records that
// the server issues no GTIDs, which is MySQL 8.0's default. Printing "gtid "
// with nothing after it reads as a value that failed to load.
func TestStatusSaysWhenTheSourceIssuesNoGTIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-gtid.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := db.Bind(store.Binding{
		Flavor:         "mysql",
		ServerIdentity: "mysql:b8f274cf-8d9b-11f1-bb37-2ae711accf27",
		GTIDDialect:    "",
	}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	db.Close()

	_, stdout, _ := run(t, "status", "-store", path)

	if !strings.Contains(stdout, "no GTIDs") {
		t.Errorf("report does not say the server issues no GTIDs:\n%s", stdout)
	}
	if strings.Contains(stdout, "gtid \n") {
		t.Errorf("report has an empty gtid field:\n%s", stdout)
	}
}

func TestStatusReportsAHealthyStore(t *testing.T) {
	path, _ := seedStore(t, 30*time.Second)

	code, stdout, stderr := run(t, "status", "-store", path)

	if code != exitOK {
		t.Fatalf("exit code = %d, want %d\nstdout: %s\nstderr: %s", code, exitOK, stdout, stderr)
	}
	for _, want := range []string{"coverage", "retention", "freshness", "integrity ok", "OK"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("report does not mention %q:\n%s", want, stdout)
		}
	}
	// Which server the store follows is the first thing to check when it looks
	// wrong, and it is recorded already.
	if !strings.Contains(stdout, "3e11fa47-71ca-11e1-9e33-c80aa9429562") {
		t.Errorf("report does not name the source server:\n%s", stdout)
	}
}

// The exit code is the whole point of the command being scriptable: a dead
// collector and a quiet database look identical, and a cron check cannot be
// asked to read prose.
func TestStatusExitsNonZeroWhenTheCollectorHasStalled(t *testing.T) {
	path, _ := seedStore(t, time.Hour)

	code, stdout, _ := run(t, "status", "-store", path)

	if code != exitFailure {
		t.Fatalf("exit code = %d, want %d\n%s", code, exitFailure, stdout)
	}
	if !strings.Contains(stdout, "NOT OK") {
		t.Errorf("report does not say the store is not OK:\n%s", stdout)
	}
	if !strings.Contains(stdout, "stale") {
		t.Errorf("report does not say why:\n%s", stdout)
	}
}

func TestStatusExitsNonZeroWhenNothingHasBeenCollected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	db.Close()

	code, stdout, _ := run(t, "status", "-store", path)

	if code != exitFailure {
		t.Fatalf("exit code = %d, want %d\n%s", code, exitFailure, stdout)
	}
	if !strings.Contains(stdout, "nothing recorded yet") {
		t.Errorf("report does not say the store holds nothing:\n%s", stdout)
	}
	// A store watch has never connected for has no binding, and printing a blank
	// identity as though it were one would be its own small lie.
	if !strings.Contains(stdout, "not bound yet") {
		t.Errorf("report does not say the store has no source:\n%s", stdout)
	}
}

// Gaps and misses are permanent history. They are reported in full, and they do
// not latch the exit code — a probe that is red forever is a probe nobody reads.
func TestStatusListsGapsAndMissesWithoutFailing(t *testing.T) {
	path, db := seedStore(t, 30*time.Second)

	now := time.Now().UTC()
	if err := db.RecordGap(now.Add(-40*time.Minute), now.Add(-38*time.Minute), "resumed past a purged binlog"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := db.RecordMiss(now.Add(-20*time.Minute), "row image larger than the store accepts"); err != nil {
		t.Fatalf("setup: %v", err)
	}

	code, stdout, _ := run(t, "status", "-store", path)

	if code != exitOK {
		t.Fatalf("exit code = %d, want %d\n%s", code, exitOK, stdout)
	}
	if !strings.Contains(stdout, "resumed past a purged binlog") {
		t.Errorf("report does not describe the gap:\n%s", stdout)
	}
	if !strings.Contains(stdout, "row image larger than the store accepts") {
		t.Errorf("report does not describe the missed change:\n%s", stdout)
	}
}

func TestStatusJSONIsMachineReadable(t *testing.T) {
	path, db := seedStore(t, 30*time.Second)
	if err := db.RecordGap(time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(-59*time.Minute), "restart"); err != nil {
		t.Fatalf("setup: %v", err)
	}

	code, stdout, _ := run(t, "status", "-store", path, "-json")

	if code != exitOK {
		t.Fatalf("exit code = %d, want %d\n%s", code, exitOK, stdout)
	}

	var got struct {
		Store   string `json:"store"`
		Healthy bool   `json:"healthy"`
		Verdict string `json:"verdict"`
		Source  *struct {
			ServerIdentity string `json:"server_identity"`
		} `json:"source"`
		Coverage *struct {
			From                string `json:"from"`
			To                  string `json:"to"`
			MaxStalenessSeconds int    `json:"max_staleness_seconds"`
			RetentionSeconds    int    `json:"retention_seconds"`
		} `json:"coverage"`
		StaleSeconds      int      `json:"stale_seconds"`
		IntegrityProblems []string `json:"integrity_problems"`
		Gaps              []struct {
			From   string `json:"from"`
			To     string `json:"to"`
			Reason string `json:"reason"`
		} `json:"gaps"`
		Misses []struct {
			At     string `json:"at"`
			Reason string `json:"reason"`
		} `json:"misses"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("-json did not produce a JSON object: %v\n%s", err, stdout)
	}

	if !got.Healthy {
		t.Errorf("healthy = false on a fresh store: %q", got.Verdict)
	}
	if got.Verdict != "" {
		t.Errorf("verdict = %q, want empty on a healthy store", got.Verdict)
	}
	if got.Source == nil || got.Source.ServerIdentity != "3e11fa47-71ca-11e1-9e33-c80aa9429562" {
		t.Errorf("source = %+v, want the bound server", got.Source)
	}
	if got.Coverage == nil {
		t.Fatal("coverage is null on a store that has collected")
	}
	if _, err := time.Parse(time.RFC3339, got.Coverage.To); err != nil {
		t.Errorf("coverage.to = %q, want RFC 3339: %v", got.Coverage.To, err)
	}
	if got.Coverage.MaxStalenessSeconds != int(store.DefaultMaxStaleness.Seconds()) {
		t.Errorf("max_staleness_seconds = %d, want %d",
			got.Coverage.MaxStalenessSeconds, int(store.DefaultMaxStaleness.Seconds()))
	}
	if len(got.Gaps) != 1 || got.Gaps[0].Reason != "restart" {
		t.Errorf("gaps = %+v, want the one recorded gap", got.Gaps)
	}

	// Empty collections must be [] rather than null: a consumer that iterates
	// them should not have to special-case "no misses" into a nil check.
	if !strings.Contains(stdout, `"misses": []`) {
		t.Errorf("misses is not an empty array:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"integrity_problems": []`) {
		t.Errorf("integrity_problems is not an empty array:\n%s", stdout)
	}
}

// An empty store has no coverage and no source, and reporting either as a zero
// value would put 0001-01-01 into a monitoring dashboard.
func TestStatusJSONReportsAbsentCoverageAsNull(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	db.Close()

	code, stdout, _ := run(t, "status", "-store", path, "-json")

	if code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stdout, `"coverage": null`) {
		t.Errorf("coverage is not null on a store that has collected nothing:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"source": null`) {
		t.Errorf("source is not null on a store that was never bound:\n%s", stdout)
	}
	if strings.Contains(stdout, "0001-01-01") {
		t.Errorf("a zero timestamp reached the output:\n%s", stdout)
	}
}
