# Runtime installation and diagnosis

[POLICY.md](../POLICY.md) owns repository rules and
[the operations contract](../skills/crw-run/references/operations.md) owns the operational ones.
This page describes the runtime entry point that installs and diagnoses the MCP bridge and the
session relay, and it is written against that contract's clause numbers so a reader can check a
claim against the rule it came from.

Two entry points exist and they are deliberately not one:

| Command | Installs | Contract |
| --- | --- | --- |
| `python3 scripts/install.py --check` or `--apply` | Skill links into Codex | OPS-2.3 |
| `python3 scripts/runtime_install.py` | The bridge, the relay, the MCP registration and the Linear hook | OPS-2.4, OPS-6.3 |

The first is unchanged by this page. It stays standard-library-only and idempotent, it refuses to
replace an existing directory or a foreign link, and runtime installation is never folded into it.
The runtime entry point reuses its `LINKED`, `MISSING` and `CONFLICT` vocabulary so one word means
one thing across both layers, and it reads the skill-link layer by running `scripts/install.py
--check` rather than by reimplementing it.

## The one definition

`scripts/crw_runtime/components.json` is the single compatibility definition OPS-1.1 requires.
Both installation and diagnosis read it; neither carries a second copy of a revision, a version or
a digest.

It carries only what this checkout can prove about itself. Every field is either re-derived from
the checkout at check time or marked with the OPS-0 status word that says it was not:

| Field | How it is established |
| --- | --- |
| `subdirectory`, `packageLocation` | Paths in this repository |
| `subdirectoryTree`, `packageTree` | `git rev-parse HEAD:<path>` |
| `sourceDigest` | The OPS-1.2 walk over the package directory |
| `version`, `requiresPython` | Read from the component's `pyproject.toml` |
| `upstream` remote, revision, tree and licence | `recorded`: carried from the import, not re-derivable here |
| `measuredPoints` | Empty, and `unmeasured`: this repository has exercised no combination |

`python3 scripts/runtime_install.py verify-definition` re-derives every derivable field and fails on
any disagreement, so the definition cannot drift from the source it describes. It runs in CI through
[scripts/ci/contracts.py](../scripts/ci/contracts.py). The upstream revision is not derivable from
this checkout, because the import brought source rather than history, so it is instead required to
appear in [packages/README.md](../packages/README.md), which is the provenance narrative OPS-1.5
says is retained rather than replaced. That keeps one machine-readable owner without letting the
prose and the definition disagree.

The repository commit is deliberately absent from the file. A commit SHA recorded inside the commit
it names is self-referential, so it is measured at run time and reported, never committed.

What the definition does **not** carry is as important. Installed locations, entry points,
interpreters, host names and measured points are host facts. OPS-3.2 makes a real record a private
receipt, so the runtime entry point writes those to a host record outside this repository and this
repository never commits one.

## The host record

The host record at `${XDG_STATE_HOME:-~/.local/state}/codex-relay-workflow/host-record.json` is the
other half of the definition and is never committed. It holds the repository commit and tree
measured at run time, checkout cleanliness, one entry per install location
(`location`, `installMode`, `entryPoint`, `environment`, `interpreter`,
`interpreterPath`, `integrity`, `reachedVia`) and the measured points.

This is what makes reuse reachable. Under OPS-1.3 a point means the combination was exercised, so
no amount of reading bytes produces one, and a component whose bytes match but whose combination
nobody has run classifies `unmeasured` and is preserved rather than reused.

`runtime_install.py measure` is the operation that produces a point, and it has to run something
real. Starting a process is not exercising a combination: the bridge's entry point starts a stdio
server and never contacts the App Server, so a recipe built on startup would record success against
an unreachable host. `measure` therefore runs two actual operations under the resolved interpreter:

| Component | Operation | What makes it an exercise |
| --- | --- | --- |
| Relay | `codex-session-relay --socket <sock> --state <dir> doctor` | `actorReachability.socketConnect` is a real connect and must equal `ok` |
| Bridge | `packages/codex-thread-bridge/scripts/check_connection.py --socket <sock>` | The package's own read-only check starts the MCP server, lists its tools and calls `get_capabilities`, which is an App Server round trip |

That check is invoked, never modified or reimplemented. A connection, protocol or tool-call failure
records **no** qualifying point and the run reports why. The point records
`{interpreter, codexCli, appServer, host, date, measuredBy, method}` bound to one install location,
one combination and the `sourceDigest` it was measured against, so it is evidence tied to the bytes
it covers rather than an independently editable expectation. A point recorded against another
interpreter is a different combination and does not satisfy this one. Points are appended, never
replaced.

### An interpreter's identity is not one line of a script

`interpreterPath` is recorded because a console script has more than one written shape. pip emits a
direct `#!<python>` shebang when the destination allows it, and a `#!/bin/sh` trampoline that execs
the interpreter on a following line when it does not, which is what a path containing a space
produces. Reading the first line answered `/bin/sh` for the second shape, so nothing could be
asked of the interpreter: it reported no version and located no module, the component classified
`unreadable`, the install never promoted, and recovery deleted the environment the run had just
built as though it belonged to somebody else.

So the interpreter for a script this command created comes from the install record, written by the
run that used it, and is confirmed by running it. Records written before `interpreterPath` existed
still name the `environment`, whose interpreter is the one that environment was built with. Reading
the shebang stays the answer only for a script this command did not create, where there is no
recorded environment to ask, and the classification reports which of the three it used.

## Reading a record, and what happens when it cannot be read

Every record this command reads — the definition, the host record, the Codex configuration, the
hook file — is read at a narrow boundary that turns a failure into an answer rather than a
traceback. The answer is one of four states, decided by an ordered observation rather than by a
convenience test:

| State | What was observed |
| --- | --- |
| `ABSENT` | nothing exists at the path. The only state that may be read as a host with no history. |
| `PRESENT` | it was read. An existing record with nothing in it is `PRESENT`, not `ABSENT`. |
| `UNREADABLE` | something is there and its shape cannot be read: a directory or other non-regular file, a symlink whose target is established missing or looping, invalid UTF-8, unparseable JSON, or containers of the wrong type. |
| `ACCESS_ERROR` | nothing could be established: a permission or I/O failure reaching the path, a symlink whose target could not be resolved, or a parent directory that cannot be traversed. |

The distinction that matters most is the last row. Being unable to ask is not being told no, so a
failure to establish existence is never reported as absence, and a permission problem is never
reported as a malformed record.

The service reading is classified the same way, and the invocation wins: a `service status` command
that did not run yields `ACCESS_ERROR`, an answer with no boolean `running` yields `UNREADABLE`, and
only an answer that arrived yields `RUNNING` or `STOPPED`. A daemon is never reported stopped
because nobody could ask it.

A refusal names what failed: the exception type, the source path, and the file and line that raised.
That is deliberate. Swallowing everything into a generic "unreadable" would file a defect in this
command as a problem with the user's data, and the defect would then disappear from the record.

### What this guarantees, and what it does not

The guarantee is bounded and stated rather than implied. **What it guarantees:** the worst case for
a record this command reads is a named refusal, not a crash. **What it does not guarantee:** that a
record which could have been read is never refused. Validation is per known consumed field where the
shape is known, and a class guarantee at the boundary everywhere else, so the residue is a record
refused conservatively. That direction is the safe one and the refusal carries its reason, so it is
reportable rather than silent.

Two further limits, for the same reason:

- **"Nothing was written" is scoped to what can be guaranteed.** Malformed input detected *before*
  the first mutating step refuses and the target file's bytes are unchanged. A read failure *after*
  a mutation reports the mutation instead of denying it: the outcome is `APPLIED_UNVERIFIED` with
  `applied`, `wrote` and `readBack: false`, and the command exits non-zero. Reporting a landed write
  as a refusal that wrote nothing would invite a retry that appends a second registration, which is
  the outcome this command exists to prevent. The unchanged-bytes claim is about the target file; a
  lock file is created and removed beside it.
- **A read-only diagnosis reports rather than refuses.** `diagnose` names the failed reading in
  `hostRecordState` and `hostRecordReading` and continues with what it could still observe, because
  refusing the whole diagnosis would discard the readings that did answer. It never reads an
  unreadable record as a clean host: the affected components classify `unreadable`. Commands that
  would write — `install`, `measure`, `register-mcp`, `hook` — refuse outright.

### One writer for the host record

Every change to the host record goes through one helper that takes the lock, loads the record
*inside* it, applies the caller's narrow delta and saves. The helper never accepts a record. A
caller that loads a record, spends minutes installing and exercising a runtime, and then hands the
record back to be saved would overwrite whatever another run committed in between, and holding a
lock over that save does not help, because the staleness is already inside the value being written.
So a caller says what it learned — this install, these points, this selection — and the merge
happens against the record as it then stands.

Recovery follows from the same rule. A failed install removes the directory it created and drops
only the install records keyed to that directory. It leaves the selection **exactly as found**,
because another run's successful promotion is not this run's to undo.

### Reading the configuration

**Registering an MCP server needs a controller on Python 3.11 or newer.** `tomllib` arrived in
3.11 and it is the reader; without it every non-empty configuration is refused, and registration
refuses even into an empty one because it reads back the content it proposes to write. The refusal
names the interpreter that is running and says what to do about it.

The controller's interpreter is not the runtime's. This command installs 3.11+ runtimes whatever
interpreter started it, so an old controller does not mean an old installation - it means the
process reading your configuration cannot parse TOML, and rerunning `runtime_install.py` on a
newer interpreter is the whole fix. Diagnosis still reports everything that does not need the
parser and marks the configuration unreadable rather than guessing at it.


`tomllib` reads the configuration wherever it exists, which is Python 3.11 and newer: the host
interpreter and every runtime this command installs. A hand-written TOML reader is an open
correctness problem, and this one cost eight review rounds - delimiter counting, escape decoding,
dotted names, quoted keys, the three-quote sequence, brackets inside quoted names, Unicode line
boundaries, quoted member assignments - so it stopped being the reader.

No fallback remains. The narrow subset written to replace the hand-written reader produced two
more defects of its own - a quoted name read as a list of its characters, and a duplicate key
silently taking the last value - and it existed only to give one CI job something to run. So the
`validate` and `tests` jobs on Python 3.10 exercise the refusal rather than a second reader, and the
checks simulate the absence of `tomllib` on an interpreter that has it, so the refusal is verified on
both jobs rather than only where it bites.

Parsing is not reading a registration. A file where `args` is the string `"ab"` parses cleanly and
`list()` turns it into `["a", "b"]`, so the shape is validated before anything is compared:
`mcp_servers` a table, each entry a table, `command` a string, `args` a list of strings. Other
fields such as `env` are left alone rather than refused, and the comparison is a symmetric
projection onto the two fields registration actually decides on.

Appending gets the same treatment. Reading a file correctly does not make a trailing table mean
what it says: a root `mcp_servers = {}` is a closed inline table that `[mcp_servers.x]` cannot
extend, and under `[[mcp_servers]]` an appended table attaches to the last array element. So the
proposed content is read back **before** it is written, and it must carry the intended registration
and leave every other one unchanged, or nothing is written.

### A judgment cell is filled only by its own reading

Every signal classification decides on carries the value its own question's reading produced,
and nothing else. A reading that did not answer leaves its cell empty and names itself
unreadable.

The failure this replaces was quiet. `definition.git` answers nothing when it cannot read,
nothing compared with a recorded tree hash is *false*, and false is what classification reads as
a disagreement: an installation this command owns was reported as somebody's fork, from a read
nobody performed. The sibling three lines above, the repository commit, was already correct.
Writing the comparison out at each site is what let one of them be right and the next one wrong.

`ownership.Judgement` is where the comparison lives now. `compare` returns nothing when the
observation was not made and records why; `answer` does the same for a reading that IS the
signal. The interpreter version, the host name, the Codex CLI, the App Server, the component
tree and the checkout status all go through it, and a repository commit nobody could read is
reported as unknown drift rather than as drift.

Four outcomes are declared per cell, because one rule would be wrong about most of them: a
reading that answers nothing has to stop the classification, a reading that raises is a named
refusal at the boundary, absence is sometimes a real *no*, and some cells are answered by no
observation this command makes. The cells come from `ownership.Signals` itself, so a signal
added without saying which reading answers it fails the inventory.

### A cell that says no reading answers it is checked, not believed

The cells come from `ownership.Signals` and each names the observation that answers it. That
catches a cell whose reading is wrong; it cannot catch a cell whose declaration is a lie. A cell
declared to have no reading is simply skipped, and that is the path the next defect took:
`link_conflict` sat empty while this command's own `skill_links()` was answering the very
question, because the declaration read "the skill installer's reading, not this command's" and
nothing tested that sentence.

So the claim is verified. A cell's subject comes off its own name by stripping the suffixes the
declaration lists, and for a cell that names no observation no function of this command may
carry that subject. `link_conflict` against `skill_links` is the case that would have failed.

The reading itself moved ahead of classification, where it should have been: `scripts/install.py --check` reports `CONFLICT` for a path this command does not own, and that is an
OPS-2.1 conflict exactly as a differing MCP registration is. A caller that makes no such reading
says so - `linkConflictRead` - because no conflict found and nobody looked are different
answers, and `install` has no Codex home in scope to read.

### The inventory for a conflict cell is the caller set

Every cell is declared, every declaration is verified, and `install` still promoted over a
conflict, because that defect lives one dimension up: the command that moves the selection
passed neither conflict reading. `CONFLICT_READINGS` names them, every call of
`classify_component` in this command has to pass each one, and the classification reports
`conflictsRead` so a caller that made no reading is distinguishable from one that found no
conflict. A cell may legitimately be `None` for a caller - the MCP registration is the bridge's
and says nothing about the relay - but the caller says so by passing the keyword.

`install` takes a `--codex-home` for this, defaulting the way `diagnose` does, and compares the
command alone. It knows which entry point it installed and knows nothing about the arguments a
host chose, and an empty argument list is not the absence of an expectation: it is the
expectation that there are none, which reports a conflict for a registration that is correct and
merely carries supported bridge arguments.

### The inventory for a store on disk is the place set

The filesystem listing is the whole inventory when the relay cannot answer, which is exactly
when hiding a store matters. The state root had its own branch for `relay.sqlite3` and
everything else was looked for in child directories, so an operations ledger beside the root
database was in neither and was never listed. A third branch would reopen at the next place, so
the places are a rule - the state home, then each scope directory under it - and every store
pattern is looked for in every one of them. The root comes first and unconditionally, so a
directory listing that cannot be read loses the scopes and not the root.

### A lock that could not be taken established nothing

`release_candidate` takes the host-record lock, and a `TimeoutError` used to leave it. That meant
the cleanup path of an already-failing install raised, and the run reported an internal error
instead of whether its destination is retriable - the two things criterion 2 and criterion 4 ask
of a failed run. A lock another run holds establishes nothing about the selection, which is the
answer the same function already gives for a record it cannot read, so it takes that branch: the
candidate is kept and the refusal says why.

### One cell, one question

Two readings that answer different questions are never joined into one value. `summarise`
made three doctor invocations and then read `selected or discovery`, so a selected store that
did not answer borrowed the discovered store's path, store id and socketConnect while the
service status, the assignment lookup and the trial all kept acting on the selected one. That is
the conflict OPS-3.4 asks this reading to surface, reported as agreement.

The summary now says which question answered, in `scopeAnsweredBy`, and hands the caller the
invocation it came from in `scopeCommand` so a field derived from that scope names the same
reading instead of deciding the provenance a second time. An explicit selection that could not
be read reports no scope at all; the discovered store is a different store.

The same distinction reaches the registration. `LINKED` means the file registers exactly the
command this run asked about; `PRESENT` means a registration is there and nothing was compared,
because no expected command was supplied. Collapsed into one set, `mcpExposed` reported
*verified* with evidence reading "the configuration registers this exact command" for a host
registering something else entirely.

### One word, one declaration

A partition belongs to the module that declares it, and a consumer asks that module rather than
testing one of its members. `== UNREADABLE` answers for one of the four reading states and
silently says yes to another, which is how a configuration that could not be reached at all
reached classification as one that had been read. The same shape produced a bridge classified
against whatever PATH resolved, a point recorded with a dimension nobody observed, and a replay
decided on one of the seven values the relay actually compares.

A check reads every UPPER_CASE module-level binding out of the source, resolves the strings it
names - including names, cross-module references and concatenations - and reports any comparison
against one of those strings from a module that can see the declaration. Scoped to importers,
because unrelated modules share short words: a destination kind spelled `host` has nothing to do
with the hostname dimension whose key is spelled the same.

Its limit is stated rather than papered over. It reads comparisons; literal key *access* is not
covered, because payload keys are data and forbidding them would forbid reading a payload at all.
The one map where that distinction decides something is guarded separately, by an access contract
over the comparison loop itself.

### A member carries its predicate and its provenance

Declaring a set fixes what belongs to it and nothing else. The gate over that set still applies
whatever predicate it wrote and the probe over it still asks whatever runtime was nearest, which
is how one defect reopened a dimension up four separate times: a whitespace-only turn id read as
supplied here and as blank by the relay, an empty server table read as an absent registration, an
artifact rule asked of this checkout while a different installed relay acts on the answer, and a
smoke check whose bytes decided a point that named only the installed package.

So a member is a pair. `TRIAL_REQUIRED_INPUTS` maps each input to its flag *and* to the predicate
its consumer applies - `NON_BLANK` for this command's own minimum, the relay's own
`validated_turn_id` for the anchors the relay refuses blank. `PREFLIGHT_PROBES` names the
read-only probes, and each runs the interpreter it was handed rather than this controller.
`PRESENCE_READINGS` pairs a presence question with the reader whose sentinel answers it, because
an empty mapping is falsey and is not an absent one. And `exerciseDigest` is a dimension of a
point, because the bridge's smoke check lives outside the installed package and its bytes decide
the claim the point records.

Two consequences are worth stating rather than discovering. `--trial` needs the selected relay's
interpreter to be resolvable before it writes anything, and says so instead of falling back to
this checkout's copy of a rule the installation owns. And a point recorded before `exerciseDigest`
existed no longer qualifies: it cannot name the instrument that produced its claim, so a host that
reached `own` on such a point measures again.

### The failure contract

The reading boundary answers questions about records. Underneath it, `main()` converts anything
that escapes a handler into a controlled result and exits non-zero. The two are deliberately
separate:

| | Reading refusal | `internalError` |
| --- | --- | --- |
| means | this record could not be read | a defect in this command reached the top |
| carries | a `state` from the four-state partition | the exception type and the line that raised it |
| about | the record | the code |

A defect is never filed as a statement about somebody's data, which is what would make it
disappear. What this guarantees is narrow and worth stating plainly: the worst case is a named
result rather than a traceback. It does not guarantee that every input was anticipated.
`diagnose` still reports unreadability and exits zero; the contract is about tracebacks, not about
forcing every command to refuse.

### Recovery has two outcomes

Reporting a refusal does not delete a directory. After a failed installation the result says which
of these happened:

- **retriable** - removal was verified on the filesystem, so the same destination can be used again.
- **not retriable** - removal could not finish. The result names the residual path and what
  recovery needs, and the original failure is reported alongside the cleanup failure rather than
  replaced by it.

Whether the candidate may be removed at all is read, never remembered. `hostrecord.update` saves
inside the lock and releasing the lock can still raise afterwards, so a run can commit its
promotion and raise anyway; a flag set from "the call returned" would then delete a runtime that is
now selected. Recovery reads the selection back under its own lock and keeps the candidate when the
environment is selected **and** when the selection cannot be established, because an unreadable
record says nothing about what is selected. A raised failure releases exactly like a returned one.

### The trial preflight matches what the relay requires

Everything the trial needs is checked before its first command, and "needs" means what the relay
itself enforces rather than what is merely present. `--turn-status` is one of the four the relay
declares; `--turn-thread` equals `--child-task`, because a receipt's thread has to be the
relationship's child task; and every `--artifact` is an absolute, already-normalised path to a
regular file with no symbolic link at any component, readable, and inside `--artifact-root`, which
is what the relay checks while building the manifest. The relay revalidates afterwards, because a
path can change in between.

The recipient's settings are checked for **usability**, not presence. `settings-record` now runs
after `register`, so a value that is there but cannot be used - a malformed object, an `@path`
that is not readable, a settings object missing a required field - would be discovered after a
relationship row exists. The preflight therefore asks the relay's own reader and the relay's own
predicate, run read-only in the relay's interpreter: neither opens a store and neither writes. A
second copy of those rules here would be a restatement of something that lives in the relay, and
the next change would move only one of them. When the relay's interpreter cannot be resolved the
answer is *unknown* and the trial refuses, because a check that could not be made is not a check
that passed.



## Installing the runtime
`runtime_install.py install` refuses unless `verify-definition` passes, then resolves an
interpreter that satisfies both components' `requires-python`. The controller itself runs on
Python 3.10 for CI and never selects itself for a runtime that requires 3.11 or newer; when no
suitable interpreter exists it refuses and names the requirement.

The environment is created as a new directory, so an existing one is never overwritten. Each
module's imported location is then read back from the interpreter rather than assumed, because an
editable install leaves nothing under site-packages and a copied install does, and its OPS-1.2
digest is computed from whatever the interpreter actually resolved.

The candidate is then exercised, and the recorded pointer moves only after a qualifying point
exists for it. OPS-2.4 sequences an update as measure, install, measure again, and the second
measurement is the one that produces the point; promoting before it would select a runtime that
imports cleanly and fails the moment it is used. A candidate whose exercise fails stays unselected
and the previously selected runtime remains selected. A failure at any step leaves the previous
runtime in place, and nothing here removes, moves or recreates the store: update failure and store
loss are different accidents and the recovery for one must not cause the other.

## Updating an installation

The first install is the easy half. The second one is where the previous runtime and the store can
be lost, and until this section existed it could not happen at all.

The environment is named from the definition version and the combined source digests, so a new
combination always gets a new directory. The entry point recorded for it is that concrete path,
and the promotion gate compared the Codex registration against it. So once an installation had
registered `env-A/bin/codex-thread-bridge`, every later update registered nothing, compared the
new entry point against the old registration, read `CONFLICT`, classified the candidate
`conflict`, refused to promote, and then deleted the environment it had just built and
exercised. The registration was pinned to the first install for ever, and `register-mcp` could
not move it either: it writes only on `CREATED` and reports `CONFLICT` for a name already
registered with a different command.

The fix is an indirection this command owns rather than a rewrite of somebody's configuration.

### The pointer is what moves

`<destination>/current` is a directory symlink. The registration and any user-facing command
name `<destination>/current/bin/<console script>`, which is stable across every update, so
`config.toml` is written once and never rewritten. That matters more than it looks: this
repository refuses to approximate TOML, and the byte-preservation proof the registration rests on
is that the prior content is an exact prefix of the new file. An in-place edit cannot satisfy
that, so a registration that had to change on every update would have to give up the one property
that makes appending safe.

A console script keeps the absolute shebang pip wrote, so a process started through the pointer
reports the concrete environment as its `sys.prefix` and its `sys.executable`. The pointer is a
way to reach a runtime and never an identity. A bridge Codex has already spawned goes on running
its own environment after the pointer moves, which is how criterion 4's process liveness survives
an update, and it survives only because nothing here removes a predecessor.

Two strings answer two questions, and they are not interchangeable. The candidate is classified
through its **concrete** entry point, because before the swap `current` still resolves to the
predecessor: classifying through it would read the previous interpreter, digest the previous
bytes, and report the new candidate as a fork of itself. Only the registration expectation uses
the pointer, and the pointer path is read from the host record rather than rebuilt from the
destination argument, because the registration comparison is string equality and `--dest`
spelled differently on a later run is a different string for the same directory.

Registering the pointer widens what a registration means, and the evidence that widening would
cost is taken back rather than lost. `LINKED` against the pointer says the configuration names
the pointer; it no longer says which runtime that is. So the link target is its own judgment cell,
read with `readlink` and compared against the recorded selection, and diagnosis reports the
registered command, the link target and where the entry point resolves as three fields. A
`current` repointed by hand at a fork is caught by the cell whose question that is, instead of
passing because a neighbouring cell was still satisfied.

### The claim a run leaves behind

The environment name is deterministic and the directory is created with an exclusive `mkdir`,
which is what proves a run owns it. That proof used to expire badly: a run killed outright left
the directory behind, and every retry of the same destination refused at the existence check for
ever.

A run now writes a claim inside the environment immediately after creating it, and holds an
`flock` on that claim for its lifetime. A later run reads the claim and asks who owns it:

| Observed | Answer |
| --- | --- |
| No claim, and the directory is not empty | Somebody else's directory. Refused, nothing touched |
| A claim, the lock held | Another run is building it. Refused, nothing touched |
| A claim, the lock free, the environment not selected | An abandoned staging this command created. Reclaimed |
| A claim, and whether anyone holds it could not be established | Kept, and reported as a residual path with what recovery needs |
| A settled claim for an environment that is selected | Already installed. Reported, nothing rebuilt |

Liveness is the lock and not the recorded process id, for the reason the relay already recorded
about its own supervisor: inside a container sharing a kernel, the same process id under the same
boot id is a different process, and a process identity that can lie is worse than no reading. The
lock cannot lie about contention. Where `flock` is unavailable the answer is that nobody could
tell, and an owner nobody could establish is never read as an owner that is gone: deleting a live
run's environment is the accident this exists to prevent. Such a directory is kept and named, so
an orphan is findable and reportable rather than either silently accumulated or silently removed.

### Reading whether it is safe to swap

OPS-4.4 sequences an update around a daemon that is not running and open attempts that have been
reconciled. Three readings answer that, each filling only its own cell:

| Cell | The reading that answers it |
| --- | --- |
| `daemon` | the relay's `service status`, whose `running` is decided by the lock a supervisor holds |
| `inFlight` | the relay's `doctor`, whose `contents.openAttempts` counts in-flight and held-uncertain attempts |
| `storeTables` | the store's own table inventory, read read-only through the relay's `read_only_rows` |

The swap proceeds only when the daemon is established stopped, the open attempts are established
zero, and the store's tables are established compatible. Any cell that could not be read decides
`UNESTABLISHED`, which keeps the existing installation exactly as a blocking answer does. A
check that could not be made is not a check that passed, and a daemon is never reported stopped
because nobody could ask it.

This command never starts or stops a daemon. OPS-4.1 gives the service to the scope operator, so a
running daemon is a refusal here and not something to resolve.

### Why the schema reading counts tables and not versions

The obvious reading would compare the store's recorded schema version with the candidate's. It
would also be worthless. The relay declares `SCHEMA_VERSION = 1`, has never raised it, writes it
once with `INSERT OR IGNORE` when the database is created, and grows its schema through
thirty-nine separate `CREATE TABLE IF NOT EXISTS` statements. Every store therefore agrees with
every candidate at version one, and the comparison would detect neither a downgrade nor an
upgrade while looking exactly like a check.

So the cell compares what actually differs: the table names in the store's `sqlite_master`
against the tables the candidate relay declares. It has four answers.

| Answer | Observed | Decision |
| --- | --- | --- |
| `ABSENT` | no store exists at the resolved selection | allowed, and reported as absence rather than as agreement |
| `AGREES` | the same tables | allowed |
| `EXTENDS` | the candidate declares tables the store does not hold | allowed, and reported as its own answer |
| `NARROWS` | the store holds tables the candidate does not declare | refused |

`NARROWS` is the implicit downgrade criterion 4 forbids: a runtime that does not know a table
cannot preserve what is in it. `EXTENDS` is the additive path every previous update has taken,
and it is reported rather than folded into agreement, because "nothing differs" and "the new one
knows more" are two facts and a reader deciding whether to take a backup needs to see which one
happened. Read strictly, OPS-4.5 makes any schema difference a migration with its own issue and
its own copied backup; this gate refuses the direction that loses data and names the other, and
the report states both so the requirement and the current behaviour never read as one claim.

The reading is the relay's own, run under the relay's own interpreter. A second copy of the rule
here would be a restatement of something the relay owns, and the next change would move only one
of them. It opens the database read-only and runs no schema script, so asking the question does
not create the store the question is about. Absence is established by looking at the path, never
inferred from a failed open, because a permission failure and a locked database also fail to open
and neither of them means nothing is there. The report names which selection answered, since an
absent store at the wrong state directory while a sibling store holds the in-flight attempts is
the OPS-3.4 conflict rather than a clean host.

### The order a swap commits in

The selection in the host record and the pointer on disk are two truths, and the order they are
written in is the whole safety argument. The record's selection is committed first, and the
pointer is replaced afterwards.

The reverse order has a real failure: the symlink lands, the record write then fails or the
process raises, recovery reads a selection that does not name this environment, concludes the
candidate was never promoted, removes it, and leaves the registered MCP command pointing into a
directory that no longer exists. OPS-4.4 requires every state transition to be committed before
its side effect, and this is that rule applied to the two halves of one promotion. Recovery also
refuses to remove an environment the pointer names, so neither truth alone can authorise deleting
a runtime the other one is still using.

Ownership of the pointer is established from the record before it is replaced. Renaming over an
existing symlink succeeds whoever created it, so a `current` this command never recorded is left
alone; a real directory at that path fails the rename outright, which is the safe direction.

The pointer is read through its own partition over `lstat` and `readlink`. The record reader
cannot answer for it: that reader follows a link and then refuses anything that is not a regular
file, so a working directory symlink would be reported as an unreadable record.

### What a failed update restores

A failed update leaves the previous runtime selected, the previous pointer target in place, the
owned configuration untouched, and the store exactly as it was. The result says which step failed
rather than only that something did: `failedStep` names the step and the boundary it was at, and
`restored` names the pointer target that was put back or says the pointer never moved.

The two outcomes recovery already had are unchanged. Removal verified on the filesystem means the
destination is retriable; removal that could not finish reports the residual path, what recovery
needs, and the original failure alongside the cleanup failure rather than replaced by it. A
candidate that is selected, or whose record could not be read, or that the pointer names, is kept.

Nothing here removes, moves or recreates the store. Update failure and store loss are different
accidents and the recovery for one must not cause the other.

This is a POSIX path. The environment layout, the interpreter under `bin`, the directory symlink
and the advisory lock are all POSIX assumptions this command already made elsewhere; Windows is
out of scope rather than approximated.


## Installation ownership

Classification reads the four OPS-2.1 signals and nothing else: where the entry point actually
resolves, the checkout's commit, tree and cleanliness, whether the definition agrees with the
installed bytes, and what the Codex configuration registers. A signal that cannot be read is
reported as unreadable and stops classification; it never counts as a signal that agreed.

Cleanliness is read across the whole checkout, not only the component's subdirectory. An
uncommitted change to a root script or to another package leaves every package digest untouched
while the checkout is no longer the revision the definition names, and only the checkout-wide
reading catches it.

The five OPS-2.2 classes are evaluated in their fixed order and the first match wins:
`conflict`, `fork`, `foreign`, `unmeasured`, `own`. Only `own` is reused. Because the committed
definition carries no measured points, a host install whose bytes match still classifies
`unmeasured` until the host record carries a point for the combination it runs under. That is the
intended answer, not a gap to close by relaxing the rule: OPS-1.3 refuses to let a matching digest
stand in for a run nobody performed. `measure` is the supported way out, and it is the only one.

Nothing outside a recorded path is ever overwritten, and no predecessor is removed: an
environment a previous install built stays on disk after the pointer moves off it, which is
what lets a process already running from it keep running. A foreign relay, a local fork, an existing
directory, an existing link and an MCP name already registered with a different command are each
reported with both values, and the run changes nothing.

## MCP registration

The server is registered as `[mcp_servers.<name>]` in `<CODEX_HOME>/config.toml`, the supported
configuration path, through `runtime_install.py register-mcp`. What it registers is the owned
pointer, `<destination>/current/bin/<console script>`, and not the environment underneath it,
so an update moves the pointer and this file is never written a second time. The two are
separate claims and stay separately reported: the registration says which command Codex will
spawn, and the link target says which runtime that command reaches. Registration is append-only and
idempotent: an identical registration is reported `LINKED` and nothing is written, an absent one is
appended, and a different command or argument list is reported `CONFLICT` and nothing is written.
Every other table in the file, including other MCP servers and hook settings, is preserved byte for
byte, which the command checks by requiring the prior content to be an exact prefix of the new file
rather than by asserting it.

The reader is `tomllib`, which arrived in Python 3.11. A controller older than that refuses every
non-empty configuration instead of approximating one, and refuses into an empty one too, because
registration reads back the content it proposes to write. The refusal names the interpreter that is
running and says to rerun on a newer one. The controller's interpreter is not the runtime's: this
command installs 3.11+ runtimes whatever started it.

Parsing correctly is still not reading a registration. The parsed shape is validated before anything
is compared - `mcp_servers` a table, each entry a table, `command` a string, `args` a list of strings -
because a file where `args` is the string `"ab"` parses cleanly and `list()` turns it into
`["a", "b"]`. Anything that fails that validation is unreadable, and an unreadable file is never
appended to. Other fields such as `env` are left alone rather than refused.

A name that is not a bare key is written quoted, because a name containing a dot written raw becomes
a sub-table of another server: the registration the command believes it made would not be the one in
the file, and the next run would append a second.

The property that fixes is a round trip, not three cases: **what the writer emits, the reader reads
back unchanged, and a rerun then answers `LINKED`** — including values carrying backslashes, quotes,
control characters and the three-quote sequence.

Writing is held under an exclusive lock for the whole read-modify-write, re-reads immediately before
replacing, and replaces by temp file. That coordinates runs of this command with each other and
removes truncation. It cannot coordinate with an editor that does not take the same lock, so it is
not called compare-and-swap: a writer ignoring the lock can still land in the remaining window. The
hook file is written the same way, with the same stated limit.

## Six results that never imply one another

Diagnosis reports the six OPS-6.1 fields separately, in the OPS-6.2 shape, each with its own
evidence, exact command, acting process and measurement time. A field with no timed observation
behind it reports `unknown`; no time is ever invented or copied from another field.

| Field | Established by | Never established by |
| --- | --- | --- |
| `installed` | The component classifies `own` | The package importing somewhere |
| `mcpExposed` | Registration plus tool names observed in a live session | A configuration entry |
| `connected` | `doctor` reporting `actorReachability.socketConnect` as `ok` | A socket file on disk |
| `deliveryAccepted` | An attempt that recorded a returned turn id | A dispatch or an absent error |
| `verificationComplete` | Every OPS-6.4 condition at once | A completed turn or a green check |
| `alwaysActive` | A supervised runtime surviving a host restart | Any of the five above |

A live session is the only thing that can list MCP tools, so `mcpExposed` stays `not_verified` on a
registration alone. It reaches `verified` from real tool names only: either `--observed-tool`
supplied by a caller that is itself in a live session, or the tool list the bridge's own
`check_connection.py` returns, which is an actual MCP session over stdio. Every record names the
destination it measured and whether that destination was temporary, so a temporary-destination
proof cannot be read as a claim about a host.

Two further results are reported beside those six and are never merged into them, because importing
and preserving settings are separately falsifiable:

| Result | Established by |
| --- | --- |
| `imported` | The module imported under the resolved interpreter, carrying the `__file__` it resolved to, so an import satisfied by another copy is visible |
| `settingsPreserved` | Every other table in `config.toml` and every other hook entry byte-identical before and after |

## Trial mode

Diagnosis creates no work. `deliveryAccepted` requires an attempt that recorded a returned turn id,
which means creating one, so it reports `not_applicable` unless `--trial` is given. Trial mode is
the only mode that registers a relationship and sends, it names the scope it acted in, and it is
never implied by any other flag.

The send has to go far enough to produce the evidence. `emit` stores the receipt and enqueues the
delivery; the attempt itself happens in `deliver`. So the trial registers, emits, and then runs a
bounded `deliver` for that one event, and the field's evidence is the attempt's returned turn id.
An emitted receipt's own turn id never satisfies it. The authorized recipient and the requested
settings are recorded before the send, and the settings the host reported back are recorded with the
result.

Everything the trial needs is checked before its first command, so an incomplete trial writes
nothing: an artifact, a dispatch turn, a recipient equal to the parent task, and the recipient's
authorized settings. Each of those was previously discovered at the relay, after rows had already
been written. Settings are supplied with `--recipient-settings`, or acknowledged with
`--settings-already-recorded` when they are already authorized on the host; that acknowledgement is
recorded as the caller's own unverified claim, because every relay read constructs a store and there
is no read-only way to confirm it from here. A false acknowledgement can still reach the relay and
leave rows behind.

The dispatch request id is keyed on the issue **and** the dispatch turn. Keyed on the issue alone, a
second trial for the same issue replays the first generation, `generation-bind` then refuses the new
anchor, and the trial can only ever succeed once — which is not a delivery test. Keyed on both, a
retry of one dispatch still replays and reaches the same generation, and a genuinely new dispatch
opens its own.

The lookup's agreement is decided against the answer's `responsibleRelationship` field, not against
its serialized text. A substring test matches an archived assignment sitting anywhere in the
payload, so the guard meant to prove this process reads the expected store would pass against a
store where that relationship is closed.

`register` is the first mutating step, and the order is the guarantee. It is the producer of
the replay rule: it compares seven values - the parent task, the child task and the issue key
it hashes into a relationship id, plus the artifact roots, the allowed recipients and the two
host ids - and either
replays the relationship that already exists or refuses the whole registration without writing
anything else. The read-only lookup exposes only the first three, and no relay command returns the
other four, so the trial compares what it can read and lets `register` decide the rest before any
settings are recorded. A settings write placed ahead of it lands for a trial that `register` then
refuses on a scope or a host the lookup could never have shown.

What the trial compares is the whole identity the lookup exposes, not the responsible child alone.
An assignment carrying this issue and this child under a different parent hashes to a different
relationship id, and comparing the child alone read it as the same relationship. The report names
which fields were compared and which are decided by `register`, so it never reads as a complete
comparison of all seven.

One thing the trial cannot promise is that nothing at all was written: `assignment-find`
constructs a store, which creates the database and its schema. What a refusal before `register`
guarantees is that no settings and no relationship row were written.


Getting that far takes more than three commands, and each of the extra ones exists because the relay
refuses the send without it. Measured against a running App Server, the sequence is:

| Step | Why the send needs it |
| --- | --- |
| `assignment-find` | Runs first. A lookup run afterwards could find the relationship the trial itself just created, which says nothing about the store |
| `register` | Creates the relationship and opens its first generation, and is the first mutating step on purpose |
| `settings-record` | A send is withheld until the recipient's authorized settings are on record, because preserving them is what the delivery checks against. Recorded after the relationship exists |
| `generation-open` | Replays that same dispatch request id to read the generation number back; it opens no second generation |
| `generation-bind` | The generation `register` opened is unbound, and an unbound generation cannot be emitted against |
| `admit-turn` | Only the anchor turn is admitted by default; a turn the child actually ran is a continuation |
| `emit` | Stores the receipt and enqueues it. It carries an artifact, because a reviewable receipt with an empty manifest is refused |
| `deliver` | The attempt itself, and the only step that can return the turn id this field needs |

Those invocations are built as data rather than inline, so a test can compare them against the
required arguments the relay's own parser declares without performing a delivery. That check reads
the parser statically, because the relay declares a newer Python than this repository runs its own
checks with, and it covers every command the trial can send rather than the ones a fixture happened
to build.

## One shared service and one store

OPS-3.1 puts one relay service and one durable store behind an entire operating scope, which is one
host, one OS user and one App Server. A second repository or a second project installs into that
same scope and reuses the same service and the same store; nothing here creates a daemon or a store
per project, per repository or per parent.

Diagnosis therefore reports the resolved scope, the store directory and database, the service owner
and whether a service is running, by calling the relay's own `doctor` and `service status` rather
than by rediscovering any of it.

`doctor` is called three times, because one call cannot answer all three questions.

A call that selects a store explicitly skips sibling discovery altogether and reports
`checked: false`, because the caller already decided which participants share that directory. That
applies to `CODEX_SESSION_RELAY_STATE` exactly as it applies to `--state`, so the **discovery** call
passes the socket, no `--state`, and a sanitized environment with that variable removed; inheriting
it would silently produce an empty conflict inventory. The **selected** call uses the explicit
`--state` for the store actually in use.

The third call is targeted at the root of the state home. Discovery enumerates child directories
only, so a `relay.sqlite3` sitting directly in `<state home>/codex-session-relay` is invisible to
both calls above. A host can be in exactly that state, so it is inspected explicitly rather than
left out of the inventory. All three results are reported, a `checked: false` is preserved as not
checked rather than as none found, and no candidate is adopted.

A relay build that reports no `siblingStores` at all is a third answer again, and it is reported as
its own: an installed relay older than the revision that added sibling reporting emits nothing for
that field, and rendering that silence as an empty inventory would hide exactly the conflict the
inventory exists to surface. The summary distinguishes not reported, not checked, and checked.

Because a relay cannot always answer, the inventory also lists every relay database and operations
ledger visible under the state home, each marked as listed rather than identified. Listing files is
not rediscovering anything: nothing opens a database, chooses between candidates or decides which
one serves a socket, and that judgement stays with the relay. It is there so a host whose installed
relay is too old to report siblings still sees every store it has.

Equality of path strings is not proof under OPS-3.4. Proof is `doctor` from each participating
process reporting the same `stateDirectory` together with `assignment-find --issue` returning the
expected relationship. A relationship count is reported as the weaker observation it is: a count of
zero where an assignment is expected means the process is pointed somewhere else, and a nonzero
count from a different populated database would satisfy a count check while proving nothing.

That lookup constructs a writable store, and a diagnosis constructs none, so plain `diagnose` does
not run it and says so rather than claiming it happened. It runs in the trial, before any write, and
its result is compared against the relationship the caller independently supplies with
`--expect-relationship`; a store that does not hold it stops the trial before anything is written.
`diagnose --assignment-lookup` runs the same lookup on its own where that is wanted, and is
documented as constructing a store. One command cannot produce the reading from every participating
process that OPS-3.4 also asks for, and the report says so.

Every subsequent call sets both selectors, `--state` and `CODEX_SESSION_RELAY_STATE`, to the same
resolved absolute path, because under OPS-3.3 the flag alone moves the store while leaving the
adapter's `operations-<scope>.sqlite3` ledger behind. That ledger is reported as its own artifact.

Other stores beside the resolved one are reported, never adopted and never hidden. `doctor` already
distinguishes stores that record no socket from stores claiming the same socket, and a host can
hold both alongside a separate `operations-<scope>.sqlite3` ledger, which OPS-3.3 explains is
selected differently from the store. A host in that state is reported as ambiguous with its
candidates listed, because adopting one on a guess is how the wrong store gets served.

## The Linear hook

`runtime_install.py hook` installs the next-step Linear hook into the user hook file. Under OPS-6.3
a hook's identity is `<source>:<event>:<matcher-index>:<hook-index>` and Codex records a trusted
hash against it, so installation appends at the end and never inserts: inserting renumbers every
later hook in the same file and detaches the trusted hash recorded against the old identity. For
the same reason removal is refused rather than performed, and no content is silently updated.

The command records the identity, the trusted hash, the hook file path with its SHA-256 and the
issue that installed it, then reads the registration back. Installed, enabled and observed to have
fired are three separate claims and are reported as three. Installation is not activation: this
command never enables a daemon, and `alwaysActive` is a separate field with separate evidence.

What survives a hook installation is every existing identity and its hook content, so the trusted
hash Codex recorded against each one stays attached. The file bytes do not: the document is
reserialized. The MCP registration is the one that preserves bytes, by appending and leaving the
prior content as an exact prefix.

## What none of this establishes

Running the entry point against a temporary destination proves what it did there. It is not
evidence about a host's real Codex home, its installed runtime, its MCP registration or its
operational database. `installed`, `mcpExposed`, `connected`, `deliveryAccepted`,
`verificationComplete` and `alwaysActive` are six separate facts under OPS-6.1, and `imported` and
`settingsPreserved` are two more beside them. None of the eight is read from another.

A successful update is not one of them either. That the pointer moved, that the gate found the
daemon stopped and no attempt open, and that the store's tables were compatible are three
readings taken at one moment, about one destination. They say a swap was permitted and
performed; they do not say the new runtime works, and the point that would say so is measured
before the swap rather than after it. Nor does a refused update establish that a store is
healthy: the gate reads whether it is safe to replace a runtime, and reads nothing about
whether the data in the store is correct.
