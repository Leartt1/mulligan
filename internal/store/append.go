package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/learttytyri/mulligan/internal/change"
)

// AppendTransaction records one source transaction and the position it advances
// the source to, in a single store transaction.
//
// The two commit together on purpose. A checkpoint that could move without the
// rows it covers would, after a crash between the two, resume past changes that
// were never stored — a hole in the window with nothing recording that it
// exists, which is the one failure the coverage model cannot describe after the
// fact.
//
// Re-delivery is expected rather than exceptional: the replication library
// reconnects on its own and can resume from a position that lags the transaction
// in flight. A transaction already stored is therefore accepted quietly and
// stored once. Storing it twice would produce its inverse twice — a script that
// aborts on a duplicate key part way through, or one that silently inserts a row
// again where the table has no primary key.
func (s *Store) AppendTransaction(txn change.Transaction, cp change.Checkpoint) error {
	// Encode before opening the store transaction. A value the codec cannot
	// represent must take the whole transaction down, and finding that out with a
	// write already in flight only makes the rollback harder to reason about.
	rows := make([]encodedRow, len(txn.Events))
	for i, ev := range txn.Events {
		enc, err := encodeEvent(ev)
		if err != nil {
			return fmt.Errorf("store: %s row %d of transaction %s: %w", ev.Table, i, txn.SourceID, err)
		}
		rows[i] = enc
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: beginning a write to %s: %w", s.path, err)
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		`INSERT INTO txn (source_txn_id, committed_at, server_id) VALUES (?, ?, ?)
		 ON CONFLICT (source_txn_id) DO NOTHING`,
		txn.SourceID, txn.CommittedAt.Unix(), txn.ServerID)
	if err != nil {
		return fmt.Errorf("store: recording transaction %s: %w", txn.SourceID, err)
	}

	inserted, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: recording transaction %s: %w", txn.SourceID, err)
	}

	// Zero means this transaction was already stored, so its rows are too and are
	// not written again. The checkpoint is still advanced below: this delivery
	// reached at least as far into the log as the one that stored it.
	if inserted > 0 {
		txnID, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: recording transaction %s: %w", txn.SourceID, err)
		}
		for i, r := range rows {
			if err := insertRow(tx, txnID, r); err != nil {
				return fmt.Errorf("store: %s row %d of transaction %s: %w", txn.Events[i].Table, i, txn.SourceID, err)
			}
		}
	}

	if err := writeCheckpoint(tx, cp); err != nil {
		return err
	}
	if err := advanceCoverageTo(tx, txn.CommittedAt); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: committing transaction %s: %w", txn.SourceID, err)
	}
	return nil
}

// encodedRow is one row change with its images already in store form.
type encodedRow struct {
	schema, table string
	op            change.Op
	logFile       string
	logPos        uint32
	query         sql.NullString
	columns       []byte
	before, after []byte
	hasBefore     bool
	hasAfter      bool
}

func encodeEvent(ev change.Event) (encodedRow, error) {
	r := encodedRow{
		schema:  ev.Schema,
		table:   ev.Table,
		op:      ev.Op,
		logFile: ev.LogFile,
		logPos:  ev.LogPos,
		query:   sql.NullString{String: ev.Query, Valid: ev.Query != ""},
	}

	var err error
	if r.columns, err = EncodeColumns(ev.Columns); err != nil {
		return r, err
	}
	if ev.Before != nil {
		if r.before, err = EncodeRow(ev.Before); err != nil {
			return r, err
		}
		r.hasBefore = true
	}
	if ev.After != nil {
		if r.after, err = EncodeRow(ev.After); err != nil {
			return r, err
		}
		r.hasAfter = true
	}
	return r, nil
}

func insertRow(tx *sql.Tx, txnID int64, r encodedRow) error {
	var before, after any
	if r.hasBefore {
		before = r.before
	}
	if r.hasAfter {
		after = r.after
	}

	_, err := tx.Exec(
		`INSERT INTO row_change
		   (txn_id, schema_name, table_name, op, log_file, log_pos, query, columns, before, after)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		txnID, r.schema, r.table, int(r.op), r.logFile, r.logPos, r.query, r.columns, before, after)
	return err
}

func writeCheckpoint(tx *sql.Tx, cp change.Checkpoint) error {
	_, err := tx.Exec(
		`INSERT INTO checkpoint (id, log_file, log_pos, gtid, updated_at) VALUES (1, ?, ?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET
		   log_file = excluded.log_file,
		   log_pos = excluded.log_pos,
		   gtid = excluded.gtid,
		   updated_at = excluded.updated_at`,
		nullable(cp.LogFile), cp.LogPos, nullable(cp.GTID), cp.UpdatedAt.Unix())
	if err != nil {
		return fmt.Errorf("store: writing the checkpoint: %w", err)
	}
	return nil
}

// advanceCoverageTo moves the high-water mark, never backwards. Out-of-order
// delivery after a reconnect would otherwise pull it back and make the window
// look shorter than what is actually stored.
func advanceCoverageTo(tx *sql.Tx, when time.Time) error {
	_, err := tx.Exec(
		`INSERT INTO meta (key, value) VALUES ('coverage_to', ?)
		 ON CONFLICT (key) DO UPDATE SET value = ?
		 WHERE CAST(excluded.value AS INTEGER) > CAST(meta.value AS INTEGER)`,
		when.Unix(), when.Unix())
	if err != nil {
		return fmt.Errorf("store: advancing coverage: %w", err)
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Checkpoint returns where the source was last consumed to, or the zero value if
// nothing has been consumed yet.
func (s *Store) Checkpoint() (change.Checkpoint, error) {
	var (
		cp      change.Checkpoint
		logFile sql.NullString
		gtid    sql.NullString
		logPos  sql.NullInt64
		updated int64
	)

	err := s.db.QueryRow(
		`SELECT log_file, log_pos, gtid, updated_at FROM checkpoint WHERE id = 1`,
	).Scan(&logFile, &logPos, &gtid, &updated)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return change.Checkpoint{}, nil
	case err != nil:
		return change.Checkpoint{}, fmt.Errorf("store: reading the checkpoint of %s: %w", s.path, err)
	}

	cp.LogFile = logFile.String
	cp.GTID = gtid.String
	cp.LogPos = uint32(logPos.Int64)
	cp.UpdatedAt = time.Unix(updated, 0).UTC()
	return cp, nil
}

// Events returns the stored changes matching f, in the order the source applied
// them, or refuses if the store cannot answer for the window.
//
// Coverage and the rows are read in one transaction on purpose. Read
// separately, a prune committing in between would truncate the front of the
// window after it had been judged sound, and the result would be
// indistinguishable from those changes never having existed.
//
// Order comes from the store's own row identity rather than from the commit
// timestamp: binlog timestamps have one-second resolution, so ordering by time
// would scramble transactions committed within the same second, and undoing a
// sequence in the wrong order restores a state that never existed.
func (s *Store) Events(f change.Filter, now time.Time) ([]change.Event, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: reading changes from %s: %w", s.path, err)
	}
	defer tx.Rollback()

	coverage, err := readCoverage(tx, s.path)
	if err != nil {
		return nil, err
	}
	gaps, err := readGaps(tx)
	if err != nil {
		return nil, err
	}
	misses, err := readMisses(tx)
	if err != nil {
		return nil, err
	}
	if err := checkCoverage(coverage, f, now, gaps, misses); err != nil {
		return nil, err
	}

	rows, err := tx.Query(
		`SELECT t.committed_at, t.server_id,
		        r.schema_name, r.table_name, r.op, r.log_file, r.log_pos, r.query,
		        r.columns, r.before, r.after
		   FROM row_change r
		   JOIN txn t ON t.id = r.txn_id
		  ORDER BY r.id`)
	if err != nil {
		return nil, fmt.Errorf("store: reading changes from %s: %w", s.path, err)
	}
	defer rows.Close()

	var out []change.Event
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		if f.Match(ev) {
			out = append(out, ev)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading changes from %s: %w", s.path, err)
	}
	return out, nil
}

func scanEvent(rows *sql.Rows) (change.Event, error) {
	var (
		ev            change.Event
		committedAt   int64
		op            int
		query         sql.NullString
		columns       []byte
		before, after []byte
	)

	if err := rows.Scan(&committedAt, &ev.ServerID,
		&ev.Schema, &ev.Table, &op, &ev.LogFile, &ev.LogPos, &query,
		&columns, &before, &after); err != nil {
		return ev, fmt.Errorf("store: reading a change: %w", err)
	}

	ev.At = time.Unix(committedAt, 0).UTC()
	ev.Op = change.Op(op)
	ev.Query = query.String

	var err error
	if ev.Columns, err = DecodeColumns(columns); err != nil {
		return ev, fmt.Errorf("store: %s.%s at %s:%d: %w", ev.Schema, ev.Table, ev.LogFile, ev.LogPos, err)
	}
	if before != nil {
		if ev.Before, err = DecodeRow(before); err != nil {
			return ev, fmt.Errorf("store: %s.%s at %s:%d before image: %w", ev.Schema, ev.Table, ev.LogFile, ev.LogPos, err)
		}
	}
	if after != nil {
		if ev.After, err = DecodeRow(after); err != nil {
			return ev, fmt.Errorf("store: %s.%s at %s:%d after image: %w", ev.Schema, ev.Table, ev.LogFile, ev.LogPos, err)
		}
	}
	return ev, nil
}

// EachEvent hands over the stored changes matching f, newest first, without
// collecting them.
//
// Newest first because that is the order a revert applies in, and getting it
// from the database rather than by reversing a slice is what keeps the whole
// matching window out of memory. A run reverting ten million rows is the case
// this tool exists for, so materializing the set fails exactly when it matters.
//
// Coverage is checked before anything is handed over. The refusals are the
// reason for reading through the store at all, and discovering one partway
// through — with output already written — would defeat them.
//
// Everything runs in one transaction, so a prune committing midway cannot
// truncate the window after it was judged sound.
func (s *Store) EachEvent(f change.Filter, now time.Time, visit func(change.Event) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: reading changes from %s: %w", s.path, err)
	}
	defer tx.Rollback()

	coverage, err := readCoverage(tx, s.path)
	if err != nil {
		return err
	}
	gaps, err := readGaps(tx)
	if err != nil {
		return err
	}
	misses, err := readMisses(tx)
	if err != nil {
		return err
	}
	if err := checkCoverage(coverage, f, now, gaps, misses); err != nil {
		return err
	}

	rows, err := tx.Query(
		`SELECT t.committed_at, t.server_id,
		        r.schema_name, r.table_name, r.op, r.log_file, r.log_pos, r.query,
		        r.columns, r.before, r.after
		   FROM row_change r
		   JOIN txn t ON t.id = r.txn_id
		  ORDER BY r.id DESC`)
	if err != nil {
		return fmt.Errorf("store: reading changes from %s: %w", s.path, err)
	}
	defer rows.Close()

	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return err
		}
		if !f.Match(ev) {
			continue
		}
		if err := visit(ev); err != nil {
			return err
		}
	}
	return rows.Err()
}
