# Mulligan v0.2 — live tailing into a window store

Status: approved, not yet implemented.
Supersedes nothing. Extends the v0.1 engine shipped on `main`.

## 1. Goal

Keep a rolling, queryable record of recent row changes so a reversal can be
generated without hunting down binlog files, and so the v0.3 console has an index
to render a timeline from.

The whole design serves one existing principle: **never emit a plausible-but-wrong
or silently-incomplete revert.** v0.1 upheld it by refusing at generation time. A
long-running collector moves the failure surface earlier — changes can now be
missed while nobody is looking — so most of what follows is about making that
detectable rather than silent.

## 2. Decisions

| Decision | Choice | Why |
|---|---|---|
| Store | SQLite via pure-Go `modernc.org/sqlite` | The query the console needs — by time, table, statement — is an index. Pure Go keeps the single static binary. |
| Seam | The store. `watch` writes, `generate` reads | `reverse` needs no changes for live tailing, which is the test that v0.1's boundaries were drawn correctly. |
| Retention | Time-based, default 7d | Matches how operators already think about binlog retention. |
| Past the edge | Refuse by name | An empty result reading as "nothing happened" is the most misleading answer this tool can give. |
| Resume | GTID when available, else file+pos | Works on both GTID and non-GTID deployments. |
| Statement capture | Opportunistic, never required | The log carries it only under a fourth setting; the tool must work without it. |

`generate` keeps reading binlog files directly. File mode needs no daemon and no
store, and someone handed a binlog on a USB stick is a real case.

## 3. Components

```
internal/source/mysql   replica connection -> change.Transaction      (new)
internal/store          SQLite: append, query, retention, coverage    (new)
internal/change         Event gains Query; new Transaction            (extend)
internal/binlog         file scanning; DecodeOptions exported         (extend)
internal/reverse        inverse SQL                                   (unchanged)
internal/cli            watch + generate                              (extend)
```

`source/mysql` knows MySQL and not SQLite. `store` knows SQLite and not MySQL.
Neither knows about `reverse`. `change` is the seam, exactly as it is for the file
path today.

## 4. Store schema

```sql
CREATE TABLE source (
  id              INTEGER PRIMARY KEY CHECK (id = 1),
  flavor          TEXT NOT NULL,        -- "mysql" | "mariadb"
  server_identity TEXT NOT NULL,        -- @@server_uuid, or gtid_domain_id:server_id
  gtid_dialect    TEXT,                 -- "mysql" | "mariadb" | NULL when not using GTID
  decode_options  TEXT NOT NULL,        -- fingerprint of binlog.DecodeOptions
  codec_version   INTEGER NOT NULL,
  schema_version  INTEGER NOT NULL
);

CREATE TABLE txn (
  id            INTEGER PRIMARY KEY,    -- store-assigned, defines revert order
  source_txn_id TEXT NOT NULL UNIQUE,   -- GTID, else the COMMIT event's log_file:log_pos
  committed_at  INTEGER NOT NULL,       -- unix seconds, UTC
  server_id     INTEGER NOT NULL
);

CREATE TABLE row_change (
  id          INTEGER PRIMARY KEY,      -- orders rows within a transaction
  txn_id      INTEGER NOT NULL REFERENCES txn(id) ON DELETE CASCADE,
  schema_name TEXT NOT NULL,
  table_name  TEXT NOT NULL,
  op          INTEGER NOT NULL,
  log_file    TEXT NOT NULL,
  log_pos     INTEGER NOT NULL,
  query       TEXT,                     -- statement text when the server logged it
  columns     BLOB NOT NULL,
  before      BLOB,
  after       BLOB
);

CREATE TABLE gap  (id INTEGER PRIMARY KEY, from_at INTEGER NOT NULL, to_at INTEGER NOT NULL, reason TEXT NOT NULL);
CREATE TABLE miss (id INTEGER PRIMARY KEY, at INTEGER NOT NULL, reason TEXT NOT NULL, txn_id INTEGER);
CREATE TABLE checkpoint (id INTEGER PRIMARY KEY CHECK (id = 1), log_file TEXT, log_pos INTEGER, gtid TEXT, updated_at INTEGER NOT NULL);
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);  -- retention, coverage_from, coverage_to

CREATE INDEX row_change_table ON row_change(schema_name, table_name, id);
CREATE INDEX txn_committed_at ON txn(committed_at);
```

`txn.id` — not `committed_at` — defines order. Binlog timestamps have one-second
resolution, so ordering a revert by time would scramble transactions committed in
the same second.

`query`, `log_file` and `log_pos` live on `row_change`, not `txn`. They are
per-rows-event; a `BEGIN; UPDATE orders…; DELETE order_items…; COMMIT` would
otherwise stamp both reversals with one statement's text and one position — the
artifact a human reads before running a destructive script, confidently wrong. A
NULL `query` omits the line; it never inherits a neighbour's.

`source_txn_id` falls back to the **commit** event's position specifically, never a
row event's. MariaDB writes `LogPos = 0` on events inside a transaction and records
the real position only on the one that commits, so any other choice would collapse
every MariaDB transaction onto the same key.

`source` is written once at store creation and revalidated on every connect. A
mismatch refuses. Without it, a repoint, failover or restore-from-backup resumes
on a valid-looking position and appends a second server's history into the same
`txn.id` order — a revert built from before-images of two unrelated timelines,
with nothing visibly wrong.

Pragmas, all required: `journal_mode=WAL` (so `generate` reads while `watch`
writes — SQLite is single-writer), `busy_timeout` non-zero, `synchronous=NORMAL`,
`foreign_keys=ON` (off by default, so the `row_change -> txn` cascade would not
otherwise be enforced).

## 5. Coverage and refusal

The store records what it has and what it knows it lacks.

- `coverage_from` — earliest instant answerable. Set to the moment `watch` first
  ran; moves to the retention **edge** after pruning, deliberately not
  `MIN(committed_at)`, so a quiet weekend cannot masquerade as coverage.
- `coverage_to` — high-water mark, advanced on every commit **and** on heartbeat
  and rotate events, so an idle database is distinguishable from a stalled
  collector. `HeartbeatPeriod` is set on the syncer for exactly this.
- `gap` — a period known to be unrecorded, written when a restart cannot resume
  because the binlogs rotated away while `watch` was down. Its bounds are
  `checkpoint.updated_at` to the timestamp of the first event actually received
  after resuming. Both are known precisely; "the earliest event the server still
  holds" is not, and guessing it would be the kind of approximation this design
  exists to avoid.
- `miss` — a single change seen but not recordable, written in the same SQLite
  transaction as its neighbours. It is a point in time, not a range.

`generate` refuses, by name, on any of five conditions:

1. window starts before `coverage_from`
2. window intersects a `gap` range
3. window contains a `miss` instant
4. window extends past `coverage_to`
5. `now - coverage_to` exceeds `--max-staleness` (default 5m, stored in `meta` by
   `watch` so `generate` applies the collector's own setting rather than its own)

Condition 5 is the one that matters most and is the least obvious. Without it a
dead or wedged `watch` is indistinguishable from a quiet database: `generate`
passes every other check and prints `no matching changes found` with exit 0 — an
affirmative "nothing happened" during the incident, after which the binlogs rotate
and the store is the only record left.

All five are read inside one `BEGIN DEFERRED` together with the row scan.
Otherwise a prune committing between the coverage read and the scan truncates the
front of the window, indistinguishable in the output from those rows never having
existed.

Pruning deletes rows and advances `coverage_from` in a single transaction.

**Once binlogs rotate away the store is the only record. It is not a rebuildable
cache.** Everything above follows from that.

## 6. Value codec

Tagged binary in `internal/store`: one tag byte, then payload.

`NULL INT UINT FLOAT32 FLOAT64 BOOL STRING BYTES TIME RAW`

- Signed widths `int8`…`int64` collapse to `INT`: `literal` formats them all
  through `FormatInt`, so the rendered SQL is identical.
- Floats do **not** collapse. `literal` formats `float32` with `bitSize 32`;
  widening `float32(0.1)` renders `0.10000000149011612`.
- `time.Time` stores the instant (unix seconds + nanos). `literal` normalizes to
  UTC.
- `RAW` re-validates as numeric on decode, so a corrupt or hostile store cannot
  reintroduce the unquoted-value hole closed in v0.1.

The codec test asserts `literal(decode(encode(v))) == literal(v)` across every
type the acceptance suite proves. Comparing through `literal` means `decode`
cannot pass by agreeing with `encode`.

## 7. Watch loop

```
preflight   SELECT VERSION()            -> flavor; set FillZeroLogPos on MariaDB
            SHOW VARIABLES              -> refuse unless ROW / FULL / FULL
                                        -> refuse if binlog_row_value_options = PARTIAL_JSON
                                        -> record whether statement capture was requested
            validate source binding, or refuse

resume      checkpoint -> StartSyncGTID(gset) | StartSync(pos)
            unresumable -> write gap, resume from earliest available

per event   RowsQuery / AnnotateRows -> hold statement text for the rows that follow
            TableMap                 -> parser tracks
            Rows                     -> Convert() -> INSERT into the open SQLite txn
            TransactionPayload       -> unwrap, run .Events through the same handler
            XID / COMMIT             -> write checkpoint, advance coverage_to, COMMIT
            ROLLBACK                 -> ROLLBACK
            Rotate / Heartbeat       -> advance coverage_to
            XA PREPARE / XA COMMIT   -> refuse by name
            unrecognised mid-txn     -> refuse by name
```

Rows stream into the open transaction rather than buffering in Go memory: a single
`UPDATE` touching ten million rows is exactly the case this tool exists for, so
buffering would fail precisely when it matters most.

`DisableRetrySync: true`. The library's default is an infinite, silent reconnect
that resumes from a position lagging the in-flight transaction — which would
re-deliver events into an open store transaction that has no idempotency key, and
duplicated inverses either abort a script mid-run or silently double-insert on a
table with no primary key. The coverage model detects missing data and is blind to
duplicated data. So: our own reconnect loop with bounded backoff, `ROLLBACK` of the
open transaction on disconnect (uncommitted, nothing lost), resume from the last
committed checkpoint, with `UNIQUE(source_txn_id)` as the backstop.

`TRANSACTION_PAYLOAD_EVENT` must be unwrapped: on MySQL 8.0.20+ with
`binlog_transaction_compression=ON`, every DML transaction arrives as one event
that an unprepared dispatch would skip while the position advanced — a healthy
connection producing an empty script.

XA is refused rather than ignored. `XA PREPARE` writes row events with no XID, so
prepared-but-unresolved changes would enter the store as committed fact and a
later `XA ROLLBACK` would be invisible.

Anything unrecognised mid-transaction refuses, so this class of defect reports
itself rather than being discovered in an incident.

**Decode options cross the seam.** `binlog.DecodeOptions` is exported and consumed
by both `ReadFile` and the source adapter. `BinlogSyncerConfig`'s zero values are
the opposite of what `newParser()` sets, so without this a `TIMESTAMP(6)` arrives
as a string pre-formatted in the watch host's local zone and restores hours off —
the identical bug class fixed in v0.1, at a new seam, and invisible to a codec test
on a UTC runner.

## 8. CLI and security

```
mulligan watch --store mulligan.db --server-id 1001 [--retain 7d] [--max-staleness 5m]
mulligan generate --store mulligan.db --tables orders --from .. --to ..
mulligan generate FILE...                                   # unchanged
```

`--server-id` is required. A colliding server ID disconnects a real replica, which
is too destructive to guess a default for.

Credentials arrive for the first time in v0.2. `MULLIGAN_DSN` is the documented
path; `--dsn` works but is called out as insecure, since it puts the password in
`ps` output for every user on the host. The DSN is redacted anywhere it can be
printed, including error messages and any logger handed to the syncer.

Rendered comments pass through the same `safeToQuote` rule as values: `--` ends at
the first newline, and `RowsQueryEvent` carries statement text verbatim, so a
multi-line migration statement would produce continuation lines that are syntax
errors partway through a hand-run script. Unprintable text becomes
`-- caused by: <unprintable, N bytes, txn 8813>`. Schema and table names get the
same treatment. `injection_test.go` extends to assert every line of a rendered
script either begins with `--` or was produced by the planner.

The store now holds full row images and statement text — as sensitive as the
tables it came from, unencrypted, at `0600`. WAL means it is three files, not one.

## 9. Testing

Unit: codec round-trip through `literal`; store append, query, prune; each of the
five refusal conditions firing.

Acceptance, against both MySQL 8.0 and MariaDB 11.4: `watch` a live server, commit
DML, assert store contents, `generate`, apply, verify exact restore. One run under
a non-UTC `TZ` to catch decode-option drift.

Fault paths, which is where every finding in the design review lived:

- stop `watch`, `PURGE BINARY LOGS`, restart -> gap recorded, `generate` refuses
- kill `watch` mid-transaction -> no partial transaction visible on restart
- force a disconnect mid-transaction -> no duplicate transaction after resume
- stall `watch` -> `generate` refuses on staleness rather than reporting nothing
- prune during `generate` -> no truncated window
- point `watch` at a different server -> refuses on the source binding

A differential test pins file-mode and store-mode to identical output over the
existing acceptance fixtures.

## 10. Deferred

Recorded in PLAN.md as named follow-ups rather than dropped:

1. **DDL and schema drift** — a window spanning `ALTER TABLE`. Each event carries
   its own column list, so failures are expected to be loud, with silent wrongness
   only on a retype. Unverified; verify before relying on that.
2. **Store format evolution** — `schema_version` and `codec_version` exist; no
   migration or downgrade-refusal policy does.
3. **Codec type coverage is defined circularly** — "every type the acceptance suite
   proves" leaves anything the suite omits untested by construction.
4. **SQLite operational reality** — disk-full mid-incident, `VACUUM`, prune lock
   duration, backup that copies only the `.db` and silently truncates WAL.
5. **Size and memory bounds** — `generate` materializes the whole matching set.
6. **Store ownership** — nothing enforces a single writer per store file.
7. **Backpressure and lag** — source purging binlogs out from under a slow reader;
   restart policy; clean shutdown flushing an open transaction.
8. **The store as a plaintext shadow database** — no table filter on `watch`, so it
   captures every row of every table including PII.
9. **Observability** — no `mulligan status`, so coverage health is discoverable
   only during an incident.
10. **`gtid` column semantics** — single transaction GTID or executed set.
11. **Time semantics** — `--from`/`--to` zone handling, source-vs-collector clock
    skew, one-second resolution at an inclusive window edge.
12. **Vector columns** (MySQL 9) remain untested, carried over from v0.1.
