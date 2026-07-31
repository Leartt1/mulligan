package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/learttytyri/mulligan/internal/binlog"
	"github.com/learttytyri/mulligan/internal/change"
	sourcemysql "github.com/learttytyri/mulligan/internal/source/mysql"
	"github.com/learttytyri/mulligan/internal/store"
)

// dsnEnv is the documented way to supply credentials. A flag value is visible in
// ps output to every user on the host; an environment variable is not.
const dsnEnv = "MULLIGAN_DSN"

// pruneEvery is how often the collector discards changes past the retention
// window. Often enough that the store does not grow unbounded between restarts,
// rarely enough that it is not competing with ingest for the write lock.
const pruneEvery = time.Minute

func watch(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { usage(stderr) }

	var (
		storePath = fs.String("store", "", "window store to write (required)")
		serverID  = fs.Uint("server-id", 0, "replication server id, unique in the topology (required)")
		dsnFlag   = fs.String("dsn", "", "connection string; prefer "+dsnEnv)
		tables    = fs.String("tables", "", `comma-separated tables to collect, as "table" or "schema.table"; empty collects everything`)
		retain    = fs.String("retain", store.DefaultRetention.String(), "how far back to keep changes")
		stale     = fs.String("max-staleness", store.DefaultMaxStaleness.String(),
			"how far behind this collector may fall before generate refuses to answer")
	)

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	if *storePath == "" {
		fmt.Fprintln(stderr, "mulligan watch: -store is required")
		return exitUsage
	}
	if *serverID == 0 {
		fmt.Fprintln(stderr, "mulligan watch: -server-id is required, and must be unique in the replication topology; "+
			"a collision disconnects whichever replica claimed it first")
		return exitUsage
	}

	dsn := os.Getenv(dsnEnv)
	if *dsnFlag != "" {
		dsn = *dsnFlag
		fmt.Fprintf(stderr, "mulligan watch: -dsn puts the password in ps output for every user on this host; "+
			"prefer %s\n", dsnEnv)
	}
	if dsn == "" {
		fmt.Fprintf(stderr, "mulligan watch: no connection string; set %s to user:password@tcp(host:3306)/\n", dsnEnv)
		return exitUsage
	}

	retention, err := time.ParseDuration(*retain)
	if err != nil {
		fmt.Fprintf(stderr, "mulligan watch: -retain %q is not a duration; try 7d as 168h\n", *retain)
		return exitUsage
	}
	maxStaleness, err := time.ParseDuration(*stale)
	if err != nil {
		fmt.Fprintf(stderr, "mulligan watch: -max-staleness %q is not a duration; try 5m\n", *stale)
		return exitUsage
	}

	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	collect := change.Filter{Tables: splitList(*tables)}

	src, err := sourcemysql.New(sourcemysql.Config{
		DSN:      dsn,
		ServerID: uint32(*serverID),
		Decode:   binlog.DefaultDecodeOptions(),
		Collect:  collect,
	}, log)
	if err != nil {
		fmt.Fprintf(stderr, "mulligan watch: %v\n", err)
		return exitUsage
	}

	// Preflight before touching the store, so a misconfigured server is reported
	// without leaving a store behind that was never written to.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	info, err := src.Preflight(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "mulligan watch: %v\n", err)
		return exitFailure
	}

	db, err := store.Open(*storePath)
	if err != nil {
		fmt.Fprintf(stderr, "mulligan watch: %v\n", err)
		return exitFailure
	}
	defer db.Close()

	// One collector per store. Two writing to one interleave their transactions
	// into a single order that reflects neither source — a file that reads
	// perfectly well and produces a revert applying changes in an order that never
	// happened.
	if err := db.Claim(); err != nil {
		fmt.Fprintf(stderr, "mulligan watch: %v\n", err)
		return exitFailure
	}
	defer db.Release()

	if db.LockUnsupported() {
		log.Warn("this platform offers no advisory lock, so a second collector " +
			"following the same store cannot be detected; run only one")
	}

	// Binding refuses a store captured from a different server, a different
	// flavor, or under a different decoding contract. Resumed against another
	// server it would append a second, unrelated history into one order.
	if err := db.Bind(store.Binding{
		Flavor:            string(info.Flavor),
		ServerIdentity:    info.ServerIdentity,
		GTIDDialect:       info.GTIDDialect,
		DecodeFingerprint: info.DecodeFingerprint,
	}); err != nil {
		fmt.Fprintf(stderr, "mulligan watch: %v\n", err)
		return exitFailure
	}

	if err := db.SetRetention(retention); err != nil {
		fmt.Fprintf(stderr, "mulligan watch: %v\n", err)
		return exitFailure
	}
	if err := db.SetMaxStaleness(maxStaleness); err != nil {
		fmt.Fprintf(stderr, "mulligan watch: %v\n", err)
		return exitFailure
	}
	if err := db.OpenCoverage(time.Now().UTC()); err != nil {
		fmt.Fprintf(stderr, "mulligan watch: %v\n", err)
		return exitFailure
	}

	log.Info("following source",
		slog.String("source", sourcemysql.Redact(dsn)),
		slog.String("flavor", string(info.Flavor)),
		slog.String("store", *storePath),
		slog.Duration("retain", retention),
		slog.Any("tables", collect.Tables),
		slog.Bool("statement_capture", info.StatementCapture))

	if len(collect.Tables) == 0 {
		log.Warn("no -tables given, so every table on the server is collected, " +
			"including any holding personal data; the store is an unencrypted copy of them")
	}

	if !info.StatementCapture {
		log.Warn("the source does not log which statement caused a change, " +
			"so generated scripts will not say what to blame")
	}

	pruning := startPruning(ctx, db, log)
	defer pruning()

	if err := src.Run(ctx, db); err != nil {
		fmt.Fprintf(stderr, "mulligan watch: %v\n", err)
		return exitFailure
	}

	log.Info("stopped")
	return exitOK
}

// startPruning discards changes past the retention window on a timer and returns
// a function that stops it.
//
// It runs alongside ingest rather than at shutdown: a collector that is killed
// rather than asked to stop would otherwise never prune at all.
func startPruning(ctx context.Context, db *store.Store, log *slog.Logger) func() {
	ticker := time.NewTicker(pruneEvery)
	done := make(chan struct{})

	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				removed, err := db.Prune(time.Now().UTC())
				if err != nil {
					// A failed prune is not a reason to stop collecting: the window grows
					// larger than asked for, which is a lesser problem than a hole in it.
					log.Warn("could not discard changes past the retention window", slog.Any("error", err))
					continue
				}
				if removed > 0 {
					log.Info("discarded changes past the retention window", slog.Int("transactions", removed))
				}
			}
		}
	}()

	return func() {
		ticker.Stop()
		<-done
	}
}
