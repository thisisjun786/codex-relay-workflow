# Runtime installation, update and diagnosis

[POLICY.md](../POLICY.md) owns repository rules and
[the operations contract](../plugins/crw/skills/crw-run/references/operations.md) owns the operational ones.
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

The `appServer` dimension is the bridge check's whole `get_capabilities` payload, serialized and
compared by exact string equality. That is deliberate: the identity of the server a combination was
exercised against is everything the round trip reported, not a version string someone chose to trust.
It also means any change to that payload's shape changes the dimension, so a point measured before
such a change does not cover a bridge built after it and the installation reads `unmeasured` until
it is measured again. The old point is not wrong and is not discarded; it remains evidence about the
build it was taken against, which is the behaviour a dimension is for. CRW-21 changed that payload,
so any point recorded before it needs one `measure` run to cover the current bridge. The same
property is why the payload carries no timestamp and no probe result: a dimension that varied
between two calls to the same build would never match itself.

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

That was one sibling. `install` and `register-mcp` answered the same event properly and the hook
path did not: with the hook file locked, `hook --apply` reported
`internalError: TimeoutError` naming `hostrecord.py:292` - a claim that this command has a
defect, which is about the code rather than about the host and sends whoever reads it somewhere
that has nothing wrong with it. Answering it at the hook and stopping would be the repair that
reopens at the next sibling, so `main()` answers a busy lock as well, ahead of the arm that files
anything unmodelled as a defect. `cmd_hook` still answers for itself, because it is the one that
knows the settings are written before the hook and a lock taken between them leaves them on disk.
The check reads the lock reachers as a call graph rather than a list, and requires the busy arm to
precede the catch-all, because an arm after it is unreachable.

Review then found the other half of it. `TimeoutError` is an `OSError`, and a destination on a
network mount raises it with `ETIMEDOUT` for an ordinary filesystem call, so answering the
built-in would claim another run holds a lock that was never involved - the same defect, inside
the contract that exists to prevent it. The lock raises `hostrecord.Busy`, its own type, which
subclasses `TimeoutError` so a caller that already answered the broader question keeps working.
The check requires the narrow type and forbids the broad one.

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

### A pair fixes that there is a predicate, not which one

Declaring a member as a pair closed the layer above and opened this one. A pair says a member HAS
a predicate and a cell HAS a reading. It says nothing about whether that predicate is the
strongest one the consumer applies, or whether the cell has more than one place that writes it.
Both gaps produced a working, well-formed, wrong answer.

`NON_BLANK` is this command's own minimum and nothing more, so a member left on it has every
further question about its value answered here. The artifact root was that member: its real
question is containment, and containment was decided by `base in path.parents`, a second copy of
a rule the relay owns. The copy was not the safe approximation it looked like. It disagreed with
the relay in **both** directions - it refused `<root>/../<root>`, which the relay accepts end to
end, and where the relay does refuse a root it named the deliverable as the thing at fault. Driven
directly, a relative root registers - the relationship row is written - and is refused at `emit`
with `scope_escape`.

So a member carries the predicate its consumer applies, and where that predicate is relational it
names the member supplying the other operand. `--artifact-root` is asked of `scope.assert_within`
after `normalize_declared_path`, which is the pair `AuthorizedFile` itself asks, in that order.
`--recipient` is asked of `scope.check_recipient`. `--turn-thread` cannot be asked of anything:
the relay holds that rule inside a method that needs a store. It is restated here and **declared**
as restated, naming where the original lives, and a check reads that place back - which is how the
commit introducing it was caught naming a class the relay does not have.

A cell has the same shape one level down. `entry_point_recorded` declared one reading and had two
assignments. The second filled the ownership cell from the interpreter a console script's first
line names, which `interpreter_of` already calls the fallback rather than the answer. A wrapper
this command never created, sitting outside every recorded root, whose author wrote a shebang
naming an interpreter inside a recorded environment, classified as this installation. The second
site now answers only from an interpreter the record names, and the cell declares both readings.

Two scans hold these instead of the instances, and neither names a member, a rule or a cell. One
follows a declared member's value through the preflight and reports any comparison this command
makes about it that is neither its own minimum nor the consumer's answer; the count comes off the
declared restatements, so a rule restated without being declared fails. The other reads which
local feeds each judgment cell, out of the `Signals` call itself, and requires every assignment to
it to name a reading that cell declares. Both carry a negative control.

### A walk that skips is not a walk that failed

The same class reached from underneath. A reading can also fill a cell wrongly because it never
reported a failure at all. `ops12_digest` walked with `rglob`, which answers a subtree it cannot
read by leaving it out. For a package with one unreadable subdirectory the digest that came back
was not merely wrong: it was byte for byte the digest that smaller tree really has. Nothing raised,
so the reading region around the call saw a value, the comparison saw a mismatch, and the component
was reported a **fork** - a claim that somebody had modified an installation nobody could read.

An incomplete reading is not a value. The walk is now explicit and fails on a directory it cannot
open, so the boundary reports `ACCESS_ERROR` and the cell goes unread. The file set is unchanged:
both committed package digests re-derive exactly, and `verify-definition` still reports no
findings. `OMITTING_READERS` names the readers whose answer to an unreadable subtree is omission -
`rglob`, `glob`, `iterdir` and `os.walk`, whose default `onerror` discards the error - and
`OMISSION_DECLARED` names each place one is used with what omission means there. `os.scandir` is
deliberately absent from that list: it raises, which is the behaviour the list exists to require.

Pruning is not omission, and review found where the difference bites. The walk opened every
directory, including the `__pycache__` the definition excludes, so a cache directory nobody can
read turned a perfectly readable package into an unreadable one at every boundary that asks for
its digest. An excluded directory cannot change the answer, so it must not be able to withhold
it: it is pruned before it is opened, and every subtree that can affect the answer still raises.

### What each of these answered before the fix

Four instances of one class, each driven against the commit before the fix and against the commit
after it, by the same probe. None of them asks whether a fix is present; each one exercises the
defect and reports what the code answered.

| Instance | Criterion it reopened | Before | After |
| --- | --- | --- | --- |
| a foreign wrapper's first line decides ownership | 3 | `entryPointInRecordedPath=True`, `interpreterFrom="the script's first line"`, class `fork` | `False`, class `foreign` |
| an incomplete walk comes back as a value | 1, 3 | raised nothing and returned the smaller tree's own digest | raises `PermissionError`; classification refuses with `ACCESS_ERROR` |
| a busy hook lock is reported as an internal defect | 6 | `internalError: TimeoutError` at `hostrecord.py:292` | `outcome: BUSY`, `internalError: null` |
| the artifact-root question is answered by a rule written here | 5 | the relay holds `<root>/../<root>` and the preflight refuses it | the two verdicts agree on every form of the root |

So the four criteria hold for the reasons they were written, rather than by assertion. Criterion 1
and criterion 3 required a reading that cannot answer to stop the classification; a walk that
omitted a subtree was answering, and it no longer is. Criterion 5 required the trial to write
nothing it cannot complete; the root is now judged by the rule that will actually be applied to
it. Criterion 6 required a failed run to report whether its destination is retriable rather than
an internal error; the hook path was the sibling still doing the latter.

One residue is recorded rather than fixed: `scripts/hook_comparison.py` also walks with `rglob`.
It is outside this change's scope and fills no judgment cell, so it is named here instead of being
swept in.

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

`restore_pointer` is the third rollback delta and the only one that puts a value **back**, for
the half of that question absence cannot answer. The pointer ownership entry answers two things
at once: `path` is which path this host's pointer **is**, and the placement keys
(`hostrecord.POINTER_PLACEMENT`) are the evidence that a link this command **placed** is there.
Absence is the right rollback only for a run that INTRODUCED the entry. A run that inherited one
and failed must not erase it, because the path goes with it and the registration names that
path — a retry with a different `--dest` then derives another path and reads a registration
nobody changed as a conflict. So an inherited entry goes back: whole where the link was put
back, and with its placement **withdrawn** where the rollback established the link is absent,
which keeps the path and still refuses a link that turns up there afterwards. It compares
against the path **this run wrote** and carries the entry it **found** as two separate values,
because a caller handed its path before the lock can have written over an entry naming
somewhere else. What the rollback actually did is read back from the record rather than inferred
from the delta having been sent, so it can answer `moved on` truthfully. It reports the state
the record was left IN, which is not the same claim as "this call wrote it": a compare that
matched what was already there reports the same answer, and that is the honest one, because the
question is what a later run will read.

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
and the previously selected runtime remains selected. A failure at any step **up to the
promotion** leaves the previous runtime in place, and nothing here removes, moves or recreates
the store: update failure and store loss are different accidents and the recovery for one must
not cause the other. Past the promotion there is one exception, and it carries its own exit
status.

### The record is not the replacement

The staging claim is written last. It says this staging finished, and until the selection is
committed and the owned pointer names the environment there is nothing finished to say -- so by
the time writing it can fail, the registered command already resolves into the new runtime. The
replacement has happened and only its record has not, and those are reported as two outcomes
rather than folded into one.

| Field | Answers |
| --- | --- |
| `promoted` | whether THIS run replaced a runtime. A resumed promotion did; adopting bookkeeping for an installation the record already selected did not |
| `inService` | whether this destination must be kept. True while the record selects this environment or the owned pointer names it, true once its claim has settled -- a runtime promoted once may still have a process running out of it, which is why `staging.decide()` never reclaims a settled claim either -- and true when none of that could be read, because an environment nobody could establish as free is not one that is free. False only when the readings say so |
| `claimSettled` | whether the claim recording it was written |
| `claim` | the claim's own two outcomes -- `settled` for the record landing, `released` for the call finishing -- with the readback and the selection snapshot that decided them, any residual path, and what raised |
| `recoveryRequires` | what has to be done next, under the same key a refusal reports it |

So `install` has three exit statuses rather than two:

| Status | What this run changed | The record | What it means |
| --- | --- | --- | --- |
| `0` | it landed | written | the run finished |
| `3` | it landed | not written | what this run changed on the host landed and the record of it did not |
| `1` | nothing | not written | this run changed nothing and recorded nothing; whatever the host selected and reached before it, it still does |

"What this run changed" is not always a replacement. Promoting a candidate replaces the
selected runtime; finishing an interrupted promotion places the pointer a dead run never
wrote; adopting an installation older than claims changes only its bookkeeping and replaces
nothing at all. All three reach `0` or `3` on the same rule -- whether the claim settled --
and `promoted` is what distinguishes them.

Exit 1 says what this run did, not that the host is consistent. A resume refuses with it when
the owned pointer is unreadable or names something this record does not account for, and in
that case a previous run had already committed the new selection before it died -- so the
record names the new environment while the pointer still names the old one. The run changed
nothing; the disagreement it found was already there, and the result names it.

**Exit 3 is not a refusal and must not be read as one.** Non-zero here means the opposite of
what it means everywhere else in this command: what the run changed on the host landed, and a
process may be running out of the environment it changed. A wrapper that reads every non-zero
install status as "nothing changed" would report the old runtime as selected, or clean up an
environment that is still in service. This command releases a candidate only on exit 1.

It does not follow that an exit-3 environment is still the one the host reaches. A competing
install can supersede it during the claim write, and then the same result carries
`selection.selects: false`, `selection.pointerNames: false` and `inService: false`. The status
says the run's change landed; `inService` says whether the destination must be kept. Read the
second for any cleanup decision.

The status says what this run did; it does not promise what is true when you read it. A
competing install can supersede this environment between the promotion and the result, and
then status 3 is still correct about this run while `inService` is `false`. Key a cleanup
decision on `inService` and never on the status alone, and read `promoted` for the narrower
question of whether this run replaced anything: an adoption reports neither.

Which accident happened, and what to do about it, is in `recoveryRequires` -- derived from the
claim as it reads back and from a selection snapshot taken under the promotion lock, never from
the exception alone:

| What the result says | What happened | What to do |
| --- | --- | --- |
| `settled` true, `released` false | the claim landed and the call failed on its way out | nothing about the record; any lock file left behind is named, and the next claim write clears one older than `STALE_LOCK_SECONDS` |
| the record still selects this environment | the write failed and the promotion stands | clear what stopped the write, then rerun: the next run reads an interrupted promotion and records it, rebuilding nothing. Rerunning before the write can succeed returns this same result and changes nothing |
| the record selects only part of this environment | the selection is split across two environments | read the record before rerunning; a resume requires every configured component in the same place and refuses otherwise, so there is no single promotion here to finish |
| the record selects elsewhere but something still reaches this environment | a promotion moved on, or died before its pointer | leave the directory alone; the next run keeps and reports it rather than repairing it |
| the record selects elsewhere and nothing reaches it | a later promotion superseded this staging | nothing; do not rerun here to settle it, because the next run reads an abandoned staging and would remove and rebuild it |
| the claim could not be read, and the record still selects this environment | the claim at that path is unreadable | make it readable or remove it first; an unreadable claim is not one this command may act on, so a rerun reports the directory and leaves it |
| the claim could not be read, and the record selects elsewhere | the claim is unreadable for a staging that has been superseded | leave it alone. Repairing it as `STAGING` has the next run read an abandoned staging and rebuild the directory, and removing it leaves one populated and claimless that every later install refuses as somebody else's |
| the claim could not be read, and the selection could not be either | both readings failed | read the host record first; which of the two rows above applies depends on it, and nothing is at risk meanwhile because a claim this command cannot read is one it leaves alone |
| the host record is gone | the authority for what this host selected was lost | restore the record before rerunning, and do not remove the environment: the owned pointer may still reach it |
| the selection could not be established | the snapshot could not be taken | read the host record before acting; it decides whether a rerun records, keeps or rebuilds |

The snapshot is consistent, not durable. Nothing holds the promotion lock until an operator
reads the result, so what is reported is what the record said at that moment; re-read it before
acting if time has passed.

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
a pointer that disagree — the same repair a resume performs, and under the same gate, which both
re-run rather than inherit.

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

It also asks OPS-4.4 again, and not the reading the interrupted run took. What a resume takes
over is durable — a selection on disk and a claim beside it, sitting there for however long it
took somebody to notice — while all three gate cells read state outside the process and all
three move in the meantime: a supervisor can be started, attempts open and close, the store’s
schema is whatever the selected runtime has since migrated it to. None of them is reusable, and
the daemon is the strongest case, where a prior `ALLOWED` cannot cross a process boundary at all.

There is nothing to reuse in any case, which states the point more exactly. The interrupted run
died before recording a verdict, so the durable state holds no gate reading: the resume was not
carrying a stale `ALLOWED`, it was moving a host’s runtime having never asked. The candidate’s own
declared schema is the one input that cannot have changed, being derived from bytes already
built, but it is read only as half of a comparison against a store that can.

The gate is asked where something is REPLACED, which is not every caller of this path. A resume
finds a link naming the predecessor and moves a host from it to this environment. An installation
older than claims has no link at all, and writing the first one changes which path reaches a
runtime the record already selects rather than which runtime is reached — so it is not gated, for
the same reason its result says its bytes were not re-measured. The test is the link and not the
caller: a link that already names this environment, or no link, replaces nothing.

A verdict that is not `ALLOWED` refuses by name, carrying the verdict and the cells that blocked
or could not answer, because "the gate said no" sends an operator to this command’s source while
the cell sends them to the daemon, the attempts or the store. It refuses before the ownership
entry, which is this call’s first write, so nothing is written and nothing removed: the selection
is left as found, the pointer still names what it named, and the destination can be retried as it
stands once the named condition is cleared.

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
| `storeSchema` | the store's own schema inventory — every object the catalog reports, read read-only through the relay's `read_only_rows`. Keyed by kind AND name, so its evidence lists carry `index sync_ready` rather than `sync_ready`: a trigger may share a table's name, and an object whose kind changed is one object lost and a different one gained rather than one redefinition. The key was `storeTables` while it already held all of that, which named it narrower than its contents |

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
zero, and the store's schema is established compatible. Any cell that could not be read decides
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
separate `CREATE ... IF NOT EXISTS` statements, tables and indexes alike. Every store therefore
agrees with every candidate at version one, and the comparison would detect neither a downgrade
nor an upgrade while looking exactly like a check.

So the cell compares what actually differs: each object's `CREATE` statement in the store's
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
previous installation. A reading that carries object names without their statements cannot answer
this cell at all and says so, because names agree while a column differs.

Every object, and not only the tables. Both readings ask the catalog one question that names no
kind at all, so indexes, triggers and views are compared on the same terms tables are. Asking
only for `type = 'table'` was the name comparison's mistake one level up: it agreed about
everything it had not looked at, and the relay's own schema has carried indexes all along. A
store that had lost one compared identical to a candidate that declares it, and the new daemon
would have re-created it on its first write-open — a migration arrived at by not looking.

What the catalog is asked for is every row it holds, less the objects SQLite maintains for
itself: the autoindexes a `UNIQUE` or `PRIMARY KEY` constraint creates, whose definition is
already inside the table statement being compared, and the bookkeeping tables `AUTOINCREMENT`
and `ANALYZE` leave behind. The exclusion is an exact prefix rather than `NOT LIKE 'sqlite_%'`,
because `LIKE` reads `_` as a one-character wildcard and that pattern also dropped a legal user
object named `sqlitexfoo`.

Each object is keyed by its kind **and** its name, so the evidence lists and the refusal text
read `index sync_ready` rather than `sync_ready`. A trigger may share a name with a table, so
names alone can collide, and an object whose kind changed would otherwise be reported as one
redefinition when it is really one object lost and a different one gained. `onlyInStore`,
`onlyInCandidate` and `definedDifferently` carry entries in that `<type> <name>` form.

| Answer | Observed | Decision |
| --- | --- | --- |
| `NO_STORE` | no store exists at the resolved selection | allowed, and reported as absence rather than as agreement |
| `AGREES` | the same schema objects, defined identically | allowed |
| `EXTENDS` | the candidate declares schema objects the store does not hold | refused |
| `DIFFERS` | a shared object is defined differently | refused |
| `NARROWS` | the store holds schema objects the candidate does not declare | refused |

`NARROWS` is the implicit downgrade the issue forbids: a runtime that does not know an object
cannot preserve what is in it. The other two refuse for the contract's reason rather than that
one. The relay opens its store read-write and runs its whole DDL script on every open, so a
candidate whose schema is not the store's schema **applies** the difference the moment the new
daemon first starts. OPS-4.5 reserves that for its own decision, in its own issue, with a copied
backup of the whole state directory taken first, so letting an update wave it through is exactly
the implicit migration the clause forbids. An update is not the place either direction is decided,
and the refusal names the objects so the next step is obvious.

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

The ownership record goes with the link, for the run that PUT IT THERE. The record is what makes
a link this command's — the promotion refuses to replace one the record never recorded placing —
so a rollback that removed the link and left the record behind said this command owns a link that
is not there, and armed that guard in favour of whatever appeared at that path next. An entry this
run introduced is therefore dropped, and only for the path this run recorded. Where the rollback
restored ABSENCE the record is written only after the link is verifiably gone, because writing it
first would leave a link nobody recorded — the refusal shape from the opposite side. Where a link
was REPLACED the record is written whichever way the restoration went, including when putting the
previous target back could not be read back: a link is at that path either way, so the ordering
that protects the absence case has nothing to protect here, and the result reports the link's own
`verified: false` for what did not land.

An entry this run INHERITED is a different question, because a link that is missing does not mean
a record that is missing: a host whose recorded link was deleted out from under it has the entry
and no link. Erasing it takes away the path the registration names, and a retry aimed at a
different `--dest` then derives another path and reads a registration nobody changed as a
conflict. So the entry stays and its PLACEMENT is withdrawn — the path the registration depends
on is kept, and the guard goes on refusing whatever link turns up at that path, which is stricter
than the state the update found. Where the link is instead put back, the entry goes back whole,
which also takes this run's refreshed stamp off one it did not introduce; that happens whenever
the link was replaced, including when the restoration could not be read back, because the path in
the record is the same either way and the payload reports `verified: false` for the link itself.

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

### Who registers the server

The CRW plugin package declares this server too, so a host can end up with two of them: the
configuration entry this command writes, and the declaration an installed package carries. Both
start a bridge.

`--owner` names which one this host uses. `user` is the default and is the behavior above.
`plugin` writes no configuration entry at all; it writes `crw-bridge-mcp.json` beside the
completion hook's settings, which is the one fact the package cannot carry: the pointer that
names the installed runtime. The packaged launcher reads that record, and the declaration is
what registers the server.

Either owner can be installed first, so the refusal runs both ways. The user path is refused by
a record naming the plugin; the plugin path is refused by an entry already in the configuration.
Both report the other side with its evidence and write nothing, and a record or a configuration
that could not be read refuses rather than defaulting, because installing on an unanswered
question is how the second bridge arrives.

Writing the record is not registering a server. On a host with no plugin installed the record
is inert, and the command says so rather than reporting an installation.

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

The decision is not made in the hook. [The hook contract](../plugins/crw/skills/crw-run/references/hook-contract.md)
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

`--socket` is recorded into those settings as `socketPath` when the install names one, and both
adapter copies pass it back as the global `--socket` option. It is what lets the guard tell that
the state directory it resolved holds another App Server's store: a store records the socket it
serves, and a Stop hook that inherits `CODEX_SESSION_RELAY_STATE` from a second installation would
otherwise read that store, find no relationship for its assignment, and hold a child that has
finished. The field is optional, so a document written before it existed stays byte-identical and
an ordinary reinstall is still unchanged rather than refused; a host with no socket configured
compares no provenance, which is what every host did before. Configure it wherever more than one
installation shares a machine.


### The fallback launcher, and why this command places it

With `--owner plugin` this command also writes `crw-stop-hook.py` beside those settings, and it
writes it **before** them. That file is the second candidate the package’s Stop declaration
opens, and it exists because a hook command is fixed when a turn starts, with the plugin root
already resolved into it. Installing a version removes the previous cache directory whole, so an
update landing while a task still holds that command leaves it naming a file that is gone, and
`python3` exits 2 for a missing script — the hook protocol’s blocking code. Measured on the
user's host: eleven repeated Stop prompts in one turn of one task and eight in a turn of a
second, and neither turn able to finish until a compatibility path was restored. An isolated
reproduction produced thirty-seven in one turn.
[Plugin packaging](plugin-packaging.md#the-cache-lifetime) owns the full reference table and the
supported range; what belongs here is who writes the file and what that writer refuses.

| Question | Answer |
| --- | --- |
| Which command writes it | This one, with `--owner plugin`. `plugin_transition.py transition` writes it too, because it installs the same plugin-owned settings and would otherwise leave a host with the settings and no fallback |
| In what order | Launcher first. A launcher with no settings stands down in silence; settings whose fallback was never placed look installed and are not |
| What it refuses | A file that does not carry the launcher marker, and anything that is not a regular file. A symlink is reported by kind and never followed, because replacing through one writes to a file this command was never given |
| What the marker proves | That CRW put a launcher at that path. Not who ran the command, and not that the bytes are intact. The digest is reported beside it for the second question |
| How it is replaced | Temp file and `os.replace`, then read back, with the kind and the marker re-judged under the lock immediately before the write |
| Which command removes it | `plugin_transition.py remove`, under the launcher’s own lock and only while the marker is still there |
| What `disable` does to it | Nothing. `disable` stops new calls by retiring the settings and deletes no bytes |

The two files take separate locks and there is deliberately no lock spanning them. Widening one
means reworking a write path that is already proven, and it is not needed: every state the pair
can be left in is harmless. A launcher alone stands down. Settings alone are what the host had
before this file existed. What an interleaving can still do is make a receipt wrong, so the run
reads both paths back at the end and reports `launcherObserved` and `settingsObserved` — what the
host held, not what the run intended — and exits `3` when either half is missing, changed, or no
longer what it wrote. That status is the same fourth answer `install` uses: not a refusal,
because bytes really were written, and not success, because the result did not stay in effect.

A plugin-owned budget is capped at `completion.MAX_PLUGIN_GUARD_SECONDS`, the launcher ceiling
minus its margin. The launcher waits `min(timeoutSeconds + 2, 9)`, so a budget above 7 collapses
the margin it exists to keep and the launcher’s deadline arrives while the adapter is still
writing the record of its own timeout. That bound lived in the transition alone until this
launcher became something every plugin host depends on; it now sits with the validation both
writers share, and the transition aliases it rather than keeping a second copy.

### Who registers the hook

The plugin package declares this hook as well, and a host holding both registrations runs both
on every Stop: each leaves a row, but only the one that claims the event's accepted record asks
the guard, and the other records itself as a duplicate (see [One accepted record per Stop
event](#one-accepted-record-per-stop-event)). `--owner` decides which registration exists.

`user` is the default and appends to the hook file as before. Its settings document is
byte-identical to what installed hosts already hold -- the owner is written only when it is not
the default -- so an ordinary reinstall is still unchanged rather than refused as settings that
say something else.

`plugin` writes the settings and registers nothing. It also records the interpreter and the
adapter entry point, which the user path carries in the command line it writes into the hook
file and a packaged command has no way to resolve for itself. The refusal runs both ways: the
user path is refused by settings naming the plugin, the plugin path by a registration already in
the hook file, and an unreadable hook file refuses both.

The packaged launcher repeats the check at run time, because installing the plugin is not a
command this repository runs and cannot be refused from here. It reads the owner out of the
settings and stands down unless the plugin owns the registration, so a host that acquires both
still answers once.

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

`hook-status` reads that same word rather than resolving a path of its own, and reports which
of the two it used. Recomputing it would answer about a file the hook may never open: an install
that used the override embedded the resolved path in its command, and a later diagnosis has no
reason to be running under the same environment.

A registration naming its settings with a relative path is reported rather than resolved. The
hook resolves such a path against each session's workspace, so no single file answers for it,
and inspecting the one the diagnosis would resolve would report an unrelated file as the hook's
own. Nothing downstream of those settings is read either.

That judgment uses the same expansion the hook applies, so a `~` path is absolute here too;
calling it relative would hide a working configuration and every cell below it.

The interpreter the registration names is its own cell beside the script. A virtual environment
that moved after installation leaves the script in place and the interpreter gone, and then the
host cannot start the adapter at all: no decision, no journal entry, and a registration that
still looks correct.

A bare interpreter name is resolved on `PATH`, the way the host resolves it, so a working hook
is not failed in diagnosis for not spelling a file path. A wrapper's own target is not followed,
and the cell says so rather than implying the program behind it was checked.
A relative spelling carrying a separator is reported as workspace-dependent instead: resolving it
would answer about a program under whatever checkout the diagnosis ran from, not the one the host
starts in a session's workspace.

Whether the runtime offers `guard-evaluate` requires it to describe the subcommand, not merely
to exit 0. A program that ignores its arguments and succeeds would otherwise be reported as
offering one it has never heard of.
A flag the real help carries is required beside the subcommand's own name, because a program
that echoes its arguments prints that name back while offering nothing.

A registration naming the adapter with a relative path is reported rather than resolved, for
the same reason a relative settings path is: the file this command would find is not the one
the host runs.

Registrations naming different settings files are reported as ambiguous and nothing below them
is read. Every one of them runs, so naming one would describe one hook while reporting the
others' state as if it were that one's.
A relative spelling counts as its own unresolved source there, because two of them name two
files, and so do one relative and one absolute.

A matcher is part of a registration. Installation only ever appends an unconditional group and
the hook file's own installer treats only that group as already installed, so an identical
command sitting under a matcher is a duplicate: appending would add a second registration beside
it and both would run on a matching `Stop`.

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

`hook-status` writes nothing of its own, but it is not inert: it RUNS two of the programs a
host registers, and what those programs do is outside this command's control. Answering whether
the runtime offers `guard-evaluate` means running that runtime with `--help`. Answering whether
the registered interpreter is an interpreter at all means running the first word of the
registered command with `-c` and a generated nonce, because a file being present and executable
establishes nothing about what runs: a program that exits quietly is indistinguishable from a
working interpreter by any reading of the filesystem, and every Stop would succeed at running it
and never reach the adapter. Both invocations exist only to answer and exit. Neither is the
adapter's firing path, and neither can write a journal record, so a reading cannot manufacture
the firing evidence it is reporting on. The command names the program it ran and the invocation
it made in the cell's own evidence, so a receipt shows the reading's cost rather than only its
conclusion.

What running the interpreter establishes is narrow, and the limit is stated rather than left to
be inferred: the registered first word answers as a supported Python at the spelling this host
resolves. It does not establish that the registration would start. The command was never run as
written, the argument convention was never exercised, and a wrapper's own target is deliberately
not followed -- so a wrapper that answers `-c` while rejecting its normal argv still reads as
answering. `firingRecordAbsence` therefore treats a present pair as "nothing establishes that
this cannot start" and never as "this starts".

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

## One accepted record per Stop event

A turn can end more than once. When any Stop hook holds, the host appends a continuation to the
same turn and fires Stop again; when a message was waiting, it appends that and does the same.
Each of those is its own Stop event, and each one gets its own decision. Two registrations
answering one Stop, or one Stop delivered twice, are a different thing: one event handled twice.
The adapter keeps exactly one accepted record per event, asks the guard once for it, and still
leaves a row for every invocation, so the two cases stay apart instead of being counted as one
number per turn.

### What identifies an event

The host hands a Stop hook nine fields and no per-invocation identifier. None of them separates
two events of one turn: `turn_id` is kept across a continuation chain, `stop_hook_active` is
false on a turn's first Stop and true on every later one, and `last_assistant_message` can repeat
word for word. An isolated run on Codex 0.154.0 produced three Stops in one turn whose second and
third payloads were byte-identical. What differs is the transcript. Every sampling that ends in a
Stop leaves one final answer, and before running Stop hooks the host records it in the file named
by `transcript_path` as an `item_completed` `AgentMessage` carrying the turn id, the thread id
and an item id of its own. So an event is

    (session_id, turn_id, stop_hook_active, answer item id)

where the answer item is the one Stop of the turn, as the transcript shows it, that reported the
payload's text under the payload's `stop_hook_active`. The Stops of a turn are the last answer
before each later continuation or user message, and the newest answer, which is the latest
sampling's; a Stop's `stop_hook_active` is true once a hook continuation has happened in the turn.
The live Stop reports the newest answer, and a late delivery of an earlier Stop reports that
Stop's own answer, so the rule names the right event for both. It is established only when

- the latest sampling's answer is recorded: when the newest item for the turn is a continuation
  or user message, the invocation may be that sampling's own Stop, whose answer the transcript
  does not show yet, or a late delivery of an earlier one, and nothing tells them apart;
- exactly one Stop of the turn matches. Two Stops that reported the same text under the same
  `stop_hook_active` cannot be told apart, and claiming would let a late delivery of one take the
  other's event and answer the real one as a duplicate. The isolated run above is such a turn:
  its third Stop is left unestablished;
- the matching answer's thread, recorded on the item, is the delivered session.

The key is a SHA-256 over the four values, so no host value becomes a path component and nothing
is minted per invocation.

The transcript is read backwards from its end to the turn's `task_started`, because the rule needs
every Stop of the turn. The read is bounded at 64 MiB and 0.75 seconds,
inside the margin the launcher keeps over the guard budget; the largest turn on the host this was
built on was 62 MB, and reading that far took about a third of a second. Nothing that could be
one of the turn's items is read past: a last line without its newline is one the host is still writing,
and any line naming the turn that does not parse could be an input. A path that is
missing, relative, not a regular file or unreadable, a scan that hits either bound, a transcript
that begins without the turn's start (its earlier Stops are not there to compare), an unfinished
last line, an unreadable line about the turn, and any failed condition above leave the identity
unestablished, with the reason on the row. An unestablished invocation is asked about exactly as
before and is never deduplicated: the adapter does not know which event it is, so it cannot know
that the event was already answered, and a reading of the journal does not vouch for a window
that holds one.

### Accepted records and attempt rows

A claim is two create-once files. The first is the host's,
`<CODEX_HOME>/crw-completion-hook/stop-events/<key>.json`, under the Codex home of the process the
Stop fired in: every registration the host starts for one Stop inherits that environment, while
the settings each one reads, and so its journal root, may differ, so this is the file two
registrations of one host always meet. Only the invocation that creates it owns the event, and
only the owner asks the guard. The owner creates the accepted record,
`<journalRoot>/accepted/<key>.json`, beside the rows and naming the host's ledger; asks the guard;
writes its row; and then writes `accepted/<key>.outcome.json` naming the session, the turn, the
outcome, the row and the `journalPolicy`. An invocation that finds either file already there asks
nothing, prints nothing and writes a row whose `adapterOutcome` is `duplicate_invocation`, with
`acceptedAs` naming the file it found (the host's relative to the Codex home, the accepted record
relative to the root). Creating a file that must not exist is atomic on a local filesystem, so two
registrations firing in the same instant produce one owner and one duplicate, whichever roots
their settings name.

When the host's file can be neither created nor found, nobody can own the event, so nobody asks:
the invocation writes an `unarbitrated` row (`adapterOutcome` `arbitration_failed`) and releases
the Stop, the adapter's ordinary failure direction. An owner whose root cannot hold the accepted
record (`claim_failed`), or whose settings name no `journalRoot` (`unclaimable`), still asks the
guard once: the host's file stays, so a later delivery of the event is a duplicate. The claim files
are state rather than invocation records, and are written under every `journalPolicy`.

Rows keep their place and their name, `<journalRoot>/<YYYYMMDD>/<32 hex>.json`, and every
existing count of them still counts invocations. Version 2 rows add `eventKey`, `eventIdentity`
(whether it was established, and why not), `identityScanMs`, `acceptance` (`accepted`,
`duplicate`, `unestablished`, `unclaimable`, `claim_failed` or `unarbitrated`), `acceptedAs` and
`guardInvoked`. `faults_only` leaves out the rows of answered and duplicate invocations of
identified events, and keeps every invocation answered without an identity or an owner: no
accepted record covers those, so the row is the only trace. `no_journal` keeps no rows at all, so
those invocations leave nothing, and a reading of such a ledger says it cannot vouch for them.

The outcome record says what the adapter answered, not what the host received: it is written
before the answer is printed, exactly as the row is. A claimant killed after its outcome and
before printing a hold leaves a complete record of a hold the host never saw; one killed after
claiming and before its outcome leaves a claim with no outcome; one killed between the host's file
and its claim leaves only the host's file. In each the event has its owner, a later delivery of it
is a duplicate and is not answered again, and the Stop was released -- the adapter's ordinary
failure direction. A reading of the journal names the last two cases; the first is
indistinguishable from success on disk.

### Reading it back

`scripts/stop_events.py --journal-root <root>` reads the rows, the accepted records and the host
ledgers the claims name, and answers one verdict. It judges an event as a unit: `--since`,
`--until`, `--session` and `--turn` choose the events the window reaches, which are the events
with any record (host file, claim, outcome or row) in it, and every record of a chosen event is
then checked whatever its own time; rows that name no event are chosen one by one. `FALSE` (exit 1)
means an event was accepted more than once: claims for one key in two roots, two accepted rows for
one key, an accepted row whose claim is missing, or a duplicate that asked the guard. `UNREADABLE`
(exit 3) means the reading cannot vouch for what it read:

- a listing that failed, including an `accepted` or host ledger path that is not a directory;
- a row, claim, outcome or host file that does not parse or lacks what its kind is written with:
  an accepted row that did not ask the guard or names another record, a duplicate row that is not a
  `duplicate_invocation`, or any record whose own session, turn, `stop_hook_active` and answer
  item do not hash to its key;
- a row version it does not know, or an entry in a ledger that is not one of its records;
- an outcome without its claim, naming another session or turn than its claim, or without the
  accepted row it names (or with none under `every_invocation`), and a claim without its outcome
  in the same root;
- a host file whose event has no claim in the root it names, or that names a root the reading was
  not given, and a claim whose host file is missing;
- a duplicate whose event has no claim in any root read, or a ledger written under `no_journal`;
- any invocation in the window it cannot judge, or nothing to judge.

`TRUE` (exit 0) otherwise. The invocations it cannot judge are those whose identity was not
established, that had no owner, whose owner's root could not hold the claim or that never reached
an event, and rows written by a runtime older than event identity: for each, the journal does not
show that its Stop was answered exactly once. They are counted by reason (`unjudgedInvocations`,
`legacyRows`), and a window that leaves them out can still read `TRUE`. The superseded count of
rows per (session, turn) is printed too, labelled as superseded, so the two readings can be
compared. `--journal-root` repeats for every root the host's registrations write to, and
`--codex-home` adds a host whose ledger no claim names yet (a host whose only event lost its owner
before the claim). The same reading is `completion.stop_events()`.

### Limits

The guarantee holds among the registrations of one Codex home, which is every registration one
host starts for a Stop. Registrations that do not share it, or an adapter from before the host's
file existed, each accept an event in their own root; a reading given both roots says `FALSE`.
The claim files are never removed, like the rows. That the host
records the answer before running Stop hooks was observed in every isolated run and is consistent
with every record in the live journal, but it is not a documented host contract; a host that ran a
Stop before recording both that sampling's continuation and its answer would show the previous
Stop as the newest, and a Stop reporting the same text as an earlier one of the same kind would be
taken for it. A sampling that
ends with no answer at all was not observed. A turn whose Stops repeat the same text trades
deduplication for safety: those Stops are asked about by every registration, and a reading of a
window that holds them is `UNREADABLE`.

## Registration is not firing

`runtime_install.py hook-status` writes nothing, and reads rather than changes -- with the one
exception above, that it runs the registered runtime and the registered interpreter to ask them
what they are. It answers registration, the host's
trust state, whether the registered command's target still exists, the settings, the runtime,
whether that runtime offers the subcommand, this hook's own record of its invocations, why
there is no such record when there is none, the guard's records, and the daemon, each as its
own cell. `not_read` is used where a question was not asked and is never written as an absence.

The cause cell is `firingRecordAbsence`, and it exists because "there is no record" had several
repairs behind it and the command answered none of them. It takes no reading of its own: each
cause declares which cells answer it, every rule runs rather than the first match winning — a
host whose settings and whose adapter are both gone needs two repairs and is told so — and an
ambiguity is carried as candidates instead of being settled by choosing. Where several
registrations name several settings files, each of those files and the journal under it is
read, because reading one of them and reporting an absence says nothing about the others, and
reading none of them is what made a hook that had fired indistinguishable from one that never
had. The operator procedure below lists every cause and the two limits that remain.

This hook records one entry per invocation by default, and the default is not frugality. The
guard publishes an observation only when it selected an assignment, so on a host with no managed
session it writes nothing at all, and an empty firing record would be indistinguishable from a
hook that never runs. The count is always reported beside the policy that produced it.

A record is whole or it is absent. A short write is finished rather than accepted, and a write
that cannot finish removes what it left: a truncated record survives under a name nothing will
reuse and would be counted as an invocation whose contents no longer read back.

The settings are read before the payload is looked at, because the settings say where a record
goes. Reading them second meant a payload the adapter could not parse was released with nothing
written anywhere, which is the one class of invocation that most needs a record.

The daemon is not on this path. The guard reads the marker and a read-only database, so a stopped
daemon is not observable from a Stop and is never inferred from one; `hook-status` answers it
separately or says it did not look. Long retries and whole verification loops belong to the daemon
and to the coordinating task, not to a hook with a five-second budget.

## The composed acceptance run

Installing, updating and hooking each have their own cases above. What none of them states is
the sequence a host actually lives through, on one destination, with the state that has to
survive it put there before the first install and read again after the last refusal. A suite of
separately passing cases is not that sequence, and the difference is where a host loses a
runtime. `scripts/ci/tests/test_install_acceptance.py` is that sequence.

It reuses the update fixture rather than restating it: the same host, the same injected seams,
the same snapshot of everything a failed update promised not to change. What it adds is that the
installation recovered at the end is one the run itself promoted, not a directory a fixture
placed on disk. An installer that had lost the ability to install would leave the update cases
green; it fails here at stage one.

| Stage | What runs | What must be true afterwards |
| --- | --- | --- |
| A new install | `install --apply` onto a destination holding nothing | An environment was built, the record selects it, `current` reaches it, the claim is `COMPLETE` |
| The same run again | the identical command | `alreadyInstalled`, staging `SETTLED`, the claim and the configuration byte-identical, no orphan directory |
| An update that fails | a source arrives, so the candidate is a different directory, and one of the eight seams refuses | exit 1, the seam names itself, and the refusal names the arriving environment |
| The install it replaced | nothing further | selection, pointer target, configuration bytes, store bytes, store inode and store rows all as stage two left them, and a further install still answers `alreadyInstalled` |

The last column is the reason the stage list is not the test. Every one of those equalities also
holds for a run that did nothing at all, so the sequence separately requires what a no-op cannot
produce: an environment in the destination, a selection naming it, and a seam that reported the
boundary it stopped at against the environment it was building.

### Seven questions, seven readings

The criterion asks for seven judgements and forbids one standing in for another. They are not
one payload: two are commands and one is a comparison the acceptance module performs. `READINGS`
declares the cell, the source that answers it and the path the answer is read from, and a single
`read()` is the only way a cell is filled. A reading that could not be made reads `UNREADABLE` --
never `False`, and never the value of the cell beside it.

| Judgement | Answered by | Read from | Never established by it |
| --- | --- | --- | --- |
| Skill link | `diagnose` | `skillLinks`, from `scripts/install.py --check` | that a linked skill is loaded or trusted by a host |
| Runtime import | `diagnose` | `checks.results.imported` | that an imported module is the one a pointer reaches |
| MCP tool exposure | `diagnose` | `checks.results.mcpExposed` | that a registered server was started, or that a session listed its tools |
| App Server connection | `diagnose` | `checks.results.connected` | that a socket that accepted a connection will accept delivery |
| Real hook callback | `hook-status` | `firingJournal` | that a registered hook is an enabled one, or that a firing was judged correctly |
| Model and permission preservation | the acceptance module | the model and permission keys, read before and again after | that anything else in the configuration survived |
| Delivery acceptance | `diagnose` | `checks.results.deliveryAccepted` | that an accepted delivery was acted on |

Reaching an answer, reaching it down the path the row names, and reaching it on the host the
rest of the run describes are three questions, and the last two are the ones a shortcut passes
silently. All seven readings are taken against one host: the Codex home this run installed
into, the destination it built in, and the state directory it was pointed at. A row answered
from a second Codex home made for it would compose readings about two machines and look
exactly like a composition. In the suite the link, import,
registration, hook and preservation rows travel the thing the run installed: the links it
created, the packages inside the candidate, the registration `register-mcp` wrote, the command
line in the hook file executed as a program with a Stop payload on its stdin, and the
configuration the install acted over. The connection and delivery rows travel a relay this
suite wrote, because no App Server runs there -- which is why those two are the rows the table
above says a host reading needs the live half for.

The hook row is worth naming twice. Calling the adapter helper directly would answer exactly as
the registered command does, while leaving the entry point, the settings argument the install
chose and the stdin contract entirely untested. The registration is half of what this page
documents, so the row reads it out of the hook file and runs it -- out of the hook file in the
Codex home the rest of the run used, because a registration read from anywhere else is a
registration on another machine.

The vocabularies are deliberately not merged. The result rows answer in `check.VALUES`, the hook
row answers in the completion module's own words, and the listing row answers with a listing. A
cell rewritten into a neighbour's vocabulary is the same borrowed answer with better manners.

Two of the seven are established by construction rather than by trusting a number. The hook row
counts records, and a count taken from a directory somebody populated says nothing, so the case
establishes it by the transition instead: the journal is established absent, the Stop hook is
actually run, and the journal then names exactly one invocation. And no verdict cell anywhere
reads model or permission state on both sides of an install, so the acceptance module performs
that comparison itself. `checks.settingsPreserved` is not that reading -- it answers whether a
command that writes nothing left `config.toml` alone, which is true of a diagnosis whatever the
model keys say -- and neither is `settings_usable`, which answers whether a value a caller
supplied is admissible to the relay. Filling the preservation cell from either would be the
borrowed answer the whole arrangement refuses, so the missing cell is recorded as missing.

A version that moved and a source that moved are likewise two findings and not one. Only
`definition.verify` reports the first, and it refuses the install rather than filling a cell; a
reader wanting a version disagreement reads the refusal, not a classification.

### What the run exercised, and what it stood in for

Every path is temporary, and fifteen names are stand-ins inherited from the update fixture: the
two build steps, the relay, the measurement, the component classification, the definition load
and verification, the interpreter version, the pointer steps and the store readings. The
acceptance module declares them in a record it derives by running the fixture and watching which
attributes are replaced, rather than by reading how the fixture is written, so a stand-in added
there fails this suite until the record acknowledges it. A provenance record a later change can
silently outgrow is worse than none.

A stand-in is only half of what a record has to say. A reading can reach its success answer, down
the path it declared, on the host it declared, and still answer about something the scenario never
built -- so the module also declares, function by function, whether what that function hands the
command is the value the scenario built or a stand-in, and a stand-in names what the scenario has
instead and what a row reading through it therefore does not prove. That inventory is derived by
asking the module for its functions, so a helper added there arrives unclassified. What it does
not reach is a value written inline inside a function body: the granularity is the function, and
the imported fixture's own replacements are covered by the separate record above.

The store is where that mattered. Every install in this suite used to tell the run its store was
absent and its tables unknown while the fixture had built a populated one at the same path, and
nothing failed, because a run told there is no store settles that cell as established absence and
moves on. A regression that detected a store and then lost it stayed green underneath. The
readings the run takes about the store now describe the store the fixture built, one case reads
them back out of the run's own result and compares them with it, and no call site may hand that
switch again.

Rows this repository has exercised are `fixture`: a temporary destination whose build steps and
relay are simulated. No committed row can say `host`, and a check enforces that. The diagnosis
runs with `HOME`, `XDG_STATE_HOME`, `CODEX_HOME` and `PATH` pointed inside the temporary
directory and with `--relay-command` naming a path in it, because redirecting `--state` alone is
not isolation: the survey runs discovery of its own, the filesystem side reads the real home, and
an installed entry point on `PATH` would be resolved and run. The case then requires every path
the diagnosis reported to be inside that directory.

Paths are only half of it. Resolving where a component lives imports it, and the bridge's smoke
script starts a server, both under whatever interpreter the record names -- so a fallback to the
interpreter running the suite would reach whatever this machine has installed, which a check may
read about and must not run. Clearing `PYTHONPATH` and the user site does not reach a system
site directory, and checking the resolved location afterwards is too late, because by then the
import has happened. The runtime is therefore supplied rather than discovered: a `-S -E`
interpreter inside the destination, named by the record for every component and reached through
the entry points on a `PATH` of the suite's own. The assertion is on the interpreter, which is
settled before any probe runs; a component asked through this machine's interpreter fails the
case whatever it happened to find.

### Running the combination against a real host

A real combination is an operator action, not a check. It needs a destination, a Codex home, a
host record and a state directory that are yours to change, and it establishes nothing until it
is recorded. Four of the seven need an input the command cannot supply for itself, so a bare
`diagnose` produces four answers and three admissions that it did not look.

```sh
# Substitute every <...> below before running any of it. They are placeholders, not literals:
# an unsubstituted one is a shell redirection rather than a value, which is as true of the
# controller assignment below as of the flags further down. Nothing here runs as it stands.

# The receipt directory has to exist before the first write, or the baseline redirect and the
# install redirect below both fail -- and the second of those stops the install from running at
# all rather than merely losing a file. It has to be a NEW one. Every reading below is written
# under a fixed name, and two of them are pairs where one run writes only one of the two names,
# so a directory still holding an earlier run's files is two runs wearing one name: a guard
# further down would find that run's snapshot and report a comparison this run never took.
# mkdir without -p is the check, because it fails rather than adopting a directory already
# there. It ends the procedure rather than reporting and continuing, because a shell without
# set -e would run every step below into the directory mkdir just refused, and the mixing works
# in both directions: an older run's snapshot read as this run's preservation, and an older
# run's absence marker read as a side this run did not have. Name the receipt under a parent
# that exists, and a new one for every run.
mkdir <receipt> || exit 1

# One controller for the whole block, on 3.11 or newer. Five of the steps below read a Codex
# configuration and they do not fail alike without a reader, so naming the interpreter once is
# the difference between a block that can be copied and a block whose readings quietly degrade.
# The runtimes this command installs are 3.11+ whatever starts it, so this is a choice about the
# controller only. What an older one does to each step is recorded after the block.
controller=<python3.11-or-later>

# Before anything: the model and permission keys as they stand, because preservation is a
# comparison and there is no cell that makes it for you.
# A fresh Codex home legitimately has no config.toml at all -- the reader treats absence as an
# empty configuration -- so record the absence rather than failing on it. And keep the two
# apart: a baseline that was ABSENT makes the later comparison one between two absences, which
# establishes that nothing was added and nothing about a posture anybody had set.
if [ -f <codex-home>/config.toml ]; then
    cp <codex-home>/config.toml <receipt>/config.before.toml
else
    printf 'no configuration existed before this run\n' > <receipt>/config.before.absent
fi

"$controller" scripts/runtime_install.py install --dest <destination> --record <record> \
    --codex-home <codex-home> --state <state> --apply > <receipt>/install.json
# The exit code belongs IN the receipt rather than on the terminal: it is the install's own, a
# pipeline would hide it, and a receipt that kept the result and lost the status cannot say
# whether the install refused.
printf 'install exit=%s\n' "$?" > <receipt>/install.exit; cat <receipt>/install.json

# The skill links are a layer of their own: install builds the runtime, and the diagnosis reads
# the links by running scripts/install.py --check separately. Skip this and the link row answers
# that every crw-* skill is missing -- an accurate reading of a Codex home nobody linked, and
# not a reading of the installation just made.
"$controller" scripts/install.py --apply --dest <codex-home>/skills

# Registration is a separate operation from installing, and tool exposure compares the
# registered command with the tools a session actually listed. Both halves or neither.
# Name a 3.11 or later interpreter here too. Registration reads back the content it proposes to
# write, so it refuses without tomllib for an absent, an empty and a populated configuration
# alike -- measured on the 3.10 floor: exit 1, outcome CONFLICT naming the interpreter, nothing
# written. Run this on the floor and there is no registration for the exposure row to compare
# against, and that row is unreadable rather than unverified.
"$controller" scripts/runtime_install.py register-mcp --codex-home <codex-home> \
    --bridge-command <destination>/current/bin/<console-script> --apply

# This payload carries five of the seven rows, plus repositoryCommit and definitionVersion --
# the revision a reader needs to reproduce any of it. Printed to a terminal it is gone, and the
# receipt then cannot substantiate the readings this procedure says it recorded, so it goes to
# the receipt with its exit status like the install did.
"$controller" scripts/runtime_install.py diagnose --dest <destination> --record <record> \
    --codex-home <codex-home> --state <state> --socket <socket> \
    --bridge-command <destination>/current/bin/<console-script> \
    --relay-command <destination>/current/bin/codex-session-relay \
    --observed-tool get_capabilities \
    --trial --issue <issue> \
    --parent-task <parent-task> --child-task <child-task> --recipient <recipient> \
    --artifact-root <artifact-root> --artifact <artifact> \
    --turn-thread <turn-thread> --turn-id <turn-id> --dispatch-turn-id <dispatch-turn-id> \
    --recipient-settings <settings-or-@path> > <receipt>/diagnose.json
printf 'diagnose exit=%s\n' "$?" > <receipt>/diagnose.exit; cat <receipt>/diagnose.json

# The hook has to have fired FOR THIS TURN, and no count can say that. hook-status reports what
# this hook has recorded about itself cumulatively, so an old nonzero count reads as evidence
# for a callback that never happened -- and comparing before with after does not repair it,
# because any other session stopping inside the measurement window moves the same number. A
# count that went up answers "did this hook fire at all lately", which is a different question
# from the one this row asks.
#
# The record carries sessionId and turnId, so ask with them.
#
# --socket is the socket diagnose was given above. The install records it as socketPath, which is
# what lets the guard refuse a state directory whose store records another App Server.
"$controller" scripts/runtime_install.py hook --codex-home <codex-home> --adapter completion \
    --dest <destination> --socket <socket> --apply
#   ... then end a real turn, and only then:
"$controller" scripts/runtime_install.py hook-status --codex-home <codex-home> > <receipt>/hook.json

# hook-status names the journal it counted; the records in it name the turn they belong to.
# This is the ONLY turn-specific reading in the block, and it is the one hook.json above cannot
# supply: that file carries the cumulative cell and nothing about which turn moved it. So this
# reading goes to the receipt with its exit status, like the install and the diagnosis did.
# Printed to a terminal it is gone, and a receipt left holding only the cumulative count cannot
# say that the named session and turn are the ones that fired.
"$controller" - <receipt>/hook.json <session-id> <turn-id> > <receipt>/hook.turn.json <<'PY'
import json, re, sys
from pathlib import Path
status, session, turn = sys.argv[1], sys.argv[2], sys.argv[3]
payload = json.load(open(status))
cell = payload["firingJournal"]
# hook-status omits journalRoot whenever its firing-journal reading could not name a usable
# journal, and that is several states rather than one. The command now answers WHICH of them,
# in firingRecordAbsence, so this reads the cause beside the absence instead of stopping at it.
# Read with a default, because a host carrying an older runtime answers the absence and not the
# cause, and a traceback where a reading belongs is worse than a row that says so.
cause = payload.get("firingRecordAbsence", {})
if "journalRoot" not in cell:
    print(json.dumps({"firingJournal": cell.get("value"),
                      "firingJournalEvidence": cell.get("evidence"),
                      "journalRoot": None,
                      "recordsForThisTurn": None,
                      "cause": cause.get("value"),
                      "causeEvidence": cause.get("evidence"),
                      "causeCandidates": [c["cause"] for c in cause.get("candidates") or []],
                      "detail": "no journal to attribute a turn to, so this row is unreadable"
                                " for this run rather than zero. 'cause' says why there is"
                                " none, and carries every candidate rather than choosing one"
                                " when it could not be settled"}, indent=2))
    raise SystemExit(0)
root = Path(cell["journalRoot"]).expanduser()
# The same shapes hook-status counts, and one entry that cannot be decoded does not take the
# reading with it: the hook creates a record before it finishes writing it, so a file being
# written while you look is neither a match nor a failure of your turn.
day, name = re.compile(r"^[0-9]{8}$"), re.compile(r"^[0-9a-f]{32}\.json$")
records, unreadable = [], 0
for directory in sorted(p for p in root.glob("*") if p.is_dir() and day.match(p.name)):
    for entry in sorted(e for e in directory.glob("*.json") if name.match(e.name)):
        try:
            records.append(json.loads(entry.read_text(encoding="utf-8")))
        except (OSError, ValueError):
            unreadable += 1
mine = [r for r in records if r.get("sessionId") == session and r.get("turnId") == turn]
# A turn can end more than once, and each end is its own Stop event with its own accepted record.
# Rows count invocations; the events are the distinct eventKeys the accepted rows name. A runtime
# older than event identity writes rows without them, and those are counted apart, not guessed at.
accepted = {r.get("eventKey") for r in mine if r.get("acceptance") == "accepted"}
print(json.dumps({"recordsRead": len(records), "recordsUnreadable": unreadable,
                  "recordsForThisTurn": len(mine),
                  "acceptedEventsForThisTurn": len(accepted),
                  "duplicatesForThisTurn": sum(r.get("acceptance") == "duplicate" for r in mine),
                  "unidentifiedForThisTurn": sum(r.get("acceptance") in
                                                 ("unestablished", "unclaimable", "claim_failed")
                                                 for r in mine),
                  "legacyRowsForThisTurn": sum(r.get("recordVersion") != 2 for r in mine),
                  "record": mine[:1]}, indent=2))
PY
printf 'hook turn exit=%s\n' "$?" > <receipt>/hook.turn.exit; cat <receipt>/hook.turn.json

# Afterwards: the other half of the preservation reading. The KEYS, not the file -- the
# registration above deliberately appended a table, so a whole-file diff reports a change that
# is this procedure's own doing and would report it whether or not anything was preserved.
#
# The post-install side becomes a file first, beside the baseline. Preservation is a comparison
# between two moments, and a receipt holding only the earlier one cannot substantiate it once
# <codex-home>/config.toml has moved on. An absence is recorded here the way the baseline
# branch recorded one, rather than being failed on.
if [ -f <codex-home>/config.toml ]; then
    cp <codex-home>/config.toml <receipt>/config.after.toml
else
    printf 'no configuration exists after this run\n' > <receipt>/config.after.absent
fi

# The comparison reads the two snapshots the receipt now holds, so a later reader can re-take
# exactly this reading from the receipt alone. Which branch runs is decided by what THIS run
# recorded, and the absence markers are read first for that reason. On a fresh Codex home the
# branch before the install wrote config.before.absent and no config.before.toml at all, and
# this reader opens its inputs by name: a comparison that ran anyway would end in
# FileNotFoundError with no reading written, or -- in a receipt carrying an older run's files
# -- would compare that run's snapshot and record it as this run's preservation. Where a side
# was absent there were no model or permission keys to preserve on that side, and the row is
# recorded as that absence, not as a preservation and not as a failure. Which side it was is
# not guessed here: the config.before.* and config.after.* names in the receipt already say it.
# Name a 3.11 or later interpreter, because the reader arrives there. On a host whose python3
# is the 3.10 floor this command exits before it reads anything, and the receipt then records
# what the suite records on that interpreter: the reading was not made, and the row is
# unreadable rather than preserved. The exit line and the captured stderr beside it are what
# say so. Do not substitute a pattern match for it -- a value guessed out of TOML is a value
# whose wrongness is invisible.
if [ -f <receipt>/config.before.absent ] || [ -f <receipt>/config.after.absent ]; then
    printf '%s\n%s\n' \
        'no comparison was made: a side of it was absent during this run' \
        'config.before.* and config.after.* in this receipt name which side' \
        > <receipt>/config.preservation.absent
    cat <receipt>/config.preservation.absent
elif [ -f <receipt>/config.before.toml ] && [ -f <receipt>/config.after.toml ]; then
    "$controller" -c 'import sys, tomllib
keys = ("model", "approval_policy", "sandbox_mode")
for path in sys.argv[1:]:
    with open(path, "rb") as handle:
        document = tomllib.load(handle)
    print(path, {key: document.get(key) for key in keys})' \
        <receipt>/config.before.toml <receipt>/config.after.toml \
        > <receipt>/config.preservation.txt 2> <receipt>/config.preservation.err
    printf 'preservation exit=%s\n' "$?" > <receipt>/config.preservation.exit
    cat <receipt>/config.preservation.txt
else
    printf '%s\n' \
        'no comparison was made: a side of it has neither a snapshot nor an absence here' \
        > <receipt>/config.preservation.absent
    cat <receipt>/config.preservation.absent
fi

# If an update has failed here, it has already restored what it found. Read that back rather
# than assuming it -- and read residualPaths out of the FAILED RUN'S OWN result, which is what
# THAT RUN left. diagnose answers residualPaths too, and it is a different reading of a
# different question: what is on the destination NOW. Neither is a superset of the other, so
# the install result above is kept rather than replaced by the diagnosis below.
#
# residualOwnership and recoveryRequires are read from the same result and for the same reason.
# A rollback can settle the LINK and fail to settle the RECORD, and what that leaves is a claim
# rather than a path: nothing is on disk to delete, so residualPaths is empty and correct while
# the record still says something about that path. residualOwnership names the path whose claim
# is outstanding. recoveryRequires is COMPOSED rather than chosen from a list, because what has
# to be settled is two separate readings -- what became of the LINK (taken away, put back to a
# named target, or not put back at all) and where the ENTRY came from (introduced by that run,
# or inherited and left carrying its stamp) -- and the consequence follows from the pair. A
# sentence that assumed either would tell an operator the link was put back when it was not, or
# report a disagreement between a link and a record that in fact agree.
#
# Both are empty for a rollback that found the entry belonged to ANOTHER run by the time it
# wrote. Nothing there is this run's to settle, so asking an operator to settle it would send
# them after somebody else's record. That case is reported where it belongs, under
# pointer.pointerRestored: 'ownership' reads "moved on", 'verified' is false because the
# rollback did not do what it set out to, and 'detail' names the path the record holds now. The
# command below prints 'pointer', so the receipt carries it.
#
# A RESUME or an adoption that fails reports the same rollback at the TOP level rather than
# under 'pointer', because it never reaches the update's exit. It carries residualOwnership and
# recoveryRequires from the same helper, so those two read the same either way, and the receipt
# reads 'pointerRestored' as well so the rollback's own detail is there for both.
"$controller" -c 'import json, sys
result = json.load(open(sys.argv[1]))
print(json.dumps({key: result.get(key) for key in
                  ("failedStep", "retriable", "residualPaths", "residualOwnership",
                   "recoveryRequires", "removedCandidate", "pointer", "pointerRestored")},
                 indent=2))' <receipt>/install.json

# Kept the same way, and under its own name: this is the recovery read-back, a different
# reading from the one above, and a receipt holding only one of them cannot say which.
"$controller" scripts/runtime_install.py diagnose --dest <destination> --record <record> \
    --codex-home <codex-home> --state <state> > <receipt>/diagnose.after-failure.json
printf 'diagnose exit=%s\n' "$?" > <receipt>/diagnose.after-failure.exit
cat <receipt>/diagnose.after-failure.json
```

What this block is, and what it is not. It installs, registers, takes the seven readings and
reads the result back. It does not re-run the install, it does not present an arriving source,
and it does not fail an update -- and this page will not tell an operator to break a runtime
their host is using in order to watch it come back. Those three stages are exercised against a
temporary destination by `scripts/ci/tests/test_install_acceptance.py`, at every one of the
eight seams an update crosses.

So a receipt from this block records the stages it actually performed, and it is not a receipt
for the composed run. The last command above is there for the host that arrives at it having
had an update fail on its own, which is the only way that stage is reached here.

A receipt also has to record which interpreter took it, and the block names one controller for
every step for exactly that reason: a page that recommends 3.11 in prose and then invokes bare
`python3` is a page whose readings degrade for anyone who copies it. Five of its steps read a
Codex configuration -- `install`, `register-mcp`, both `diagnose` invocations and the
preservation reader -- and they do not fail alike without `tomllib`, so an operator who runs it
on the 3.10 floor anyway gets a mixture rather than a refusal. Measured on 3.10 rather than
inferred: `register-mcp` refuses outright,
exit 1 with outcome `CONFLICT` naming the interpreter and nothing written, for an absent, an
empty and a populated configuration alike, against a temporary Codex home. `diagnose` does not
refuse: run the same way it reports the configuration `UNREADABLE` and the exposure row reads
`not_verified` -- for want of a reader, not for want of a registration. `install` does not
refuse either, and that one is measured against a temporary destination with the build steps
simulated, which is the acceptance suite's arrangement and not a host: it promotes there on
3.10. Nobody has run this block against a real host from this repository, and it does not claim
otherwise. The preservation reader exits before reading anything.

So on the floor two of the seven readings are unreadable and the rest still stand, and a receipt
records them that way rather than carrying them forward. The runtimes this command installs are
3.11 or newer whatever interpreter started it, so an old controller is never a reason to
postpone the install; it is only a reason two of the seven cannot be taken.

Every field this section tells you to read is one the command it names actually emits, which is
worth stating because it was not always true: the closing `diagnose` used to be where an
operator was sent for `residualPaths` when only an install failure result carried that field.
`failedStep`, `retriable`, `residualPaths`, `removedCandidate` and `pointer` come from the
install result kept above. `diagnose` emits `residualPaths` and `residue` of its own, read
from the destination as it stands: an entry is residue when the installer's own decision
would reclaim it, so this reports that decision rather than a second opinion about the same
directory. It is not guaranteed to be the same SET as a later install's: this command asks
about the pointer the host record names, `cmd_install` asks about the destination it was
invoked with, and on a host whose recorded pointer lies elsewhere those differ -- with this
command the conservative of the two. Which question the installer should ask is a decision
about the installer and is not this issue's to make. A staging the record selects, one somebody
still holds, one whose owner could not be established, and a finished environment nothing
selects are each reported with that decision's own reason and none of them is listed for
removal — a dead staging lock says no installer holds the directory, never that nothing is
running out of it. The owned pointer reaches `residualPaths` only when it dangles AND the host
record records that a link **this command placed** is at that path; a dangling link the record
does not claim is reported as foreign and left alone, because a link's shape is not its
ownership. Those are two different readings of one entry: a failed promotion keeps the pointer
`path`, so a retry derives the same pointer, and a rollback that established the link it placed
is gone withdraws `recordedAt` and `recordedBy`. A record in that state names a location and
claims no link, so whatever link stands there afterwards is reported as foreign.
`firingJournal`, `journalRoot` and `firingRecordAbsence` come from `hook-status`;
`skillLinks`, the `checks.results` cells, `scope.socketConnect`, `definitionVersion` and
`repositoryCommit` from `diagnose`; `sessionId` and `turnId` from the journal records
themselves, which is why the snippet reads the records rather than the count.

That reading used to stop at the absence. `hook-status` omits `journalRoot` whenever its
firing-journal reading could not name a usable journal, and that covers several different
states — no hook registered for the event, registrations naming different settings files, a
settings path spelled relatively, settings the command could not read, and journalling not
configured — which an operator then had to guess between or stop at. `firingRecordAbsence`
answers which one. It takes no reading of its own: every cause is decided over the cells
beside it, and each declares which of them answers it.

| Cause | What it says | What it does not say |
| --- | --- | --- |
| `not_registered` | the hook file was read and registers this adapter for nothing, so nothing on this host invokes it now | that the file is the one the host loads, or that nothing was ever recorded — a registration removed after the hook fired leaves its journal where it was, and this answer names those records rather than reading past them |
| `record_path_unidentified` | a registration spells its settings relatively, or names none, so no file reachable from here answers for it | that the hook has or has not recorded |
| `adapter_cannot_run` | the registered adapter or its interpreter is not there, so the host cannot start it | that it was ever startable |
| `settings_absent` / `settings_unusable` | **one or more** registrations name a settings file that is absent, or that this hook's own reader rejects, so every invocation of *those* registrations releases without recording | which repair the file needs, or anything about a peer registration whose settings are fine |
| `journalling_off` | one or more registrations keep no journal, so those record nothing about their own invocations by configuration | anything about firing, for those registrations |
| `recorded_on_another_path` | one journal these registrations name holds records while another was read and holds none | which registration the host ran |
| `nothing_recorded` | every journal belonging to a registration that can start and has usable settings was read and holds no record | that the hook never ran |
| `several_causes` | more than one cause is established and each needs its own repair | that repairing one of them is enough |
| `cause_unreadable` | the cause was not settled; `candidates` carries every one still standing | which of them it is |

Every cause above is decided **per registration**, because every registration in the hook file
runs and reads its own settings. One registration with a missing settings file beside one that
is fine answers `several_causes`, not the healthier of the two — a peer that works is not
evidence about a peer that does not. The one place that goes the other way is deliberate: a
registration the host cannot start is left out of the journal questions entirely, because its
journal is empty *because* it cannot start, and reading it as a fact about journalling would
invent a second cause for one repair.

Two limits remain, and they are the reason the last two values exist. Under
`journalPolicy: faults_only` the guard records only an invocation that faulted, so an empty
journal is equally what a hook that fires constantly and never faults leaves behind and what a
hook that never fired leaves behind; that host answers `cause_unreadable` carrying both
`policy_records_only_faults` and `nothing_recorded`, and it does not choose. And
`nothing_recorded` is named for the journal rather than for the hook on purpose: a journal
write that fails removes what it left and cannot record its own failure, so "it never ran" and
"it ran and every record failed to be written" are one observation here. **Neither of those is
resolved by this command, and neither is guessed at.**

A third limit is about cost rather than about truth. `hook-status` now opens every absolute
settings path a registration names and lists the journal under it, so its work is bounded by
the number of registrations rather than by one file. That bound is a count and not a clock: a
journal root on an unavailable network mount makes this command slow, and it has no budget of
its own to stop at. The hook's own Stop path is unaffected — it reads the one settings file its
own registration names, under the timeout it is registered with.

This changes what the command answers and not what the acceptance readings are. The hook
callback row is still answered by `firingJournal`; the cause is detail beside it, and the
seven readings remain seven.

Name the relay too. Left out, the entry point is discovered on `PATH`, which finds whichever
relay this host already has rather than the runtime just installed under the destination -- and
with none on `PATH` the trial refuses before it runs. Every flag after `--trial` is required and
a blank one is refused before anything is written; the set is declared once in the source as
`TRIAL_REQUIRED_INPUTS`, together with `--recipient-settings`, which is additionally asked of the
relay's own settings reader.

The absences are answers, and they are different answers. Omitting `--trial` leaves delivery
`not_applicable`: nothing was attempted. Asking for a trial whose inputs are missing or blank
gives `not_verified` naming the input that was not supplied: something was attempted and did not
establish itself. Without `--observed-tool` the exposure answer is that no tool names were
observed, and before a Stop has reached the hook the callback row is an absence. What none of
them is, is a failure of the thing they were asked about, and recording them as though the
questions had been put is the one way this procedure can lie.

`recordsForThisTurn` is the reading. A record naming the session and the turn that was ended
is a callback this procedure can attribute; zero is not a smaller number of callbacks, it is a
turn that did not reach the hook, and the row is unreadable for this run whatever the totals
say. Do not record a total instead -- it is the answer to a question nobody asked here, and it
is the one piece of this procedure another session can move.

More than one record for the turn is not a duplicate by itself. A turn whose Stop was held ends
again, and that second end is a second event: `acceptedEventsForThisTurn` counts them, and
`duplicatesForThisTurn` counts invocations that found their event already accepted, which is
what two registrations answering one Stop leave behind. Whether any event anywhere in the journal
was accepted twice is the question `scripts/stop_events.py` answers; see [One accepted record per
Stop event](#one-accepted-record-per-stop-event).

Read `recordsUnreadable` before concluding. Zero matches beside a nonzero unreadable count is
not an answer either: a record the hook had created but not finished writing is neither your
turn nor evidence against it, and the honest move is to look again rather than to write down a
callback that did not happen or rule out one that did.

### What this block cannot produce on its own

Three of the seven are readings of something live, and the command supplies none of it. The
fixture answers them with stand-ins it builds; an operator has the real thing or has nothing,
and an absence recorded as a result is the one way a receipt from here misleads.

| Reading | What has to be there already | How you know it was |
| --- | --- | --- |
| App Server connection | an App Server accepting connections at `<socket>` | `scope.socketConnect` reads `ok`; anything else leaves `connected` `not_verified` or `unknown`, which is an answer about the socket and not about the install |
| MCP tool exposure | a session that actually listed the bridge tools, whose names go in `--observed-tool` | without the flag the row says no tool names were observed; with it, the evidence names the tools compared against the registered command |
| Delivery acceptance | a relay that can carry the eight steps through to a returned turn id, and a recipient whose settings its own predicate accepts | `deliveryAccepted` reads `verified` only with that turn id in the evidence; every refusal names the step or the input that stopped it |

None of those is a precondition to arrange around. They are the questions, so if the live half
is absent the honest receipt records the absence for that row and says the rest. What it must
not do is carry a row forward as though the question had been put.

Stopping is not on that list, because nothing here starts anything. The installer never starts or
stops a daemon, and a successful install is reported as `alwaysActive: not_verified` however well
it went; whoever operates the service starts and stops it. A refused update naming a residual
pointer is telling you to look at that link rather than telling you it is fine: the restoration
happened on disk but could not be read back, and the command declines to claim what it could not
confirm.

A real run records the exact revision it ran at, the interpreter and host it ran on, the
destination kind, and the answer to each of the seven with the command that produced it and the
time it was produced. `repositoryCommit` and `definitionVersion` are in the payload for that
reason. Those receipts are host facts: they belong in the private record outside this repository,
not in a commit, and `measuredPoints` in the committed definition stays empty until a measured
point is made. This page is the procedure and the shape. It is not a record that anybody ran it.

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
