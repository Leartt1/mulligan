<h1 align="center">Mulligan</h1>
<p align="center"><em>Ctrl-Z for the database you already have.</em></p>

---

**Mulligan** is a self-hostable **undo console** for your existing database. Point
it at the database's change log, see a timeline of recent writes, preview the
diff, and generate — or apply — the reverse SQL. No migration to a new database,
no lock-in.

> Status: **v0.2 — live tailing.** Follows a running MySQL or MariaDB and keeps a
> rolling window of recent changes, so a revert can be generated without going
> looking for binlog files. The web console is next; see [PLAN.md](PLAN.md).

## The idea

You run `UPDATE orders SET status = 'shipped'` — and forget the `WHERE`. Today the
fix is a restore-from-backup that loses every good write since, or a hand-written
recovery script parsed out of the binlog by an expert. Mulligan turns that into:
open the console → find the statement → preview what it changed → generate the
reverse → review → apply.

It's the _bounded_ version of database undo: recent, row-logged changes you
review before anything runs. Not a magic "undo anything forever" (that's a myth —
see the plan for why); a focused tool for the accident you actually have.

## Quick start

Mulligan reads what MySQL already writes, so the source server has to log enough
to reconstruct a row. Three settings, all of which Mulligan checks and refuses
loudly without:

```ini
[mysqld]
binlog_format       = ROW    # log row images, not statements
binlog_row_image    = FULL   # log every column, not just the changed ones
binlog_row_metadata = FULL   # log column names and the primary key
```

`binlog_row_image=FULL` is what makes an `UPDATE` reversible at all — without it
the log never records the values that were overwritten. `binlog_row_metadata=FULL`
is what lets Mulligan read a binlog file on its own, without also connecting to
the database it came from.

MariaDB 10.5+ spells all three the same way, and is covered by the same
end-to-end tests as MySQL.

### Follow a server

`watch` connects as a replica and keeps a rolling window of recent changes:

```console
$ export MULLIGAN_DSN='repl:secret@tcp(db.internal:3306)/'
$ mulligan watch -store mulligan.db -server-id 1001 -retain 168h \
    -tables shop.orders,shop.order_items
```

**Name the tables you care about.** Without `-tables` the collector follows every
table on the server, and the store holds full row images — so it becomes an
unencrypted partial copy of everything, including whatever holds personal data.
Collecting less is a data-protection measure, not an optimisation.

Only one collector may follow a store at a time; a second is refused. Two writing
to one would interleave their transactions into an order that reflects neither
server.

`-server-id` has no default on purpose: it must be unique across your
replication topology, because a collision disconnects whichever replica claimed
it first. The account needs `REPLICATION SLAVE` and `REPLICATION CLIENT`, and
nothing else.

Then generate from what it collected:

```console
$ mulligan generate -store mulligan.db -tables shop.orders \
    -from '2026-07-30 13:05:00' -to '2026-07-30 13:10:00'
```

### Check what the store can answer for

A collector that has stopped and a database nobody is writing to look identical
from the outside. `status` is how you tell them apart — before you need to:

```console
$ mulligan status -store shop.db
store     shop.db
source    mysql · server mysql:b8f274cf-8d9b-11f1-bb37-2ae711accf27 · gtid none — the server issues no GTIDs
coverage  2026-08-01 11:27:30 → 2026-08-01 11:27:34 UTC
retention 168h0m0s
freshness last change 3s ago (allowed 30s)
integrity ok
gaps      none
misses    none

OK
```

It exits 0 when the store is sound and its collector is keeping up, and 1 when
something needs looking at — so it works as a cron check without parsing prose:

```console
$ mulligan status -store shop.db
...
freshness last change 38s ago (allowed 30s)
...
NOT OK: the store is stale — nothing has been collected for 38s (allowed 30s, last change at 2026-08-01 11:27:34 UTC); the collector may have stopped, and an empty result here would read as though nothing happened
$ echo $?
1
```

Gaps and missed changes are always listed in full, but they never decide the exit
code. They are permanent history — nothing removes them — and a check that
latches red forever is a check nobody reads. `generate` still refuses any window
that overlaps one.

`-json` writes the same report as a single object, for anything that would rather
not read the text:

```console
$ mulligan status -store shop.db -json
{
  "store": "shop.db",
  "healthy": true,
  "verdict": "",
  "source": {
    "flavor": "mysql",
    "server_identity": "mysql:b8f274cf-8d9b-11f1-bb37-2ae711accf27",
    "gtid_dialect": "",
    "decode_fingerprint": "v1;parse_time=true;use_decimal=true;verify_checksum=true;tz=UTC"
  },
  "coverage": {
    "from": "2026-08-01T11:27:30Z",
    "to": "2026-08-01T11:27:34Z",
    "max_staleness_seconds": 30,
    "retention_seconds": 604800
  },
  "stale_seconds": 3,
  "integrity_problems": [],
  "gaps": [],
  "misses": []
}
```

### Or read a binlog file directly

No collector, no store — useful when someone hands you a log:

```console
$ mulligan generate -binlog /var/lib/mysql/binlog.000004 -tables shop.orders
-- mulligan revert script
-- source: binlog.000004
-- 2 statements, newest change first
-- REVIEW BEFORE RUNNING — nothing here has been executed.

SET time_zone = '+00:00';
SET NAMES utf8mb4;

-- undo UPDATE shop.orders — binlog.000004:546 at 2026-07-27 13:15:57 UTC
UPDATE `shop`.`orders` SET `status` = 'packed' WHERE `id` = 2 LIMIT 1;

-- undo UPDATE shop.orders — binlog.000004:546 at 2026-07-27 13:15:57 UTC
UPDATE `shop`.`orders` SET `status` = 'pending' WHERE `id` = 1 LIMIT 1;
```

The script pins the session zone and charset because that is the footing the
values were written on: timestamps are rendered in UTC, and text in utf8mb4.
Run it in a session set otherwise and MySQL will shift or convert values without
complaining.

Both rows share a log position because one `UPDATE` statement produced them —
that's a single event in the log, and a third row was left alone because it
already had the value being set. Only the `status` column is rewritten: whatever
else changed on those rows since is not clobbered.

Narrow it with `-from` / `-to`, and save it with `-out revert.sql`. Both accept a
bare `13:05` — what you read off a graph mid-incident — as well as
`2006-01-02 15:04:05` in local time or `2006-01-02T15:04:05Z07:00`. A bare time
means its most recent occurrence, so `23:50` typed at 00:10 is fifty minutes ago
rather than a day away, and the script header states the instant it resolved to. Reversals come
out newest first, because undoing a sequence means applying the inverses in the
opposite order.

A window rarely lines up with a log rotation, so name as many files as it spans —
oldest first, which is what a glob already gives you:

```console
$ mulligan generate -tables shop.orders /var/lib/mysql/binlog.00000[4-6]
```

Every script ends with a line saying how many statements it holds. A script
without it was cut short — by a crash, a full disk — and is not safe to run.
Writing with `-out` goes through a temporary file renamed once the whole script
is written, so a failed run leaves nothing rather than something that looks
finished.

Nothing is executed. Mulligan proposes; you review, then you run it.

### Generated columns

MySQL writes a generated column's computed value into the log like any other
column, and records nothing to say it is computed — but assigning to one is an
error, so a revert that does fails on `ERROR 3105`.

`watch` handles this for you: it has a live connection and asks the server which
columns it computes. Reading a **binlog file** has no such connection, so name
them there:

```console
$ mulligan generate -binlog binlog.000004 -generated invoices.tax,invoices.gross
```

Named columns are read but never assigned; restoring the columns they derive from
is what brings them back.

## What it will and won't do

- Reverses `INSERT`, `UPDATE` and `DELETE` on the tables you select, restoring
  only the columns a statement actually changed.
- Refuses rather than guesses: a partial row image, a log without column names,
  a partial JSON update, or a value it cannot render exactly all stop the whole
  script. A half-correct revert is worse than none. (Partial JSON updates come
  from `binlog_row_value_options=PARTIAL_JSON`, which logs a diff rather than the
  document; set it empty to make those tables revertable.)
- Does **not** undo `DROP` or `ALTER` — schema changes aren't in the row log. It
  does **notice** them: a revert whose window spans one carries a warning naming
  each statement, because the script describes each table as it was at the time.
  Measured against a real server: a dropped, renamed or narrowed column makes the
  script fail loudly, but a **retyped** column restores a converted value with no
  error at all. That silent case is what the warning is for.
- Does **not** resolve conflicts. If something else touched the row after the
  statement you're undoing, a reversal will clobber it. Conflict warnings land
  in v0.4.
- Reaches back only as far as your binlogs are kept, or as far as the store's
  retention window when reading a store.

**The store says when it cannot answer.** This is the part worth understanding,
because the alternative is worse than an error. `generate` refuses — rather than
returning fewer rows — when the window reaches back before collection began,
overlaps a period the collector missed, or when the collector has stopped:

```console
$ mulligan generate -store mulligan.db
mulligan generate: store: is stale — nothing has been collected for 41m29s
(allowed 5m0s, last change at 2026-07-31 06:43:52 UTC); the collector may have
stopped, and an empty result here would read as though nothing happened
```

A dead collector and a quiet database look identical from the outside. Without
that check the answer during an incident would be `no matching changes found`
and exit 0, which reads as "nothing happened" at exactly the moment something
did.

## Why not just…

- **binlog2sql / my2sql** — great, but CLI-only, MySQL-only, no timeline, no UI.
- **Dolt** — wonderful, but it's a whole new database to migrate to.
- **Oracle Flashback / Snowflake Time Travel** — proprietary and locked in.
- **Backups / PITR** — whole-database, downtime, loses the good writes.

Mulligan works with the database you _already have_, and it's open source.

## Roadmap

MySQL/MariaDB first (via ROW binlog), Postgres next (via WAL). Engine + CLI →
live timeline → web console → guarded apply. Details in [PLAN.md](PLAN.md).

## Security

The generated script is a code generator's output whose input is whatever anyone
could write into a row, so quoting is the thing that matters most here. See
[SECURITY.md](SECURITY.md) for the threat model and the limits.

## License

MIT — see [LICENSE](LICENSE).
