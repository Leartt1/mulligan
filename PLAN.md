# Mulligan — Project Plan

> **Mulligan** *(noun)* — a second chance; a do-over.
> **Tagline:** *Ctrl-Z for the database you already have.*

The self-hostable **undo console** for your existing database. Point it at the
database's change log → see a timeline of recent writes → preview the diff →
generate (and optionally apply) the reverse SQL. No migration, no lock-in, open
source.

---

## 0. Decisions on the table (confirm at start of next session)

| Decision | Current call | Rationale | Status |
|---|---|---|---|
| **Name** | `Mulligan` | Do-over/second-chance; friendly, distinct. Alts: *Rewind, Reverso, Backstep* | changeable — only touches README/module path |
| **First database** | MySQL / MariaDB | Huge install base; ROW binlog is a proven source; `my2sql`/`binlog2sql` precedent | recommend lock-in |
| **Engine language** | Go | Single static binary (ideal for self-host), mature `go-mysql` binlog library, easy Docker, simple concurrency | recommend lock-in |
| **Delivery shape** | Go core + REST API + embedded web console (`go:embed`), plus a thin CLI | One binary ships everything; API is reusable; UI is the differentiator | recommend lock-in |
| **License** | MIT | Simplest for adoption | recommend lock-in |

**Genuinely open questions** (don't pre-decide — resolve when we hit them):
- Web UI stack: React vs Svelte vs server-rendered + htmx. Lean minimal.
- ~~Do we persist a rolling event window (SQLite/bolt) or re-scan binlog on demand?~~
  **Resolved 2026-07-27: persist, in SQLite.** A timeline that re-scans cannot feel
  live, and the query the console needs — by time, table and user — is an index,
  which is the thing SQLite is. Pure-Go `modernc.org/sqlite`, so the single static
  binary survives.
- Auth model for the console (v1 can be single-user / bind-to-localhost only).

---

## 1. What it is

The universal "undo any committed write, forever" is a **myth** — once another
transaction reads the bad value, there is no unambiguous undo (it becomes the
merge-conflict problem). Mulligan ships the **bounded** version that is genuinely
useful and genuinely buildable:

- **IN scope:** recent, ROW-logged DML (`INSERT` / `UPDATE` / `DELETE`) on tables
  you select; a single reversal that you review before it runs.
- **OUT of scope (v1):** DDL undo (`DROP`/`ALTER`), automatic conflict resolution
  across concurrent dependent writes, recovery beyond the binlog retention window.
- **Safety principle:** *generate-and-review by default.* Execution is always an
  explicit, dry-run-first action — the tool proposes, the human commits.

---

## 2. Why it's worth building — the gap

| Existing | What it is | Why it's not this |
|---|---|---|
| `binlog2sql`, `my2sql` | CLI flashback tools | CLI-only, MySQL-only, dev-facing, no UI, no timeline |
| Dolt / DoltgreSQL | Git-for-data database | You have to *migrate to a new database* |
| Oracle Flashback, Snowflake Time Travel | Built-in rewind | Proprietary, lock-in, $$$ |
| PITR / backups | Disaster recovery | Whole-DB, downtime, loses good writes after the mistake |

**The empty niche:** a friendly, self-hostable console that works with the
database you *already run*. That's Mulligan.

---

## 3. Architecture

```
            ┌──────────────┐   ┌──────────────┐   ┌───────────────┐
 MySQL ───► │ source adapter│──►│ change events │──►│ reverse engine│──► inverse SQL
 binlog     │ (binlog/ROW)  │   │ (normalized)  │   │ INSERT<->DEL  │
            └──────────────┘   └──────┬───────┘   │ UPDATE reversed│
                                      │           └───────────────┘
                                      ▼
                              ┌───────────────┐   ┌──────────────┐
                              │ window store   │──►│ REST API      │──► Web console
                              │ (time/table/stmt│  │               │    timeline · diff
                              │  index)        │   └──────────────┘    generate · apply
                              └───────────────┘
```

- **Source adapter** — reads MySQL ROW binlog (file or live replica stream),
  emits normalized change events. Pluggable so Postgres WAL slots in later.
- **Reverse engine** — event → inverse SQL. Needs the *before* image (see §6).
- **Window store** — a rolling, queryable index of recent events (time / table /
  statement) so the timeline is instant. SQLite, and it records what it does *not*
  have as explicitly as what it does: coverage bounds, gaps, and changes it saw but
  could not store.
- **API** — REST over the store + reverse engine.
- **Web console** — the differentiator: timeline, filter, diff preview, one-click
  "generate revert," guarded apply.
- **Packaging** — one binary (`go:embed` the UI) + a Docker image.

---

## 4. Roadmap (phased, each phase shippable)

- **v0.1 — Engine + CLI. ✅ done.** Read a MySQL binlog file for a time/table range,
  parse ROW events, emit reverse SQL for `INSERT`/`UPDATE`/`DELETE`.
  *Accept:* given a known bad `UPDATE` on a test DB, the generated SQL restores the
  prior state exactly. — met by `internal/acceptance`, which runs a real MySQL in a
  container and applies the generated script.
- **v0.2 — Live tailing. ✅ done.** Connect as a replica, stream events into the
  rolling window store, query by time and table. *(Not by user: the binlog does
  not carry one. What it can carry is the statement that caused the rows, which
  Mulligan captures when the server logs it — see §0.)*
- **v0.3 — API + Web console.** Timeline UI, diff preview, generate revert,
  download `.sql`.
- **v0.4 — Guarded apply.** Execute a reversal with dry-run first + concurrent-write
  conflict warnings ("row changed again after the target statement").
- **v0.5 — Postgres adapter.** WAL via logical decoding, same reverse engine.
- **v1.0 — Hardening.** Auth, retention config, audit log, Docker image, tests, docs.

---

## 5. v1.0 acceptance criteria

1. Connect to a live MySQL/MariaDB and show a timeline of recent row changes.
2. Filter by table and time range, and show the statement that caused a change
   where the server logs it. *(Not by user: the binlog does not carry one.)*
3. Preview a before/after diff for any change.
4. Generate correct reverse SQL for `INSERT`/`UPDATE`/`DELETE`.
5. Apply a reversal with an explicit confirm + dry-run, and warn on conflicts.
6. Ship as a single binary and a Docker image; docs cover setup + limits.

---

## 6. Risks & walls (name them, don't pretend to solve them)

- **Before-image requirement.** Reversing `UPDATE`/`DELETE` needs the full prior
  row. MySQL must run `binlog_format = ROW` **and** `binlog_row_image = FULL`.
  Detect this on connect and refuse loudly if not set — otherwise reversals are wrong.
  *(v0.1: implemented — a partial row image is refused by name.)*
- **Column names are not in the log by default.** ROW events identify columns by
  ordinal, not name. `binlog_row_metadata = FULL` (MySQL 8.0.1+) puts names and
  the primary key into the table map event, which is what lets Mulligan reverse a
  binlog *file* without also connecting to the source database. Without it the
  alternative is querying `information_schema` — which is wrong anyway if the
  schema changed since the event. v0.1 requires the setting and refuses without
  it. MariaDB 10.5+ spells it the same way — verified, and the earlier note here
  guessing otherwise was wrong.
- **Concurrency.** If a later statement also touched the row, a naive undo clobbers
  it. v1 detects and *warns*; it does not auto-merge.
- **Retention.** Can only reach back as far as binlogs are kept (`expire_logs_days`).
- **DDL.** Schema changes aren't reversible from ROW events — out of scope, flag clearly.
- **Non-determinism.** Auto-increment, `NOW()`, sequences won't restore identically.
- **Generated columns.** Logged with their computed values but not flagged as
  computed, and assigning to one is an error. v0.1 takes their names via
  `-generated` and never assigns to them. *(Found the hard way: every table with
  one produced a script the server rejected outright.)*
- **Partial JSON.** With `binlog_row_value_options=PARTIAL_JSON` an update logs a
  diff rather than the document. Reversed as if it were a value it would write the
  diff into the column — valid JSON, wrong content. v0.1 refuses these events.
- **Session footing.** Values are logged on the source session's terms. TIMESTAMP
  decodes to an instant and DATETIME does not, and text is in the column's
  charset — so the generated script pins `time_zone` and `NAMES`, and any value
  that is not valid UTF-8 is emitted as a hex literal rather than quoted.
- **Permissions.** Needs `REPLICATION SLAVE` + `REPLICATION CLIENT` (live) or read
  access to binlog files.
- **MariaDB omits event positions inside a transaction.** It records the real
  position only on the event that commits, leaving zero on the row events
  themselves. Since provenance is what makes a generated statement reviewable, the
  scan reconstructs the position from event sizes. *(MariaDB 11.4 is otherwise
  compatible — same three settings, same spelling, verified end-to-end. The
  earlier note here claiming it spelled `binlog_row_metadata` differently was
  wrong.)*
- **Performance.** Large binlogs — stream, don't load whole; index into the store.

---

## 7. Key dependencies (Go)

- `github.com/go-mysql-org/go-mysql` — binlog replication + ROW event parsing (the engine's heart).
- CLI: stdlib `flag` for v0.1, consider `cobra` when commands grow.
- Store: `modernc.org/sqlite` (pure-Go, no cgo) or `go.etcd.io/bbolt`.
- Tests: dockerized MySQL (testcontainers-go) for end-to-end reversal checks.
- Web: decide at v0.3; whatever it is, embed via `go:embed`.

---

## 8. Next tasks

v0.1 is done — `internal/change`, `internal/binlog`, `internal/reverse`,
`internal/cli`, and an end-to-end acceptance suite against a real MySQL.

**v0.2 is built** — designed in
[docs/superpowers/specs/2026-07-28-live-tailing-design.md](docs/superpowers/specs/2026-07-28-live-tailing-design.md),
after an adversarial review of that design confirmed 16 defects before any code
was written. Every critical and high finding is closed and verified against a
live server; §10 of the spec records what was deliberately deferred.

Four further bugs were found by end-to-end tests and by nothing else, which is
worth remembering when weighing how much to invest in them:

1. A collector with no checkpoint replayed the server's entire retained history.
2. Heartbeats never advanced coverage, because a real heartbeat carries timestamp
   zero — so every idle database would have tripped the staleness refusal.
3. An unresumable checkpoint was retried forever, so a collector whose binlogs had
   been purged never came back and the store silently stopped collecting.
4. **With `gtid_mode` off — MySQL 8.0's default — every transaction was stored
   under the same identifier.** The library reports an anonymous GTID as one whose
   SID is all zeros, so the store's uniqueness constraint discarded every
   transaction after the first as a re-delivery. The guard against duplicated
   changes had become a path that lost them, and every unit test passed
   throughout: the bug needs two transactions to appear, and nothing had asked
   for two.

Toward **v0.3 (API + web console)**:

1. Serve the store over HTTP: timeline, filter, diff preview, download `.sql`.
2. Settle the auth question in §0 before anything binds to a port.
3. Decide the UI stack (§0), and embed it with `go:embed`.
4. `mulligan status`, so coverage health can be looked at deliberately rather than
   discovered mid-incident. The store already exposes what it needs.

Deferred from v0.2, in rough order of how much they would cost to hit:

- ~~DDL drift — a revert spanning `ALTER TABLE`.~~ **Measured** in
  `internal/acceptance`, and it behaves better than feared: a column added
  afterwards is harmless, and dropped, renamed or narrowed columns all fail
  loudly when the script runs. Exactly one case is silent — a **retyped** column
  restores a value coerced into the new type with no error — so schema changes
  are now carried through both the file and streaming paths and reported as a
  warning in the script. Mulligan still cannot reverse DDL and does not claim to;
  what changed is that it no longer stays quiet about it.
- **Kill-mid-transaction is untested.** The atomicity is designed for and covered
  by a unit test for the rejected-transaction case, but no test SIGKILLs a running
  collector. That needs `watch` as a subprocess.
- **Store format evolution** — `schema_version` and `codec_version` exist and a
  newer store is refused; no migration path does.
- **No table filter on `watch`**, so it collects every table on the server,
  including any holding personal data. Retention is currently the only bound on
  how long that copy lives.
- **Nothing enforces a single collector per store.** Two would interleave into an
  order that means nothing.
- **`generate` materializes the whole matching window in memory**, which is an OOM
  at exactly the wrong moment.
- **Backpressure** — a source purging binlogs faster than the collector reads them
  is now survivable (it records a gap), but nothing warns before it happens.

Known gaps in v0.1, worth closing whenever they get in the way:

- **Generated columns must be named by hand** (`-generated`). The table map's
  optional metadata has no flag for them, so the log genuinely cannot tell us.
  v0.2 has a replica connection that could read `information_schema` once and mark
  them automatically; it does not yet, and that remains the natural fix.
- ~~`-from` / `-to` take a full timestamp only.~~ **Done:** a bare `13:05` now
  resolves to its most recent occurrence, and the script header states which
  instant that was — a resolution the reviewer cannot see is worse than a refusal.
- **A table with no primary key** falls back to matching the full row image, which
  is correct but slow on a large table and matches one duplicate at a time.
- **Vector columns are untested** (MySQL 9). Everything else a real table holds is
  covered end-to-end by `internal/acceptance`: unsigned 64-bit, negative ints,
  DECIMAL, DOUBLE, BIT, ENUM, SET, DATE/TIME/DATETIME/TIMESTAMP with fractional
  seconds, YEAR, JSON, TEXT, BLOB, POINT, NULL, utf8mb4, embedded quotes and
  backslashes.
- **The whole matching event set is held in memory.** §6 called for streaming and
  this does not stream. Fine for a window; not for a whole retention period.

---

## 9. Naming note

`Mulligan` (a golf do-over) reads as "second chance" without being a literal
"undo" — friendly and distinct for an OSS project. Binary and module name:
`mulligan`. If we change it, only the README and module path move. Alternates kept
on the shelf: **Rewind**, **Reverso**, **Backstep**.
