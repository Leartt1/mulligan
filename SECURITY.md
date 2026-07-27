# Security

## What Mulligan is, as of v0.1

An offline command-line program. It reads a binary log file, writes a SQL script
to stdout or a file, and exits. It opens no network connections, listens on no
port, starts no server, and executes no SQL. It holds no credentials, because
file mode never connects to a database.

That shape is most of the security story. It changes in v0.2, when live tailing
adds a replication connection and stored credentials, and again in v0.3 with the
web console.

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
- **No authentication or authorization exists yet**, because there is nothing to
  authenticate to. The web console in v0.3 needs an answer before it ships; see
  PLAN.md §0.

## Reporting a vulnerability

Open a GitHub issue for anything already public. For something that should not be
public yet, use GitHub's private vulnerability reporting on this repository.
