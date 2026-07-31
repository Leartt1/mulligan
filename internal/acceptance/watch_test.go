package acceptance

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/learttytyri/mulligan/internal/binlog"
	"github.com/learttytyri/mulligan/internal/change"
	"github.com/learttytyri/mulligan/internal/cli"
	"github.com/learttytyri/mulligan/internal/reverse"
	sourcemysql "github.com/learttytyri/mulligan/internal/source/mysql"
	"github.com/learttytyri/mulligan/internal/store"
)

// collector is a running watch, driven directly rather than through the command
// so the test controls when it stops.
type collector struct {
	db   *store.Store
	stop func()
	done chan error
}

// startCollector opens a store, binds it to the server, and follows it.
func startCollector(t *testing.T, s *mysqlServer, storePath string) *collector {
	t.Helper()
	return startCollectorWithLog(t, s, storePath, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// startCollectorLogging is startCollector with the collector's own log visible,
// for tests about what it does when something goes wrong.
func startCollectorLogging(t *testing.T, s *mysqlServer, storePath string) *collector {
	t.Helper()
	return startCollectorWithLog(t, s, storePath, slog.New(slog.NewTextHandler(testWriter{t}, nil)))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("collector: %s", string(p))
	return len(p), nil
}

func startCollectorWithLog(t *testing.T, s *mysqlServer, storePath string, log *slog.Logger) *collector {
	t.Helper()

	src, err := sourcemysql.New(sourcemysql.Config{
		DSN:      s.dsn(),
		ServerID: 4001,
		Decode:   binlog.DefaultDecodeOptions(),
		// Short enough that an idle server keeps coverage moving within a test.
		HeartbeatPeriod:  time.Second,
		ReconnectBackoff: 200 * time.Millisecond,
	}, log)
	if err != nil {
		t.Fatalf("preparing the collector: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	info, err := src.Preflight(ctx)
	if err != nil {
		cancel()
		t.Fatalf("preflight: %v", err)
	}

	db, err := store.Open(storePath)
	if err != nil {
		cancel()
		t.Fatalf("opening the store: %v", err)
	}
	if err := db.Bind(store.Binding{
		Flavor:            string(info.Flavor),
		ServerIdentity:    info.ServerIdentity,
		GTIDDialect:       info.GTIDDialect,
		DecodeFingerprint: info.DecodeFingerprint,
	}); err != nil {
		cancel()
		t.Fatalf("binding the store: %v", err)
	}
	if err := db.OpenCoverage(time.Now().UTC()); err != nil {
		cancel()
		t.Fatalf("opening coverage: %v", err)
	}

	c := &collector{db: db, done: make(chan error, 1)}
	go func() { c.done <- src.Run(ctx, db) }()

	var once sync.Once
	c.stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-c.done:
			case <-time.After(10 * time.Second):
				t.Error("the collector did not stop when asked")
			}
			db.Close()
		})
	}
	t.Cleanup(c.stop)
	return c
}

// waitForEvents blocks until the store holds at least n changes.
func (c *collector) waitForEvents(t *testing.T, n int) []change.Event {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		got, err := c.db.Events(change.Filter{}, time.Now().UTC())
		if err == nil && len(got) >= n {
			return got
		}
		select {
		case err := <-c.done:
			t.Fatalf("the collector stopped early: %v", err)
		case <-time.After(200 * time.Millisecond):
		}
	}

	got, err := c.db.Events(change.Filter{}, time.Now().UTC())
	t.Fatalf("the store held %d changes after 20s, want %d (last read error: %v)", len(got), n, err)
	return nil
}

// This is the v0.2 promise: a collector following a live server captures enough
// to undo an accident, without anyone having gone looking for a binlog file.
func TestWatchCapturesChangesThatCanBeReverted(t *testing.T) {
	s := startMySQL(t)

	s.exec("CREATE DATABASE shop")
	s.exec(schema)
	s.exec(seed)

	c := startCollector(t, s, filepath.Join(t.TempDir(), "mulligan.db"))

	before := s.query(snapshot)

	// The accident, committed while the collector is attached.
	s.exec("UPDATE shop.orders SET status = 'shipped'")

	events := c.waitForEvents(t, 2)
	if len(events) != 2 {
		t.Fatalf("the store holds %d changes, want 2", len(events))
	}

	plan, err := reverse.Plan(events)
	if err != nil {
		t.Fatalf("generating the reversal: %v", err)
	}
	var script strings.Builder
	if err := cli.Render(&script, "mulligan.db", change.Filter{}, nil, plan); err != nil {
		t.Fatalf("rendering: %v", err)
	}
	t.Logf("generated script:\n%s", script.String())
	s.applyScript(script.String())

	if restored := s.query(snapshot); !reflect.DeepEqual(restored, before) {
		t.Errorf("rows were not restored\n got: %q\nwant: %q", restored, before)
	}
}

// Every provenance line comes out of the store, so a change collected live has to
// arrive with a position and a timestamp just as a file-read one does.
func TestCollectedChangesCarryTheirProvenance(t *testing.T) {
	s := startMySQL(t)

	s.exec("CREATE DATABASE shop")
	s.exec(schema)
	c := startCollector(t, s, filepath.Join(t.TempDir(), "mulligan.db"))

	s.exec(seed)
	events := c.waitForEvents(t, 3)

	for _, ev := range events {
		if ev.LogFile == "" {
			t.Errorf("a collected change names no log file")
		}
		if ev.LogPos == 0 {
			t.Errorf("a collected change has no position, so a reviewer cannot find it in the log")
		}
		if ev.At.IsZero() {
			t.Errorf("a collected change has no timestamp")
		}
		if ev.ServerID == 0 {
			t.Errorf("a collected change names no server")
		}
	}
}

// The collector decodes TIMESTAMP into an instant and the engine renders it as
// UTC. Run the whole round trip somewhere other than UTC and a decoding contract
// that had drifted between the two paths would show up as a shifted value.
func TestWatchRestoresTemporalValuesFromANonUTCHost(t *testing.T) {
	t.Setenv("TZ", "America/New_York")
	time.Local = time.FixedZone("EST", -5*60*60)
	t.Cleanup(func() { time.Local = time.UTC })

	s := startMySQL(t)

	s.exec("CREATE DATABASE shop")
	s.exec(`CREATE TABLE shop.events (
		id INT PRIMARY KEY,
		when_ts TIMESTAMP(6) NOT NULL,
		when_dt DATETIME(6)  NOT NULL
	)`)
	s.exec(`INSERT INTO shop.events VALUES (1, '2026-07-27 13:45:05.123456', '2026-07-27 13:45:05.123456')`)

	c := startCollector(t, s, filepath.Join(t.TempDir(), "mulligan.db"))

	const snap = `SELECT id, when_ts, when_dt FROM shop.events ORDER BY id`
	before := s.query(snap)

	s.exec(`UPDATE shop.events SET when_ts = '2000-01-01 00:00:00', when_dt = '2000-01-01 00:00:00'`)

	events := c.waitForEvents(t, 1)
	plan, err := reverse.Plan(events)
	if err != nil {
		t.Fatalf("generating the reversal: %v", err)
	}
	var script strings.Builder
	if err := cli.Render(&script, "mulligan.db", change.Filter{}, nil, plan); err != nil {
		t.Fatalf("rendering: %v", err)
	}
	t.Logf("generated script:\n%s", script.String())
	s.applyScript(script.String())

	if restored := s.query(snap); !reflect.DeepEqual(restored, before) {
		t.Errorf("temporal values were not restored from a non-UTC host\n got: %q\nwant: %q", restored, before)
	}
}

// A store is bound to the server it was captured from. Pointed at another one it
// would resume on a position that is valid there and means something else,
// appending a second unrelated history into one order.
func TestAStoreRefusesToFollowADifferentServer(t *testing.T) {
	first := startMySQL(t)
	first.exec("CREATE DATABASE shop")
	first.exec(schema)

	path := filepath.Join(t.TempDir(), "mulligan.db")
	c := startCollector(t, first, path)
	first.exec(seed)
	c.waitForEvents(t, 3)
	c.stop()

	// A different server, with its own identity and its own unrelated positions.
	second := startServer(t, mysql8)

	src, err := sourcemysql.New(sourcemysql.Config{
		DSN:      second.dsn(),
		ServerID: 4002,
		Decode:   binlog.DefaultDecodeOptions(),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("preparing the collector: %v", err)
	}

	info, err := src.Preflight(context.Background())
	if err != nil {
		t.Fatalf("preflight against the second server: %v", err)
	}

	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopening the store: %v", err)
	}
	defer db.Close()

	err = db.Bind(store.Binding{
		Flavor:            string(info.Flavor),
		ServerIdentity:    info.ServerIdentity,
		GTIDDialect:       info.GTIDDialect,
		DecodeFingerprint: info.DecodeFingerprint,
	})
	if err == nil {
		t.Fatal("the store accepted a different server, so two histories would be interleaved")
	}
	if !strings.Contains(err.Error(), "server identity") {
		t.Errorf("error = %v, want it to say the server differs", err)
	}
}

// MariaDB is a supported flavor, and the streaming path diverges from MySQL's
// more than the file path does — its own GTID dialect, its own annotations, and
// positions it leaves at zero inside a transaction.
func TestWatchAgainstMariaDBCapturesRevertableChanges(t *testing.T) {
	s := startServer(t, mariadb)

	s.exec("CREATE DATABASE shop")
	s.exec(schema)
	s.exec(seed)

	c := startCollector(t, s, filepath.Join(t.TempDir(), "mulligan.db"))

	before := s.query(snapshot)
	s.exec("UPDATE shop.orders SET status = 'shipped'")

	events := c.waitForEvents(t, 2)

	// MariaDB zeroes the position of events inside a transaction. A collected
	// change with no position cannot be traced back to the log it came from.
	for _, ev := range events {
		if ev.LogPos == 0 {
			t.Errorf("a MariaDB change arrived with no position")
		}
	}

	plan, err := reverse.Plan(events)
	if err != nil {
		t.Fatalf("generating the reversal: %v", err)
	}
	var script strings.Builder
	if err := cli.Render(&script, "mulligan.db", change.Filter{}, nil, plan); err != nil {
		t.Fatalf("rendering: %v", err)
	}
	t.Logf("generated script:\n%s", script.String())
	s.applyScript(script.String())

	if restored := s.query(snapshot); !reflect.DeepEqual(restored, before) {
		t.Errorf("rows were not restored\n got: %q\nwant: %q", restored, before)
	}
}

// A collector that has stopped must not read as a quiet database. This is the
// staleness refusal end to end: the store holds real changes, the window is
// inside coverage, and the only thing wrong is that nothing is collecting.
func TestGenerateRefusesOnceTheCollectorHasStopped(t *testing.T) {
	s := startMySQL(t)

	s.exec("CREATE DATABASE shop")
	s.exec(schema)

	path := filepath.Join(t.TempDir(), "mulligan.db")
	c := startCollector(t, s, path)

	if err := c.db.SetMaxStaleness(2 * time.Second); err != nil {
		t.Fatalf("SetMaxStaleness: %v", err)
	}
	s.exec(seed)
	c.waitForEvents(t, 3)
	c.stop()

	// Long enough to exceed the allowance the collector recorded.
	time.Sleep(3 * time.Second)

	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopening the store: %v", err)
	}
	defer db.Close()

	_, err = db.Events(change.Filter{}, time.Now().UTC())
	if err == nil {
		t.Fatal("the store answered although nothing has collected for longer than the allowance")
	}
	if !strings.Contains(err.Error(), "stale") {
		t.Errorf("error = %v, want it to say the store is stale", err)
	}
}

// An idle database keeps coverage moving through heartbeats, so a healthy but
// quiet system does not trip the staleness refusal.
func TestAnIdleServerKeepsCoverageMoving(t *testing.T) {
	s := startMySQL(t)

	s.exec("CREATE DATABASE shop")
	s.exec(schema)

	c := startCollector(t, s, filepath.Join(t.TempDir(), "mulligan.db"))
	s.exec(seed)
	c.waitForEvents(t, 3)

	first, err := c.db.Coverage()
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}

	// Nothing is written during this window; only heartbeats arrive.
	time.Sleep(4 * time.Second)

	second, err := c.db.Coverage()
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	if !second.To.After(first.To) {
		t.Errorf("coverage did not move while the server was idle (%s then %s); "+
			"a quiet database would look like a stopped collector", first.To, second.To)
	}
}
