package mysql

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"

	"github.com/learttytyri/mulligan/internal/binlog"
	"github.com/learttytyri/mulligan/internal/change"
)

// Config is what a collector needs to follow a server.
type Config struct {
	// DSN is user:password@tcp(host:port)/. Its password never appears in any
	// message this package produces.
	DSN string

	// ServerID must be unique across the replication topology. A collision
	// disconnects whichever replica claimed it first, which is why there is no
	// default.
	ServerID uint32

	// Decode is the value-decoding contract, shared with the file reader so the
	// two cannot disagree about what a column meant.
	Decode binlog.DecodeOptions

	// HeartbeatPeriod is how often an idle server should confirm it is still
	// there. Without it a quiet database is indistinguishable from a stopped
	// collector, and the store's staleness refusal would fire on a healthy system.
	HeartbeatPeriod time.Duration

	// ReconnectBackoff is how long to wait before re-attaching after the
	// connection drops.
	ReconnectBackoff time.Duration
}

// DefaultHeartbeatPeriod keeps coverage moving on an idle server well inside the
// store's default staleness allowance.
const DefaultHeartbeatPeriod = 30 * time.Second

// DefaultReconnectBackoff is long enough not to hammer a server that is down and
// short enough that a brief blip costs little.
const DefaultReconnectBackoff = 2 * time.Second

// Info is what the server reported about itself.
type Info struct {
	Flavor Flavor

	// ServerIdentity distinguishes this server from any other. A store is bound to
	// it, so following a different server is refused rather than silently
	// appending a second history.
	ServerIdentity string

	// GTIDDialect is empty when the server issues no GTIDs.
	GTIDDialect string

	// StatementCapture reports whether the log will say which statement caused a
	// set of rows. Never required.
	StatementCapture bool

	// DecodeFingerprint identifies the decoding contract rows will be captured
	// under.
	DecodeFingerprint string
}

// Sink is where a collector puts what it reads.
//
// It is an interface so this package does not depend on the store: a sink only
// has to accept normalized changes and record what could not be recorded.
type Sink interface {
	AppendTransaction(txn change.Transaction, cp change.Checkpoint) error
	RecordGap(from, to time.Time, reason string) error
	RecordMiss(at time.Time, reason string) error
	AdvanceCoverage(to time.Time) error
	Checkpoint() (change.Checkpoint, error)
}

// Source follows one server.
type Source struct {
	cfg  Config
	dsn  dsn
	info Info
	log  *slog.Logger

	// startFresh ignores the stored resume point on the next attempt, set when the
	// server has told us it can no longer supply it.
	startFresh bool
}

// New prepares a source. It does not connect.
func New(cfg Config, log *slog.Logger) (*Source, error) {
	if cfg.ServerID == 0 {
		return nil, fmt.Errorf("mysql: a server id is required; it must be unique in the replication topology, " +
			"because a collision disconnects whichever replica claimed it first")
	}

	parsed, err := parseDSN(cfg.DSN)
	if err != nil {
		return nil, err
	}
	if cfg.HeartbeatPeriod == 0 {
		cfg.HeartbeatPeriod = DefaultHeartbeatPeriod
	}
	if cfg.ReconnectBackoff == 0 {
		cfg.ReconnectBackoff = DefaultReconnectBackoff
	}
	if log == nil {
		log = slog.Default()
	}

	return &Source{cfg: cfg, dsn: parsed, log: log}, nil
}

// Preflight asks the server about itself and refuses a configuration that cannot
// produce reversible changes.
//
// It runs before any streaming. Finding a wrong setting partway through a week of
// collection is too late: the changes logged under it are already unrecoverable,
// and once the binlogs rotate the store is the only record.
func (s *Source) Preflight(ctx context.Context) (Info, error) {
	conn, err := client.Connect(s.dsn.addr, s.dsn.user, s.dsn.password, "")
	if err != nil {
		return Info{}, fmt.Errorf("mysql: connecting to %s: %w", Redact(s.cfg.DSN), err)
	}
	defer conn.Close()

	version, err := scalar(conn, "SELECT VERSION()")
	if err != nil {
		return Info{}, err
	}
	flavor := flavorFromVersion(version)

	vars, err := serverVariables(conn)
	if err != nil {
		return Info{}, err
	}
	if err := checkSettings(vars); err != nil {
		return Info{}, err
	}

	identity, dialect, err := serverIdentity(conn, flavor, vars)
	if err != nil {
		return Info{}, err
	}

	s.info = Info{
		Flavor:            flavor,
		ServerIdentity:    identity,
		GTIDDialect:       dialect,
		StatementCapture:  statementCapture(flavor, vars),
		DecodeFingerprint: s.cfg.Decode.Fingerprint(),
	}
	return s.info, nil
}

func scalar(conn *client.Conn, query string) (string, error) {
	r, err := conn.Execute(query)
	if err != nil {
		return "", fmt.Errorf("mysql: %s: %w", query, err)
	}
	if len(r.Values) == 0 || len(r.Values[0]) == 0 {
		return "", fmt.Errorf("mysql: %s returned nothing", query)
	}
	return string(r.Values[0][0].AsString()), nil
}

func serverVariables(conn *client.Conn) (map[string]string, error) {
	r, err := conn.Execute("SHOW GLOBAL VARIABLES")
	if err != nil {
		return nil, fmt.Errorf("mysql: reading server variables: %w", err)
	}

	vars := make(map[string]string, len(r.Values))
	for _, row := range r.Values {
		if len(row) < 2 {
			continue
		}
		vars[string(row[0].AsString())] = string(row[1].AsString())
	}
	return vars, nil
}

// serverIdentity finds something that names this server and no other.
//
// MySQL has server_uuid. MariaDB has no equivalent, so its domain and server ids
// stand in — together they are what its GTIDs are built from, which is the same
// thing that would make a position from another server meaningless here.
func serverIdentity(conn *client.Conn, flavor Flavor, vars map[string]string) (identity, dialect string, err error) {
	if flavor == FlavorMariaDB {
		domain := vars["gtid_domain_id"]
		server := vars["server_id"]
		if server == "" {
			return "", "", fmt.Errorf("mysql: the server does not report server_id, so this store cannot be bound to it")
		}
		return "mariadb:" + domain + ":" + server, "mariadb", nil
	}

	uuid := vars["server_uuid"]
	if uuid == "" {
		return "", "", fmt.Errorf("mysql: the server does not report server_uuid, so this store cannot be bound to it")
	}

	dialect = ""
	if mode := vars["gtid_mode"]; mode != "" && !strings.EqualFold(mode, "OFF") && !strings.EqualFold(mode, "OFF_PERMISSIVE") {
		dialect = "mysql"
	}
	return "mysql:" + uuid, dialect, nil
}

// currentPosition asks where the server is writing right now.
//
// A collector with no stored checkpoint starts here rather than at the beginning
// of the oldest retained log. Starting at the beginning would replay whatever
// history the server still holds — on a fresh container, the whole of its own
// initialization — and every one of those changes would fall outside the coverage
// this collector opens, so they could never be answered for anyway.
//
// The statement was renamed twice. MySQL 8.0 knows only the oldest spelling,
// MySQL 8.4 the middle one, MariaDB the newest.
func (s *Source) currentPosition() (mysql.Position, error) {
	conn, err := client.Connect(s.dsn.addr, s.dsn.user, s.dsn.password, "")
	if err != nil {
		return mysql.Position{}, fmt.Errorf("mysql: connecting to %s: %w", Redact(s.cfg.DSN), err)
	}
	defer conn.Close()

	var lastErr error
	for _, stmt := range []string{"SHOW BINLOG STATUS", "SHOW BINARY LOG STATUS", "SHOW MASTER STATUS"} {
		r, err := conn.Execute(stmt)
		if err != nil {
			lastErr = err
			continue
		}
		if len(r.Values) == 0 || len(r.Values[0]) < 2 {
			return mysql.Position{}, fmt.Errorf("mysql: %s reported no position; is binary logging on?", stmt)
		}
		return mysql.Position{
			Name: string(r.Values[0][0].AsString()),
			Pos:  uint32(r.Values[0][1].AsInt64()),
		}, nil
	}
	return mysql.Position{}, fmt.Errorf("mysql: could not read the current binlog position: %w", lastErr)
}

// Run follows the server until ctx is cancelled.
//
// Reconnection is handled here rather than by the library. Its own retry is
// silent and unbounded, and resumes from a position that can lag the transaction
// in flight — which would re-deliver events into a half-assembled transaction
// with nothing to notice the duplication. Owning the loop means every disconnect
// discards the partial transaction, which was never committed, and resumes from
// the last position the sink durably recorded.
func (s *Source) Run(ctx context.Context, sink Sink) error {
	for {
		err := s.stream(ctx, sink)
		switch {
		case err == nil, errors.Is(err, context.Canceled):
			return nil
		case ctx.Err() != nil:
			return nil
		}

		// A refusal is a statement about the data, not about the connection.
		// Retrying would loop forever on something a human has to decide about.
		var refusal *Refusal
		if errors.As(err, &refusal) {
			return err
		}

		// The stored position is gone from the server. Retrying it would loop
		// forever on a position that will never come back, so the hole is recorded
		// and collection resumes from wherever the server is now. Without this the
		// collector never returns, and the store quietly stops at the last thing it
		// saw while still claiming coverage up to that point.
		var lost *unresumable
		if errors.As(err, &lost) {
			until := time.Now().UTC()
			if gapErr := sink.RecordGap(lost.from, until,
				"the collector was down and the source purged the logs covering that period"); gapErr != nil {
				return gapErr
			}
			s.log.Warn("the source can no longer supply the stored resume point; "+
				"the period since it was recorded is a gap and cannot be reverted",
				slog.Time("from", lost.from),
				slog.Time("until", until),
				slog.Any("error", err))
			s.startFresh = true
			continue
		}

		s.log.Warn("replication connection lost, reattaching",
			slog.String("source", Redact(s.cfg.DSN)),
			slog.Duration("after", s.cfg.ReconnectBackoff),
			slog.Any("error", err))

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(s.cfg.ReconnectBackoff):
		}
	}
}

// unresumable is the server saying it can no longer supply the position the store
// last recorded — the binlogs holding it have been purged.
//
// Whatever happened between that position and now is gone from the source and was
// never stored, so nothing can reconstruct it. The only correct response is to
// record the hole and carry on from wherever the server actually is.
type unresumable struct {
	from time.Time
	err  error
}

func (u *unresumable) Error() string { return u.err.Error() }
func (u *unresumable) Unwrap() error { return u.err }

// isUnresumable reports whether err is the server refusing a resume point.
//
// Matched on the code rather than the message, which is assembled from a nested
// error and differs between the two flavors and across versions.
func isUnresumable(err error) bool {
	var mye *mysql.MyError
	if errors.As(err, &mye) {
		return mye.Code == mysql.ER_MASTER_FATAL_ERROR_READING_BINLOG
	}
	return false
}

// Refusal is an error the collector will not retry: the server is reachable and
// something about what it is sending cannot be stored faithfully.
type Refusal struct{ err error }

func (r *Refusal) Error() string { return r.err.Error() }
func (r *Refusal) Unwrap() error { return r.err }

// stream attaches once and reads until the connection fails or ctx ends.
func (s *Source) stream(ctx context.Context, sink Sink) error {
	cp, err := sink.Checkpoint()
	if err != nil {
		return err
	}
	if s.startFresh {
		cp = change.Checkpoint{}
		s.startFresh = false
	}

	cfg := replication.BinlogSyncerConfig{
		ServerID: s.cfg.ServerID,
		Flavor:   string(s.info.Flavor),
		Host:     hostOf(s.dsn.addr),
		Port:     portOf(s.dsn.addr),
		User:     s.dsn.user,
		Password: s.dsn.password,

		// Own the retry. See Run.
		DisableRetrySync: true,

		// Keep coverage moving while the database is idle.
		HeartbeatPeriod: s.cfg.HeartbeatPeriod,

		// MariaDB records zero as the position of events inside a transaction, and
		// this is also what makes it send the annotations that name the statement.
		FillZeroLogPos: s.info.Flavor == FlavorMariaDB,

		// The library logs its own config; give it one that cannot print a password.
		Logger: s.log,
	}
	s.cfg.Decode.ApplyToSyncer(&cfg)

	syncer := replication.NewBinlogSyncer(cfg)
	defer syncer.Close()

	streamer, startedAt, err := s.attach(syncer, cp)
	if err != nil {
		if !cp.IsZero() && isUnresumable(err) {
			return &unresumable{from: cp.UpdatedAt, err: err}
		}
		return err
	}

	a := &assembler{logFile: startedAt.Name, flavor: s.info.Flavor}
	resumed := !cp.IsZero()
	var pendingGap *change.Checkpoint
	if resumed {
		saved := cp
		pendingGap = &saved
	}

	for {
		e, err := streamer.GetEvent(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if !cp.IsZero() && isUnresumable(err) {
				// The refusal usually arrives here rather than from the attach: the
				// library starts reading in the background and surfaces the server's
				// complaint on the first read.
				return &unresumable{from: cp.UpdatedAt, err: err}
			}
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("mysql: the server closed the replication stream")
			}
			return fmt.Errorf("mysql: reading the replication stream: %w", err)
		}

		// The first event after a resume tells us whether anything was missed while
		// we were away. Both bounds are known exactly here, where "the oldest event
		// the server still holds" is not.
		if pendingGap != nil {
			when := time.Unix(int64(e.Header.Timestamp), 0).UTC()
			if when.After(pendingGap.UpdatedAt.Add(time.Second)) {
				if err := sink.RecordGap(pendingGap.UpdatedAt, when,
					"the collector was not running, and the source could not resume from where it left off"); err != nil {
					return err
				}
			}
			pendingGap = nil
		}

		res, err := a.handle(e)
		if err != nil {
			return &Refusal{err: err}
		}

		if res.miss != nil {
			if err := sink.RecordMiss(res.miss.at, res.miss.reason); err != nil {
				return err
			}
		}

		if res.txn != nil {
			next := change.Checkpoint{
				LogFile:   a.logFile,
				LogPos:    e.Header.LogPos,
				GTID:      gsetOf(e),
				UpdatedAt: res.txn.CommittedAt,
			}
			if err := sink.AppendTransaction(*res.txn, next); err != nil {
				return err
			}
			continue
		}

		if !res.advance.IsZero() {
			if err := sink.AdvanceCoverage(res.advance); err != nil {
				return err
			}
		}
	}
}

// attach starts the stream from where the sink says we left off, preferring a
// GTID because a position is only meaningful on the server that issued it.
func (s *Source) attach(syncer *replication.BinlogSyncer, cp change.Checkpoint) (*replication.BinlogStreamer, mysql.Position, error) {
	if cp.GTID != "" && s.info.GTIDDialect != "" {
		set, err := mysql.ParseGTIDSet(s.info.GTIDDialect, cp.GTID)
		if err != nil {
			return nil, mysql.Position{}, fmt.Errorf("mysql: the stored resume point %q is not a %s GTID set: %w",
				cp.GTID, s.info.GTIDDialect, err)
		}
		streamer, err := syncer.StartSyncGTID(set)
		if err != nil {
			return nil, mysql.Position{}, fmt.Errorf("mysql: resuming from the stored GTID set: %w", err)
		}
		return streamer, mysql.Position{Name: cp.LogFile, Pos: cp.LogPos}, nil
	}

	pos := mysql.Position{Name: cp.LogFile, Pos: cp.LogPos}
	if cp.IsZero() {
		// Nothing collected yet: begin where the server is writing now, matching the
		// coverage this collector is about to open.
		current, err := s.currentPosition()
		if err != nil {
			return nil, mysql.Position{}, err
		}
		pos = current
	}

	streamer, err := syncer.StartSync(pos)
	if err != nil {
		return nil, mysql.Position{}, fmt.Errorf("mysql: resuming from %s:%d: %w", pos.Name, pos.Pos, err)
	}
	return streamer, pos, nil
}

// gsetOf reads the executed GTID set the library stamps onto a commit event.
//
// A single transaction's GTID is not enough to resume from: StartSyncGTID takes
// the whole executed set. When the commit arrived inside a compressed payload the
// outer event carries none, and the checkpoint falls back to the file position —
// which is sound here because the store is bound to the server that issued it.
func gsetOf(e *replication.BinlogEvent) string {
	switch ev := e.Event.(type) {
	case *replication.XIDEvent:
		if ev.GSet != nil {
			return ev.GSet.String()
		}
	case *replication.QueryEvent:
		if ev.GSet != nil {
			return ev.GSet.String()
		}
	}
	return ""
}

func hostOf(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i]
		}
	}
	return addr
}

func portOf(addr string) uint16 {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			var p uint16
			for _, c := range addr[i+1:] {
				if c < '0' || c > '9' {
					return 3306
				}
				p = p*10 + uint16(c-'0')
			}
			if p == 0 {
				return 3306
			}
			return p
		}
	}
	return 3306
}
