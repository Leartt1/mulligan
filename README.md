<h1 align="center">Mulligan</h1>
<p align="center"><em>Ctrl-Z for the database you already have.</em></p>

---

**Mulligan** is a self-hostable **undo console** for your existing database. Point
it at the database's change log, see a timeline of recent writes, preview the
diff, and generate — or apply — the reverse SQL. No migration to a new database,
no lock-in.

> Status: **v0.1 — engine + CLI.** Reads a MySQL ROW binlog file and generates
> the SQL that undoes it. Live tailing and the web console are next; see
> [PLAN.md](PLAN.md) for the roadmap.

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

Then point it at a binlog and read what it proposes:

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

Narrow it with `-from` / `-to`, and save it with `-out revert.sql`. Reversals come
out newest first, because undoing a sequence means applying the inverses in the
opposite order.

A window rarely lines up with a log rotation, so name as many files as it spans —
oldest first, which is what a glob already gives you:

```console
$ mulligan generate -tables shop.orders /var/lib/mysql/binlog.00000[4-6]
```

Nothing is executed. Mulligan proposes; you review, then you run it.

### Generated columns

If your table has a generated column, name it:

```console
$ mulligan generate -binlog binlog.000004 -generated invoices.tax,invoices.gross
```

MySQL writes a generated column's computed value into the log like any other
column, but records nothing to say it is computed — and assigning to one is an
error. Without this flag the script fails on `ERROR 3105`. Named columns are read
but never assigned; restoring the columns they derive from is what brings them
back.

## What it will and won't do

- Reverses `INSERT`, `UPDATE` and `DELETE` on the tables you select, restoring
  only the columns a statement actually changed.
- Refuses rather than guesses: a partial row image, a log without column names,
  a partial JSON update, or a value it cannot render exactly all stop the whole
  script. A half-correct revert is worse than none. (Partial JSON updates come
  from `binlog_row_value_options=PARTIAL_JSON`, which logs a diff rather than the
  document; set it empty to make those tables revertable.)
- Does **not** undo `DROP` or `ALTER` — schema changes aren't in the row log.
- Does **not** resolve conflicts. If something else touched the row after the
  statement you're undoing, a reversal will clobber it. Conflict warnings land
  in v0.4.
- Reaches back only as far as your binlogs are kept.

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
