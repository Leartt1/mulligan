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
                              │ (time/table/usr│   │               │    timeline · diff
                              │  index)        │   └──────────────┘    generate · apply
                              └───────────────┘
```

- **Source adapter** — reads MySQL ROW binlog (file or live replica stream),
  emits normalized change events. Pluggable so Postgres WAL slots in later.
- **Reverse engine** — event → inverse SQL. Needs the *before* image (see §6).
- **Window store** — a rolling, queryable index of recent events (time / table /
  user) so the timeline is instant. Candidate: embedded SQLite or bbolt.
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
- **v0.2 — Live tailing.** Connect as a replica, stream events into the rolling
  window store, query by time/table/user.
- **v0.3 — API + Web console.** Timeline UI, diff preview, generate revert,
  download `.sql`.
- **v0.4 — Guarded apply.** Execute a reversal with dry-run first + concurrent-write
  conflict warnings ("row changed again after the target statement").
- **v0.5 — Postgres adapter.** WAL via logical decoding, same reverse engine.
- **v1.0 — Hardening.** Auth, retention config, audit log, Docker image, tests, docs.

---

## 5. v1.0 acceptance criteria

1. Connect to a live MySQL/MariaDB and show a timeline of recent row changes.
2. Filter by table, time range, and (where available) user/connection.
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
  it. Revisit for MariaDB, which spells this differently.
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

**v0.2 is designed** — see
[docs/superpowers/specs/2026-07-28-live-tailing-design.md](docs/superpowers/specs/2026-07-28-live-tailing-design.md).
An adversarial review of that design confirmed 16 defects before any code was
written; the criticals and highs are folded into the spec, and §10 of it records
what was deliberately deferred. The largest deferred unknown is **DDL drift** — a
revert spanning `ALTER TABLE` — which may also affect shipped v0.1 and is unverified.

Earlier sketch, superseded by the spec but kept for the ordering:

1. Replica-mode source: connect with `REPLICATION SLAVE`, stream events instead of
   reading a file, and check the three required settings on connect rather than
   discovering the problem mid-scan.
2. Window store — decide SQLite vs bbolt (§0 open question) and index by
   time / table / user so the timeline is instant.
3. Carry the originating user and transaction id on `change.Event`; the binlog
   knows both and the timeline needs them for filtering.
4. Conflict detection groundwork: record whether a row was touched again after the
   target statement, ahead of the v0.4 guarded apply.

Known gaps in v0.1, worth closing whenever they get in the way:

- **Generated columns must be named by hand** (`-generated`). The table map's
  optional metadata has no flag for them, so the log genuinely cannot tell us.
  A v0.2 replica connection could read `information_schema` once and mark them
  automatically — the natural place to fix this.
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
