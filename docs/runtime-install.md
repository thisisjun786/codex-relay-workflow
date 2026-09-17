# Runtime installation, update and diagnosis

[POLICY.md](../POLICY.md) owns repository rules and
[the operations contract](../skills/crw-run/references/operations.md) owns the operational ones.
This page describes the runtime entry point that installs, updates and diagnoses the MCP bridge
and the session relay, and it is written against that contract's clause numbers so a reader can
check a claim against the rule it came from.

Updating is the half that can lose something. A first install has nothing to destroy; a second one
is standing on a runtime somebody is using and a database nobody can rebuild, so most of what
follows is about what is read before anything moves and what is put back when it does not.
[Updating an installation](#updating-an-installation) is where that lives.

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

### An answer about a state that was found has to be able to say there was nothing there

Three review rounds in a row produced what read as three separate defects, and they were one
thing missing in three places. It was never a check nobody had written. It was a **value an
answer set could not express**.

| Where | The answer it could not give | What that cost |
| --- | --- | --- |
| the in-flight cell | established absent | a clean host could never promote, while the schema cell answered `NO_STORE` about the same store |
| the pointer rollback | restore to absence | a failed first install left a link naming a candidate nothing selected, and the candidate was then kept BECAUSE the pointer named it |
| the selection rollback | remove a selection that had none | the same install left its own candidate selected, and a selected candidate is never released |

All three end in the permanent refusal the update path exists to remove, reached from three
directions. So the rule is stated at the layer the instances came from rather than patched a
fourth time: **where absence is a normal state, the answer set is incomplete until it can say
so.** A rollback that can only restore a value cannot restore "there was nothing"; a cell that
can only report a reading or a failure cannot report a question whose true answer is zero.

`ABSENCE_ANSWERS` declares each place and the operation it answers absence with, and the check
**derives** the places from the source instead of reading that list: a function that is handed
the state it found — a parameter named `previous`, `before` or `presence` — is answering about
something that may not have been there. A derived place with no declaration fails, and so does a
declared operation that either does not exist or is never used where the answer is given, because
a capability nothing calls is the same silence as no capability at all. The scan carries injected
violations of each form, so an empty finding list is not mute.

The two absence deltas are compare-and-remove rather than remove. `deselect` takes away only an
entry that still names what this run wrote, and `drop_pointer` only the ownership record for the
path this run recorded. Undoing a promotion this run never made is a worse outcome than the
failure being rolled back.

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

### A registration written before the pointer existed

The pointer only helps a host that has one. A host installed by an earlier version of this
command registered a concrete entry point, and comparing that with the pointer reads as a
conflict — which refuses the update and then deletes the candidate it has just built. That
made the installed base whose pinned registration the pointer exists to unpin the one base
that could never receive it.

So a conflict is checked against the host record before it is believed. A registered command
that the record names as an entry point of an install this command made is this command's own
earlier registration, not somebody else's, and it does not refuse the update. Ownership is
established positively from the record: a path that merely looks familiar proves nothing, and a
registration nobody recorded stays the conflict it is.

Recognising it is not migrating it, and the difference is worth stating plainly. After the
update the configuration still names the predecessor. That is not a broken host — the
predecessor is preserved and still works — but Codex goes on spawning the previous bridge until
`register-mcp` is aimed at the pointer. Moving an existing registration would need this
repository to rewrite a table it did not write, and the append-only writer proves it preserved
everything by requiring the prior content to be an exact prefix of the new file, which an
in-place edit cannot satisfy. That is a different contract, so it is named here rather than
improvised.

### The claim a run leaves behind

The environment name is deterministic and the directory is created with an exclusive `mkdir`,
which is what proves a run owns it. That proof used to expire badly: a run killed outright left
the directory behind, and every retry of the same destination refused at the existence check for
ever.

A run now leaves two files in the directory, and they are two because they answer two questions.
The **lock** answers whether anybody is still building, and it is created once and never
replaced. The **claim** answers what that run said it was doing, and it is rewritten when the
staging settles. Collapsing them is not a tidiness question: an advisory lock belongs to an inode
rather than to a name, so locking the file that is later replaced by rename leaves the lock on an
unlinked inode while the next reader opens the new one and finds it free. That reported a live
build as abandoned, and the next run deleted a directory somebody was still building. It is two
files because of that.

Removing anything needs positive proof of ownership, so the claim has to carry this command's own
marker, its claim version, and a state from the declared set. Readable JSON at that path is not
proof; a file somebody else left is left alone.

| Observed | Answer |
| --- | --- |
| No claim, and the directory holds files | Somebody else's. Refused, nothing touched |
| No claim, and the directory is empty | Taken over as it stands with `rmdir`, which succeeds only on an empty directory, so the operation is its own proof that nothing was destroyed |
| A claim of this command's, the lock held | Another run is building it. Refused, nothing touched |
| A claim of this command's, the lock free, nothing using it | An abandoned staging. Reclaimed |
| A claim, and whether anyone holds it could not be established | Kept, and reported as a residual path with what recovery needs |
| A settled claim, and the environment is in use | Already installed. Reported, nothing rebuilt |
| A settled claim, and nothing selects it any more | Kept. It is a runtime that was promoted once, and a process may still be running out of it |
| An unsettled claim for an environment that IS selected | An interrupted promotion. Finished rather than rebuilt |
| A lock held with no claim written | A run between taking the lock and writing its claim. Refused, nothing touched |

The first row hid the installed base. Claims are newer than the installations they describe, so
every installation made before them is populated and carries nothing saying who made it — which
is exactly how the table read somebody else's directory. The environment name is derived from the
sources, so that refusal is permanent for that combination: there was no installed host this
updater could move forward, which makes it not an updater.

| Observed | Answer |
| --- | --- |
| No claim, the directory holds files, and the host record selects a runtime inside it | This host's own installation, older than claims. Brought under this command's bookkeeping; nothing rebuilt, nothing removed |

The branch order is deliberately unchanged. Asking the conservative protection reading earlier
would let a reading that FAILED authorise reuse, which is the one substitution this whole path
exists to prevent. Ownership is established positively instead, from the narrow reading: the host
record was read, and it says the runtime it selects lives in this very directory. Nothing else
qualifies — a populated directory the record does not select is still somebody else's, and a
selection reading that failed authorises nothing.

What that writes is the bookkeeping the installation never had: a settled claim, and a pointer
aimed at the environment the record already selects, recorded as this command's. The record
matters as much as the link, because the promotion refuses to replace a link this record never
recorded placing — so adopting a host without recording the pointer would adopt it once and
refuse it for ever after.

Two limits belong with it. The adopted environment's bytes are not re-measured here and the swap
gate is not asked, because this replaces nothing: the directory can only be at that path if it
was built from these sources, and the record already selects it, so the runtime a host reaches
afterwards is the one it was already running. And where a pointer exists naming a different
recorded environment, aiming it at the selected one is the documented repair for a selection and
a pointer that disagree — the same repair a resume performs, and with the same limit, which is
that neither re-runs the gate conditions.

A directory taken over with `rmdir` first has this command's own two files cleared from it, and
only those two. A run whose claim write failed used to leave its lock file behind, and `rmdir`
refuses a directory that still holds one — so the deterministic destination was blocked for ever,
which is the failure this whole path exists to remove, arriving by a narrower door. Such a failure
now releases the directory it created like any other.

Finishing an interrupted promotion asks a narrower question about the link than an ordinary
promotion does. It cannot ask for agreement, because a resume necessarily finds the pointer
disagreeing with the selection — that IS the interruption it repairs. It asks instead whether the
link still names a runtime this host record accounts for, and refuses one repointed by hand while
the run was dead.

"Accounts for" is equality against a recorded environment or install location, and containment in
neither direction. A target that CONTAINS a recorded path is not a recorded runtime: the
destination root is the parent of every environment under it, so a link repointed at the
destination read as accounted for and was replaced. The containment helper asks the opposite
question — is this path inside that root — and is right everywhere it is used; it was the wrong
question here.

Liveness is the lock and not the recorded process id, for the reason the relay already recorded
about its own supervisor: inside a container sharing a kernel, the same process id under the same
boot id is a different process, and a process identity that can lie is worse than no reading. The
lock cannot lie about contention. Where `flock` is unavailable the answer is that nobody could
tell, and an owner nobody could establish is never read as an owner that is gone: deleting a live
run's environment is the accident this exists to prevent. Such a directory is kept and named, so
an orphan is findable and reportable rather than either silently accumulated or silently removed.

The lock's lifetime is the run's. The operating system releases it when the process ends however
it ends, which is what makes a killed run readable as abandoned, and a run that reaches an end of
its own releases it rather than leaving the answer to exit.

Deciding and acting are one step, under a second lock beside the directory. Reading first and
acting later is not safe even with everything above: two retries can both find the same
abandoned staging and both decide to reclaim it, and the first then deletes it, recreates it and
starts building while the second deletes that live build on the strength of an answer it got
before any of it happened. So the reading is taken again inside that lock, immediately before the
removal, and a run that cannot take the lock reports that and touches nothing. Past this point
the exclusive `mkdir` is what a competing run loses to, as it always was.

### Reading whether it is safe to swap

OPS-4.4 sequences an update around a daemon that is not running and open attempts that have been
reconciled. Three readings answer that, each filling only its own cell:

| Cell | The reading that answers it |
| --- | --- |
| `daemon` | the relay's `service status`, whose `running` is decided by the lock a supervisor holds |
| `inFlight` | whether a store is there at all, then the relay's `doctor`, whose `contents.openAttempts` counts in-flight and held-uncertain attempts |
| `storeTables` | the store's own table inventory, read read-only through the relay's `read_only_rows` |

The in-flight cell reads twice, and the order is the point. The relay reports contents
unavailable both for a store that is missing and for one it cannot read, and those are opposite
answers here: an absent store has no open attempt, an unreadable one has an unknown number.
Without the first reading the cell could not say "established absent", so a first install on a
clean host refused for ever while the schema cell, which does look at the path, answered
`NO_STORE` about the very same store. Two readings of one cell's own question is not a cell
borrowing its neighbour's answer; it is the ordered observation the record reader already makes,
where absence is settled by looking before anything is opened. Two readings that disagree are
still no answer.

The swap proceeds only when the daemon is established stopped, the open attempts are established
zero, and the store's tables are established compatible. Any cell that could not be read decides
`UNESTABLISHED`, which keeps the existing installation exactly as a blocking answer does. A
check that could not be made is not a check that passed, and a daemon is never reported stopped
because nobody could ask it.

This command never starts or stops a daemon. OPS-4.1 gives the service to the scope operator, so a
running daemon is a refusal here and not something to resolve.

What the daemon cell does **not** guarantee is worth stating, because the gate would otherwise
read as stronger than it is. The relay's own liveness answer releases its lock before returning,
so `STOPPED` describes a moment that has already passed. Taking the reading inside the promotion
lock narrows the window to the promotion's own length; it cannot close it, because that lock
excludes other runs of this command and says nothing to a supervisor. Closing it would mean
holding the store's daemon lock across the gate and the promotion — a lock OPS-4.1 deliberately
keeps in the scope operator's hands — so it is a question about the contract rather than about
this code, and it is left open rather than answered here.

### Why the schema reading compares statements and not versions

The obvious reading would compare the store's recorded schema version with the candidate's. It
would also be worthless. The relay declares `SCHEMA_VERSION = 1`, has never raised it, writes it
once with `INSERT OR IGNORE` when the database is created, and grows its schema through
thirty-nine separate `CREATE TABLE IF NOT EXISTS` statements. Every store therefore agrees with
every candidate at version one, and the comparison would detect neither a downgrade nor an upgrade
while looking exactly like a check.

So the cell compares what actually differs: each table's `CREATE` statement in the store's
`sqlite_master` against the statements the candidate relay declares. Statements and not names,
because names agree while a column, a constraint or a default differs, and that difference is a
schema change the new runtime would apply the first time it opens the store for writing.
Runs of whitespace **outside** quoted text are normalised away, because SQLite keeps the original
CREATE text verbatim and formatting drifts between a store written long ago and a candidate's
current DDL. Nothing else is. Going further is not free: lowercasing the statement made
`DEFAULT 'A'` and `DEFAULT 'a'` compare equal, and collapsing whitespace inside quotes made
`'a  b'` and `'a b'` compare equal, and both are real schema differences reported as agreement.
What remains is stated rather than implied: two statements that mean the same thing written
differently are reported as a difference, which refuses an update and therefore keeps the
previous installation. A reading that carries table names without their statements cannot answer
this cell at all and says so, because names agree while a column differs.

| Answer | Observed | Decision |
| --- | --- | --- |
| `NO_STORE` | no store exists at the resolved selection | allowed, and reported as absence rather than as agreement |
| `AGREES` | the same tables, defined identically | allowed |
| `EXTENDS` | the candidate declares tables the store does not hold | refused |
| `DIFFERS` | a shared table is defined differently | refused |
| `NARROWS` | the store holds tables the candidate does not declare | refused |

`NARROWS` is the implicit downgrade the issue forbids: a runtime that does not know a table
cannot preserve what is in it. The other two refuse for the contract's reason rather than that
one. The relay opens its store read-write and runs its whole DDL script on every open, so a
candidate whose schema is not the store's schema **applies** the difference the moment the new
daemon first starts. OPS-4.5 reserves that for its own decision, in its own issue, with a copied
backup of the whole state directory taken first, so letting an update wave it through is exactly
the implicit migration the clause forbids. An update is not the place either direction is decided,
and the refusal names the tables so the next step is obvious.

The reading is the relay's own, run under the relay's own interpreter. A second copy of the rule
here would be a restatement of something the relay owns, and the next change would move only one
of them. It opens the database read-only and runs no schema script, so asking the question does
not create the store the question is about. Absence is established by looking at the path, never
inferred from a failed open, because a permission failure and a locked database also fail to open
and neither of them means nothing is there. The report names which selection answered, since an
absent store at the wrong state directory while a sibling store holds the in-flight attempts is
the OPS-3.4 conflict rather than a clean host.

### The order a swap commits in

The selection in the host record and the pointer on disk are two truths, and both the order they
are written in and the lock they are written under are the safety argument. They are written
inside **one** critical section, holding the pointer's lock, and the selection is committed first.

The reverse order has a real failure: the symlink lands, the record write then fails or the
process raises, recovery reads a selection that does not name this environment, concludes the
candidate was never promoted, removes it, and leaves the registered MCP command pointing into a
directory that no longer exists. OPS-4.4 requires every state transition to be committed before
its side effect, and this is that rule applied to the two halves of one promotion. Recovery also
refuses to remove an environment the pointer names, so neither truth alone can authorise deleting
a runtime the other one is still using.

Every judgment the promotion makes is decided on state read INSIDE that lock, and that rule is
declared rather than remembered. Three separate review findings turned out to be one defect
arriving three times: the swap gate ran against the record loaded before the build, the rollback
baseline was captured before the build, and the classification read a pointer at the destination
rather than the one the record names and the swap replaces. Each is the same shape, a decision
taken in the critical section on a value read outside it, and each was reported on its own
because nothing was looking at the class. `PROMOTION_FRESH` names the set and a check fails any
member read there without being read fresh there, so a fourth fails a test instead of arriving as
another round.

That lock is an advisory lock on one host-wide file beside the host record, and both halves of
that are corrections. The lock this command uses for the short staging decision excludes by
FILENAME on a path the caller derives, and decides validity by a 300-second modification time.
Neither survives a promotion: two installs with different destinations derived different pointer
paths, locked different files and never met, and a promotion that outstayed the window had its
lock unlinked by a waiter while it was still working — because excluding by filename gives the
holder's open descriptor no protection at all. Three reported defects, one set drawn wrong. The
promotion lock is created once, never unlinked, and released by the operating system when its
owner dies however it dies.

Holding one lock across both writes is what keeps two runs of this command from interleaving there
and finishing with the record selecting one runtime while the pointer reaches another. It is a lock
between runs of this command and nothing more: an editor or another tool that does not take it is
not coordinated with, exactly as the configuration writer says of its own. And it cannot stop the
process being killed, so a kill inside that window leaves a runtime that is selected and
unreachable. That state is recognisable rather than fatal: the claim is unsettled and the
environment is selected, so the next run finishes the promotion instead of rebuilding it.
Rebuilding would be the wrong repair, because the runtime is built, it is already selected, and a
process may be running out of it.

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
`restored` names the selection that was put back or says there was none to put back.

"Restores the previous selection" is narrower than it sounds, and deliberately. The rollback runs
under the promotion's own lock and puts back only the entries that still name what **this** run
wrote. If another run has promoted something else in the meantime, that entry is left alone and
the result says so in `movedOnByAnotherRun`: rolling back on top of somebody else's success is a
worse outcome than the failure being rolled back.

Putting a selection back includes putting it back to nothing. A component that had no previous
selection — a first install, and a legacy install whose combination was never selected before —
has the entry this run wrote taken away, in the same write that restores the entries that had a
previous value, so half a rollback cannot land. Until the delta set could say that, the run
reported a rollback, the pointer correctly went back to absence, and the candidate stayed
selected; being selected is then exactly what keeps a candidate from being released, so the
destination could never be retried. The result names what went back in `restored` and what was
taken away in `removed`.

What the pointer is put back to includes being put back to nothing. A first or legacy install
has no pointer, so the swap creates one, and a rollback that could only restore a previous
target left that link naming a candidate the selection had just been taken away from — after
which recovery kept the candidate precisely BECAUSE the pointer named it, and the staging could
never be reclaimed. Absence was the value missing from that answer set, the same shape as the
established-absent answer the in-flight cell was missing. Removing a pointer is guarded the way
placing one is: only a symbolic link, only while it still names what this run placed, and the
absence is read back before it is claimed. A restoration that cannot be read back reports a
residual pointer and keeps the candidate rather than claiming the rollback completed.

The ownership record goes with the link. The record is what makes a link this command's — the
promotion refuses to replace one the record never recorded placing — so a rollback that removed
the link and left the record behind said this command owns a link that is not there, and armed
that guard in favour of whatever appeared at that path next. It is dropped only after the link is
verifiably gone, and only for the path this run recorded.

The two outcomes recovery already had are unchanged. Removal verified on the filesystem means the
destination is retriable; removal that could not finish reports the residual path, what recovery
needs, and the original failure alongside the cleanup failure rather than replaced by it. A
candidate that is selected, or whose record could not be read, or that the pointer names, is kept.

Two of the failure points the issue names do not exist in this command, and saying so is better
than implying a rollback that has nothing to roll back. **Applying configuration** is
`register-mcp`'s, not `install`'s, and with the pointer in place it happens once rather than on
every update; its own failure contract is above, including the one case where a write lands and
cannot be read back, which is reported as `APPLIED_UNVERIFIED` rather than as a refusal that wrote
nothing. **Starting** does not happen here at all: OPS-4.1 gives the service to the scope operator,
so this command refuses while a daemon runs and never starts one, and there is no start to fail or
to undo.

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

## The completion hook

`runtime_install.py hook --adapter completion` registers the Stop hook that catches a managed
turn ending without the records a completion needs. It is the same install path as above, with the
command derived from this checkout instead of typed, and it lands on `Stop` unless the caller
names another event.

The decision is not made in the hook. [The hook contract](../skills/crw-run/references/hook-contract.md)
fixes the rules and the relay's `guard-evaluate` implements them, down to the exact Stop JSON to
print. `scripts/completion_hook.py` is the piece between the host and that guard: it reads the
delivered payload, asks the configured runtime, and prints only a block that runtime produced.

It cannot cost a turn. The host reads exit 2 as the blocking code and takes stderr as the
continuation prompt, and `argparse` exits 2 on any usage error, so the entry point parses no
arguments, writes nothing to stderr, captures the subprocess's streams rather than inheriting
them, and exits 0 on every path. A stale flag left in somebody's hook file is a hook that does
nothing, not a hold on every ordinary turn.

Its settings are its own file, `crw-completion-hook.json` beside the hook file, and they are
written before the hook that reads them: a hook registered against settings that are not there
answers `config_absent` on every Stop and releases, which is an installed hook that does
nothing and says so nowhere. Settings that already say something else are refused rather than
overwritten, because they carry the mode and a silent rewrite changes whether turns can be held
at all. `observe` is what an install writes; holding depends on per-session write isolation this
command cannot grant.

Settings this command can generate but its own reader cannot act on are refused rather than
written, so an install cannot report success and leave every later Stop reading those settings as
malformed. The adapter's budget is checked against the registered timeout at the same point,
because that is the one value whose meaning needs both files: a budget the host's timeout does
not exceed lets the host kill the adapter before it records why it did not answer.

The event is checked the same way and for the same reason. This adapter implements the `Stop`
contract and has a decision for no other event: elsewhere the payload means something else and
the output schema carries no top-level decision, so a hook registered on another event would
never see the turn it was installed to watch. Naming one is refused; another event still takes
an explicit `--hook-command`. Settings, budget and event are three checks of one kind, and the
kind is that an install must not succeed and leave a hook that is registered, inert and silent
about it.

The read-back belongs to that same kind. Settings that were written and could not be read back
as written answer `config_applied_unverified`: applied, because reporting a landed write as
nothing written invites a retry over a file that now exists, and unsettled, because a hook must
not be registered against settings nobody has read. Which outcomes settle follows the read-back
rather than the write's intention.

The registered command is a command line, so its two words are joined with shell quoting and
read back by the same rules. Concatenating them raw fails in two sizes: a path holding a space
is delivered as more words than it is, and a path holding shell syntax is delivered as syntax
and runs on every Stop. Ordinary paths are unchanged by the quoting. `hook-status` recognises
this adapter's own registrations by a complete argument whose last component is the entry point's
name, never by the command's text containing it: a program called `not-completion_hook.py`
contains that name and is a different program.

The runtime is named through the owned pointer, `<dest>/current/bin/codex-session-relay`, and
never through `PATH` or a checkout path. A host can carry a relay on `PATH` whose build
predates the guard: the file is there, it runs, and it rejects the call. That the executable
exists and that it offers `guard-evaluate` are two questions, and `hook-status` answers them
as two cells for that reason.

Every path the settings record is made absolute when they are written and required to be
absolute when they are read, because this hook runs with the session's own workspace as its
directory: a relative path would resolve somewhere the install never named, and a bare name
would be looked up on `PATH`. The pointer itself is not followed, so an update moves the link
and these settings keep naming the runtime that is actually selected.

The registered command carries the settings path the install resolved, as a third word the entry
point reads positionally and never parses. Otherwise the path would be resolved twice, in two
different directories and under two different values of `CODEX_HOME`, and the second resolution
is the one that decides what every Stop reads. The install decided which file it wrote, so the
install is what says which file to read.

The interpreter in the registered command is settled the same way, and a bare name is looked up
at install time, on the machine doing the install. That is the only moment the lookup means
anything, because the hook runs later from each session's own workspace.

It also has to be a Python this adapter runs on. Executable is not the question: `/bin/true` is
executable and exits 0, and a Python below the supported version fails the same way and looks
identical from the hook file. Both are refused. The candidate is executed only when the command
is going to write, because a plan that writes nothing should not run a program the caller named,
and what it did not check it does not claim.

The registered timeout is held to the one this repository has evidence for. The host clamps an
over-long timeout at discovery and the clamped value was not measured, so a large number is not
the deadline it appears to be and could land under the guard budget.

The marker root follows the relay's own resolution, including
`CODEX_SESSION_RELAY_MARKER_ROOT`. A default that skipped it would not be a default but a
disagreement: the coordinator would publish its intents under one tree while this hook looked
under another, and every managed turn would read as unmanaged with nothing recorded.

`--mode hold` additionally requires `--isolation-asserted-by`, recorded in the settings and
shown by `hook-status`. The contract makes per-session write isolation a prerequisite for
holding and not for observing, so the assertion is a named record rather than something
inferred from the mode having been set.

A second registration of this adapter that differs from the one already there is refused rather
than appended. Installation appends and never removes, so appending would leave two copies
running on every `Stop`; the existing identity is named so it can be edited.

Every precondition is checked before any write, and that ordering is the point rather than an
accident of how the command grew. The interpreter, the budget, the event, the settings' own
readability and the duplicate registration are one list. The shape this protects against is the
expensive one: settings written, duplicate refused afterwards, and a hook already in the file
running against settings the command had just reported it would not install. The duplicate check
and the append are still not one atomic step, so the registration is read back afterwards and a
second copy is reported rather than claimed away.

`hook-status` writes nothing of its own, but it is not inert: answering whether the runtime
offers `guard-evaluate` means running that runtime with `--help`, and what that runtime does
is outside this command's control. The command says so in its own output.

`hook-status` probes these paths through the four reading states rather than asking whether a
file is there. A runtime behind a permission wall and one that was never installed answer
differently, because they are repaired in different places, and a runtime that could not be
reached is not asked whether it offers the subcommand.

Only a verdict that agrees with itself is acted on. Both halves are read: a verdict whose own
decision releases while its `hook_output` holds did not come from the guard, and rebuilding a
block out of the nested half alone would let this adapter deliver a hold nobody decided. Any
disagreement reads as `guard_verdict_incomplete` and releases.

Failures keep their own names. A runtime that could not be run carries its `errno`, because a
moved pointer and a file that cannot be executed are different repairs. An exit of 2 carrying the
relay's own error record is the relay declining a request it understood; an exit of 2 carrying
nothing is its argument parser refusing before any command ran. Every one of these releases the
turn and is recorded.

## Registration is not firing

`runtime_install.py hook-status` reads and writes nothing. It answers registration, the host's
trust state, whether the registered command's target still exists, the settings, the runtime,
whether that runtime offers the subcommand, this hook's own record of its invocations, the
guard's records, and the daemon, each as its own cell. `not_read` is used where a question was
not asked and is never written as an absence.

This hook records one entry per invocation by default, and the default is not frugality. The
guard publishes an observation only when it selected an assignment, so on a host with no managed
session it writes nothing at all, and an empty firing record would be indistinguishable from a
hook that never runs. The count is always reported beside the policy that produced it.

The daemon is not on this path. The guard reads the marker and a read-only database, so a stopped
daemon is not observable from a Stop and is never inferred from one; `hook-status` answers it
separately or says it did not look. Long retries and whole verification loops belong to the daemon
and to the coordinating task, not to a hook with a five-second budget.

## What none of this establishes

Running the entry point against a temporary destination proves what it did there. It is not
evidence about a host's real Codex home, its installed runtime, its MCP registration or its
operational database. `installed`, `mcpExposed`, `connected`, `deliveryAccepted`,
`verificationComplete` and `alwaysActive` are six separate facts under OPS-6.1, and `imported` and
`settingsPreserved` are two more beside them. None of the eight is read from another.

A registered completion hook is not a fired one, and a fired one is not a delivered hold. That a
line is in the hook file says nothing about the host having run it, about the runtime it names
being able to answer, or about any turn having been judged. Those claims need the host's own
evidence, not this command's.

A successful update is not one of them either. That the pointer moved, that the gate found the
daemon stopped and no attempt open, and that the store's tables were compatible are three
readings taken at one moment, about one destination. They say a swap was permitted and
performed; they do not say the new runtime works, and the point that would say so is measured
before the swap rather than after it. Nor does a refused update establish that a store is
healthy: the gate reads whether it is safe to replace a runtime, and reads nothing about
whether the data in the store is correct.
