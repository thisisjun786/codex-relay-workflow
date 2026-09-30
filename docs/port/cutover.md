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
  (`${XDG_STATE_HOME:-~/.local/state}/codex-session-relay/<scope>/`). A relative
  `XDG_STATE_HOME` or `HOME` is read against the working directory, and both runtimes ask the
  stores under that absolute directory which socket each records (decision 45).
- `D`: the existing database pathname inside `S`. Never a freshly computed default.
- `K`: the existing scope key for the App Server socket (service.py:102-159 hashes the resolved
  socket pathname; Go reproduces that canonicalization, it does not substitute `$HOME`).
  `K.lock` and `K.json` are the registry root as spelled plus `/K.lock` and `/K.json`, never
  cleaned: a `..` after a symlink in an isolated root is resolved by the kernel in both
  runtimes, so both lock the same inode.

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
| `S/takeover-inbox/.replay.lock` | permanent-inode exclusive lock held by an inbox replayer from reading an entry until after its unlink and directory fsync |
| `S/control.sock` | stable local control and `guard-evaluate` RPC address served by both runtimes |
| `S/takeover-backups/<transition-id>/` | SQLite backup plus inventory manifest; disaster-recovery evidence only, never rollback input |

Rule that applies to every lock file, old or new: **never unlink it and never replace it
atomically.** The inode is the rendezvous point. Atomic replace is for `takeover.json` only.
Nothing chmods a lock either. An existing lock is trusted when it is a regular file owned by the
effective user and either grants no group or other access (0600, in any directory, as Python and
every earlier Go build trust it) or sits in a directory owned by that user with no group or world
write bit, whatever its own mode (under umask 002 both runtimes leave 0664 locks in 0700
directories). Only a lock that grants group or other access inside a group- or world-writable
directory is refused. Locks Go creates are 0600, so Go never leaves behind a lock file it then
refuses, and a 0775 state directory (Python's `mkdir` under umask 002) serves in both runtimes.

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
`appServerSocket` and `scopeKey` are null exactly when `schema_meta.socket_path` is absent:
an **unbound** store, created by a socketless writable opener (crw-run's
`--state "$RELAY_STATE" register`, a `default/` scope). An unbound store is never a takeover
subject. Otherwise `appServerSocket` is absolute, normalized and equal to
`schema_meta.socket_path`, and `scopeKey` is the non-empty scope-registry key the binding
process recorded for it (including the `isolated-<salt>-` namespace of an overridden registry
root). Both runtimes refuse a record that breaks this (`scope without socket`,
`invalid socket/scope identity`) at admission and at every revalidation. Go also refuses a
`scopeKey` that is not the key the validating process would lock
(`scope key disagrees with lock authority`); Python checks only that it is a non-empty string
until the refactor-backlog item `[todo36][audit 20/24/46]` lands.

Socket binding. A writable open that passes an App Server socket `K` (canonicalized) to an
unbound store owned by the opening runtime, in `phase=active` with `transition` null, binds
it: under `write-gate EX` (bounded, Lock order) it revalidates the rest of the record,
commits `schema_meta.socket_path = K`, then publishes the mirror with `appServerSocket = K`
and `scopeKey` = the key for `K`, every other field unchanged except `updatedAt`. Owner,
epoch, phase and transition never change. A crash between that commit and the publication
leaves `socket_path = K` under a null mirror: every other opener refuses it
(`scope without socket`), and the next writable opener passing `K` completes the same
binding (its preflight treats that record as unbound for `K`). Read-only commands and the
takeover candidate never bind; a store owned by the other runtime or mid-transition is
refused as usual and never bound. So crw-run's socketless `register` followed by
`--socket "$SOCK" deliver|recover|daemon` on the same `--state` works, and a later
socketless `emit` or `ack` still opens the bound store. Opening a bound store with a socket
other than `appServerSocket` is refused before any DDL or socket recording
(`requested socket disagrees with ownership record`; both runtimes answer it as reason
`store_owned_by_other`, exit 2; at the CLI both first answer a socket the store records
otherwise with `state_directory_serves_another_socket`), and initialization of a legacy `D`
records the store's own `schema_meta.socket_path` in the mirror, refusing a requested socket
that disagrees with it. Both runtimes implement the binding: Go in `store.Open`
(`internal/relay/store/ownership.go` `bindSocket`), whose start preflight
(`store.StartPreflight` over `ownership.CheckStart`) takes the command's socket as
`check_start` does, so the opener that completes a torn binding is let through. Like
`cli.py` `main`, Go runs that preflight before `daemon` and every `service` form but
`status`: a store the other runtime owns, or one draining or starting, refuses them before
any lock or record is written, and so does any store Go would not open (a legacy `D`
included, which Go never initializes). The marker commands get `check_start` itself, read in
the fence's order (`store.CheckStartLikeFence`): `main` runs it before the two forms that name
the selected store (`intent-declare` without `--no-db-path`, `intent-register` without
`--db-path`), `intent-claim` runs it on the store the intent names, and `intent-disposition`
meets it opening that store before it publishes (`declarations.Held`). The exception is the
legacy store: with no ownership key and no `takeover.json` it passes, as `check_start` lets
it, so `intent-declare` records it and a claim or disposition is published; only a command
that then opens it meets Go's refusal (`intent-register`'s hold, and the store record a claim
or disposition answers with). An unreadable mirror refuses in the fence's words, and an
unreadable database is the host error `check_start` raises (claim and disposition leave it to
their record, as the fence does). Any other disagreement between the halves has
no automatic repair; before todo 42, check that the chosen store's `appServerSocket` equals
its `schema_meta.socket_path`.

Torn publications. The DB commit always precedes the mirror publication, so a crash, full
disk or permission failure between them leaves the durable half ahead of the mirror. Besides
the transition gaps named in Steps 5 and Rollback, initialization has one: **initial stamp
committed, mirror absent** - all six keys present, `takeover_id` empty, `owner_epoch` `1`,
and `S/takeover.json` absent (ENOENT only; a malformed mirror still refuses as malformed).
Every writer refuses it (non-queueable) and `doctor --json` reports its ownership `detail` as
`takeover record missing`. No opener or writer repairs it. Recovery is an explicit controller
action, `crw relay takeover repair-mirror` (`takeover status` reports the state from the stamp,
with `jsonStale` true), under the full lock order (`takeover.lock`, `daemon.lock`, the scope's
`K.lock` when `socket_path` names one, `write-gate.lock` EX). It refuses every other state,
unchanged, and publishes the mirror derived only from `schema_meta`:
protocol 1, `storeId`, `database` from the physical store, `appServerSocket`/`scopeKey` from
`socket_path` (both null when absent), epoch 1, the stamped owner, `phase=active`, null
`transition`/`holder`/`controller`, `rollbackAllowed` and `pythonCompatibilityBuild` from the
stamp, `relayRPCSocket` = `S/control.sock`. It never writes `schema_meta`.

Read-only clients never create a store and never bind one. Read-only names how a form opens a
store, not a promise that it writes nothing: on a store its runtime may write, a read-only form
is admitted as a writer, and the owner's own `fault-next` expires lapsed leases there (Read-only
clients under a foreign owner). When `D`, `S/takeover.json` and
`S/write-gate.lock` are all absent, a read-only command answers exit 2
`{"error":"refused","reason":"store_absent","detail":"no relay store exists at <D>; a read-only command never creates one"}`
and creates nothing, so the first reader never chooses the owner. A read-only Python command
that finds an existing unfenced (legacy) `D` still performs the Step 0.4 initialization under
the exclusive gate, as every Python fence opener does; Go never initializes an unfenced `D`.

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
  -> write-gate SH | EX   SH for admitted writers; EX for the transfer barrier,
                          store creation/initialization and socket binding
  -> inbox .replay.lock EX inbox replay only, while holding write-gate SH
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
- The read-only Stop path takes none of these locks. It decides ownership and routing from the
  mirror `S/takeover.json` and one in-place read of `schema_meta` that copies nothing and
  creates no SQLite sidecar (Python `ownership.check_stop`/`stop_metadata`, Go
  `store.OpenStopRead` in the native hook's `ownsGuard`). `D` is the file SQLite opens, resolved
  as its unix VFS resolves a path (a symlinked `D` keeps its sidecars beside the file it names),
  and Go's read is refused unless SQLite names that file. With `D-wal` and `D-shm` both present
  it opens `mode=ro`, which sees committed WAL frames and creates nothing; with no `D-wal`, or
  one holding no frame, every commit is in `D` and it reads `D` with `immutable=1`. A `D-wal`
  holding frames beside no usable `D-shm` (an unclean shutdown) has no such read, since an
  immutable read would miss its commits: Go's read fails and the hook asks the owner rather than
  trusting `D` (`store.InPlaceRead`; Python's `stop_metadata` still reads `D` immutable there,
  a deferred review finding). The disposable snapshot copy
  (`metadata`, `SnapshotMeta`) stays with writers' admission and diagnostics; on a Stop it was a
  write outside the hook's root and a copy of the whole live store per turn end.
- Two waits that another writer can hold for an unbounded time are bounded by 30 s, the
  store's SQLite busy timeout and Go's default `BusyTimeout`: the socket binding's
  `write-gate EX` (other admitted connections hold SH for their lifetime) and the inbox
  `.replay.lock EX` (a replayer holds it across its handlers' App Server calls). When the bound
  expires the command has changed nothing and answers a retryable host error (exit 3 in both
  runtimes,
  `{"error":"host","detail":"LockWaitExpired: <what> was not acquired within 30s; retry"}`); a
  daemon's startup recovery fails the same way. Python's admission `write-gate SH` and the
  initializer's `EX` keep blocking. Go's admission never blocks on the gate, with one
  exception taken before it: a Go writer that finds the gate held EX while the store is being
  created (no `D` or no mirror) or is in phase active (a creation finishing, a socket binding)
  waits until it can share the gate, bounded by its busy timeout (30 s), then is admitted or
  refused as usual. A transfer barrier is taken only after the record left phase active, so
  a Go writer still refuses it at once, with the refusal the fence's lock-free `check_start`
  gives that record (a draining store: queueable, so a Go `emit`, `ack` or
  `fault-notification-ack` is durably queued, Wire format), and a read-only form never waits
  (it reads `mode=ro`). Go's inbox replay (`internal/relay/inbox`) uses the same bound.
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
   Only the Python fence initializes an existing unfenced (legacy) `D`. A truly absent store
   (no `D`, no `takeover.json`, no `write-gate.lock`) is created by the first writable opener
   of either runtime with itself as owner at epoch 1, under the exclusive gate it created.
   The creator places `write-gate.lock` already held EX: it creates the gate under a temporary
   `S/.write-gate-*` name, locks it, then `link(2)`s it into place (losing to an existing name
   means another opener created it), so a concurrent first opener that finds the gate waits
   for the creation instead of seeing a gate without a database. Both runtimes place it so
   (Go: `internal/relay/store/ownership.go` `placeGate`), and a Go first opener waits for such a
   gate within the bound Lock order names. Anything partial - a
   `write-gate.lock` with no `D` that this opener found rather than created - is refused and
   never repaired by a runtime.
5. Confirm every launch path reaches the fence build: `doctor --json` shows
   `python_compatibility_build` equal to the fence build for every running relay process.

Failure and recovery: if any old writable process or uncontrolled launch path can't be excluded,
do not cut over. Python stays in control. Writing `owner=go` would not repair the missing
precondition, and a Go opener cannot infer it from the database. If step 4 stops between the
`schema_meta` COMMIT and the `takeover.json` publication, the store is in the torn state
**initial stamp committed, mirror absent** (Record): writers refuse, step 5's `doctor --json`
fails loudly with `takeover record missing`, and only the explicit mirror recovery,
`crw relay takeover repair-mirror`, completes the publication. If a first opener dies after
placing the gate and before creating `D`, `S` holds only `write-gate.lock` (and possibly a
stray `S/.write-gate-*`): every writable opener refuses it non-queueably with
`partial store: write-gate.lock without a database` (Go words it `ownership refused: existing
database required: lstat <D>: no such file or directory`, reason `store_owned_by_other`, exit 2,
after finding the gate unheld).
Recovery is an operator action, the only removal of a lock file this protocol allows:
confirm that `D` and `S/takeover.json` are absent and that no process has the gate open
(`fuser S/write-gate.lock` or `lsof` lists none), then remove `S/write-gate.lock` and any
`S/.write-gate-*`; the next writable opener creates the store.

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

The abort is `crw relay takeover abort`. Under `takeover.lock` it is accepted only while the
phase is `draining` and the DB stamp still names the old owner at the old epoch, that is, before
the Step 5 COMMIT. It writes no `schema_meta` key and takes no barrier: it republishes
`takeover.json` for the unchanged owner and epoch with `phase=active` and the installed
transition rebuilt from `takeover_id` (null when `takeover_id` is empty). Repeating it on an
active record changes nothing. After the COMMIT it refuses and names the recovery (Step 5).
A draining that the reverse transfer began from `phase=starting` (Step 5: a candidate that
never became ready, so its mirror names no holder) returns to `phase=starting`, not active:
no ready holder exists to advertise, and the recovery stays `takeover activate` or the
reverse transfer.
The drained runtime's service does not restart by itself: after an abort the operator starts it
again (`service start`), and entries queued in the inbox meanwhile replay on its next writable
start or writable command, whichever runtime owns it. The same command aborts a rollback that
has not reached its COMMIT, back to active Go.

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
stop or abort the transition back to active Python (`crw relay takeover abort`).

### Step 3: acquire the transfer barrier

Still holding `takeover.lock`, take `daemon.lock EX`, then scope `K.lock EX`, then
`write-gate.lock EX`, each with a bounded wait. All three together prove exclusion: the daemon
and scope locks exclude lingering service holders, the exclusive gate excludes direct CLI
connections and admitted operations that the daemon lock does not cover.

Failure and recovery: release whatever was acquired, in reverse order, and stay in draining.
Find and finish the holder, then resume, or abort to Python (`crw relay takeover abort`).
Contention is never a reason to delete a lock file, and
a stale timestamp or a dead supervisor PID never bypasses this barrier.

### Step 4: preserve and inspect the existing state

While holding the barrier: revalidate the physical database and WAL identity against the
`database` block, open the existing `D`, validate schema compatibility and `PRAGMA
integrity_check`, create a consistent backup through the SQLite backup API into
`S/takeover-backups/<transition-id>/`, fsync the backup and its directory, and record the
receipt/ACK, attempt, inbox, receiver-ledger and transport-ledger inventory in the manifest.
The takeover inbox is read here as the candidate's drain will read it: first its replay lock
`S/takeover-inbox/.replay.lock`, opened for writing without following a link (or, where there
is none, the directory writable, as creating it needs), neither created nor taken; then the
names the entry grammar admits directly in `S/takeover-inbox`, each opened without following a
link and validated as the replay validates it. A lock the drain could not open (a symbolic
link, a directory, a file or inbox this user may not write) or an entry it would fail closed on
(a symbolic link, a non-regular file, bytes that are no valid entry) refuses the transfer with
the drain's own error, while the owner has not changed. Any other name, and anything below a
subdirectory, is no entry, is not inventoried and refuses nothing.

Never copy only the main DB file, never require WAL truncation as a precondition, never delete
WAL or SHM to make the store look clean.

Failure and recovery: the owner does not change. Close the inspection connection, repair the
inspection or backup failure or abort to Python (`crw relay takeover abort`), leave the
original files untouched.

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
disagreement. `takeover abort` refuses from here on, because ownership has moved: the recovery
is `takeover activate`, or the reverse transfer, which `crw relay takeover rollback --to python`
begins from `owner=go phase=starting` and `crw relay takeover begin --to go` begins from
`owner=python phase=starting`, each installing `owner_epoch=current+1`. If that reverse
transfer fails before its own COMMIT, `takeover abort` returns the store to this
`phase=starting`, never to active.

### Step 6: start Go on the original store

Release `write-gate EX`, `K.lock` and `daemon.lock` in reverse order; keep `takeover.lock`.
Start the Go candidate with the expected `transition.id` and epoch: the controller executes
`crw relay --state S --socket K service run --takeover-candidate` (plus `--allow-isolated-scope`
for an isolated scope registry) as its direct child, in a new session, with the activation
channel on fd 3 (decision 28). The candidate is the service supervisor, run under the service's
launch declaration exactly as `service start` runs it, not a bounded worker segment; the
service intent must be enabled. Go then takes `daemon.lock`,
`K.lock`, `write-gate SH`; verifies DB and record ownership agree; opens `D` in existing-file
mode (never create); applies `journal_mode=WAL`, `synchronous=FULL`, `foreign_keys=ON` on every
connection; recovers pending operations under their existing identities; drains compatible
inbox entries; binds `S/control.sock`; and reports readiness with store ID, epoch, build and
process identity. Readiness comes only after recovery succeeds, matching today's supervisor
(service.py:1926-1945). During `phase=starting` only the designated candidate may enter;
ordinary client mutations stay queued. After activation the supervisor releases its readiness
listener and starts workers under ordinary active admission; the first worker binds
`S/control.sock` for its lifetime. Workers inherit neither the channel nor its selector.

The Go candidate drains the takeover inbox in its recovery, under its starting permit and before
`RecoverOnStart`, exactly as the Python candidate replays it (Wire format): every entry present
when it drains, whether queued while the store drained or after Step 5, is applied or retained
before readiness, and one queued later is drained at its first worker's daemon start or by the
next writable command. Step 5 does not wait for an empty inbox. An entry the drain cannot apply
(an invalid entry, a host or storage failure) fails recovery: no readiness, ownership stays
starting, the entry is kept. Entries the retained Python CLI queues while Go is active are
applied by the Go owner's next drain, and `takeover commit` closes the way back to Python only
once a drain has emptied the inbox (Commit point).

The controller checks the candidate's launch preconditions before Step 1 and again before
Step 5 and before launching it: the service intent is enabled, the launch declaration is not
refused, and Go can open `D` (the live-state guard). The wait for readiness spans recovery and
is bounded by the controller's `--ready-timeout` (default 600 seconds, on `takeover activate`
and `takeover rollback`); the 20-second channel bound covers the candidate's validation of
`start` and, separately, the activation exchange after `ready`. The candidate sets no bound of
its own on recovery, and the start bound never carries over: both candidates (Go
`CandidateChannel.Ready`, Python `CandidateChannel.ready`) send `ready` and wait for `active`
or EOF under a fresh 20 seconds, as the controller's `Activate` does after readiness.

Failure and recovery: ownership stays Go, no active service is advertised, inbox entries are
retained. Restart Go, or run the reverse protocol to the fence Python release. Never launch an
unfenced Python fallback because Go failed. If the controller dies before activation, the
candidate either observes a matching durable active record or quiesces when its activation
channel closes; no indefinitely writable, unadvertised candidate may remain.

### Step 7: publish active Go and switch new hook registrations

After readiness: publish `phase=active` with the Go owner, epoch and verified holder identity
(the candidate supervisor's; diagnostic only: a later `service start` or `restart` does not
rewrite it, and a dead holder PID authorizes nothing);
point new host registrations at the native Go Stop hook; preserve legacy settings and cached
executable paths; release `takeover.lock`.

The plugin payload that declares the native wiring (its Stop command runs `crw hook
--plugin-launch`, decision 26) is installed only after the pointer names a Go runtime built with
decision 26. It is never cached beside a pointer at a Go runtime built before it, which releases
every Stop without output or a journal row, or beside one at a Python `env-*` runtime. The
order, and the reverse one for a rollback, are in docs/plugin-packaging.md "Turning the wired
surfaces on" and "Update and roll back". `crw install rollback` refuses, with nothing written, to
point at a Python `env-*` runtime while a cached plugin version declares that wiring
(docs/runtime-install.md "Rolling back").

The legacy `guard-evaluate` CLI (stopadapter.py:69, command assembled at invocation time from
settings at stopadapter.py:418-442) keeps its flags and its full verdict envelope so cached
Python adapters keep working. The Go `guard-evaluate` must not claim the Stop event again; the
Python adapter already claimed it.

Failure and recovery: if registration or configuration publication fails, Go remains the owner
and the retained legacy hook path still reaches a compatible evaluator. Repair the
configuration; do not touch DB ownership.

### When the other runtime owns the record

Fence Python finding `owner=go`: no writable open, no initialization DDL, no refusal journaling,
no auto-restart. A Stop's `guard-evaluate` goes to `S/control.sock`; a receipt or acknowledgment
(`emit`, `ack`, `fault-notification-ack`) is published in the durable inbox as a file (Wire
format), never sent over the socket; every other CLI request returns an explicit
ownership/retry error; a Stop invocation turns that into empty stdout and exit 0.
`S/control.sock` serves `guard-evaluate` only, in both runtimes: an `inbox-submit` request is
rejected like any unknown method (the Go build's former `inbox-submit` placeholder, which no
sender ever used, is outside decision 25 and was removed with todo 31). An already-admitted
Python writer delays the takeover through its shared gate; it is never displaced.

Once a Python client has sent a guard request on `S/control.sock`, it never falls back to a
local evaluation and never invents that ownership refusal, whatever the owner: a missed
deadline is `guard_timed_out`, EOF without bytes `guard_said_nothing`, unreadable bytes
`guard_output_unreadable`, and a reset or broken pipe an adapter fault, as the Go client
classifies them. The legacy `guard-evaluate` CLI answers the first three with exit 3
`{"error":"host","detail":...}`. The Python control server accepts frames up to decision 24's
64 MiB, answers a request it could not evaluate with `{"error":"host","detail":...}` and keeps
serving, closes an unauthenticated peer unanswered, and applies the CLI's selection refusals
to an unpinned request using its `socketPath` and `program` (decision 24). The Go owner's server
(`service.ListenControl`, `hook.HandleControl`) reads a request as `control.py` reads it and
answers failures the same way. The line is `json.loads` of its bytes: decoded as `json.loads`
decodes bytes (UTF-8 with or without its byte order mark, UTF-16 or UTF-32 by their marks or
NUL bytes, a lone surrogate passed and kept), then scanned to the nesting the C scanner reaches
in `GuardServer`'s serving thread (9996 containers under CPython 3.13; the 9997th raises
`RecursionError`). The deadline is `datetime.fromisoformat` of it as CPython 3.13 parses it
(after `Z` becomes `+00:00`), subtracted from the aware present. A frame whose bytes do not
decode, that is not a JSON object, nests too deep, lacks `params` or `stopInput`, carries a
deadline that is not a string, does not parse, has no offset or has passed, or a `socketPath` or
`program` that is not a string, is answered by both owners with the same host detail,
`control.py`'s `<exception class>: <message>` (`TypeError: guard params must be an object`,
`TypeError: can't subtract offset-naive and offset-aware datetimes`, `TimeoutError: guard
request deadline expired`), and a peer that has not finished its request line when the 5 s
read timeout expires is answered `TimeoutError: timed out`. Nothing a peer does ends or fails
either owner (PR #185 4128954449): a peer that goes away before its answer is skipped, an
accept the kernel refuses for want of descriptors, buffers or memory is retried after 50 ms,
and neither reaches the daemon's exit status. One difference is kept: `control.py`'s timeout
applies to each read, so a peer that trickles its line, each part within 5 s of the last, is
served by Python however long it takes and answered `TimeoutError: timed out` by Go 5 s after
it connected; the Go owner serves each connection concurrently and bounds each one whole.

Every client sends those two selection inputs: the Go hook client (`RequestGuard`), the Stop
adapter's pinned route (`socketPath` from its settings, `program` its `relayExecutable`) and
the legacy `guard-evaluate` CLI (its `--socket` and the program its own refusals print). An
owner resolves an unpinned Stop's store in its own configuration with them, so a routed Stop
is refused exactly as the CLI's local fallback refuses it under the owner's configuration;
the CLI first applies its own read-only preflight in the Stop's environment, so a routed Stop
is refused wherever either side's selection refuses it (PR #185 thread 4127894191).
That preflight makes the selection and nothing else (`guard.selected_store`: the gates the
evaluation passes before its receipt read, then the resolver exactly where the evaluation would
call it). It reads no receipt and evaluates nothing, so a routed Stop's one receipt read and
evaluation happen at the owner, and a Stop evaluated locally resolves its selection once and
reuses it (PR #185 thread 4128457287). Each read is bounded on its own (`SQLITE_TIMEOUT`, 2 s),
so the evaluating preflight spent one Stop budget twice: with the store held exclusively, a
Stop whose intent records `dbPath` took 4.2 s through the legacy CLI and now takes 2.2 s. Where
a Stop falls back to the run's own selection, its selection read stays on both sides (the
store's recorded socket; decision 24 applies both sides' selection), so that path spends three
bounded reads where it spent four, 6.4 s held instead of 8.4 s, still past the adapter's 5 s.
The Go hook and the Go CLI evaluate once, in process or at the owner, and Go's selection reads a
snapshot copy that waits on no lock.

The owner decides where a Stop is evaluated, never the requester (PR #185 thread 4127894432).
Both owners (Python `control.py` `owner_paths`, Go `hook.HandleControl` `ownerPaths`) evaluate
only under their own marker root, the one the relay resolves without `--marker-root` in the
owner's configuration (`CODEX_SESSION_RELAY_MARKER_ROOT`, then `XDG_STATE_HOME`, then the home
default; the installer records the same root as the hook's `markerRoot`), and read only their
own store `S/relay.sqlite3`. A request's `markerRoot`, and its `dbPath` when not null, must be an
absolute path naming that root or store, by the same normalized spelling or as the same existing
file; the owner then uses its own path, never the requester's spelling. Anything else is
answered before any read or record as a host error that both owners word alike,
`control.sock evaluates Stops only under this owner's marker root <root>; the request named <markerRoot>`
or `control.sock reads only this owner's store <S/relay.sqlite3>; the request named <dbPath>`
(`no path` for a non-string), which the adapters journal as `guard_host_error`. So a
same-user client cannot make the owner write observations or holds under another root, or
SQLite sidecars beside another store. `journalRoot` and the other hook settings are not request
parameters: the adapter journals in its own process. The legacy CLI sends its `--db-path` as an
absolute path. An owner whose configuration resolves another root than the hook's settings
answers every routed Stop with that host error; set `CODEX_SESSION_RELAY_MARKER_ROOT` in the
service's environment to the hook's `markerRoot` when an installation chose its own.

Go finding `owner=python`: normal daemon startup refuses to open the DB writable; only an
explicit transition controller may start the protocol; a Go `guard-evaluate` uses the compatible
endpoint, a Go `emit`, `ack` or `fault-notification-ack` is queued in the inbox exactly as the
fence queues it, and every other Go writer refuses. There is no "lock looks stale, start another
daemon" path.

The `guard-evaluate` CLI routes a Stop the same way in both runtimes (`cli.py`
`cmd_guard_evaluate` and `stopadapter.socket_guard`, Go `internal/relay/cli/guard.go` and
`hook.RouteGuard`). A Stop judged on its marker alone reads no store, so it is evaluated
in-process whoever owns one. A Stop that reads its receipt (`guard.selected_store`,
`hook.SelectedStore`, whose selection refusal is answered first) is sent to `<S>/control.sock` of
the store it reads: `--db-path` (sent as an absolute path), else the `dbPath` the intent
recorded, else the selected store. The request is sent only once the peer is established as this
user's direct socket in a directory no group or other user may write, and within one 5 s budget.
The owner's answer is the command's, with the exit status its error record carries (`refused` 2,
`host` 3, `usage` 4). An owner that does not answer in time is `{"error": "host", "detail": "the
owner did not answer guard-evaluate within 5s"}` and one that says nothing readable (no bytes,
bytes that are not UTF-8 or not JSON, more than 64 MiB of them, or the JSON value `null`) is
`{"error": "host", "detail": "the owner closed control.sock without a readable guard-evaluate
answer"}`, both exit 3. Bytes that are not UTF-8 are unreadable even where replacing them would
leave JSON, an error record included. A failure after the request was sent is a host error too,
never a refusal or a second evaluation. When the socket cannot be reached or trusted, the CLI
evaluates in-process only where `takeover.json` is absent or names its own runtime as owner,
after the read-only Stop path has checked that store (`ownership.check_stop`, `store.CheckStop`:
no copy, no sidecar), which refuses a store of its own that is draining or mid-transition.
Otherwise it refuses, exit 2 `store_owned_by_other`, `the owner could not answer guard-evaluate:
<error>` in Python's `str(OSError)` words
(`TestGuardEvaluate_routes_to_the_owners_control_socket_as_the_fence_does`, against the live
fence and a live Python owner).

### Read-only clients under a foreign owner

Read-only Store transactions use deferred `BEGIN` with `query_only=ON`, never
`BEGIN IMMEDIATE`. On a store opened this way - one its runtime may not write - the
housekeeping a writer persists is projected and never written: `fault-notifications` projects
expired leases and withdrawn faults, and `fault-next` projects lease expiry (below).
The connection remains `mode=ro`, not `immutable=1`: immutable reads ignore
committed WAL frames and can report stale domain data or ownership. `mode=ro`
may create empty WAL/SHM sidecars for a checkpointed WAL database; these are SQLite
coordination files, not admitted domain writes, and are never removed by the
client. Avoiding them by immutable reads would change behavior when the Go writer
is live. The live-WAL regression test checks this distinction.

Both runtimes serve the same read-only forms: `cli.py` `READ_ONLY_COMMANDS`, plus
`fault-policy` without `--fault-class`, `fault-limit` without `--kind`, `service status`
and `store-challenge --read`. A read-only form first asks the lock-free ownership
preflight; where this runtime may write (its own active store) it opens the admitted
store as a writer command would, and on any ownership refusal it reads through the
`mode=ro`, `query_only` store instead, taking no write gate. Go's admission does not wait
for an exclusively held write gate: that contention is a refusal, so the read goes
`mode=ro`, where Python's blocking `LOCK_SH` waits for the gate.
The read-only classification decides how a form opens the store, never what the owner's own
housekeeping does. The owner runtime's `fault-next` on its own store is admitted as a writer
and expires lapsed leases there (`expire_leases`, which nothing else calls): it writes, as
every `fault-next` did before the fence, so a claim whose worker died is offered again and
`fault-claim` takes it. Only a `fault-next` on a store its runtime may not write (one its
ownership preflight or write admission refuses: the other runtime's, or one mid-transition),
which it opened read-only, projects that expiry on a private SQLite backup
(`readonly_queue_state`) without writing it to the store. Both runtimes branch on how the
store was opened (`services.store.read_only`, Go's `Store.ReadOnly`), never on the command's
read-only classification. Its `--limit` is checked before the store is opened, as
`cmd_fault_next` checks it before its first store access. For a store that exists, the answer
is the same whichever runtime serves it, except that `doctor`'s `ownership`, `access`, `actorReachability`,
`accessReceipt` and `runtime` blocks are each runtime's own diagnosis (decision 31).

A read-only client never creates, initializes, binds or repairs a store. When `D`,
`takeover.json` and `write-gate.lock` are all missing, a read-only form that opens the
store answers `{"error": "refused", "reason": "store_absent", "detail": "no relay
store exists at <D>; a read-only command never creates one"}` with exit 2 and leaves `S`
uncreated, in both runtimes alike (Record). A partial store (`takeover.json` or
`write-gate.lock` without `D`) is refused by Go with the answer its writer admission gives
it: reason `store_owned_by_other`, detail `ownership refused: existing database required:
lstat <D>: no such file or directory`, exit 2, and `S` unchanged. The forms that do not open the store this way (`ack-proof`,
`dispositions-show`, `doctor`, `guard-evaluate`, `intent-show`, `managed-show`,
`merge-evidence`, `packet-check`, `reporting-derive`, `reporting-show` and
`service status`) answer an absent store in their own shape and create nothing either. Only a writer command or a daemon initializes an absent store (decision 30).
Where a command's own argument refusal falls follows `cli.py` `main`: a read-only form's
`Services.store` is lazy, so a handler that refuses its arguments before reaching the store
(`merge-turn-show`'s selectors, `linkage-up`'s `--scope`) answers that refusal and creates
nothing, never `store_absent`; a write form's store is opened by `_ownership_preflight` before
its handler, so its own argument refusal comes after admission: an absent store is left
initialized and a store another runtime owns answers `store_owned_by_other` (decision 31).
The Python half for a partial store is pending (refactor-backlog.md, Deferred review
findings, audit 25): until it lands, the fence answers a read-only form on a gate-only or
mirror-only store with a host error (exit 3), creating nothing.

### The live-state guard (until todo 43)

The Go build refuses a database under `~/.local/state/codex-session-relay` or
`$XDG_STATE_HOME/codex-session-relay` unless `CRW_ALLOW_LIVE_STATE=1` is set. The todo-42
runbook exports it for every step that runs a Go process against the live state: the
controller, and through it the candidate, the supervisor and each worker, which all inherit
the controller's or service's environment. A read the guard refuses reports the refusal,
`store: live state requires CRW_ALLOW_LIVE_STATE=1`, instead of answering as if the store
could not be opened. A read-only form whose store open the guard refuses, including
`packet-check`'s store reading, answers it as a host error (exit 3). The owner's `control.sock` answers a Stop's `guard-evaluate`
with the same host record (`{"error": "host", "detail": ...}`, as `control.py` answers a
guard that raised), and the Stop adapter journals it as `guard_host_error`. The fault
sweep's managed readings name it in their `unmeasured` reason
(`store_unreadable: store: live state requires CRW_ALLOW_LIVE_STATE=1`). Removing the
guard is a todo-43 change.

## Rollback

Rollback is the same protocol run in reverse, Go to the retained fence Python release, on the
**same live database**, installing `owner_epoch = current + 1`:

1. Controller takes `takeover.lock`, publishes `phase=draining`, `to=python`.
2. Drain Go: finish admitted operations, close writable connections, stop the Go daemon by
   verified identity.
3. Acquire `daemon.lock EX`, `K.lock EX`, `write-gate EX`.
4. Validate that the running Python build equals `python_compatibility_build`. The retained
   entry point is named explicitly, never searched for: `crw relay takeover rollback --to python
   --python-relay <absolute path of the retained fence release's codex-session-relay>`, and
   `takeover activate --python-relay <path>` when a Python activation is resumed. Before step 1
   and again before step 5 the controller refuses unless that path is an executable file, the
   service intent is enabled (the Python candidate is its supervisor) and the launch declaration
   is not refused, so a missing precondition never leaves the store at `owner=python
   phase=starting`. The build itself is proven at readiness: the candidate's holder identity
   must carry `build == python_compatibility_build`.
5. `BEGIN IMMEDIATE`; require `owner=go` and the expected epoch; set `owner=python`,
   `owner_epoch=old+1`, `takeover_id=<new transition>`; COMMIT; publish `phase=starting`.
6. Activate Python under the fence release: the controller executes `<python-relay> --state S
   --socket K service run --takeover-candidate` (plus `--allow-isolated-scope` for an isolated
   registry) directly, with no shell or `uv` wrapper, as its child in a new session with the
   channel on fd 3; the Python candidate replays the remaining inbox entries and recovers; the
   controller publishes `phase=active`; release `takeover.lock`.

A rollback that has not reached its step-5 COMMIT is aborted with `crw relay takeover abort`:
back to active Go when it began from active Go, and back to `owner=go phase=starting` when it
began from a Go candidate that never became ready (the reverse transfer of cutover Step 5),
where the recovery stays `takeover activate` or the rollback. After the COMMIT the recovery is
`takeover activate --python-relay` or the reverse transfer `crw relay takeover begin --to go`
(Step 5).

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
Between that COMMIT and the mirror publication, admitted Go writers treat a mirror
`rollbackAllowed=true` against the DB's `"0"` (owner Go, same epoch, phase active) as committed
and keep serving; every other `rollbackAllowed` disagreement still refuses. If the command's
response is lost, rerun `crw relay takeover commit`: it is idempotent, rereads the key and
republishes the mirror.

The commit ends every way back to a Python owner. While Go owns the store the retained Python
CLI queues its `emit`, `ack` and `fault-notification-ack` in `S/takeover-inbox` (Inbox), and the
Go owner applies them at its next drain (Wire format, "Both owners drain": a writable relay
command, daemon start, the service supervisor's recovery). The controller holds no handler
table and no App Server host, so the commit applies nothing itself; it closes the way back only
over an empty inbox, so that every entry queued while that way was open has been applied by a
Go drain, not merely accepted. `takeover commit` refuses, inside its write transaction and
before the CAS, while `S/takeover-inbox` holds any entry: every name the replay reads (the entry
grammar in Wire format; Go's replay and the commit share one classification,
`ownership.IsInboxEntry`), whatever its bytes. A name outside that grammar, such as an
unpublished `.tmp-` file, is never read or removed by a replay and blocks nothing. The refusal
(`store_owned_by_other`, exit 2) changes nothing and names the recovery: a writable Go relay
command with the service's socket (for example `crw relay --socket <socket> store-challenge
--write`), which drains before its handler, or a restart of the Go service, whose recovery
drains; then rerun the commit. An entry a drain keeps holds the commit back until a later drain
applies it: a receipt whose host could not confirm its turn is retained for the next drain, and
an invalid entry fails every drain closed until the operator moves it out (Wire format).
Meanwhile `takeover rollback --to python --python-relay <path>` stays available, and its Python
candidate replays the inbox before readiness. No other controller step ends the way back:
`takeover abort` and `takeover rollback` keep `rollback_allowed=1`, and the owner either leaves
replays the inbox at its next drain. A receipt the retained Python CLI queues after the commit
is applied by the Go owner's next drain like any other (PR #185 thread 4128457226;
`Test30CommitRefusesOverQueuedInboxEntries`).

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
   and is refused while the earlier entry still exists. After that entry is retired the name is
   free again; replay then records the reused ID as a terminal conflict (Wire format below).
4. The current owner applies the request through the existing handler.
5. The owner commits the domain result and an ingestion dedup/result marker in one DB
   transaction. The marker uses a reserved `schema_meta` namespace; no Go-only schema.
6. The owner unlinks the inbox file only after that commit, then fsyncs the directory. It holds
   `S/takeover-inbox/.replay.lock` (`flock` EX, created `0600` with `O_NOFOLLOW`, never unlinked
   or replaced; waited for at most the Lock order bound, then a retryable host error) from
   listing and reading entries until after that unlink and directory fsync, and
   unlinks only the entry whose bytes it read (same device and inode), never a newer entry that
   a sender linked under the same name.

Crash before application leaves the file. Crash after commit but before unlink causes an
idempotent replay that the marker resolves. A queued request is acknowledged as **durably
queued**, never as applied or verified. A disk-full or fsync failure never receives a durable
acknowledgment; supported senders keep retrying until they get one.

A queued request is judged when it is applied, not when it was accepted: the envelope carries no
acceptance time. A durably queued ACK can therefore go stale. A lease-bound
`fault-notification-ack` applied after its lease lapsed is refused (`fault_claim_stale`) and the
notification stays uncertain; the sender resolves it with
`fault-notification-reconcile --delivered yes --ref <ref>`. Entries queued while Go owns the store
are applied by the Go owner (Wire format, "Both owners drain"), and `takeover commit` refuses
while any is still queued (Commit point).

Original identifiers, generations, payloads and unresolved states are preserved: managed
creation IDs are deterministic and an uncertain operation keeps its reservation rather than
authorizing a replacement child (managed.py:1-5, 178-180, 357-383). The non-DB receiver ledger
keeps its own answer-versus-applied distinction and its fsync-then-rename save
(receiver.py:736-786, 788-850).

### Wire format (decision 25)

Both owners use canonical JSON: `json.dumps(value, sort_keys=True,
separators=(",", ":"), ensure_ascii=False).encode("utf-8")`, without a trailing
newline. An entry is the canonical encoding of:

```json
{"inboxVersion":1,"operationId":"<command>.<stable-key>","command":"<CLI subcommand>","arguments":{},"payloadDigest":"sha256:<hex>"}
```

`arguments` contains the subcommand's parsed option values keyed by argparse dest;
values equal to their defaults are omitted. Values retain their parsed types
(string, integer, boolean, or list of strings). Global routing options are not
included: the entry is already addressed to S. The payload digest is lowercase
SHA-256 hex over canonical `arguments`, prefixed with `sha256:`. There are no
clock, process, or host fields in the envelope.

The queueable ingress set is closed:

| Command | Stable key | Receipt/ACK ingested |
|---|---|---|
| `emit` | First 32 hex digits of payload digest | Child completion receipt; event ID is derived only inside the handler, not supplied as a parsed option |
| `ack` | `event` | Parent acknowledgment |
| `fault-notification-ack` | `notification` | Notification delivery acknowledgment |

Verification claims/verdicts, publication completion, reconciliation, receiver-local
ledger writes, and operator state changes are not receipt/ACK ingress. They do not
queue. `supervisor-read` is a host-verified readback operation, not an authored
receipt: new calls refuse with `store_owned_by_other` rather than queue. Entries
accepted by the preceding fence build remain readable using the `message` stable
key; they replay through the existing handler, including its terminal usage result
when no host socket is supplied. Queueing happens only on admission refusal for another runtime's ownership
or `phase=draining`; malformed/mismatched/unsupported records still refuse. Both runtimes judge
that refusal first with the lock-free `check_start`, before the selection refusal and before
`--kind-module`, and again at the writable open: under another owner a `--socket` that
disagrees with the store's recorded socket, or a `--kind-module` that cannot be imported,
does not stop a queueable request from being queued.

The operation ID is `<command>.<stable-key>`. Encode every UTF-8 byte outside
`[A-Za-z0-9._-]` as uppercase `%XX`; the encoded filename is at most 200 characters.
An overlength identifier is rejected, never truncated into another operation's ID.
Publish via `.tmp-<operation-id>-<pid>-<random hex>` in the inbox directory:
write, file fsync, `link(2)` to the final name without replacement, directory fsync.
Remove the temporary name after publication or failure. A failed publication receives
no durable acknowledgment. A byte-identical retry that finds the final name acknowledges
only after its own successful directory fsync.

An inbox entry is a directory entry whose name does not begin with `.`, matches
`(?:[A-Za-z0-9._-]|%[0-9A-F]{2})+` and has at most 200 characters, and that is a regular
file checked without following a link (`O_NOFOLLOW`, `fstat`; a FIFO is never waited on).
Readers ignore every other name, exactly like `.`-names, and never unlink it. A
grammar-valid name that is a symbolic link or not a regular file is refused by name
(`invalid takeover inbox entry: <name>: symbolic link` or `...: not a regular file`) and
never followed. A grammar-valid regular file whose bytes do not validate (not canonical
JSON, `inboxVersion` other than the integer `1`, unknown command, mismatched name,
arguments, identifier or digest) is storage corruption. Both runtimes keep it and fail
closed: the writable command exits 3 and the daemon does not start. Recovery is an
operator action: move that one named entry out of `S/takeover-inbox/` (keeping it for
diagnosis) and rerun; never edit it in place.

CLI answers use the ordinary JSON stdout envelope:

- New entry or byte-identical retry: exit 0,
  `{"status":"durably_queued","operationId":"...","payloadDigest":"...","detail":"durably queued; not yet applied"}`.
- Same ID with different bytes while the earlier entry exists: exit 2,
  `{"error":"refused","reason":"inbox_conflict","detail":"..."}`; preserve the existing file.
- An identifier that encodes to more than 200 characters, or an argument that is not
  valid UTF-8: exit 4, `{"error":"usage","detail":"..."}`. This is decided before any
  I/O: no temporary file, no final entry, never retried as a host error.
- Write, file-fsync or link failure (before `link(2)` succeeds): exit 3,
  `{"error":"host","reason":"inbox_unavailable","detail":"..."}`; remove the temporary
  file and publish no final entry.
- Directory-sync failure after a successful `link(2)`, or after finding a byte-identical
  final entry: exit 3 with the same envelope and no acknowledgment. Remove the temporary
  file but never unlink the final name: another sender may already hold a durable
  acknowledgment for the same bytes. The entry is a valid publication; the owner applies
  it once or resolves it through its marker, and a byte-identical retry answers
  `durably_queued`.

Only `error`, `reason` and the exit code are normative in these answers; `detail` text is
implementation-specific (Python prints `<exception class>: <message>`). A publisher never
removes a linked final entry; only the owner does, after committing its marker.

The applying owner's transaction reads `schema_meta['inbox:<operation-id>']`:

- Same `payloadDigest` and `exit` the integer `0`: the request was applied; the handler
  is not run again and the entry is retired. This is the only deduplication, and it is
  what makes a crash between commit and unlink replay the marker, not the handler.
- No marker, or the same digest with a nonzero exit: run the existing handler inside a
  savepoint and record its domain result together with the canonical marker value
  `{"payloadDigest":"...","exit":0,"answer":{}}` (the actual integer exit and handler
  stdout object replace the example), replacing any earlier marker for that key. A
  refused request is thus judged again when a byte-identical retry is applied, as a
  direct call would be. A domain refusal rolls back handler writes and stores its
  exit-2 answer. A usage refusal (exit 4), including a replay whose required global
  arguments cannot be supplied, likewise rolls back handler writes, stores the exact
  usage answer in its marker, and retires the entry.
- A different digest (a retired ID reused with different bytes): a terminal conflict.
  Never run the handler. In the same transaction insert, if absent,
  `schema_meta['inbox-conflict:<operation-id>:<first 16 hex digits of the digest>']` =
  canonical `{"payloadDigest":"<new digest>","exit":2,"answer":{"error":"refused","reason":"inbox_conflict","detail":"committed inbox payload differs"}}`,
  leave the `inbox:` marker unchanged, commit, retire the entry and go on draining.

Retained, never terminal: an ownership/admission refusal raised by the handler after
the entry's transaction opened (Python `OwnershipRefused` from transaction
revalidation, including an unreadable `takeover.json`; Go `ownership.Refused` from
`Admission.Revalidate`; in both, every transaction the handler opens revalidates before it
joins the entry's, so an ownership change during one handler is retained with that entry),
and a receipt refusal caused by a host that could not confirm
the turn (`HostUnavailable`). The owner rolls back the handler writes, writes no
marker, keeps the entry and goes on to the next entry, so a retained entry never
blocks unrelated writers or daemon recovery. Any other host/storage exception rolls the
transaction back, keeps the entry and fails the writable command or recovery. Only
after commit may the owner unlink the final entry and fsync its directory.

Both owners drain entries, the same way, including after rollback:

- before writable CLI work, once the command's writable store open is admitted and after
  the selection refusal and `--kind-module`, before the handler reads its own arguments:
  Python in `cli.main` after `_ownership_preflight` opened `services.store`, Go at the relay
  CLI's dispatch, which then hands that same store to the handler's own open
  (`store.WithAdmitted`). Both: every writable command but `service`, `daemon`,
  `managed-start` and `intent-*`, which open their own admitted connection; a handler that
  refuses its own arguments (`deliver`, `reconcile`, `recover`, `verify-acks`,
  `supervisor-send` or `supervisor-read` without `--socket`, `supervisor-stage` without a
  subject, `store-challenge` without `--write` or `--read`) drains first all the same;
- at daemon start (`daemon`, including an adopted worker) and in the service supervisor's
  recovery before `RecoverOnStart`, which for the takeover candidate is under its starting
  permit and before readiness (Step 6).

Each entry is applied through the command's existing handler on the drainer's own admitted
store (Go: in `store.Compose`, never through a second `store.Open`, which would wait on the
connection the replay's transaction holds), with the drainer's App Server socket as its host:
a queued `emit` reads its turn from the host as a direct `emit --socket` does, and a queued
`ack` confirms a delivery through it. Go's queued-ack host is an adapter that shares no store,
so the replayed ack records no discovery cursor, where Python's adapter records one inside the
replay's transaction. A legacy `supervisor-read` entry replays as a direct `supervisor-read`
does on the drainer's store (Go: `supervisor.Channel.ReadBack` inside the replay's
transaction): with no drainer socket, its no-host usage answer (exit 4); with one, the
readback's own store checks first (no such message, another recipient, a wrong proof: the same
exit-2 answer in both runtimes), and a host opened only on its first use, as Python's
`_LazyAdapter` is. Go's readback host, like its queued-ack host, shares no store.

Golden entry bytes live in `contract/golden/takeover-inbox/`, one per queueable command plus
the retained legacy `supervisor-read` format; Go's envelope (`internal/relay/inbox.Envelope`)
reproduces them byte for byte, and its replay accepts exactly the entries the fence accepts: it
re-derives an entry from its typed arguments, as the fence does from the rebuilt namespace, so
an empty append list (which no producer writes) is kept and accepted, never dropped.

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
Decision 32 sets where they are enforced: an allocation bounds time spent waiting on the host
or a peer (stdin readiness, the guard's answer), never time the process spent unscheduled.
The settings read, the Unix connect (which never waits) and the decision-22 row are bounded by
the absolute deadline.

A Stop spends one receipt read and one evaluation wherever it is evaluated: the legacy CLI's
preflight before it routes to the owner makes the selection without reading the receipt
(When the other runtime owns the record).

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

`crw doctor retention-scan --json` (todo 37, `internal/runtime/doctor`) enumerates exactly this
fixed list. It resolves every executable reference through the owned pointer and every link and
classifies what the reference resolves to, never the text of the reference: on the relay host
`current/bin/codex-session-relay` names no Python while `current` points at a venv. A reference
is Python when it resolves to a native Python interpreter (named `python*` or `pypy*`, ABI flags
and `-dbg` included, or an ELF image that carries CPython's `.PyRuntime` section, links
`libpython` or `libpypy`, or holds `Py_BytesMain`, `Py_Main` or `pypy_main_startup`, so a
`python3.13t` or a copy under any name is found), a venv or any non-native file inside one, a
`.py` file, the `#!/bin/sh` then `'''exec'` launcher pip and uv write for a long or spaced
interpreter path, a `#!` script whose interpreter resolves to one of these, or a shell script
that runs one. A `#!` interpreter is itself resolved and classified: `sh`, `bash` and `dash`
scripts are read (below), `env NAME` runs the program PATH finds for `NAME`, one that does not
exist cannot run, and any other interpreter (perl, node, busybox, a relative one) is unreadable,
as is a non-native file with no `#!`, which a shell would run as a shell script.

A hook command, a shell script a reference reaches, and a program handed to `sh -c` are parsed
with `mvdan.cc/sh/v3/syntax` (Bash grammar; decisions.md 37) and judged only as far as they are
written in a grammar the scan reads completely. That grammar is:

- simple commands joined by `;`, `&`, `&&`, `||`, `|`, `|&` and newlines, with `!`;
- words that are literal once the scan's expansions are made: a leading `~` or `~/` and `$HOME`
  from the scan's environment, `$CODEX_HOME` as the scan reads it, and `${PLUGIN_ROOT}` (row 5
  only) as the cached version's directory; a word that is exactly one positional parameter
  (`"$@"`, `$1`, ...) as an argument, standing for the arguments its caller passed, which are
  judged where it is called;
- redirections to such words;
- as commands: the builtins `exit`, `true` and `:`; `exec` and the command after it; a command
  word that is an absolute path, or a bare name found on the scan's PATH searched in order, where
  a relative or empty directory met before the match makes the name unreadable rather than
  skipped; `sh`, `bash` and `dash` with the flags `-e`, `-u`, `-x`, `-f` and `-c`, whose `-c`
  program is judged the same way and whose script operand is read as a shell script whatever its
  `#!` says; `env` with `-i` and `--` (after `-i` only a command path); a Python program, a
  reference whatever its arguments, which are reported too when they name Python; and any other
  program, judged by what it is, with each argument judged as something it may run (`sudo`,
  `xargs`, `flock`, `timeout` and `uv` run theirs): one naming a Python program is a reference,
  and one naming a shell, holding program text (whitespace or shell syntax) or an assignment,
  needing an expansion the scan does not make, or naming a script this scan cannot read is
  unreadable.

Everything else is unreadable and listed with its row, source, field and the construct, never
interpreted: a function definition, any assignment (a prefix, `export`, `PATH=...`, `env NAME=`),
a compound command (`if`, `case`, a loop, a subshell, a `{ }` group, `[[ ]]`, arithmetic,
`time`, `coproc`), a here-document or here-string, a command or process substitution, an
unquoted glob or brace pattern, any other expansion, a relative command or script word (a hook
runs in the session's workspace, which the scan cannot name), a command no PATH directory holds,
any other builtin (`cd`, `set`, `.`, `eval`, `trap`, ...), a shell other than those three, a
login or interactive shell or one reading standard input, a program the parser rejects, and
nesting deeper than four programs. The native Stop command
(`"$HOME/.local/share/crw-runtime/current/bin/crw" hook --plugin-launch; exit 0`), the pre-native
`python3 -c` bootstrap, a `<CODEX_HOME>/crw-stop-hook.py` launcher invocation and the bridge
launcher `sh ./wiring/crw-bridge.sh` are inside the grammar.

An MCP server is judged as Codex starts it: its `command` and `args` exec'd with no shell and no
expansion, in its declared `cwd` (row 5: `.`, `./...` or `${PLUGIN_ROOT}` in the version
directory; an absolute one as it stands), with its declared `env` over the `HOME` and `PATH`
Codex passes on (not `CODEX_HOME` or `PLUGIN_ROOT`, docs/plugin-packaging.md). A relative word
with no `cwd` the scan can place is unreadable; a bare command is found on the declared `PATH`,
else the scan's. It reports:

- `pythonReferences`: every reference that resolves to Python, with its row, source file, field
  and what it resolves to;
- `liveHolds`: every turn that may still be running a command this scan cannot see (rows 1 and
  2); a hold is not a Python reference, it is a reason to wait;
- `unscanned` and `unreadable`: the rows the scan did not read, and everything it could not
  read, resolve or judge: files, references whose target cannot be read, every construct and
  word outside the grammar above, a `crw-*.json` value that is not an absolute path (or not a
  string or list of strings), a hooks document, event, group, hook, command, type or timeout,
  or an MCP server table, entry, command, `args`, `cwd` or `env`, that is not what the host
  reads there (a malformed Stop entry also leaves row 2's settings unknown), a relay state
  directory that cannot be established, alive pids whose `exe` or `cmdline` cannot be read, and
  a program whose reading failed inside the scan (recovered around that one program, never
  ending the scan). A row with anything of its own listed unreadable is unscanned. Nothing the
  scan cannot judge is dropped;
- `clear`: true only when all four are empty. Todo 43 removes nothing until `clear` is true.

| # | Surface | What counts |
|---|---|---|
| 1 | `<CODEX_HOME>/crw-completion-hook/stop-events/*.json` | a claim whose outcome `<claimedBy.journalRoot>/accepted/<eventKey>.outcome.json` is absent, or that names no journal root or one that is not absolute (never looked up against the scan's own directory): a live hold |
| 2 | `<journalRoot>/<day>/*.json` rows under every journal root a retained Stop registration can write to: the `journalRoot` of `crw-completion-hook.json` (which the packaged launcher, `crw hook --plugin-launch` and a registration naming no settings read), the `journalRoot` of each settings document a `hooks.json` or cached plugin Stop command names as its settings argument (the word after `completion_hook.py`, `crw-completion-hook` or `crw hook`), or, naming none, the `CRW_COMPLETION_HOOK_CONFIG` of the scan's environment; `<CODEX_HOME>/crw-completion-hook/journal` (always, and for settings naming none) and each root a Stop-event claim names. Settings that cannot be established or read (a Stop command holding anything the scan cannot judge, or running a script other than the adapter or the launcher, which may name settings of its own) and a relative `journalRoot` are unreadable | a row younger than twice the longest configured Stop hook timeout (the settings `timeoutSeconds` and every cached or user Stop hook `timeout`, at least 10 s), read from every day directory the window reaches back into: a live hold. A row's time is the `at` its hook wrote, never the file's modification time, so a row that cannot be read, is not an object or has no RFC 3339 `at` is unreadable. A window too large for a duration holds every row. A timeout that is not finite is unreadable, naming its value and where it is configured: Infinity still holds every row (the conservative window) and NaN gives no window, and either leaves the row unscanned, since the window it sets is not one the settings meant |
| 3 | every relay state directory's `daemon.json`: the state root, each directory under it (a link to one included), `CODEX_SESSION_RELAY_STATE` with `~` expanded, and each `stateDir` the relay's scope registry (`<passwd home>/.codex-session-relay/scopes/*.json` and `CODEX_SESSION_RELAY_SCOPE_DIR`) records, whose own pids are judged too | a supervisor or worker pid whose start time and boot still match the record (a zombie is not alive); a `pid` or `workerPid` that is present but not a positive integer pid is unreadable and leaves the row unscanned, while an absent or null one records no process, running a Python interpreter or a `.py` program. Where no process table can be read (darwin has no procfs), a record names a boot and the current boot id cannot be read, or a state directory cannot be established (a relative `CODEX_SESSION_RELAY_STATE`, a registry that cannot be listed), the row is unscanned and the cause unreadable |
| 4 | `<CODEX_HOME>/crw-*.json` (`crw-completion-hook.json`, `crw-bridge-mcp.json`, any other) | `relayExecutable`, `bridgeExecutable`, `adapterEntryPoint`, `adapterInterpreter`, `interpreterPath`, `command`, each resolved as written (the launchers run them with no shell and no expansion), and each `args` entry as an argument (above). One of those values that is not an absolute path is a reference when it names Python (`python3`, a `.py` file) and unreadable otherwise: the Stop and bridge launchers accept only absolute paths, and the scan never resolves one against its own working directory. A file that is neither native nor `#!` is unreadable |
| 5 | `<CODEX_HOME>/plugins/cache/crw/crw/*/wiring/hooks/*.json`, `wiring/mcp.json`, `.mcp.json` in every version directory (a stray file there declares nothing) | every hook command, judged under the grammar above, and every MCP server, judged as Codex starts it |
| 6 | every `managed-start-*.lock` in those state directories | its `/proc/locks` flock holders and waiters, matched by the device the kernel prints (the superblock's, read from `/proc/self/mountinfo` for the mount holding the file; `stat` differs on btrfs) and inode, judged as in row 3, less the pids row 3 reports. The table names the pid that took a lock, not whoever holds it now: a taker that is 0 (not visible here), gone or no longer has the file open leaves the row unscanned and the lock unreadable. The directories rows 4, 5 and 6 enumerate (the Codex home, a cached version's `wiring/hooks`, each state directory) are listed explicitly: one that cannot be listed leaves its row unscanned and is unreadable |
| 7 | resumable Codex threads (`crw bridge` `list_threads`) younger than the host's turn-command cache lifetime | not read by todo 37: it needs the cache lifetime todo 43 records on codex-cli 0.154.0, so the scan reports this row unscanned and is never `clear` until todo 43 adds it |
| 8 | `<CODEX_HOME>/crw-stop-hook.py` | the launcher copy the cached Python bootstrap falls back to, whenever it exists |
| 9 | `<CODEX_HOME>/hooks.json` | every hook command, as row 5 |
| 10 | `<CODEX_HOME>/config.toml` `mcp_servers.*` | every MCP server, as row 5 (a relative `cwd` is unplaced) |
| 11 | the owned pointer `~/.local/share/crw-runtime/current` (or `--dest`) | its target, when that is a venv |

The list is closed: adding a surface is a plan change, not a scan option. Rows 1, 4 (the
`adapterInterpreter`, `command` and `args` keys), 8-11 and the pointer resolution are the scope
analysis's corrections to the list first written here (.omo/ulw-execute/scope-analysis-31-46.md
"# 37").
