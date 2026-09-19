# The hook off/on comparison

Adding a hook does not establish that fewer completions are missed. Two things in this repository
already look like that establishment and are not: `hook_probe.py replay` checks the contract's decision
table against fixtures recorded beside it and says of itself that it re-runs no hook and would keep
passing on a machine where hooks are switched off, and the composed acceptance run in
[runtime-install.md](runtime-install.md) establishes that one registered hook fired once on one
temporary destination. Neither compares a destination carrying the hook against the same destination
without it.

`scripts/hook_comparison.py` runs that comparison. It builds two destinations from one command, drives
the same scenarios at both, and reads what each one has afterwards. The criteria it reports against
were fixed before it existed, in [the hook contract](../plugins/crw/skills/crw-run/references/hook-contract.md),
and this document neither restates nor extends them. Two of the six are reported as not performed
for reasons given below, which is the honest count rather than a shortfall discovered later.

## The one switch

Both arms are built by `scripts/runtime_install.py hook --adapter completion` with identical
arguments. The off arm omits `--apply`, which is that command's own dry run; the on arm passes it.

| Arm | Exit | The command's result | Its settings outcome | Adapter entries under `Stop` |
| -- | -- | -- | -- | -- |
| off | 0 | `MISSING` | `config_would_create` | none |
| on | 0 | `CREATED` | `config_created` | one |

The exit status and both outcomes are three readings rather than one, taken from the command's own
report, and they are a precondition of everything after them. An installer that failed before writing leaves exactly the empty Codex
home a successful dry run leaves, so an off arm judged only on what is missing would pass on a run
that never happened.

What the harness executes at each arm is derived from that arm's own `hooks.json` through
`completion.adapter_entries`, never written out a second time here. At the off arm the derivation
returns no entry, so nothing is executed. **That absence is the off arm, and every off-arm cell
records it as an absence naming its reason rather than as a false, a zero, or a release.** A harness
that ran a helper at the off arm to have something to compare would be comparing two runs of its own
code.

The on arm registers in hold mode. In observe mode, which is what `runtime_install.py hook` installs
by default, `guard.decide` downgrades every block to a release and the adapter prints nothing, so
wrong blocking would be a question no run could answer either way. The contract permits holding only
where a held child cannot forge the facts the decision reads, and what establishes that here is not
the name passed to `--isolation-asserted-by` but the arrangement itself: no child exists, and the
harness is the only writer of every marker fact and store row under its temporary root. The result
document records that rather than asserting it, every block and reservation it produces is labelled
synthetic hold-mode output, and none of it describes what a default installation does when a turn
ends, which is nothing.

## Twenty-two cells, twenty-two readings

Each cell is filled by the reading its own question called for. A reading that could not be made
answers `unreadable` and names why; it never answers false and never takes the value of the cell
beside it. Since CRW-103 that answer is a distinct type rather than a spelling of one, so a
consumption site that treats it as the value it sits beside raises there instead of being let
through. In the result document it is written as `{"notRead": ..., "why": ...}`: an object,
precisely so that no cell's answer can equal it, and its reason repeats the cell's own `detail`
because the places that hold one are not all cells.

| Cell | Answered by | Read from | Never established by |
| -- | -- | -- | -- |
| `installExit` | the install command | its exit status | the state of the Codex home afterwards |
| `installResult` | the install command | its own report, `result.outcome` | the exit status alone |
| `installSettings` | the install command | its own report, `settings.outcome` | the result beside it |
| `registration` | the arm's `hooks.json` | the entries `adapter_entries` finds under `Stop` | the install command's report |
| `foreignRegistration` | the arm's `hooks.json` | whether the entry another owner had there is still there | the count of entries |
| `firedCommand` | that entry | its `command` string, which is the command that gets run | a helper call into `completion.run`, or what the tracer says actually ran |
| `adapterOutcome` | the hook's journal record | `adapterOutcome` | the process exit code, which is always zero |
| `observation` | the hook's journal record | `observation`, what the turn was | the guard's own stdout, which this run never sees |
| `guardDecision` | the hook's journal record | `guardDecision`, block or release | the state beside it |
| `guardState` | the hook's journal record | `guardState`, the decision state | the observation or the decision |
| `printedBlock` | the hook process | its stdout | the journal's `guardDecision` |
| `recordedAs` | the hook's journal record | `guardRecordedAs` | a directory walk |
| `observationFile` | the marker root | whether that path resolves to a file under the assignment | `recordedAs` being present |
| `heldFile` | the assignment directory | the create-once `hold.json` reservation itself | counting decisions that said block |
| `journalElapsedMs` | the hook's journal record | `elapsedMs`, the adapter's measure of itself | the harness's clock |
| `processWallMs` | the harness | the wall clock around the process, interpreter start included | the adapter's `elapsedMs` |
| `processExit` | the harness | the status the hook process exited with, relayed by the tracer when one started it | anything it wrote |
| `startedExecutable` | the tracer | the path argument of the first `execve` it recorded | the argv this harness reported about itself |
| `startedCommand` | the tracer | whether the argv it recorded is the argv the arm's own `hooks.json` names | either of the two alone |
| `startedEntryPoint` | the tracer | the program position of that argv, which is where `command_for` puts the entry point | a mention of the entry point anywhere in the argv |
| `unexpectedExecutions` | the tracer | every later `execve` that is not a descendant running this run's own launcher | a reading of the first line only |
| `writesOutsideRoot` | the tracer | the write-class calls it recorded, resolved, that fall outside the run's own directory | the places this run created, which is a different question |

Several of these pairings deserve their reason in the open. `printedBlock` answers in the vocabulary
the host sees, which is that a block was printed or that nothing was printed; it is not translated
into `release`, because the adapter prints nothing for a release and for several faults alike, and a
cell that turned silence into a decision would be asserting the thing it failed to read.
`recordedAs` and `observationFile` are two questions rather than one, because what the guard said it
wrote and what is on disk are different claims and the second is the one that survives the process.
`heldFile` is read from the reservation rather than from the decisions, because the hold is the
critical section: exactly one caller wins the create-once file, and a count assembled from what each
evaluation returned would be a count taken outside the lock that decides it. The last two are two
intervals rather than one number, and neither may stand in for the other.

The journal's `observation` and the record published under the marker root are the same guard value
copied into two places. Agreement between them establishes that one decision reached two writers and
nothing more, so the claims here are narrowed to match: `observation` says the adapter received a
verdict naming that state and journalled it, and it is meaningful only where `adapterOutcome` is
`guard_answered`. Any other adapter outcome means the scenario was not measured. It never means no
omission was present.

## The scenarios, the observation, and the decision

Every scenario builds its own workspace, session, assignment and issue key. This is not tidiness.
Hold budgets are one per turn, two per generation and three per rolling session window, and the relay
refuses a second relationship under one issue key, so scenarios sharing either would decide each
other's outcomes. The child's session identity is its child task id, which is what a dispatch receipt
returns and what the receipt lookup matches against.

The observation, the decision and the decision state are three columns because they are three
answers. A turn whose continuation is already running is still an omission, `hold_in_flight` says
not right now, and `unresolved_handoff` says the assignment has stopped converging. The last two
are both a silent release carrying no new reservation, so a scenario that declared one and accepted
the other would pass while the thing it was watching for had happened.

| Scenario | What it builds | Observation | Decision | Decision state | Printed | Reservation |
| -- | -- | -- | -- | -- | -- | -- |
| `receipt_missing` | declared, bound, claimed, registered, `ready_for_review` recorded, no receipt emitted | `receipt_missing` | block | `receipt_missing` | a block | reserved |
| `managed_unregistered` | declared, bound, claimed, no relationship fact published | `managed_unregistered` | block | `managed_unregistered` | a block | reserved |
| `undeclared_turn_end` | declared, bound, claimed, registered, nothing recorded for the turn | `undeclared_turn_end` | block | `undeclared_turn_end` | a block | reserved |
| `declared_ready_receipted` | the same as `receipt_missing` plus a real `emit` over real bytes | `declared_ready_receipted` | release | `declared_ready_receipted` | nothing | none |
| `declared_in_progress` | a progress report recorded for this turn | `declared_in_progress` | release | `declared_in_progress` | nothing | none |
| `declared_blocked_needs_input` | a turn waiting on a person | `declared_blocked_needs_input` | release | `declared_blocked_needs_input` | nothing | none |
| `declared_interrupted` | a turn the user stopped | `declared_interrupted` | release | `declared_interrupted` | nothing | none |
| `unmanaged` | a workspace no marker names | `unmanaged` | release | `unmanaged` | nothing | none |
| `cxc_concurrent` | an omission turn carrying `stop_hook_active`, beside a foreign CXC entry in the same hook file | `undeclared_turn_end` | release | `hold_in_flight` | nothing | none |
| `duplicate` | one omission turn, the command fired twice | `undeclared_turn_end` both times | block, then release | `undeclared_turn_end`, then `hold_in_flight` | a block, then nothing | reserved once, and still the one |

The first three are the omissions CRW-68 asks for: a declared readiness whose receipt never arrived
is the missing `emit`, and a claimed marker whose relationship was never registered is the management
marker without its registration. The fourth exists because without it the omission rows would be
assertions about a cell that had never been seen to move the other way, which is the shape of a suite
that passes while the guard sees nothing.

`cxc_concurrent` carries a second reading of its own: the foreign entry that was in the hook file
before the install is read back out of it afterwards, because a hook that displaced another owner's
`Stop` entry would have broken the property this scenario is named for. That foreign command is never
executed. The harness runs only the entry naming this adapter, and the `stop_hook_active` flag is
supplied by the harness rather than delivered by a host.

## The witness at the process boundary

Two of the things this harness could not see were the same failure wearing two faces: nothing observed
the process. The command was read out of the arm's `hooks.json`, executed, and then reported back by the
same file that executed it, so a run that started something else while reporting the registration would
have agreed with itself. And the containment check confirmed the places the run *creates*, which says
nothing about where its subprocesses actually wrote.

Every firing is now **started by a tracer** rather than started and then traced. Attaching afterwards was
measured and it works, and it is the weaker answer: the `execve` is over before any attach can land, so
the only thing left to read would be this file consulting `/proc` and reporting what it saw, which is the
harness attesting to itself. Started under the tracer, the kernel's own `execve` record is the first line
of a file another program wrote, and the checks read it the way they read a journal.

Five readings come out of that file, and each is a comparison against something fixed before any
subprocess started.

| Reading | What it catches |
| -- | -- |
| `startedExecutable` | a different binary behind the registration: a shim, a wrapper, a name resolved elsewhere |
| `startedCommand` | this harness running something other than the command it reports, since the argv compared is the kernel's and the argv compared against is the arm's own hook file |
| `startedEntryPoint` | a registration substituted to a different program. It reads the argv's **program position**, which is where `command_for` puts the entry point, and not a mention of the entry point anywhere in the argv: `python -c '<impostor>' completion_hook.py` names the canonical file in a position nothing executes |
| `unexpectedExecutions` | a process that starts the expected interpreter and then becomes something else. Any later `execve` by the root process is unexpected even at the same image |
| `writesOutsideRoot` | a write to a path the run never named |

The two path readings compare the **raw strings the kernel recorded** against `sys.executable` and this
checkout's entry point. They are not resolved first, and that is deliberate: a resolution taken after the
run answers about the filesystem as it is afterwards, so a symlink standing where the entry point belongs,
repointed at the real file by the program it started, resolves to exactly the right answer. The resolved
forms are recorded beside the readings and decide nothing.

A parser that read nothing answers "nothing was written outside" exactly as convincingly as a run that
wrote nothing outside. So every on-arm firing must have been **seen writing something inside the root**.
Each one journals its own invocation, which is such a write, so a firing this witness saw write nothing is
a witness that stopped seeing rather than a hook that stayed quiet.

The tracer is probed once per run, before the arms are built, by starting a process the same way a firing
is started, having it write one file, and requiring that write to come back out of the same parser. A
tracer that attaches and reports nothing is reported unusable rather than trusted.

**Where no tracer can be established, `processWitness` answers not performed** and carries no verdict at
all, beside the two criteria the contract itself cannot reach. The readings still say a reading was not
taken, `notPerformed` still names the reason, and `standIns` truthfully regains the reported-argv entry. It
never becomes "nothing was written". What it costs is that such a host does not answer this question, which
is what `--require-witness` is for: with it, a run that cannot witness the boundary refuses instead.

The witness fails a judgment rather than a row. A row answers whether that scenario reached the state it
declared, and under a substitution it did; the finding is read from `judgmentsThatFailed`, where
`processWitness/met` sits, in the same way a latency bound is missed while every row agrees with its own
table.

What it does not cover is emitted as data in `processWitness.doesNotCover` rather than left for a reader to
assume: identity is by path, so a file replaced at that path during the run and put back before the last
digest is not caught; writes through a descriptor this trace never saw opened, and the calls that write
through one rather than through a path; a filesystem socket made by `bind`; ownership, timestamps and
extended attributes, which are not in the traced set at all; where a path led at the syscall, since
containment resolves it afterwards; the tracer itself, which is trusted rather than checked; and anything a
process does after the tracer stops.


## Where absence is the answer, and why it is normal there

| Place | What is absent | Why that is the correct answer |
| -- | -- | -- |
| every off-arm firing cell | the journal record, the stdout, the published observation, the reservation | `adapter_entries` found no registration under `Stop`, so no command existed to run, and the install reported the dry run that leaves none |
| `unmanaged`, `recordedAs` | the published observation | no assignment directory was selected, so there was nowhere to publish |
| every scenario the guard releases on its own declaration, `heldFile` | the reservation | a turn that declared itself releases on that declaration, and a reservation here would be the defect the scenario watches for |
| `cxc_concurrent`, `heldFile` | the reservation | a continuation is already running for the turn, so the omission is recorded and nothing is held |

The set of places is built from the scenario and cell declarations rather than listed by hand, so a
scenario or cell added later either declares its absence answer or fails.

## The six measures

The contract fixes these at
[Decision criteria, fixed before implementation](../plugins/crw/skills/crw-run/references/hook-contract.md).
Where this arrangement cannot reach one, it is reported as not performed rather than rewritten into
something it can reach.

| Measure | Here | What narrows or prevents it |
| -- | -- | -- |
| Missed-detection, state and receipt both absent | measured | nothing; every injected `undeclared_turn_end` reaches that observation and is counted on its own |
| Missed-detection, receipt absent only | measured | nothing; every injected `receipt_missing` reaches that observation with the declared state present |
| Handoff success | not performed | it is a reported omission followed by a real receipt and one parent verification with no new user message. There is no parent and no verification round trip here |
| Wrong block | measured as this hook's own holds | read from the reservation and the stdout together. The contract makes every bound self-imposed and enforced by the hook, so the hook's own reservation is the thing to count; whether a host would honour a printed block is not observed |
| Duplicate execution | not performed | it is that no verification or correction runs twice for one event id across a hold, a daemon restart and a recovery. Nothing verifies or corrects here and no daemon runs |
| Added latency | measured against the budget, for the hook process only | the distribution of `processWallMs` is reported as minimum, median, 95th percentile and maximum against the 2 s median and 5 s 95th percentile bounds. The contract's budget is per Stop as the host sees it, so meeting it here is necessary and not sufficient |

Where this harness classifies what a host would see, the rule comes from the code that decides it
rather than from a copy here. Whether printed output is a block the host acts on is asked of the
adapter's own validator, because a second copy of that rule agrees with the original only until
one of them changes, and it already had: the copy accepted a block that never asked for a
continuation, which the host reports as a failed run. A reading that could not be taken is also
never consumed as a value, at any site: the sentinel is a non-empty string and slips past exactly
the tests that look like they exclude it.

A judgment declares which arms it speaks for, and its predicate reads exactly those. Nothing is
taken out of a relay response that the command did not promise to answer with, declared per
command rather than checked at each site. A verdict is computed in one place and nowhere else, so it cannot be computed without the guard
that refuses it while a reading under it was not taken. Stating that rule and leaving each
verdict to apply it is how one of them came not to, and the check meant to catch that only fired
when a reading had already failed, which never happens in a healthy run. Every failure mode is
executed rather than inferred from a handler being present, because a handler existing is not
evidence that the path reaches it.

Every judgment that carries a verdict can also say that a reading under it could not be taken,
and none of them is met while it is saying so. A criterion that dropped an unreadable reading and
concluded from what was left would report a bound as kept on evidence nobody has, which is the
substitution the readings above exist to refuse; it was fixed one measure at a time until the
property was asked of all of them at once.

One supplemental observation is reported under its own name and is not offered as the duplicate
execution measure: one turn fired twice reserves exactly one hold and publishes two numbered
observations, which is the hold leg of that measure and none of the rest of it.

CRW-68 also requires the marker-without-registration case, which the contract's table does not name
as a measure of its own. It is reported as a third missed-detection row, separately, and is never
merged with the other two.

**The pass is never the difference between the arms.** Off having no records and on having them is
settled by the `--apply` flag before any Stop is delivered, so a harness whose criterion was that
difference would be measuring the flag. Each on-arm row instead asserts the exact observation and
decision named in the scenario table, a managed scenario whose `recordedAs` is null is reported as
unmeasured rather than as a pass, and the cross-arm difference is reported as information.

## The result document

One JSON object on stdout, and nothing else on stdout.

| Field | What it carries |
| -- | -- |
| `source` | `hook-comparison`, the stamp a reader checks before walking a path into this document |
| `sourceIdentity` | the commit, whether the working tree was clean, and a digest of every file in each source whose contents decide a run: the harness, the installer, the entry point, the runtime modules and the relay. Taken before the first subprocess and again after the last, because a file edited during a run means earlier scenarios executed different bytes from later ones, and an identity from either end would name a source no scenario ran. A commit identifies bytes only in a clean checkout, and anyone developing this runs it in a dirty one |
| `pythonVersion` | the interpreter that ran it |
| `mode` and `isolation` | the mode the on arm was registered in, and what makes holding legitimate here |
| `arms` | per arm, the install reading, the registration reading, the Codex home and the argv of the install |
| `scenarios` | per scenario, per arm, every cell as a value with the source that answered it, the path it was read from, whether it was readable, and the detail when it was not; a reading that could not be taken is written as an object rather than as an answer; beside the observation and decision the scenario declared in advance, and the provenance of what was handed to the command |
| `measures` | the six, each with its answer, the rows it was computed from, and its narrowing sentence |
| `supplemental` | observations reported under their own name because they are not one of the six |
| `wroteOnlyInsideItsRoot` | every place the run CREATES, and whether each resolves inside the directory it made for itself. Where the processes actually wrote is `processWitness` |
| `processWitness` | what the tracer recorded about every on-arm firing: the executable started, the argv recorded, the program position, any later execution, and the writes outside the root; or, where no tracer could be established, that this was not performed and why |
| `judgmentsCounted` and `judgmentsThatFailed` | every field in this document named passed or met, and which of them said false. The exit status is taken from that list and from nothing else |
| `measuresThatMissedTheirBound` | the measured criteria that were not met, kept as a readable summary of part of the list above |
| `notPerformed` | what was not run and why, including the CRW-68 criteria this arrangement cannot reach |
| `standIns` | per stand-in, what it replaces and what a row travelling through it therefore does not prove |

A run passes when no judgment in its document said false. A judgment is any field named
`passed` or `met`, wherever it sits, and they are collected by walking what was assembled
rather than from a list of the kinds that produce them: three times that list left one out, and
each time the thing left out was a judgment that could fail while the command exited zero. A row on the on arm passes when the install succeeded as its arm declares, exactly
one entry names this adapter, the hook process exited zero, the adapter outcome is `guard_answered`, the observation and the decision and the
decision state and the printed answer and the reservation are all the ones the scenario declared in
advance, and a managed scenario resolves its published observation on disk. A row on the off arm
passes when the install succeeded as a dry run and every firing cell is absent for the one declared
reason.

Everything in the document comes from the run's own inputs and its own temporary root. The command
lines it reports carry absolute paths because they are the command lines it was given to run: the
interpreter,
the entry point and the installer inside this checkout, and the relay launcher, the Codex home, the
marker root and the store under the temporary root. The sessions, turns, issue keys and task
identities are names the harness invents. Nothing is read from a Linear document, a transcript, a
credential store, or a Codex home belonging to the caller, so nothing from any of those can reach
it.

## Running it

```
python3 scripts/hook_comparison.py --root <a directory outside this checkout> > result.json
```

The run needs Python 3.11 or newer, because the relay requires it; on an older interpreter the
harness refuses and says so rather than reporting rows it could not take.

It also needs a tracer to witness the process boundary. Where there is none, every row is still
taken and `processWitness` answers not performed with the reason, which means a run on such a
host does not answer which executable was started or where the processes wrote. Pass
`--require-witness` to make that a refusal instead; evidence for those two questions has to come
from a run that made it.

The place named by `--root` is where the run makes a directory of its own; it is not where the run
works. The harness writes a launcher and two Codex homes at fixed names, so using the named
directory itself would replace whatever was already using those names, and a directory an operator
points at is exactly where something else already lives. Everything the run writes goes in the
directory it created, including the bytecode its subprocesses would otherwise leave beside the
source they import. `wroteOnlyInsideItsRoot` in the result is that claim checked against the
resolved path of every place the run CREATES rather than against how the paths are spelled, and
`processWitness` is the same claim checked against every path the processes were seen writing. With no
`--root` the run makes a temporary directory and removes it afterwards; with one it keeps everything,
which is what an operator wants when a row has to be explained.

Every way a subprocess or a parse can fail ends in a document. A timeout, an executable that
could not be started, a nonzero exit and output that is not JSON each produce a refusal naming
the step, and the boundaries are derived from the source rather than listed, because two of them
were found that way: both `git` calls handled a missing executable and not a timeout.

It starts no daemon and no service, registers nothing outside its own temporary destination, and
leaves no process behind. The relay it calls is the one in this checkout, reached through a launcher
the run writes, so no installed runtime is read or changed.

## What stood in for what

| Stand-in | What it replaces | What a row through it cannot prove |
| -- | -- | -- |
| the relay launcher | the console script an install places under the pointer at `<destination>/current/bin` | that the pointer resolves, or that an installed build offers `guard-evaluate` at all |
| the temporary Codex home | a Codex home a host actually reads | that the host discovers this registration, trusts it, invokes it, enforces the registered timeout, or accepts what it prints |
| the composed Stop payload | a payload a host delivered | anything about what a host sends; it establishes classification given the fields it carries |
| the supplied stop_hook_active | a host reporting a continuation in flight | that a host sets it when it continues a turn |
| no daemon and no App Server | the running service | delivery, acknowledgement, parent verification, and recovery after a fault |
| the harness as sole writer | a sandbox grant | that a real child under a real grant could not forge the facts the decision read |
| the reported argv | a witness at the process boundary, **on a run that could not establish one** | which executable ran. It appears in this table only when `processWitness` answers not performed. Where a tracer could be established the kernel's own `execve` record answers it instead, and the row is absent from that run's `standIns` |

## What this does not answer

CRW-68 asks for seven things. Three of them are not here, and they are recorded as not performed
rather than covered.

The real installed path, from a child's completion through a parent's verification, a correction, a
re-verification and a Linear write read back, on the same persistent database, needs an install on a
host and a real round trip between tasks. The separation of installation, registration, firing,
delivery acceptance and artifact verification on a real host, together with recovery after a daemon,
connection or hook fault, needs the installed runtime and the daemon; this harness separates
registration from starting the process from firing on a temporary destination, witnesses which
executable that start reached, and stops there. Two parents in different
repositories and Linear projects against one installed shared relay, with concurrent handover and
per-parent separation, needs that shared service.

What this harness measures is hook behaviour reached through a launcher backed by the source in
this checkout. It does not measure the installed runtime: packaging, the pointer, and whether an
installed entry point resolves and offers the subcommand are all untested here, and a failure in
any of them would not appear in any row. That is the same boundary as criteria four, five and
seven, and it opens behind CRW-90.

No adoption or hold decision is written anywhere in this harness or this document, because the
results that would support one are the three that were not performed.
