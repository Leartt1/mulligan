# Mulligan v0.3a — read-only HTTP API over the window store

Status: approved, not yet implemented.
Supersedes nothing. Extends the v0.2 store shipped on `main`.

## 1. Goal

Serve what the store already knows over HTTP, so a console — and anything else —
can render a timeline, preview a diff, and download a revert script without
shelling out to the CLI.

v0.3 in `PLAN.md` is "API + web console". This spec is the first half only. The
API is testable without a browser, and splitting it means the undecided UI stack
(§0 of the plan) blocks nothing.

The existing principle carries over unchanged: **never emit a plausible-but-wrong
or silently-incomplete revert.** Over HTTP that pressure is sharper, because a
`200 OK` with an empty array is a friendlier-looking lie than an empty script.

## 2. Decisions

| Decision | Choice | Why |
|---|---|---|
| Scope | Read-only. Nothing executes | Generate-and-review is the safety model. A stolen token must not be able to mutate the database. Guarded apply stays v0.4. |
| Auth | Loopback-only by default, no accounts | Self-hosted, single operator. The store is already an unencrypted partial copy of the rows; the file permission is the real boundary. |
| Non-loopback | Refused unless `MULLIGAN_TOKEN` is set; then required on every request | Binding to `0.0.0.0` by accident publishes the contents of production tables. |
| Coverage refusals | HTTP 409 with the reason text | An empty array reads as "nothing happened" exactly the way an empty script does. |
| Row values in JSON | Strings or `null`, never JSON numbers | Every browser parses JSON numbers as float64. A `DECIMAL(10,2)` or a large `BIGINT` would render wrong in the diff a human approves. |
| Paging | Cursor on the store's own row id, newest first | Offsets shift under an active collector. Newest first is the order a revert applies in. |
| Change identity | `store.Entry{ID, Event}`, not a field on `change.Event` | A store-assigned row id means nothing to a binlog source or a future WAL one, and `change` is the shared seam between them. |

## 3. Components

```
internal/store    Entry, Page — paged, coverage-checked reads        (extend)
internal/api      HTTP handlers over a store                         (new)
internal/cli      serve command; status wire shape moves to api      (extend)
internal/reverse  inverse SQL                                        (unchanged)
```

`internal/api` depends on `store` and `reverse`. It does not depend on `cli`, so
the wire shape `mulligan status -json` publishes moves into `api` and `cli`
consumes it from there — one definition, and v0.3's endpoint cannot drift from
the command.

## 4. The store read path

```go
// Entry is a stored change together with the identity the store assigned it.
type Entry struct {
    ID    int64
    Event change.Event
}

// Page returns up to limit entries matching f, newest first, starting after the
// cursor. A zero cursor starts at the newest.
func (s *Store) Page(f change.Filter, before int64, limit int, now time.Time) ([]Entry, error)
```

Same transaction, same `checkCoverage` call, same refusals as `Events` and
`EachEvent` — a page is a window like any other, and a page that quietly omitted
a gap would be the failure this design exists to prevent.

Matching stays in Go via `change.Filter.Match`, as the existing reads do, so
there is one definition of what a filter means rather than one in Go and another
in SQL. Only the cursor is pushed into SQL (`r.id < ?`). This means a narrow
filter over a large store scans rows it discards; that is a known cost, recorded
in §8, not a thing to fix by duplicating the matching rules.

`limit` is clamped to a maximum, so a client asking for everything gets a page
rather than the whole store.

## 5. Endpoints

All responses are JSON except the script. All are `GET`; nothing has a side
effect on the database being watched.

### `GET /api/status`

The object `mulligan status -json` already prints, unchanged. HTTP 200 whether
or not the store is healthy — the report *is* the answer, and its `healthy`
field carries the verdict. A monitoring client reads that field; the CLI's exit
code is the same information for shells.

### `GET /api/changes?from=&to=&tables=&limit=&before=`

```json
{
  "changes": [
    {
      "id": 4182,
      "at": "2026-08-01T11:27:34Z",
      "schema": "shop",
      "table": "orders",
      "op": "UPDATE",
      "log_file": "binlog.000004",
      "log_pos": 576,
      "query": "UPDATE shop.orders SET status = 'shipped' WHERE id = 1",
      "schema_change": false
    }
  ],
  "next": 4182
}
```

`next` is the cursor to pass as `before` for the following page, and is absent on
the last page. Row images are **not** in the list — a timeline of ten thousand
rows should not carry ten thousand row images — they come from the detail route.

`from`/`to` accept the same formats the CLI does, including a bare `15:04`.
`tables` is comma-separated. Times in responses are RFC 3339 UTC.

### `GET /api/changes/{id}`

One change with its columns and both row images:

```json
{
  "id": 4182,
  "at": "2026-08-01T11:27:34Z",
  "schema": "shop", "table": "orders", "op": "UPDATE",
  "log_file": "binlog.000004", "log_pos": 576,
  "query": "UPDATE shop.orders SET status = 'shipped' WHERE id = 1",
  "columns": [
    {"name": "id", "primary_key": true, "read_only": false},
    {"name": "status", "primary_key": false, "read_only": false}
  ],
  "before": ["1", "pending"],
  "after":  ["1", "shipped"]
}
```

`before` and `after` are indexed in step with `columns`, and are `null` where the
operation has no such image — before on an INSERT, after on a DELETE. Each value
is a JSON string or `null`; see §6.

404 when no change has that id. The id is only meaningful within one store.

### `GET /api/revert.sql?from=&to=&tables=&generated=`

The same script `mulligan generate -store` writes, streamed as it is produced,
`Content-Type: text/plain; charset=utf-8` and
`Content-Disposition: attachment; filename="mulligan-revert.sql"`.

Streamed rather than buffered for the same reason the CLI streams: the window
that will not fit is the one that matters. A failure partway through cannot be
retracted from a response already being written, so the coverage check runs
before the first byte, and any error after that terminates the response —
truncated output is detectable, silently-complete-looking output is not.

The script is a proposal. Nothing here runs it.

## 6. Rendering row values

Every value is rendered to a JSON string, or `null` for SQL NULL. Never a JSON
number, never a JSON boolean.

The reason is precision. JSON numbers are IEEE doubles everywhere a browser is
involved, so `19.99` stored as `DECIMAL(10,2)` and a `BIGINT` past 2^53 both
survive the trip only as text. The diff is the artifact a human reads before
running something destructive; a value that displays as almost-right is precisely
the failure mode this project refuses everywhere else.

Rendering mirrors what the generated script shows for the same value, so the
preview and the SQL cannot disagree. Binary values are rendered the way the
script renders them.

## 7. Serving

```
mulligan serve -store FILE [-listen 127.0.0.1:8080]
```

- Default listen is `127.0.0.1:8080`. Loopback needs no token.
- A non-loopback `-listen` is refused unless `MULLIGAN_TOKEN` is set, naming the
  variable in the refusal. With it set, every request carries
  `Authorization: Bearer <token>`; comparison is constant-time.
- The store is opened without `Claim` — serving is a reader, and refusing to
  start because a collector holds the store would be backwards.
- Requests are logged one line each: method, path, status, duration. Query
  strings are logged; they contain table names and times, not credentials.
- Shutdown on SIGINT/SIGTERM, draining in-flight requests.

## 8. Known limits, stated rather than hidden

1. **Filter matching scans.** Only the cursor is pushed into SQL. A narrow
   `tables` filter over a large store reads and discards rows. Fixing it means
   duplicating the matching rules into SQL, which is a worse trade until it is
   measured to matter.
2. **No total count.** `COUNT(*)` over a filtered window costs a scan, so the
   timeline has "more pages" and not "page 3 of 97".
3. **No compression, no caching headers.** Later, and measured first.
4. **Token is static.** No rotation, no expiry, no per-user identity — an audit
   trail could only ever say "someone holding the token".
5. **The store is unencrypted.** Serving it over loopback does not change that;
   `SECURITY.md` already says so and gains a line about the listener.

## 9. Acceptance

Against a real MySQL in a container, as everything else here is:

1. `watch` collects a known workload; `serve` starts.
2. `GET /api/changes` lists them newest first, and paging with `before` walks the
   whole set without repeating or skipping one.
3. `GET /api/changes/{id}` shows both row images for an UPDATE.
4. `GET /api/revert.sql` returns a script that, applied to the database, restores
   the prior state exactly — the v0.1 acceptance bar, reached over HTTP.
5. Killing the collector and waiting past the staleness allowance turns
   `/api/changes` into a 409 naming the stall, not a 200 with an empty list.
6. A non-loopback `-listen` without `MULLIGAN_TOKEN` refuses to start.
