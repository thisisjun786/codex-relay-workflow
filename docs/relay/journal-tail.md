# Journal tail and receive

The sessions in this document are named by [the level table](../../plugins/crw/skills/crw-canon/references/hierarchy.md#the-levels): the parent is the repository session, the child is the project session, and a `child` session (a code id, not a layer) is no layer.

Two relay commands let a process that works inside a delivery turn follow the relay and finish a
review in one call: `journal-tail`, an awaitable read of the journal, and `receive`, which composes
the claim, the acknowledgement and the verdict. Both belong to the relay's command line
(`crw relay ...`); neither adds a table, a daemon or a listener, and neither is listed by `doctor`.

## journal-tail

```
crw relay [--state DIR] journal-tail --after SEQ [--kinds k1,k2] [--wait N] [--limit M]
```

The command reads the `journal` table in `seq` order and prints the rows after the cursor. `seq` is
the table's `INTEGER PRIMARY KEY AUTOINCREMENT`, so it grows with every row and a number is never
reused: a reader that keeps the last `seq` it saw resumes exactly where it stopped, however long it
was away.

| Option | Meaning |
| --- | --- |
| `--after SEQ` | the last `seq` already seen; `0` reads from the first row. Required |
| `--kinds k1,k2` | only rows of these journal kinds; a kind named twice counts once, and an empty name is a usage error |
| `--wait N` | seconds to wait for a row when there is none after the cursor (0 to 3600). `0`, the default, answers at once |
| `--limit M` | at most this many rows (1 to 1000; 100 by default) |

A value outside those ranges is a usage error (exit 4). A state directory with no store is the
refusal `store_absent` (exit 2), and a write gate or an ownership mirror beside no database is the partial-store
refusal (`store_owned_by_other`, exit 2), each judged on the database path the state resolves to (a link naming no
file is an absent store) as the other read-only commands judge them; the command never creates a store.

### The answer

The output is `crw-journal-tail/1`. The format is defined here and by the Go types in
`internal/relay/cli/journal_tail.go`; it is not part of `contract/schema`.

| Key | Meaning |
| --- | --- |
| `ok`, `schema` | `true`, `crw-journal-tail/1` |
| `after` | the cursor the read started from |
| `kinds` | the kinds the read was limited to, or `null` for every kind |
| `cursor` | the `seq` of the last entry returned; `after` when there is none. It is the next read's `--after` |
| `more` | `true` when the page stopped at `--limit` and a row beyond it exists |
| `entries` | the rows in `seq` order |

An entry is `{"seq", "at", "kind", "subject", "detail"}`. `subject` and `detail` are `null` where
the row has none; `detail` is the stored text unchanged (the relay writes JSON documents there, and
the command does not parse them).

With `--kinds`, the cursor is the last entry returned, not the last row scanned, so a reader that
filters and one that does not agree on what "after" means. A reader that wants the cursor of the
journal's head reads without `--kinds`.

### Reads and waiting

* The command opens the store with the read-only opener (`mode=ro`, `query_only`): it takes no write
  gate, runs no schema statement and creates nothing, and it answers for a store whose writes are
  halted or that another runtime owns.
* One read is one short statement: the primary key's range (or the `journal_kind` index when
  `--kinds` limits it) ordered by `seq`, asking for one row past the limit to learn `more`.
* `--wait` is that read repeated, with a one second pause between two reads, until a row arrives or
  the wait is over. Nothing is held while it sleeps: no transaction, no cursor, no snapshot, so a
  waiting reader never keeps the write-ahead log from being checkpointed. A wait that ends with no
  row answers an empty page whose cursor is `after`. A row of a kind `--kinds` leaves out does not
  end the wait.
* The command reads no clock but the wait's own, and writes nothing.

A renderer, a watcher or the pump follows the relay by calling the command with the cursor of its
last answer; one call with `--wait` is the cheap way to idle until something happens.

## receive

```
crw relay [--state DIR] [--socket PATH] receive --event E --ack-turn T --verdict-file F
```

`receive` is `claim`, `ack` and `verdict` run one after another in one process, for a child (project session) that
already holds the review it is going to rule. `--ack-turn` is the caller's own turn id. It is the turn
the claim is made from, the turn the acknowledgement is proved from and the turn the verdict is ruled
from. `--verdict-file` is a JSON object whose keys are the `verdict` command's options:
`verdict` (required), `criterion` and `finding` (lists of strings), `criteria` (a JSON array of
findings, or a string: its text, or `@path`), `restoration`, `reason`, `expect-criteria-digest` and
`verified-head`. A key it does not know, including a verdict turn of its own, a key an object gives twice, and
anything after the JSON document are usage errors.

* **One command per step.** Each step is the library function its command calls, in the store
  transaction that function opens: the claim (`Ack.ClaimVerification`), the acknowledgement
  (`AckCommand`) and the verdict (`PreparedVerdict.Record`). The records, the journal rows and the
  refusals are the ones the three commands write and name; `receive` adds no refusal and no record.
* **The proof is the caller's.** The proof is `AckProof(event, ackTurn)` (`delivery.AckProof`, the
  `ack-proof` command's function), computed from the turn id the caller gave and nothing else. The
  command has no option that brings a proof, and the relay never makes one for another actor: a turn
  the host does not show on the recipient's thread leaves the acknowledgement unverified and the
  verdict is refused `not_acknowledged`, as it is when the steps are run by hand. An event whose
  acknowledgement is already verified keeps it: `ack` returns the settled record before it looks at the
  turn or the host, so `receive` over such an event answers from the record, with or without a socket.
* **With a socket** the acknowledgement is verified against the host exactly as `ack --socket` does
  it (the delivery is confirmed through the acknowledging turn first). Without one the
  first acknowledgement of an event is recorded as the project session's intent (unverified) and the verdict
  that follows is refused `not_acknowledged`. A settled acknowledgement, and a verdict already on
  record, are returned as they are, so a repeated `receive` of a finished event succeeds offline.
* **Before the store is opened or anything is written**, `receive` reads what the three commands would read
  before they open the store, and a refused line creates no store and binds none to its socket: the event id and the turn (refused as `ack-proof` refuses them), the verdict file, the
  findings, `--criteria` and `--restoration`, and the shape of `verified-head`. A line the verdict
  command would refuse for those reasons writes no claim and no acknowledgement.
* **A refusal ends the receive** with the refusal of the step that made it, unchanged. What the steps
  before it recorded stays recorded, as it does when the commands are run one after another.
  Running `receive` again replays each step: the claim answers `already_claimed`, a settled
  acknowledgement is returned as it is and the verdict on record is returned marked `_replay`.
* The claim is not a gate: `already_claimed` (another turn holds the claim) is reported and the
  acknowledgement and the verdict follow, as they follow when `claim` is run first by hand and its
  answer is ignored.

The answer is `{"claim": ..., "ack": ..., "verdict": ...}`: the claim's `proceed` or
`already_claimed`, the acknowledgement's record, and the verdict's record with its `_restoration`.

## Integration

Both commands add to the relay's command surface, so they integrate after the cut ref
(`refs/cutover/crw-cutover-cut`, a ref and not a tag). `doctor`'s `offlineCommands` and
`hostRequiredCommands` lists, the existing `claim`, `ack` and `verdict` commands and the
contract corpus's goldens are not changed by them.
