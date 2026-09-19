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
creation receipt echoed, so those two arrive as captures the operator recorded elsewhere. The
lifecycle capture is per participant rather than per parent: a child created without a standby turn
cannot be looked up either, and a correction is delivered to it. A capture
is the caller's claim about a read this process did not make, it carries the time it was taken, and
it is stale past the record's own bound, which may not exceed fifteen minutes. Every capture is
aged again at the end of the run, because one fresh when it was read can expire while the readings
after it are being taken and readiness is published after all of them.

| Reading | Answered by | Met when | Never established by |
| -- | -- | -- | -- |
| `processPersistence` | `os.kill(pid, 0)` and `os.getsid`, the supervisor's witness file, and `service status` where the supervisor is the relay's own service | the process is alive at two observations, sits outside the caller's session, has been running at least the declared minimum by its own start time where the host reports one, and by the record's declaration where it does not, the cell saying which it read, since a restarted supervisor keeps the old declaration, and its witness names that pid and advances its counter between them; where a service is declared, `ownership` is `ours`, the lock is held, `staleRecord` is false, and the pid and store id agree | that a launch command returned, or that a pid appears in a record. A recorded pid with a free lock is what `staleRecord` is for |
| `lifecycle` | a captured host response per participant | the capture names that task, carries no error, and resolves a thread status. That is all it establishes: whether the recipient is archived, paused, usage-limited or able to accept input is what the relay reads for itself at delivery, and this reading does not stand in for it. The host answers that status as a structured object rather than a word, so either shape resolves it: a word, or that object carrying its state at `type`, which is where the relay's own adapter reads it. No particular state is required, only that one is there | a creation receipt. `thread not found`, `missing source rollout` and `no rollout found` are the three answers that fail it by name, read from the fields a host puts a refusal in rather than from anywhere in the payload, so a goal or a title describing one of them is not one. A null goal is not one of them: a healthy task without a goal has one. A `status` no state can be read from is unreadable rather than resolved, and the start is refused. JSON `false`, `0`, a null and a list are not statuses at all, and `str()` turns the first two into nonempty words; an object with keys but no state at `type`, such as `{"unexpected": true}`, is not one either. Reading any of them as an answer resolves a participant the host never resolved, which is the missing-rollout condition this reading exists to catch |
| `capability` | the captured creation receipt, and `settings-show` for the same task | each payload names the task it is about, the receipt's `settings.actual` equals every setting the record declares, its `findings` are empty, and the store's own settings record is usable, complete and carries the same values, including the workspace the record states for that participant, which the store holds per task and delivery reads | that a provider served that model. A matching echo says the host recorded the request. `usable` alone is completeness, not agreement |
| `storeIdentity` | `doctor` with `--expect-store`, `--expect-inode` and `--expect-nonce`, and each peer's captured `doctor` | `doctor` reaches the App Server socket, the relay's own `sameStore` reports `proven` here, and every declared participant has a captured `doctor` of its own reporting `proven` about this trial's challenge, with its own store identity agreeing with the record. A verdict speaks only for the nonce it was given, so a peer asked about an older challenge is not this trial's proof. A participant with no capture is unknown, never absent from the count | an equal path string, or an agreeing store id and inode. The relay grades those as `unproven` on their own: proof takes a nonce another participant wrote, found beside an agreeing device and inode |
| `boundaries` | the record's declaration, each boundary's captured registration receipt, `git rev-parse --show-toplevel` in each declared directory, and `git rev-parse --git-common-dir` in each declared root | there are at least two boundaries whose issue key, scope reference, resolved repository root and participants are all pairwise distinct, because two boundaries sharing a participant are not two parents, no two of them are in one repository, which `--git-common-dir` settles because it resolves to a single directory for every linked worktree of one repository, each receipt names that boundary's issue, an active status, its scope reference, both endpoints' tasks and workspaces, and both endpoints among the recipients it authorises, and for the assignment being dispatched its relationship and generation as well, and each directory is the repository it was declared to be | a display name, a title or a working directory, none of which identifies anything ([OPS-7.2](../plugins/crw/skills/crw-run/references/operations.md)). Distinct roots are not distinct repositories, and neither are distinct `--show-toplevel` answers: linked worktrees of one repository have both. Run the two boundaries as worktrees of one checkout and a reading of the roots calls one repository two, which is the ordinary arrangement on a host that keeps its work in linked worktrees rather than an edge of it |
| `assignmentState` | `assignment-find --issue` and `criteria-show` | the responsible relationship is the one the record names, its entry carries the same parent task, child task and execution generation with status `active`, and the criteria set matches by digest, source reference and count. The parent is compared because a relationship under another parent delivers to that parent, whatever the trial intended | a relationship id written in a file. A non-empty criteria set is not the intended one |

Every fact a reading later compares a payload against is required in the record itself, before any
command runs. Two absent values compare equal to each other, so a record that stated nothing would
have agreed with a payload that carried nothing, and the run would have reported a precondition
nobody established.

Every boundary declares exactly one parent and exactly one child, the one being dispatched and the others alike. The assignment being dispatched belongs to one of the declared boundaries, and its parent and child are that boundary's own; otherwise the readings would cover one set of tasks while the dispatch went to another. A captured registration is compared as a whole registration, because one agreeing on a scope and two task names while carrying another issue, relationship, generation or an archived status is a stale registration whose authorised roots belong to something else.

A reading that could not be taken answers `unknown` and refuses the start. It never answers false,
and it never takes the value of the reading beside it. The distinction is the whole point: a
missing answer and a wrong answer call for different actions, and a start that proceeded on an
unknown is how two of the three failures above were discovered after the fact rather than before.

The store readings have an ordering rule of their own. Every relay command opens the store on
construction, creating the directory and an empty database when they are absent, so a mistyped
path produces a silent empty store rather than an error ([OPS-3.4](../plugins/crw/skills/crw-run/references/operations.md)).
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

**Preflight last, immediately before the dispatch.** The record declares the window this dispatch opens, and
it has to be ahead: a record whose window has already opened is a trial already running, and
starting from it again dispatches a second time into one measured interval. The window is also the one this dispatch opens rather than any window ahead: a record whose window
opens hours later is refused, because the completion and any intervention would happen before the
declared interval and a later ledger would report a window it never measured. It is read again at
the end of the run, because the
readings take real time and a window a few seconds ahead can open while they are being taken. The
ledger still grades that same record afterwards, which is what it is for, and it refuses a line
dated after the moment it is being graded: the ledger is appended as things happen. All six readings are taken by one run at one
moment, because that is the only moment at which all of them are simultaneously true. All three
observed failures were checks made too early, or not made.

**Then dispatch.** The window opens here.

### What the order gate compares, and what it cannot promise

    the assignment file      the store's own answer           the message about to be sent
    relationshipId    ═══    responsibleRelationship    ═══    contains that id
    childTaskId       ═══    childTaskId
    executionGeneration ═    executionGeneration
                             relationshipStatus is active
    artifacts         ═══    the artifacts the record declares
    artifacts         ─────  inside the registration receipt's artifact roots
                                                              contains each artifact path

A file written for a previous relationship carries that relationship's id, and that is the
comparison that catches it. The artifacts compared are the file's own: the file is what the child
reads, so a missing or empty list there is a mismatch rather than an occasion to fall back on what
the record intended. Every artifact path is absolute and normalised, and its containment in an authorised root is
decided on the path rather than on the place it resolves to, because that is the contract the
relay's own manifest enforces: it compares normalised paths without following links and then opens
every component refusing to follow one, so an absolute symlink outside a root whose target lands
inside it is outside the root, and an artifact under a symlinked directory is refused at emit however
its string reads, which the gate checks by looking at the components that exist. A NUL byte is
refused for the same reason the relay refuses it. Private trial records are the opposite case and keep the resolved
comparison, where following the link is exactly the escape worth catching: a relative one is resolved against whichever directory a reader happens to be in, and an
unnormalised, trailing-slashed or tilde-bearing one is refused at emit, so a gate that accepted it
would report a dispatch as ready that the relay then refuses. The message is prose, and the identities inside it are
named as words of their own: each appears delimited by whitespace, nothing else. A POSIX filename
may hold any byte but a separator and a NUL, so no rule could tell a trailing bracket or full stop
that belongs to the sentence from one that belongs to the path, and two attempts proved it: a
substring test read `/repo/artifact.py.bak` as naming `/repo/artifact.py`, and stripping punctuation
afterwards read `/repo/file!` as naming `/repo/file` while refusing a real artifact ending in a bracket.
The requirement therefore sits on the message, where an operator can meet it exactly, and an
artifact path holding whitespace is refused: the relay would accept one, but no message can name it
as a word. The artifact roots come from the captured registration receipt, because
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

The window is one named interval, so its open and its close must name the same segment, and no preparation segment may overlap it: preparation still running inside the measured window is preparation inside it, whether or not anyone wrote an intervention down. The window must have a duration: an open and a close at the same instant hold none of the completion, delivery, verdict and correction a trial exists to measure, and are clean of interventions by construction. It must also have closed: a window whose close is still ahead has not yet had the interventions it would need to be clean of. A segment is bounded by its own start and end, which are times rather than positions in the file:
segments are built as intervals and two that intersect are a refusal, because a pairing taken from
line order accepted two overlapping segments and counted one intervention inside both. `segment_end`
carries `failed` or `succeeded`.
The window is bounded by `window_open` and `window_close`, which must be the same window the record declares;
a ledger naming a different one is a refusal, because a moved boundary moves interventions out of
the measured window. Every segment must close, and close saying `failed` or `succeeded`: a segment left
open is an attempt whose outcome was never recorded, and it is refused rather than reported. Classification is by timestamp and by
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
start record, the captures, the ledger and the result. The shared state directory the relay probes
reach is held to the same rule, because operational state never lives inside a repository and the
first command to open one there would construct it. The assignment file is the one in the owning child's own workspace, because the file under test is
the file that child reads, and a matching copy anywhere else is a different file. It and the dispatch
message sit where that workspace is, outside this repository. Paths are compared after symbolic links are followed, so a
link out of the trial root is the escape it looks like rather than a spelling that passes, and no
private record may sit in a repository nested under the root either: the rule is that operational
state is in no repository, not that the root itself is in none.

The reason is not tidiness. Operational state never lives inside a repository
([OPS-3.2](../plugins/crw/skills/crw-run/references/operations.md)), a committed host identifier is a private
receipt in a public repository, and a synthetic harness that acquired one would stop being
synthetic. This document carries placeholders for the same reason.

## Running it

    python3 scripts/trial_startup.py preflight --start <trial-root>/start.json
    python3 scripts/trial_startup.py ledger    --start <trial-root>/start.json

The first is run once, immediately before the dispatch that opens the window. The second is run
after the window has closed, which is what it needs in order to grade anything. Both print one JSON object on standard
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
      "store": {"storeId": "<from store-identity>", "device": 64512, "inode": 4242,
                "challengeNonce": "<written by a participant during preparation>"},
      "supervisor": {"pid": 12345, "witness": "<trial root>/supervisor.jsonl",
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
                                                   "sandbox": "<mode>", "approvalPolicy": "<policy>"}},
                                       {"role": "child", "taskId": "<task>", "cwd": "<directory>",
                                        "expect": {"model": "<model>", "reasoningEffort": "<effort>",
                                                   "sandbox": "<mode>", "approvalPolicy": "<policy>"}}]},
                     {"name": "B", "issueKey": "<another issue>",
                      "scopeRef": "<another scope reference>",
                      "repositoryRoot": "<another repository>",
                      "participants": [{"role": "parent", "taskId": "<task>", "cwd": "<directory>",
                                        "expect": {"model": "<model>", "reasoningEffort": "<effort>",
                                                   "sandbox": "<mode>", "approvalPolicy": "<policy>"}},
                                       {"role": "child", "taskId": "<task>", "cwd": "<directory>",
                                        "expect": {"model": "<model>", "reasoningEffort": "<effort>",
                                                   "sandbox": "<mode>", "approvalPolicy": "<policy>"}}]}],
      "captures": {"parentLifecycle": {
                       "<parent A>": {"path": "<trial root>/lifecycle-<parent A>.json",
                                      "capturedAt": "<ISO-8601 UTC>"},
                       "<child A>": {"path": "<trial root>/lifecycle-<child A>.json",
                                     "capturedAt": "<ISO-8601 UTC>"},
                       "<parent B>": {"path": "<trial root>/lifecycle-<parent B>.json",
                                      "capturedAt": "<ISO-8601 UTC>"},
                       "<child B>": {"path": "<trial root>/lifecycle-<child B>.json",
                                     "capturedAt": "<ISO-8601 UTC>"}},
                   "creationReceipt": {
                       "<parent A>": {"path": "<trial root>/receipt-<parent A>.json",
                                      "capturedAt": "<ISO-8601 UTC>"},
                       "<child A>": {"path": "<trial root>/receipt-<child A>.json",
                                     "capturedAt": "<ISO-8601 UTC>"},
                       "<parent B>": {"path": "<trial root>/receipt-<parent B>.json",
                                      "capturedAt": "<ISO-8601 UTC>"},
                       "<child B>": {"path": "<trial root>/receipt-<child B>.json",
                                     "capturedAt": "<ISO-8601 UTC>"}},
                   "registration": {"A": {"path": "<trial root>/register-A.json",
                                          "capturedAt": "<ISO-8601 UTC>"},
                                    "B": {"path": "<trial root>/register-B.json",
                                          "capturedAt": "<ISO-8601 UTC>"}},
                   "peerDoctor": {
                       "<parent A>": {"path": "<trial root>/doctor-<parent A>.json",
                                      "capturedAt": "<ISO-8601 UTC>"},
                       "<child A>": {"path": "<trial root>/doctor-<child A>.json",
                                     "capturedAt": "<ISO-8601 UTC>"},
                       "<parent B>": {"path": "<trial root>/doctor-<parent B>.json",
                                      "capturedAt": "<ISO-8601 UTC>"},
                       "<child B>": {"path": "<trial root>/doctor-<child B>.json",
                                     "capturedAt": "<ISO-8601 UTC>"}}},
      "captureMaxAgeSeconds": 600,
      "window": {"opensAt": "<ISO-8601 UTC>", "closesAt": "<ISO-8601 UTC>"}
    }

The record names parameters, never command lines. The checker composes every command it runs, and
every relay probe carries both the state flag and the state environment variable, because the flag
moves the store and the variable moves the adapter ledger beside it, and the only programs it can
start are `git` and the relay entry point that the runtime host record
under the state home names through its owned pointer. The record cannot nominate another program:
the host record's location comes from the environment, the declared launcher has to be that pointer
as the host record spells it rather than something that resolves to it, and the pointer is the path
that runs. Its bytes are read again after the last probe, because the pointer is an
atomically movable symlink and moving it is how an update is meant to work: reading it at both ends
reports a move rather than preventing one. A link accepted because it resolved to the pointer could be replaced afterwards, and the
probes would have run whatever replaced it.

The ledger command reads the trial root and the window and nothing else, so it does not ask for the
installation at all: a trial that finished stays gradable after the relay it ran against is upgraded
or removed.

The nonce is not written by the checker. During preparation one participant writes a challenge into
the store and the rest read it back, which is what turns an agreeing store id and inode into proof.

## What this does not answer

The checker reads. It does not create a task, register a relationship, emit, deliver, record a
verdict, or start or stop anything, so nothing in its document is evidence that a delivery
happened. That is not the same as saying nothing is written: this process creates no file of its
own, but every relay command it runs opens the store on construction, and `doctor` measures whether
the state directory is writable by writing a temporary file in it. The honest claim is that it composes only `doctor`, `service status`,
`assignment-find`, `criteria-show` and `settings-show`, that none of those registers, emits,
delivers, records a verdict or changes service state, and that `doctor` runs before anything that
could construct a store.

| Stand-in | What it replaces | What a reading through it cannot prove |
| -- | -- | -- |
| the captured lifecycle response | a lifecycle read this process made | that the host would answer the same way at the moment of dispatch. It carries its own time, and it goes stale |
| the captured creation receipt | the host's own echo, read live | that a provider served the model. Only that the host recorded the request |
| the supervisor's witness | a witness at the process boundary | that the pid inside it is the process that wrote it. That witness is CRW-102's, and this reading is that something naming the pid advanced the file |
| a peer's captured doctor | a reading attributed to the participant that took it | which participant took it. `doctor` does not name the process that ran it, so two peers legitimately produce identical payloads and the report says when they did. Filing a capture under a participant is the operator's attribution |
| the runtime host record | a trusted inventory of what is installed | the provenance of the launcher's bytes. It is a private file the same operator writes, so the launcher agrees with the installed-runtime record rather than being proven to be the relay |
| the declared window bounds | times taken from the store | that the window is where the operator says it is, unless a corroborating store time was supplied. The report says which of the two it had |

A preflight that passed says the trial may start. It says nothing about whether the trial will
succeed, and a trial that succeeds afterwards does not retroactively make an unread precondition
read.
