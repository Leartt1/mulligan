package store

// schemaSQL is the store's layout. Every statement is IF NOT EXISTS so opening
// an existing store is the same code path as creating one.
//
// Two choices here carry weight beyond storage:
//
// txn.id, assigned by the store, is what orders a revert — not committed_at.
// Binlog timestamps have one-second resolution, so ordering by time would
// scramble transactions committed within the same second, and undoing a
// sequence in the wrong order restores a state that never existed.
//
// query, log_file and log_pos live on row_change rather than on txn. They
// describe a rows event, and one transaction routinely contains several
// statements; hanging them off the transaction would stamp every reversal in it
// with one statement's text and one position. That is the artifact a human reads
// before running something destructive, so it being confidently wrong is worse
// than it being absent.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS source (
	id              INTEGER PRIMARY KEY CHECK (id = 1),
	flavor          TEXT    NOT NULL,
	server_identity TEXT    NOT NULL,
	gtid_dialect    TEXT    NOT NULL,
	decode_options  TEXT    NOT NULL,
	codec_version   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS txn (
	id            INTEGER PRIMARY KEY,
	source_txn_id TEXT    NOT NULL UNIQUE,
	committed_at  INTEGER NOT NULL,
	server_id     INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS row_change (
	id          INTEGER PRIMARY KEY,
	txn_id      INTEGER NOT NULL REFERENCES txn(id) ON DELETE CASCADE,
	schema_name TEXT    NOT NULL,
	table_name  TEXT    NOT NULL,
	op          INTEGER NOT NULL,
	log_file    TEXT    NOT NULL,
	log_pos     INTEGER NOT NULL,
	query       TEXT,
	columns     BLOB    NOT NULL,
	before      BLOB,
	after       BLOB
);

CREATE TABLE IF NOT EXISTS gap (
	id      INTEGER PRIMARY KEY,
	from_at INTEGER NOT NULL,
	to_at   INTEGER NOT NULL,
	reason  TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS miss (
	id     INTEGER PRIMARY KEY,
	at     INTEGER NOT NULL,
	reason TEXT    NOT NULL,
	txn_id INTEGER
);

CREATE TABLE IF NOT EXISTS checkpoint (
	id         INTEGER PRIMARY KEY CHECK (id = 1),
	log_file   TEXT,
	log_pos    INTEGER,
	gtid       TEXT,
	updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS row_change_table ON row_change(schema_name, table_name, id);
CREATE INDEX IF NOT EXISTS txn_committed_at ON txn(committed_at);
`
