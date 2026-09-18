# Preparing and starting a live trial

A live trial drives the installed runtime through a real completion, a real delivery to a parent,
a real verdict and a real correction round trip, on the shared store that ordinary work uses. The
automatic legs of one have closed with no operator inside the window. What went wrong on the way
there was the preparation, three times, and each failure produced a result that looked like a
runtime problem until it was read: a supervisor that stopped polling, a parent the relay refused to
deliver to, and a child that emitted against a relationship that had already been archived.

This document fixes what is confirmed before a trial starts, in what order the parts are built, and
how the interventions during preparation are kept apart from the ones inside the window that
produces the result. `scripts/trial_startup.py` performs the checks it can perform and grades the
evidence it cannot take itself.

Two other things in this repository are named similarly and are not this.
[The hook off/on comparison](hook-comparison.md) runs against two temporary Codex homes with no
host, no daemon and no App Server, and it says so about itself. `runtime_install.py --trial` is a
diagnosis mode that registers, emits and delivers once to fill one field of its own record, and
[its own trial preflight](runtime-install.md#the-trial-preflight-matches-what-the-relay-requires)
owns that name for the checks that mode needs. Neither name is reused here.

## The three failures this exists for

| What happened | What it looked like | What catches it now |
| -- | -- | -- |
| A supervisor launched with `nohup` did not survive the tool shell that started it, so emissions sat staged with nothing polling | a delivery that never arrived | the `processPersistence` reading, which requires the supervisor to be outside the caller's session and to have advanced its own witness between two observations |
| Participants created without a turn could not be looked up at all, and the relay withheld delivery with `lifecycle_unknown` rather than sending to a recipient whose lifecycle it could not read | a recipient that never woke | the `parentLifecycle` reading, and the standby turn that the order now puts before registration |
| A child read its assignment file before the new relationship id had been written into it, emitted against the archived relationship, and was refused with `relationship_not_active` | a child that failed for a reason it had not caused | the order gate, which compares the file, the store and the message about to be sent, before the message is sent |

None of the three was a defect in the runtime. All three were preparation, and the relay refused
each of them correctly. That is the reason this document is about preparation and says nothing
about the runtime's own guarantees.

## Four readings are taken, and two are graded

Six things are confirmed before the first dispatch. Four of them the checker reads itself. The
other two it cannot: no read-only relay command performs a lifecycle read, and none returns what a
creation receipt echoed, so those two arrive as captures the operator recorded elsewhere. A capture
is the caller's claim about a read this process did not make, it carries the time it was taken, and
it is stale past the record's own bound, which may not exceed fifteen minutes.

| Reading | Answered by | Met when | Never established by |
| -- | -- | -- | -- |
| `processPersistence` | `os.kill(pid, 0)` and `os.getsid`, the supervisor's witness file, and `service status` where the supervisor is the relay's own service | the process is alive at two observations, sits outside the caller's session, has lived at least the declared minimum since it was launched, and its witness names that pid and advances its counter between them; where a service is declared, `ownership` is `ours`, the lock is held, `staleRecord` is false, and the pid and store id agree | that a launch command returned, or that a pid appears in a record. A recorded pid with a free lock is what `staleRecord` is for |
| `parentLifecycle` | a captured host response per parent | the capture names that task, carries no error, and resolves a thread status | a creation receipt. `thread not found`, `missing source rollout` and `no rollout found` are the three answers that fail it by name. A null goal is not one of them: a healthy task without a goal has one |
| `capability` | the captured creation receipt, and `settings-show` for the same task | each payload names the task it is about, the receipt's `settings.actual` equals every setting the record declares, its `findings` are empty, and the store's own settings record is usable, complete and carries the same values | that a provider served that model. A matching echo says the host recorded the request. `usable` alone is completeness, not agreement |
| `storeIdentity` | `doctor` with `--expect-store`, `--expect-inode` and `--expect-nonce`, and each peer's captured `doctor` | the relay's own `sameStore` reports `proven` here, and every declared participant has a captured `doctor` of its own reporting `proven` while its own store identity agrees with the record. A participant with no capture is unknown, never absent from the count | an equal path string, or an agreeing store id and inode. The relay grades those as `unproven` on their own: proof takes a nonce another participant wrote, found beside an agreeing device and inode |
| `boundaries` | the record's declaration, each boundary's captured registration receipt, and `git rev-parse --show-toplevel` in each declared directory | there are at least two boundaries whose issue key, scope reference and resolved repository root are all pairwise distinct, each receipt names that boundary's scope reference, child task and directory, and each directory is the repository it was declared to be | a display name, a title or a working directory, none of which identifies anything ([OPS-7.2](../skills/crw-run/references/operations.md)). Two boundaries in one repository are not two repository identities |
| `assignmentState` | `assignment-find --issue` and `criteria-show` | the responsible relationship is the one the record names, its entry carries the same child task and execution generation with status `active`, and the criteria set matches by digest, source reference and count | a relationship id written in a file. A non-empty criteria set is not the intended one |

Every fact a reading later compares a payload against is required in the record itself, before any
command runs. Two absent values compare equal to each other, so a record that stated nothing would
have agreed with a payload that carried nothing, and the run would have reported a precondition
nobody established.

A reading that could not be taken answers `unknown` and refuses the start. It never answers false,
and it never takes the value of the reading beside it. The distinction is the whole point: a
missing answer and a wrong answer call for different actions, and a start that proceeded on an
unknown is how two of the three failures above were discovered after the fact rather than before.

The store readings have an ordering rule of their own. Every relay command opens the store on
construction, creating the directory and an empty database when they are absent, so a mistyped
path produces a silent empty store rather than an error ([OPS-3.4](../skills/crw-run/references/operations.md)).
`doctor` constructs nothing, so it runs first, and a store whose `createdAt` is not earlier than the
checker's own start is a refusal rather than a pass.

## The order

    create ──▶ standby turn ──▶ register ──▶ write the start record ──▶ preflight ──▶ dispatch

Each arrow carries the failure that happens when it is reversed.

**Create, then a standby turn, before anything is registered.** A task with no turn has no rollout,
so the host cannot resolve its goal or its active turn, and the relay withholds delivery from a
recipient whose lifecycle it cannot read. Materialising every participant with a completed turn is
what makes the `parentLifecycle` reading answerable at all.

**Register before dispatching, and record settings and criteria while registering.** Registration
is the first mutating step, it mints the relationship id, and it either replays an existing
relationship or refuses the whole registration without writing anything else. Everything after it
reads what it returned. An issue that already holds an active or paused assignment cannot acquire a
second registered child, so two parents on one store need two distinct issues; a trial that binds
its assignment onto a real backlog issue puts a trial row in the path of real coordination, and the
genuine project identity travels in the scope reference instead.

**Write the start record from what `register` returned, not from what the previous window left.**
The assignment file in the child's workspace is the file the third failure read too early. It is
written after registration, from the returned relationship id, and the dispatch message carries the
same identity in its own text, so the child's first turn does not depend on reading that file at
all.

**Preflight last, immediately before the dispatch.** All six readings are taken by one run at one
moment, because that is the only moment at which all of them are simultaneously true. All three
observed failures were checks made too early, or not made.

**Then dispatch.** The window opens here.

### What the order gate compares, and what it cannot promise

    the assignment file      the store's own answer           the message about to be sent
    relationshipId    ═══    responsibleRelationship    ═══    contains that id
    childTaskId       ═══    childTaskId
    executionGeneration ═    executionGeneration
                             relationshipStatus is active
    artifacts         ─────  inside the registration receipt's artifact roots
                                                              contains each artifact path

A file written for a previous relationship carries that relationship's id, and that is the
comparison that catches it. The artifact roots come from the captured registration receipt, because
no read-only command returns them; what actually enforces them is the manifest `emit` builds, which
is where it belongs.

A read-only checker makes this race **refusable, not impossible**. It reads at one moment and the
child reads at a later one. What makes it impossible is the order: register first, write the file
from what register returned, carry the same identity in the message, and leave the relay's own
`relationship_not_active` refusal as the guard it already is. The gate's job is to stop a dispatch
that would fail, and to say which field disagreed.

## Interventions, counted in two places

An operator who fixes something mid-run has not produced an uninterrupted result, and a record
that reports one is worse than a record that reports none. The trial keeps a ledger, appended as it
goes at <trial root>/ledger.jsonl, and the checker grades it.

    {"at": "<ISO-8601 UTC>", "kind": "segment_start|segment_end|window_open|window_close|intervention",
     "segment": "<name>", "actor": "operator|user", "target": "<task, store or process>",
     "action": "<what was done>", "claimed": "preparation|window"}

A segment is bounded by its own start and end, which are times rather than positions in the file:
segments are built as intervals and two that intersect are a refusal, because a pairing taken from
line order accepted two overlapping segments and counted one intervention inside both. `segment_end`
carries `failed` or `succeeded`.
The window is bounded by `window_open` and `window_close`. Classification is by timestamp and by
nothing else: an intervention inside the window's bounds is a window intervention whatever it calls
itself, and `claimed` exists so that a label disagreeing with its own timestamp is a refusal rather
than a correction.

The report gives the preparation segments with their own counts, the failed ones among them named,
and the window's count beside `windowIsClean`. That boolean is never printed without the provenance
of the bounds it was computed from: `corroborated` when the record supplies a store time to compare
against and compared with them, `declared` when the bounds are the operator's word; a corroborating time that disagrees is a refusal. A window with any intervention in it
fails its judgment and the command exits non-zero, so a corrected run cannot be reported as a clean
one by a gate that only read the exit status.

What this accounting cannot see is an intervention nobody wrote down.

## Private records, public harness

Everything the trial writes for itself lives under one trial root outside every git worktree: the
start record, the captures, the ledger and the result. The assignment file and the dispatch message
belong to the child's workspace and sit where that workspace is, and the only rule for them is that
they are not inside this repository. Paths are compared after symbolic links are followed, so a
link out of the trial root is the escape it looks like rather than a spelling that passes.

The reason is not tidiness. Operational state never lives inside a repository
([OPS-3.2](../skills/crw-run/references/operations.md)), a committed host identifier is a private
receipt in a public repository, and a synthetic harness that acquired one would stop being
synthetic. This document carries placeholders for the same reason.

## Running it

    python3 scripts/trial_startup.py preflight --start <trial-root>/start.json
    python3 scripts/trial_startup.py ledger    --start <trial-root>/start.json

The first is run once, immediately before the dispatch that opens the window. The second is run
after the window closes, or at any point while it is open. Both print one JSON object on standard
output and write nothing. Exit 0 means no judgment in the document said false, 1 means one did, and
2 means it refused before it could assemble a result and printed the refusal instead.

The start record is one JSON object under the trial root, with absolute paths throughout:

    {
      "source": "live-trial-start",
      "recordVersion": 1,
      "trialRoot": "<trial root, outside every git worktree>",
      "relay": {"launcher": "<destination>/current/bin/codex-session-relay",
                "launcherSha256": "<digest of that file>",
                "stateDirectory": "<the shared state directory>",
                "socket": "<the App Server control socket>"},
      "store": {"storeId": "<from store-identity>", "device": 0, "inode": 0,
                "challengeNonce": "<written by a participant during preparation>"},
      "supervisor": {"pid": 0, "witness": "<trial root>/supervisor.jsonl",
                     "launchedAt": "<ISO-8601 UTC>", "minimumAliveSeconds": 60,
                     "witnessAdvanceSeconds": 5, "service": false},
      "assignment": {"relationshipId": "<from register>", "parentTaskId": "<task>",
                     "childTaskId": "<task>", "issueKey": "<issue>",
                     "executionGeneration": 1, "artifacts": ["<absolute path>"],
                     "assignmentFile": "<child workspace>/assignment.json",
                     "dispatchMessageFile": "<trial root>/dispatch.txt",
                     "criteria": {"setDigest": "<from criteria-show>", "sourceRef": "<reference>",
                                  "count": 4}},
      "boundaries": [{"name": "A", "issueKey": "<issue>", "scopeRef": "<scope reference>",
                      "repositoryRoot": "<repository>",
                      "participants": [{"role": "parent", "taskId": "<task>", "cwd": "<directory>",
                                        "expect": {"model": "<model>", "reasoningEffort": "<effort>",
                                                   "sandbox": "<mode>", "approvalPolicy": "<policy>"}}]}],
      "captures": {"parentLifecycle": {"<task>": {"path": "<trial root>/lifecycle-parent.json",
                                                  "capturedAt": "<ISO-8601 UTC>"}},
                   "creationReceipt": {"<task>": {"path": "<trial root>/receipt-parent.json",
                                                  "capturedAt": "<ISO-8601 UTC>"}},
                   "registration": {"A": {"path": "<trial root>/register-A.json",
                                          "capturedAt": "<ISO-8601 UTC>"}},
                   "peerDoctor": {"child-A": {"path": "<trial root>/doctor-child-A.json",
                                              "capturedAt": "<ISO-8601 UTC>"}}},
      "captureMaxAgeSeconds": 600,
      "window": {"opensAt": "<ISO-8601 UTC>", "closesAt": "<ISO-8601 UTC>"}
    }

The record names parameters, never command lines. The checker composes every command it runs, and
the only programs it can start are `git` and the relay entry point that the runtime host record
under the state home names through its owned pointer. The record cannot nominate another program:
the host record's location comes from the environment, and the launcher it names is compared with
the one the record declares.

The nonce is not written by the checker. During preparation one participant writes a challenge into
the store and the rest read it back, which is what turns an agreeing store id and inode into proof.

## What this does not answer

The checker reads. It does not create a task, register a relationship, emit, deliver, record a
verdict, or start or stop anything, so nothing in its document is evidence that a delivery
happened. That is not the same as saying it writes nothing: every relay command opens the store on
construction. The honest claim is that it composes only `doctor`, `service status`,
`assignment-find`, `criteria-show` and `settings-show`, that none of those registers, emits,
delivers, records a verdict or changes service state, and that `doctor` runs before anything that
could construct a store.

| Stand-in | What it replaces | What a reading through it cannot prove |
| -- | -- | -- |
| the captured lifecycle response | a lifecycle read this process made | that the host would answer the same way at the moment of dispatch. It carries its own time, and it goes stale |
| the captured creation receipt | the host's own echo, read live | that a provider served the model. Only that the host recorded the request |
| the supervisor's witness | a witness at the process boundary | that the pid inside it is the process that wrote it. That witness is CRW-102's, and this reading is that something naming the pid advanced the file |
| the runtime host record | a trusted inventory of what is installed | the provenance of the launcher's bytes. It is a private file the same operator writes, so the launcher agrees with the installed-runtime record rather than being proven to be the relay |
| the declared window bounds | times taken from the store | that the window is where the operator says it is, unless a corroborating store time was supplied. The report says which of the two it had |

A preflight that passed says the trial may start. It says nothing about whether the trial will
succeed, and a trial that succeeds afterwards does not retroactively make an unread precondition
read.
