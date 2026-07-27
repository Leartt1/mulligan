<h1 align="center">Mulligan</h1>
<p align="center"><em>Ctrl-Z for the database you already have.</em></p>

---

**Mulligan** is a self-hostable **undo console** for your existing database. Point
it at the database's change log, see a timeline of recent writes, preview the
diff, and generate — or apply — the reverse SQL. No migration to a new database,
no lock-in.

> Status: **early / planning.** See [PLAN.md](PLAN.md) for the design and roadmap.

## The idea

You run `UPDATE orders SET status = 'shipped'` — and forget the `WHERE`. Today the
fix is a restore-from-backup that loses every good write since, or a hand-written
recovery script parsed out of the binlog by an expert. Mulligan turns that into:
open the console → find the statement → preview what it changed → generate the
reverse → review → apply.

It's the *bounded* version of database undo: recent, row-logged changes you
review before anything runs. Not a magic "undo anything forever" (that's a myth —
see the plan for why); a focused tool for the accident you actually have.

## Why not just…

- **binlog2sql / my2sql** — great, but CLI-only, MySQL-only, no timeline, no UI.
- **Dolt** — wonderful, but it's a whole new database to migrate to.
- **Oracle Flashback / Snowflake Time Travel** — proprietary and locked in.
- **Backups / PITR** — whole-database, downtime, loses the good writes.

Mulligan works with the database you *already have*, and it's open source.

## Roadmap

MySQL/MariaDB first (via ROW binlog), Postgres next (via WAL). Engine + CLI →
live timeline → web console → guarded apply. Details in [PLAN.md](PLAN.md).

## License

MIT — see [LICENSE](LICENSE).
