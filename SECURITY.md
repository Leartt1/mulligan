# Security

## What Mulligan is, as of v0.2

Two commands, and they have different shapes.

`mulligan generate` is offline. It reads a binary log file or a local store,
writes a SQL script to stdout or a file, and exits. It listens on no port, starts
no server, and executes no SQL.

`mulligan watch` is a long-running process that connects to a database as a
replica. It holds credentials, keeps an outbound connection open for as long as
it runs, and writes a store to disk containing the row data it collects. It also
executes no SQL beyond reading server settings on connect.

Neither command listens on a port. That changes in v0.3 with the web console.

## Threat model

The input is untrusted. Anyone able to write a row into the source database
chooses bytes that end up in a generated script, and that script is run by hand,
usually by someone with enough privilege to undo anything. So Mulligan is a code
generator whose input is attacker-influenced, and the risk that matters is a
value or an identifier escaping its literal and becoming executable SQL.

Two inputs carry that risk:

- **Row values** — arbitrary bytes from any column.
- **Identifiers** — schema, table and column names, which MySQL permits to
  contain backticks and other punctuation.

## How that is handled

- **Text** is single-quoted with `''` doubling, which is standard SQL and holds
  under every `sql_mode`. Anything containing a backslash, a control character,
  or bytes that are not valid UTF-8 is emitted as a hex literal instead — a
  backslash means different things depending on `NO_BACKSLASH_ESCAPES`, and
  non-UTF-8 bytes would be reinterpreted by the applying session's charset.
- **Identifiers** are backtick-quoted with `` ` `` doubling.
- **Numbers, times and binary data** are rendered by fixed formatters and cannot
  carry syntax.
- **Unknown types are refused**, not coerced into something plausible.
- **`change.Raw`** is the one value emitted without quoting. It exists so DECIMAL
  survives without rounding through a float, and it is validated to be a plain
  number. A source adapter cannot use it to place arbitrary text into a
  statement.

`internal/acceptance` proves this against a real MySQL rather than by argument:
hostile values containing quote characters, backslashes, comment openers and
statement terminators, in a table and a column whose names contain backticks. A
canary table stands by; the test fails if the generated script drops it.

## Credentials

`watch` needs an account with `REPLICATION SLAVE` and `REPLICATION CLIENT`. It
does not need read access to your tables to stream their changes, so the account
it runs as should not have any.

Supply the connection string in `MULLIGAN_DSN`. The `-dsn` flag works and is
supported, but an argument is visible in `ps` output to every user on the host,
and the command warns when you use it.

No message Mulligan produces contains the password — not errors, not logs, and
not the replication library's own startup log, which is handed a redacted
configuration. If you find a path that prints one, that is a vulnerability and
worth reporting.

On Linux the environment of a running process is readable through `/proc` by the
same user and by root, so `MULLIGAN_DSN` protects against other users on the box
rather than against someone who is already you.

## The store

`watch` writes a SQLite file holding full before and after images of every row
that changed, plus the statement text when the server logs it. **Treat it as
being as sensitive as the tables it came from** — it is a partial copy of them.

- It is created mode `0600`, and so are scripts written with `generate -out`.
- It is not encrypted.
- WAL mode means it is three files, not one. A backup that copies only the `.db`
  captures a truncated store.
- `watch` has no table filter yet, so it captures every table on the server,
  including any holding personal data. Retention (`-retain`, default 7 days) is
  currently the only thing that bounds how long that copy persists.

## Limits you should know about

- **The script is executed by you.** Mulligan proposes; nothing runs until a
  human runs it. Review it. That is the design, not a disclaimer — but it does
  mean the last line of defense is the reader.
- **The generated script contains row data in clear text**, including whatever
  the reverted rows held. Treat a saved `.sql` as being as sensitive as the table
  it came from. Files written with `-out` are created mode `0600`.
- **The whole matching event set is held in memory.** A very large binlog with a
  broad filter can exhaust it. Narrow with `-tables`, `-from` and `-to`.
- **Binlog parsing happens in a third-party parser**
  (`github.com/go-mysql-org/go-mysql`) over untrusted bytes. Checksums are
  verified, but a malformed log could still crash the process. For a
  short-running CLI that is a failed run, not a foothold — reconsider when v0.2
  makes it long-lived.
- **No authentication or authorization exists yet**, because nothing listens.
  The web console in v0.3 needs an answer before it ships; see PLAN.md §0.
- **A compromised source server can influence the collector.** Binlog parsing
  happens in a third-party parser over bytes the server chooses, and the values
  it sends are stored and later rendered into SQL. The quoting above is what
  stands between that and executable SQL, and it is tested against hostile input
  — but a hostile *server* is a wider surface than a hostile *row*, and Mulligan
  is not hardened against one.
- **Nothing enforces a single collector per store.** Two `watch` processes on one
  file would interleave into an order that means nothing. Run one.

## Reporting a vulnerability

Open a GitHub issue for anything already public. For something that should not be
public yet, use GitHub's private vulnerability reporting on this repository.
