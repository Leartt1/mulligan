package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/learttytyri/mulligan/internal/api"
	"github.com/learttytyri/mulligan/internal/store"
)

// tokenEnv supplies the bearer token a non-loopback listener requires. An
// environment variable rather than a flag: a flag value is visible in ps output
// to every user on the host.
const tokenEnv = "MULLIGAN_TOKEN"

// defaultListen binds loopback only. The store holds full row images, so a
// default reachable from the network would publish production data the first
// time someone started this without reading anything.
const defaultListen = "127.0.0.1:8080"

// serveShutdownGrace is how long in-flight requests have to finish after a
// signal. A revert script for a large window can take a while to stream, and
// cutting one off mid-body leaves whoever is downloading it with a file that
// looks like a script.
const serveShutdownGrace = 30 * time.Second

func serve(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { usage(stderr) }

	var (
		storePath = fs.String("store", "", "window store to serve (required)")
		listen    = fs.String("listen", defaultListen, "address to listen on")
	)

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	token := os.Getenv(tokenEnv)
	if err := checkServe(*storePath, *listen, token); err != nil {
		fmt.Fprintf(stderr, "mulligan serve: %v\n", err)
		return exitUsage
	}

	db, err := store.Open(*storePath)
	if err != nil {
		fmt.Fprintf(stderr, "mulligan serve: %v\n", err)
		return exitFailure
	}
	defer db.Close()

	// No Claim: serving is reading, and refusing to start because a collector owns
	// the store would be backwards — the live store is the one worth serving.
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	var handler http.Handler = api.New(db, *storePath, func() time.Time { return time.Now().UTC() })
	if token != "" {
		handler = api.RequireToken(token, handler)
	}
	handler = logRequests(log, handler)

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintf(stderr, "mulligan serve: cannot listen on %q: %v\n", *listen, err)
		return exitFailure
	}

	srv := &http.Server{
		Handler: handler,
		// A revert script streams for as long as the window is large, so no write
		// deadline: cutting one off would deliver a truncated file. The header and
		// idle timeouts still bound what an unattended listener will hold open.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	fmt.Fprintf(stdout, "mulligan serving %s on http://%s\n", *storePath, ln.Addr())
	if token == "" {
		fmt.Fprintf(stdout, "no token required: this listener is loopback-only\n")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errs := make(chan error, 1)
	go func() { errs <- srv.Serve(ln) }()

	select {
	case err := <-errs:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "mulligan serve: %v\n", err)
			return exitFailure
		}
	case <-ctx.Done():
		log.Info("shutting down", "grace", serveShutdownGrace)

		shutdownCtx, cancel := context.WithTimeout(context.Background(), serveShutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(stderr, "mulligan serve: %v\n", err)
			return exitFailure
		}
	}
	return exitOK
}

// checkServe applies every refusal that has to happen before anything binds.
//
// Separate from serve so it can be tested without a listener. Left inline, a
// test for "this must be refused" could only assert the refusal by watching a
// server fail to start — and if the refusal were ever removed, that test would
// hang rather than fail, which is the least useful thing a test can do.
func checkServe(storePath, listen, token string) error {
	if storePath == "" {
		return fmt.Errorf("-store is required")
	}

	// Opening a store creates it. A mistyped path would otherwise be served as a
	// store whose collector has never run — and unlike a one-shot command, this
	// one stays up saying so.
	if _, err := os.Stat(storePath); err != nil {
		return fmt.Errorf("no store at %s", storePath)
	}

	// Binding somewhere reachable is a decision, not a typo in a flag. What this
	// serves is an unencrypted partial copy of production rows, so a listener off
	// loopback has to carry a secret or not exist.
	local, err := isLoopback(listen)
	if err != nil {
		return fmt.Errorf("-listen %q: %w", listen, err)
	}
	if !local && token == "" {
		return fmt.Errorf("-listen %s reaches beyond this host, and the store is an "+
			"unencrypted partial copy of your rows; set %s to require a bearer token, "+
			"or bind %s and reach it through an SSH tunnel", listen, tokenEnv, defaultListen)
	}
	return nil
}

// isLoopback reports whether addr binds only to this host.
//
// An empty host — ":8080" — binds every interface, which is the form most likely
// to be typed without meaning it.
func isLoopback(addr string) (bool, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false, fmt.Errorf("not an address with a port: %w", err)
	}
	if host == "" {
		return false, nil
	}
	if host == "localhost" {
		return true, nil
	}

	ip := net.ParseIP(host)
	if ip == nil {
		// A name that is not localhost has to be resolved to be judged, and a name
		// that resolves differently later would change what this decided. Treat it
		// as reachable: the cost of being wrong that way is a token requirement.
		return false, nil
	}
	return ip.IsLoopback(), nil
}

// logRequests writes one line per request.
//
// Query strings are included: they carry table names and time windows, which is
// what makes the line useful, and no route accepts a credential in one.
func logRequests(log *slog.Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		h.ServeHTTP(rec, r)

		log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"query", r.URL.RawQuery,
			"status", rec.status,
			"duration", time.Since(started).Round(time.Millisecond))
	})
}

// statusRecorder remembers the status code so it can be logged.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}
