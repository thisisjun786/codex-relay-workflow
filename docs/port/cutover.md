# Cutover protocol: fenced single-writer handoff from Python to Go

Status: specification (todo 5 of `.omo/plans/crw-go-port.md`). This is the executable checklist
that todo 36 (the Python fence release) implements, todo 29/33 (Go store and daemon) honour, todo 42
runs on Jun's host, and todo 43 closes. Source of record: the takeover advisory
`.omo/senpi-task/completion-results/st_01a0d6ff.txt` and draft findings L40 to L48. Citations of
the form `file.py:N` name `packages/codex-session-relay/src/codex_session_relay/` unless a path
is given.

Two facts shape everything below.

1. A metadata stamp cannot fence a deployed binary that never reads it. Today's `Store.__init__`
   connects, sets PRAGMAs, runs DDL and writes initialization rows with no ownership check
   (store.py:1830-1889). `intent.py:808` (`registration_hold()`) and the diagnostic probe at
   store.py:2464 open the DB directly and take `BEGIN IMMEDIATE` on their own. So a final
   Python **fence release** is a required deliverable and every pre-fence process must be
   replaced before any Go writer opens the DB.
2. A cached hook command of the form `python3 /old/path.py` keeps being spawned by the host
   after an upgrade (plugins/crw/wiring/crw_stop_hook.py:16-28 documents the exit-2 loop that
   a missing script caused). Python interpreters and the tiny compatibility entry points stay
   until the retention scan (below) reports zero references.

Names used throughout:

- `S`: the validated canonical relay state directory
  (`${XDG_STATE_HOME:-~/.local/state}/codex-session-relay/<scope>/`).
- `D`: the existing database pathname inside `S`. Never a freshly computed default.
- `K`: the existing scope key for the App Server socket (service.py:102-159 hashes the resolved
  socket pathname; Go reproduces that canonicalization, it does not substitute `$HOME`).

Objects that already exist and keep their identity: `D`, `D-wal`, `D-shm` (SQLite alone manages
the last two), `S/daemon.lock`, the scope registry's `K.lock` and `K.json`, receiver-ledger
files with their `.lock` sidecars, `managed-start-<request hash>.lock` files, the transport
operation ledger, marker root, hook hold records and Stop-event arbitration files.

Objects the fence release adds, all owner-only (`0700` directories, `0600` files):

| Object | Purpose |
|---|---|
| `S/takeover.lock` | permanent-inode exclusive lock that serializes transition controllers |
| `S/write-gate.lock` | permanent-inode shared/exclusive admission barrier for DB writers |
| `S/takeover.json` | durable transition record and admission mirror (atomic replace) |
| `S/takeover-inbox/` | immutable, durable receipt/ACK requests awaiting application |
| `S/control.sock` | stable local control and `guard-evaluate` RPC address served by both runtimes |
| `S/takeover-backups/<transition-id>/` | SQLite backup plus inventory manifest; disaster-recovery evidence only, never rollback input |

Rule that applies to every lock file, old or new: **never unlink it and never replace it
atomically.** The inode is the rendezvous point. Atomic replace is for `takeover.json` only.

## Record

Ownership has two halves. The durable truth lives in the existing `schema_meta` table; the
admission mirror lives in `S/takeover.json`. Writer admission requires that both agree while the
writer holds `write-gate SH`. A missing, malformed, unsupported or disagreeing record means no
new write admission, no alternate empty database, and no automatic owner reset.

`schema_meta` keys (authoritative; `SCHEMA_VERSION` stays `1`, no migration):

```text
writer_protocol             integer; initially 1
owner                       "python" or "go"
owner_epoch                 monotonically increasing ownership generation; starts at 1
takeover_id                 the transition that installed the current owner/epoch
rollback_allowed            "1" until the commit point, then "0" forever
python_compatibility_build  exact retained Python fence build identifier
```

`S/takeover.json` fields (admission mirror; verbatim from the advisory):

```text
protocol                 integer; initially 1
storeId                  existing schema_meta.store_id
database:
  realPath               canonical existing database pathname
  device                 observed device number
  inode                  observed inode number
  walDirectoryDevice     device of SQLite's actual WAL directory
  walDirectoryInode      inode of that directory
  walBasename            database basename used for WAL naming
appServerSocket          canonical existing scope socket pathname
scopeKey                 existing scope-registry key
epoch                    monotonically increasing ownership generation
owner                    "python" or "go"
phase                    "active", "draining", or "starting"
transition:
  id                     random transition identifier
  from                   previous owner
  to                     requested owner
  targetEpoch            epoch to install
holder:
  bootId                 boot identity
  pid                    daemon/supervisor PID
  startTicks             process start identity
  build                  exact runtime build identifier
controller:
  bootId
  pid
  startTicks
rollbackAllowed          boolean
pythonCompatibilityBuild exact retained Python build
relayRPCSocket           S/control.sock
updatedAt                diagnostic timestamp
```

`holder`, `controller` and `transition` may be null when not applicable.

The record is fenced, not timed. `updatedAt` is diagnostic only.
No TTL and no heartbeat authorizes takeover.
No expiry, no age of the record and no staleness of a PID authorizes it; a Python process
paused inside an admitted transaction keeps its right to finish it. The existing code already treats the lock, never a PID
written in a file, as the ownership evidence (service.py:174-179), and this protocol keeps that.

The `database` block is how Go proves it opened the same physical store: `Store.locate()`
(store.py:1901-1940) already distinguishes DB identity from the WAL location, and identical
`store_id` values alone are not proof of the same file.

## Lock order

Every path that needs more than one of these locks takes them in this order and releases them
in reverse:

```text
takeover.lock EX          transition controller only
  -> daemon.lock EX       daemon start and transition paths
  -> scope K.lock EX      daemon start and transition paths
  -> write-gate SH | EX   SH for admitted writers; EX for the transfer barrier
  -> operation locks      managed-start, receiver ledger; sorted by path if several
  -> SQLite transaction   BEGIN IMMEDIATE
```

Details that matter:

- Ordinary CLI writers take `write-gate SH`, then their operation locks, then the SQLite
  transaction. They never take the daemon or scope locks afterwards.
- A daemon takes daemon and scope ownership before opening a writable Store, which is the
  existing supervisor and foreground-service order (service.py:1885-1905, service.py:2090-2097).
- Every writable connection holds `write-gate SH` for the connection's whole lifetime. An
  operation that spans a DB transaction and a recoverable external effect keeps its admission
  through the whole operation. The connection is closed before the admission is released.
- The transition controller takes `write-gate EX` only after it has requested shutdown. It never
  holds the gate exclusively while asking the old daemon to finish DB work.
- The read-only Stop path takes none of these locks.
- All of them are `flock` locks (`fcntl.flock` today at daemon.py:95-144, receiver.py:660-672,
  managed.py:311-333, service.py:192-205 and 946-962). Go uses `golang.org/x/sys/unix.Flock` on
  the same permanent files. `FcntlFlock`, POSIX record locks and `lockf`-style libraries live in
  a different locking domain on Linux and would not contend; they are forbidden.

## Steps 0-7

Each step names what the controller does, then its failure branch and recovery. No step deletes
`D-wal` or `D-shm`, and no step creates an alternate empty database.

### Step 0: deploy and activate the Python fence release

1. Install the fence release (write-gate admission before `Store.__init__`, owner/epoch check at
   transaction entry, coverage of `intent.py:808` and `store.py:2464`, draining phase in the
   supervisor loop, refusal that never journals into a foreign-owned store).
2. Install the stable hook routing, the common RPC contract on `S/control.sock`, and the durable
   inbox.
3. Stop and replace every pre-fence supervisor, worker and direct DB-writing client. The fence
   release's own `service restart` stops supervisor and worker both (service.py:1304-1421).
4. Initialize ownership as `owner=python`, `owner_epoch=1`, `writer_protocol=1`,
   `rollback_allowed=1`, under an exclusive maintenance gate; write the matching `takeover.json`.
5. Confirm every launch path reaches the fence build: `doctor --json` shows
   `python_compatibility_build` equal to the fence build for every running relay process.

Failure and recovery: if any old writable process or uncontrolled launch path can't be excluded,
do not cut over. Python stays in control. Writing `owner=go` would not repair the missing
precondition, and a Go opener cannot infer it from the database.

### Step 1: serialize and announce the transition

The controller takes `takeover.lock EX`, reads and validates the current record, DB identity,
scope identity, compatibility build and `rollback_allowed`, then publishes `takeover.json` with
`owner=python`, `phase=draining`, a fresh `transition.id`, `to=go`,
`targetEpoch=currentEpoch+1`. Publication is temp file in the same directory, fsync, rename,
directory fsync.

From this point the fence release refuses new write admission. Already-admitted operations keep
their shared gate and finish.

Failure and recovery: before durable publication Python runs as before. After it, a controller
crash leaves admission closed. A replacement controller takes `takeover.lock`, reads the DB
owner and epoch, and either resumes or explicitly aborts back to `phase=active`. It never
decides from the JSON alone.

### Step 2: drain Python and stop all of its holders

Request a graceful quiesce: stop scheduling new work and new outbound host effects, finish
admitted operations or leave their durable pending state intact, commit receipt/ACK results
already obtained, close writable connections, close receiver-ledger and managed-start handles,
stop supervisor replacement of workers, shut down the Python RPC listener. Then stop and reap
the supervisor **and its current worker** using identity-checked pidfds. A bounded drain
failure may escalate to termination, but only for verified owned processes
(service.py:1304-1421 stop semantics, service.py:1483-1519 SIGTERM to SIGKILL escalation).

A stopped supervisor is not sufficient: workers inherit the daemon and scope descriptors
(service.py:1766-1802).

Failure and recovery: if identity can't be established or a holder is still alive, ownership
stays Python and the phase stays draining. Go is not started as a writer. Either complete the
stop or abort the transition back to active Python.

### Step 3: acquire the transfer barrier

Still holding `takeover.lock`, take `daemon.lock EX`, then scope `K.lock EX`, then
`write-gate.lock EX`, each with a bounded wait. All three together prove exclusion: the daemon
and scope locks exclude lingering service holders, the exclusive gate excludes direct CLI
connections and admitted operations that the daemon lock does not cover.

Failure and recovery: release whatever was acquired, in reverse order, and stay in draining.
Find and finish the holder, then resume. Contention is never a reason to delete a lock file, and
a stale timestamp or a dead supervisor PID never bypasses this barrier.

### Step 4: preserve and inspect the existing state

While holding the barrier: revalidate the physical database and WAL identity against the
`database` block, open the existing `D`, validate schema compatibility and `PRAGMA
integrity_check`, create a consistent backup through the SQLite backup API into
`S/takeover-backups/<transition-id>/`, fsync the backup and its directory, and record the
receipt/ACK, attempt, inbox, receiver-ledger and transport-ledger inventory in the manifest.

Never copy only the main DB file, never require WAL truncation as a precondition, never delete
WAL or SHM to make the store look clean.

Failure and recovery: the owner does not change. Close the inspection connection, repair the
inspection or backup failure or abort to Python, leave the original files untouched.

### Step 5: commit the ownership fence

Still holding every transfer lock: `BEGIN IMMEDIATE`; require `owner=python` and the expected
`owner_epoch`; set `owner=go`, `owner_epoch=old+1`, `takeover_id=<transition.id>`,
`rollback_allowed=1` in one statement group; `COMMIT` with `synchronous=FULL`; publish
`takeover.json` with the Go epoch and `phase=starting`; close the controller's connection.

This COMMIT is the **ownership-transfer point**. It is not the irreversible commit point
(see Commit point).

Failure and recovery: before COMMIT the DB still belongs to Python. After COMMIT but before
JSON publication, Python refuses because the durable owner is Go and new writers refuse the
mismatched file. Recovery reads the DB stamp and completes Go activation, or performs an
explicit reverse transfer with a new epoch. It never restores the old JSON to paper over the
disagreement.

### Step 6: start Go on the original store

Release `write-gate EX`, `K.lock` and `daemon.lock` in reverse order; keep `takeover.lock`.
Start the Go daemon with the expected `transition.id` and epoch. Go then takes `daemon.lock`,
`K.lock`, `write-gate SH`; verifies DB and record ownership agree; opens `D` in existing-file
mode (never create); applies `journal_mode=WAL`, `synchronous=FULL`, `foreign_keys=ON` on every
connection; recovers pending operations under their existing identities; drains compatible
inbox entries; binds `S/control.sock`; and reports readiness with store ID, epoch, build and
process identity. Readiness comes only after recovery succeeds, matching today's supervisor
(service.py:1926-1945). During `phase=starting` only the designated candidate may enter;
ordinary client mutations stay queued.

Failure and recovery: ownership stays Go, no active service is advertised, inbox entries are
retained. Restart Go, or run the reverse protocol to the fence Python release. Never launch an
unfenced Python fallback because Go failed. If the controller dies before activation, the
candidate either observes a matching durable active record or quiesces when its activation
channel closes; no indefinitely writable, unadvertised candidate may remain.

### Step 7: publish active Go and switch new hook registrations

After readiness: publish `phase=active` with the Go owner, epoch and verified holder identity;
point new host registrations at the native Go Stop hook; preserve legacy settings and cached
executable paths; release `takeover.lock`.

The legacy `guard-evaluate` CLI (stopadapter.py:69, command assembled at invocation time from
settings at stopadapter.py:418-442) keeps its flags and its full verdict envelope so cached
Python adapters keep working. The Go `guard-evaluate` must not claim the Stop event again; the
Python adapter already claimed it.

Failure and recovery: if registration or configuration publication fails, Go remains the owner
and the retained legacy hook path still reaches a compatible evaluator. Repair the
configuration; do not touch DB ownership.

### When the other runtime owns the record

Fence Python finding `owner=go`: no writable open, no initialization DDL, no refusal journaling,
no auto-restart. A CLI request goes to `S/control.sock` or the durable inbox; one that can't be
forwarded returns an explicit ownership/retry error; a Stop invocation turns that into empty
stdout and exit 0. An already-admitted Python writer delays the takeover through its shared
gate; it is never displaced.

Go finding `owner=python`: normal daemon startup refuses to open the DB writable; only an
explicit transition controller may start the protocol; Go clients use the compatible endpoint or
the inbox. There is no "lock looks stale, start another daemon" path.

## Rollback

Rollback is the same protocol run in reverse, Go to the retained fence Python release, on the
**same live database**, installing `owner_epoch = current + 1`:

1. Controller takes `takeover.lock`, publishes `phase=draining`, `to=python`.
2. Drain Go: finish admitted operations, close writable connections, stop the Go daemon by
   verified identity.
3. Acquire `daemon.lock EX`, `K.lock EX`, `write-gate EX`.
4. Validate that the running Python build equals `python_compatibility_build`.
5. `BEGIN IMMEDIATE`; require `owner=go` and the expected epoch; set `owner=python`,
   `owner_epoch=old+1`, `takeover_id=<new transition>`; COMMIT; publish `phase=starting`.
6. Activate Python under the fence release; replay the remaining inbox entries; publish
   `phase=active`; release `takeover.lock`.

Never restore the `S/takeover-backups/` snapshot as rollback. It would discard every receipt
and ACK accepted while Go was the owner. The backup exists for disaster recovery evidence only.

Rollback is available only while `schema_meta.rollback_allowed == "1"`. The todo-42
rehearsal runs cutover, this rollback (Python at epoch 3), and cutover again (Go at epoch 4),
with row counts and a `schema_meta` dump equal across the three states except for `owner`,
`owner_epoch` and `takeover_id`.

## Commit point

The irreversible point is an operator-authorized, durable write of
`schema_meta.rollback_allowed = "0"` inside a SQLite transaction (`crw relay takeover commit`,
run only after Jun ends the observation window, todo 43). The JSON mirror follows afterwards.
If the command's response is lost, reread the key to learn whether the commit happened.

Only after this point may a later Go release change the schema or the meaning of stored data.
Cached hook compatibility paths have a separate retirement condition (Retention); closing
rollback does not by itself permit removing them.

## Inbox

`S/takeover-inbox/<operation-id>` is the durable ingress that preserves receipts and ACKs across
the interval when no owner is active. It ships in the fence release, not only in Go.

1. Every receipt/ACK ingress carries a stable operation ID and a payload digest.
2. Before reporting durable acceptance, the sender publishes the complete request: write a temp
   file in the inbox directory, fsync it, link it under the final operation-ID name without
   replacing an existing entry (`link(2)`, never `rename` over an existing name), fsync the
   directory.
3. A repeated ID with identical bytes is a retry; the same ID with different bytes is a conflict
   and is refused.
4. The current owner applies the request through the existing handler.
5. The owner commits the domain result and an ingestion dedup/result marker in one DB
   transaction. The marker uses a reserved `schema_meta` namespace; no Go-only schema.
6. The owner unlinks the inbox file only after that commit, then fsyncs the directory.

Crash before application leaves the file. Crash after commit but before unlink causes an
idempotent replay that the marker resolves. A queued request is acknowledged as **durably
queued**, never as applied or verified. A disk-full or fsync failure never receives a durable
acknowledgment; supported senders keep retrying until they get one.

Original identifiers, generations, payloads and unresolved states are preserved: managed
creation IDs are deterministic and an uncertain operation keeps its reservation rather than
authorizing a replacement child (managed.py:1-5, 178-180, 357-383). The non-DB receiver ledger
keeps its own answer-versus-applied distinction and its fsync-then-rename save
(receiver.py:736-786, 788-850).

## Hook budget

Two interfaces stay separate: the native Go Stop hook (host hook output only, exit 0 on every
controlled path) and the legacy `guard-evaluate` command (full verdict envelope, validated by
the old adapter at stopadapter.py:528-618).

Limits in force today:

| Limit | Value | Where |
|---|---:|---|
| registered plugin hook timeout | 10 s | plugins/crw/wiring/hooks/stop-recording-completion.json:9 |
| default guard subprocess budget | 5 s | stopadapter.py:83-85 |
| legacy launcher deadline | `min(timeoutSeconds + 2, 9)`, 7 s by default | plugins/crw/wiring/crw_stop_hook.py:60-73, 118-122 |
| live settings `timeoutSeconds` | 5 s | `~/.codex/crw-completion-hook.json` |
| transcript identity scan | 0.75 s / 64 MiB | stopadapter.py:158-164 |

Go hook: an absolute 5 s end-to-end self-deadline that starts at process entry, shortened when
the invoking configuration supplies a smaller budget. Every nested operation receives the
remaining deadline; nothing starts a fresh timer.

| Allocation | Maximum |
|---|---:|
| startup, bounded configuration and input handling | 100 ms |
| one Unix-socket connection attempt | 50 ms |
| event identity scan when needed | 750 ms |
| guard request and evaluation | 3,500 ms |
| output and final bookkeeping | 100 ms |
| reserved margin | 500 ms |

These are design limits, not measured Go numbers; todo 34's QA replaces them with measurements.

Unreachable fast path (target under 150 ms including startup): read only the small routing
configuration needed to find the socket; attempt one non-blocking Unix-socket connection; on
`ENOENT`, `ECONNREFUSED`, `EACCES` or the 50 ms deadline emit nothing and exit 0. This happens
before any transcript scan and before any durable Stop-event claim. No Python import, relay CLI
subprocess, daemon start, DB open, SQLite busy wait, writer lock, retry loop or synchronous
diagnostic fsync belongs on this path. Nothing is lost by it: the guard reads receipt evidence
and creates no receipts, verdicts or verification facts (guard.py:9-16).

Event arbitration stays byte-identical: event key =
`sha256(json.dumps([EVENT_KEY_TAG, session, turn, active, item], separators=(",",":")))` in
ASCII (stopadapter.py:857-858); claims without an outcome are never deleted or replayed
(stopadapter.py:929-1000).

Exit-status discipline for the Go hook: error-returning flag parsing (never `flag.ExitOnError`),
no `log.Fatal`, no panic-based error paths, panics recovered at every goroutine boundary, stderr
silent, stdout buffered and validated before it is written, exit 0 for malformed input,
ownership refusal, cancellation, unreachable relay, invalid response and timeout. The enforceable
promise is exit 0 on every controlled path with enough margin to avoid host termination; SIGKILL
and an unlaunchable outer bootstrap are outside any hook's control.

## Retention

Rule: a Python interpreter, venv, `.py` entry point or `python3 -c` launcher that any live or
resumable task can still spawn is retained, regardless of DB ownership and regardless of the
commit point. Retirement of a cached Python path happens only when the retention scan reports
zero references to it.

What "live or resumable" means here:

- live: a Stop-event claim without an outcome file, a hook journal row younger than twice the
  longest configured hook timeout, an alive `daemon.json` pid, or an alive `managed-start-*.lock`
  holder;
- resumable: a Codex thread listed by `crw bridge`'s `list_threads` whose last turn is younger
  than the Codex host's turn-command cache lifetime (todo 43 records the value observed on
  codex-cli 0.154.0 via docs/plugin-packaging.md:98).

Until then the following stay in place: the retained fence Python runtime and its interpreter
(`~/.local/share/crw-runtime/env-*`), the `<CODEX_HOME>/crw-stop-hook.py` shim, the legacy
`crw_stop_hook.py` and `crw_bridge_mcp.py` in the plugin package, the `guard-evaluate` CLI
envelope and its settings keys. Replacing a `.py` file with an ELF binary at the same path is not
the protocol.

## Retention scan surface

`crw doctor retention-scan --json` (todo 37) enumerates exactly this fixed list and reports every
reference to a Python interpreter, venv or `.py` path together with its source. Todo 43 removes
nothing until the report's `python_references` is empty.

| # | Surface | What counts as a reference |
|---|---|---|
| 1 | `<CODEX_HOME>/crw-completion-hook/stop-events/*` claims without an outcome file | the claim's recorded command or evaluator path |
| 2 | `<CODEX_HOME>/crw-completion-hook/journal/<day>/*.json` rows younger than the longest configured hook timeout x 2 | the row's recorded command |
| 3 | every alive process recorded in `S/daemon.json` (supervisor and worker pids) | the process's executable and argv |
| 4 | `~/.codex/crw-*.json` (`crw-completion-hook.json`, `crw-bridge-mcp.json`, and any other `crw-*.json` record) | `relayExecutable`, `bridgeExecutable`, `adapterEntryPoint`, `interpreterPath`, `command` values |
| 5 | `~/.codex/plugins/cache/crw/crw/*/wiring/*` | hook and MCP command strings (`hooks/*.json`, `mcp.json`) |
| 6 | every alive holder of an `S/managed-start-*.lock` file (flock holder pid, resolved via /proc/locks), excluding pids already reported by row 3 | the holder's executable and argv |
| 7 | Codex threads from `crw bridge` `list_threads` whose last turn is younger than the host's turn-command cache lifetime | the cached Stop hook command for that thread |

The list is closed: adding a surface is a plan change, not a scan option.
