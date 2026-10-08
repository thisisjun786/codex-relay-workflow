# codex-session-relay

Durable, same-host communication between two independent Codex tasks.

A child task finishes a piece of work and needs its parent to verify it. The two are separate
Codex sessions with separate harnesses, and neither can call the other. This package is the thing
in between: it registers the relationship, identifies the completion, stores it durably, wakes the
parent when the parent can actually receive a message, and records an acknowledgement the parent
cannot produce by echoing what it was sent.

Installation and live activation belong to the host operator. The implementation checks below
and a particular host's installed bytes, running processes and delivery receipts are separate evidence.

## What it is not

Each Codex session owns its workflow, CXC phase state, goal, plan and hooks. The relay never reads
or writes CXC state files and never mutates a peer goal or FSM. It reads the App Server's runtime,
archive and goal status to decide whether the recipient can accept input. Its default durable
store lives outside repositories; caller-selected receipt and artifact locations stay explicit.

## Status

| Component | State | Proof |
|---|---|---|
| identity, manifest, scope | implemented | `internal/relay/store`, `internal/relay/adapter` |
| durable store | implemented | `internal/relay/store` |
| registry, generations, turn admission | implemented | `internal/relay/registry`, `internal/relay/delivery` |
| receipts and outcome classification | implemented | `internal/relay/store` |
| delivery, transport classification, bounds | implemented | `internal/relay/delivery` |
| acknowledgement, verdicts, revision routing | implemented | `internal/relay/delivery` |
| reconciliation and restart recovery | implemented | `internal/relay/delivery` |
| bridge host adapter | implemented | `internal/relay/adapter` |
| bounded daemon | implemented | `internal/relay/daemon` |
| CLI | implemented | `internal/relay/cli`; the corpus's `cli-shape` and `exit-codes` domains |
| frozen-schema conformance | implemented | `internal/relay/store`; the corpus's `records` domain |
| DAG plans: validated, append-only revision log and its additive store zone | implemented | [DAG plans](dag-plans.md); `internal/relay/dag`, `internal/relay/store` |
| DAG scheduler: ready set, edit regions, capacity and pass records; release of a ready node to a Codex child; acceptance, integration, decisions and corrections; merge eligibility, conflict observations and cap basis; the coordinator epoch that fences these writes, restart and adoption | implemented (installed proof is a later issue) | [DAG scheduler](dag-scheduler.md); `internal/relay/dagsched` |
| DAG progress: a read-only query of a plan's stage distribution, cumulative accepted and integrated counts, denominator per revision, a reason per blocked or stale node, and links | implemented, with a rebuild of the view from events, a cursor and a snapshot as a Go API (the Linear summary is the summary outbox, below) | [DAG progress](dag-progress.md); `internal/relay/dagsched` |
| DAG summary outbox: the project-level queue of a plan's Linear summaries (ordered per plan and document, an older summary never over a newer one), drained by the parent with its own connector: claim, write, read back, confirm, retry only that entry. The relay holds no Linear credential | implemented | [DAG summary outbox](dag-outbox.md); `internal/relay/dagsched`, `internal/relay/store` |

Until todo 44 each row's proof was a test of the Python package under
`packages/codex-session-relay/tests`; [the test map](../port/test-map.md) names where each one's
properties went.

## Install

A host runs this relay as the `codex-session-relay` name of the `crw` runtime, one Go binary
built from this repository and installed with `crw install`
([runtime installation](../runtime-install.md)). Its command line, records and store are the ones
this README documents, and the Go implementation is held to them by
[the contract corpus](../../contract/README.md).

This README first documented the Python package that runtime was ported from,
`packages/codex-session-relay`, which the fence release installed as a Python virtual environment
until the cutover. Todo 44 removed that package from the repository with the rest of the Python
execution path; its last source is the parent of the commit that deleted it, and these documents
moved to `docs/relay/` from its `docs/`. JUN-93 exercised a dedicated installed environment of
the Python package against a real App Server: child completion, automatic parent review, a
correction to the same child, busy-recipient deferral, accepted-response loss and recovery
without resending. Host versions, exact revisions, remaining cases and activation/stop procedures
are maintained in the canonical Linear project record. These observations do not activate a
service on another host or establish that an earlier test process is still running.

## State

Runtime state lives outside any repository, in
`$XDG_STATE_HOME/codex-session-relay/<endpoint-hash>/` (mode 0700), holding `relay.sqlite3`,
`daemon.lock`, `daemon.pid` and `daemon.log`. Precedence, highest first: `--state`,
`CODEX_SESSION_RELAY_STATE`, `XDG_STATE_HOME`, then `~/.local/state`.

**Every process must point at the same state directory.** The child emitting, the parent
acknowledging and the daemon delivering share one store; a mismatched `--state` means they simply do
not see each other. The endpoint hash is derived from the socket path, so passing the same
`--socket` is enough. Without `--socket` it is derived from the default App Server socket,
`$CODEX_HOME/app-server-control/app-server-control.sock` (`CODEX_HOME` defaulting to
`~/.codex`), the one the bridge defaults to; a legacy `default` directory is kept only while it
alone holds a store. See [where the state lives](operations.md#where-the-state-lives).

`doctor` reports which rule won, the database it resolved to and the access this process really
has. To prove two participants share one store rather than two copies of one, take
`store-identity` on one side and write a nonce with `store-challenge --write`, then check both
from the other side: `doctor --expect-inode <device>:<inode> --expect-log <device>:<inode>:<name>
--expect-nonce <nonce>`. Each part is necessary. An identifier is copied along with the file; a
nonce is copied too when the copy is taken after the challenge was written; and a device and
inode that agree do not say both processes opened the same pathname for that inode. That last
one is what `--expect-log` answers: SQLite writes the write-ahead log beside the pathname a
connection opened, so two participants share one log when their databases sit under the same
directory entry, and `store-identity` reports that entry for each side to send. A file bind
mount reaches one inode at a second pathname without changing the name count, which is why the
count is not the check. Anything short of all of it is reported `unproven` and exits non-zero
rather than being read as yes. See [operations](operations.md).

## Authorized execution settings

A send carries the settings the recipient task was actually created with. This is not defensive
decoration: on this host a `thread/resume` carrying only `{threadId, excludeTurns}` returned
`sandbox: dangerFullAccess` for a task created with `workspaceWrite` and `networkAccess: false`.
The pinned bridge sends exactly that resume and says so in its own source. So the relay records
what the host reported at creation, carries it on the resume and again on the turn, and withholds
the send when it cannot establish or preserve it. It never accepts a host default and never
invents a narrower or wider profile.

Record them at registration, or later:

    codex-session-relay register ... \
      --parent-settings @/path/to/parent-settings.json \
      --child-settings  @/path/to/child-settings.json

   codex-session-relay settings-record --task <task id> --settings @/path/to/settings.json
   codex-session-relay settings-show   --task <task id>


### The role a task holds

A task bound to a scope holds a role, and its recorded authorization is checked against the pair
this host's execution policy declares for that role. The policy is the bridge's file, read from
THIS process's environment through the bridge's own parser, so the two never drift into two
readings of one document.

Record the role the creation cited alongside the settings, and the operator exception where one
authorized the pair instead of the role policy. Both come from the creation receipt's
`executionPolicy`:

    codex-session-relay register ... --parent-role parent --child-role child
    codex-session-relay settings-record --task <id> --role parent --exception <id>

The cited role is compared with the role the task is actually bound to, by whichever of the two
facts arrives second, and a disagreement is `role_binding_mismatch` — refused rather than
recorded, because the recovery for that is not re-recording. A cited exception is verified against
the same policy file by id, role, pair and directory; one this host does not authorize exempts
nothing.

After a user changes an existing task's model, re-record its authorization from a user-attributed
source before the next send:

    codex-session-relay settings-record --task <id> --source user_transition --settings @file

The record is what a send verifies against, and a value observed on the host is evidence of what
the task is running rather than a new approval, so nothing adopts a drifting setting on its own.
Until it is re-recorded the send is refused as `settings_record_stale_for_role`, before any
transport call. A process that cannot read a role policy refuses role-bound sends as
`role_policy_unconfigured` rather than skipping the check; that hold is retry-safe, so declaring
the policy and restarting resumes the held deliveries with nothing lost.

One further rule applies to a pair the policy did not derive — a supervisor's, whose pair is the
user's own selection, or one admitted by an exception. A resume transmits the recorded settings
and may apply them to a thread the host has to load first, which could restore a value the user
has since changed, so such a pair is never transmitted. The transport resumes that recipient with
nothing requested instead, loaded or not, which loads it under its own state when the host had
unloaded it, and compares what the host reports with the record before any turn: a difference
refuses as `settings_differ_after_load` with nothing started, and the workspace roots, which a
load does not restore, may come back narrower than recorded and never wider. `doctor` reports the policy digest this process resolved; the bridge's own is
reported by `get_capabilities` through its MCP surface and is not readable from here.

`--settings` takes a JSON object inline or `@path` to a file. Every field is required, because a
partial record cannot say what it is preserving:

    {
      "sandbox": {"type": "workspaceWrite", "writableRoots": [],
                  "networkAccess": false, "excludeTmpdirEnvVar": false,
                  "excludeSlashTmp": false},
      "approvalPolicy": "never",
      "cwd": "/abs/path",
      "runtimeWorkspaceRoots": ["/abs/path"],
      "model": "anthropic/claude-opus-5-5",
      "reasoningEffort": "xhigh",
      "environments": [{"environmentId": "local", "cwd": "/abs/path",
                        "runtimeWorkspaceRoots": ["/abs/path"]}]
    }

Every one of these comes straight back from the thread creation response, so the caller that
created the task already holds them. Recording them is reusing that result, not a separate
handshake and not a new approval.

`environments` is required and the distinction matters: `[]` means no environments are selected,
while a resume response of `null` means the server did not expose the selection at all. The
second is unknown, and unknown withholds rather than being read as none.

What happens at send time:

| Situation | Result |
|---|---|
| no record for the recipient | withheld before any transport call, `settings_unavailable` |
| record missing a field | withheld before any transport call, naming the missing fields |
| record whose `cwd`, `model` or `reasoningEffort` is not text | withheld before any transport call, `settings_mistyped`, naming each field and the type it holds |
| record whose `sandbox` is not a policy object, or whose type has no resume mode | withheld before any transport call, `unsupported_sandbox_type`, naming what the row holds |
| resume returns a different sandbox, cwd, roots, model or effort | withheld, `settings_not_preserved`, no turn started |
| resume returns no value for one of them | withheld, `setting_unobservable`, no turn started |
| resume returns `environments: null` | withheld, `environments_unknown` |
| resume returns an approval policy other than `never` or `on-request` | `inbox_only`: stored, not woken |
| resume returns `never` or `on-request` and it differs from the record | the turn begins; the difference is noted on the transport receipt as `approval_policy_differs_from_record` |
| resume returns no approval policy at all | withheld, `setting_unobservable`, still retryable |
| resume matches | the turn begins, carrying no overrides |

Those three fields are the ones `ThreadResumeParams` types as strings, and the resume carries
each recorded value as it stands, so a record holding something else is refused rather than sent
for a host to interpret. `settings-record` and `register` run the same check, so such a record
is refused when it is written too; a store holds one only from an older writer or a hand edit.
`runtimeWorkspaceRoots` and `environments` are not typed there, and that is not a promise that
a wrong shape is caught further on: `runtimeWorkspaceRoots` of `"abc"` passes and reaches the
resume as `["a", "b", "c"]`. What those two hold is a separate question from this one.

The sandbox is checked by a different question, because the record holds the policy OBJECT the
creation result reported while the resume carries only its mode plus the config keys around it. A
row holding anything else — the bare name `"workspaceWrite"`, a list, a number — records no policy
for a resume to restore, and a policy object whose `type` is not a mode name names nothing to
restore either. Both are refused before any send as `unsupported_sandbox_type`, the same answer an
unreadable policy already gets, with the detail naming what the row actually holds. An absent
sandbox is not in this group: `null` or omitted is `settings_incomplete`, because the repair is to
record the field rather than to replace it.

A withheld send is retry-safe in the only sense that matters: nothing was sent, so nothing can be
delivered twice. It is not a permanent hold either, because settings that were never recorded can
be recorded and the next pass decides again.

**Why the turn carries no overrides.** `TurnStartResponse` defines only `turn`, and `Turn`
carries `id`, `items`, `status`, `startedAt`, `completedAt`, `durationMs` and `error`. There is
no settings echo anywhere in the start response, so a setting bound there could never be read
back, and reporting the send accepted off a turn ID alone would be calling an unverifiable
binding a success. The checked resume is the guarantee instead: it establishes that the thread
already is in the authorized state, which makes overrides redundant rather than protective.
Dropping them also removes a side effect, since `TurnStartParams` scopes a model override to
this turn *and subsequent turns*, so delivering one message used to rewrite the thread for every
later turn as well.

That guarantee is about the resume observation, not the dispatch. No host-side exclusivity is
held, so another client could change a thread between the check and the turn.

A sandbox type with no `ThreadResumeParams.sandbox` mode, such as `externalSandbox`, is refused as
`unsupported_sandbox_type` rather than approximated with a mode that means something else.

### The worker's own policy

The policy THIS process resolves and the policy the serving worker resolved are two separate
processes' snapshots. The worker's snapshot is what send-time approval is decided from: it records
the pairs the process that actually delivers a turn will approve. It is not an observation of what
a delivered turn ran on. A daemon worker publishes its own snapshot when it starts, and `doctor`
reads it back:

    codex-session-relay doctor

The report carries `workerPolicy` (the live worker's snapshot, or the exact reason there is none),
`callerWorkerAgreement` (`same`, `different` or `unknown` between this CLI's snapshot and the
worker's), and `rolePolicy` (this CLI's own). Without `--require-worker-policy` these are
diagnostics: doctor exits 0 even when the worker is unobserved or mismatched, because describing is
its job and gating is not.

To make readiness a gate, name the pair each role must run on. The value is a JSON list of
`{role, model, reasoningEffort}` objects, inline or as `@path`, and only fixed parent/child pairs
are supported:

    codex-session-relay doctor \
      --require-worker-policy '[{"role": "parent", "model": "anthropic/claude-opus-5-5", "reasoningEffort": "xhigh"}]'
    codex-session-relay doctor --require-worker-policy @/path/to/requirements.json

doctor then exits 2, carrying the whole diagnosis plus `workerReadiness`, whenever the live
worker's snapshot is missing, stale, a different digest, or does not declare the requested pair.
A configured caller never substitutes for the worker's own snapshot: a worker that published an
unresolved policy is refused as `worker_policy_unconfigured` no matter what the caller resolved.
Malformed JSON or an unreadable `@path` is a usage error (exit 4). A valid JSON document that is
not a list of `{role, model, reasoningEffort}` objects - a wrong type or an empty list - is
refused (exit 2) as `worker_policy_requirements_invalid`, and a role outside the fixed
parent/child pairs above as `worker_policy_role_unsupported`.

This is a point-in-time readiness observation. `managed-start` consumes a fresh observation
before creation and again before business dispatch. The receipt is evidence between cooperating
same-user processes, not authentication; source tests do not establish installed-host behavior.

### Recoverable managed start

Use the managed entry when creating a new issue assignment. Supply a complete, immutable request
and explicit store, socket and marker paths:

    codex-session-relay --state /absolute/relay-state --socket /absolute/app-server.sock \
      managed-start --request @/absolute/request.json --marker-root /absolute/markers

The request schema is `managed-start/1`. Required fields are `schema`, `requestId`, `issueKey`,
`parent`, `child`, `artifactRoots`, `allowedRecipients`, `criteria`, `criteriaSource`,
`baselineRevision`, `scopeRef` and `prompt`; `projectKey` is optional. `parent` carries `taskId`,
`hostId` and `settings`; `child` carries `hostId`, `title` and `settings`. Settings use the
existing full settings record, including environments, approval policy and sandbox. A settings
record may carry `expectedPermissionProfile`: the whole profile object Codex reports for the thread
(an `id` of text, an optional `extends` of null or text, kept as written, and no other key unless
the host adds one, as text, a boolean or null; at most 16 keys and no number or nested value), or
text. Every key and text value must be nonblank, free of NUL and at most 500 characters. The
request is the only authority for it: the creation response's profile is checked against it and never
copied into the child's record. Each
criterion carries its `id`, `title` and boolean `required`. Unknown fields are refused. Both
roles must have explicit permitted pairs, the parent must be an allowed recipient, and this
local entry requires the same host, approval `never`, and existing absolute workspace paths.
It does not select remote environments or approximate an unsupported sandbox.

Here is a request shape for a local workspace. Replace the task/host identities, existing
paths, baseline and issue criteria with observations from your own authorized assignment.
The role pairs below are examples; read your host's declared
[role policy](#the-role-a-task-holds) rather than copying them as defaults.

```json
{
  "schema": "managed-start/1",
  "requestId": "example-42-attempt-1",
  "issueKey": "EXAMPLE-42",
  "parent": {
    "taskId": "observed-parent-task", "hostId": "observed-local-host",
    "settings": {
      "model": "anthropic/claude-opus-5-5", "reasoningEffort": "xhigh",
      "approvalPolicy": "never",
      "sandbox": {"type": "workspaceWrite", "writableRoots": [], "networkAccess": false},
      "cwd": "/workspace/project", "runtimeWorkspaceRoots": ["/workspace/project"],
      "environments": [{"environmentId": "local", "cwd": "/workspace/project",
                        "runtimeWorkspaceRoots": ["/workspace/project"]}]
    }
  },
  "child": {
    "hostId": "observed-local-host", "title": "EXAMPLE-42 · Implement the assigned change",
    "settings": {
      "model": "anthropic/claude-opus-5-5", "reasoningEffort": "xhigh",
      "approvalPolicy": "never",
      "sandbox": {"type": "workspaceWrite", "writableRoots": [], "networkAccess": false},
      "cwd": "/workspace/issue-42", "runtimeWorkspaceRoots": ["/workspace/issue-42"],
      "environments": [{"environmentId": "local", "cwd": "/workspace/issue-42",
                        "runtimeWorkspaceRoots": ["/workspace/issue-42"]}]
    }
  },
  "artifactRoots": ["/workspace/issue-42"],
  "allowedRecipients": ["observed-parent-task"],
  "criteria": [{"id": "c1", "title": "The agreed behavior is verified", "required": true}],
  "criteriaSource": "issue:EXAMPLE-42", "baselineRevision": "observed-revision",
  "scopeRef": "issue:EXAMPLE-42", "prompt": "The complete authorized assignment goes here."
}
```

Identifiers, source/scope references, baseline and prompt are nonblank strings. Roots and
recipients are nonempty string arrays; criteria is a nonempty object array. `projectKey`, when
used, is a nonblank project identifier. `requestId` is at most 128 characters and `prompt` at
most 90,000; the complete JSON is at most 256,000 UTF-8 bytes. The full settings shape is
described under [authorized execution settings](#authorized-execution-settings). A permitted
pair is the exact model and reasoning effort declared for that role by the execution policy;
both the caller and live worker must report the same policy digest.

The name the engine gives the child is `child.title` normalized with the request's `issueKey`:
a title that already starts with the key, followed by the end of the title or a character that
is neither a letter nor a digit, is sent as it is; any other nonempty title is sent as
`<issueKey> · <title>` (middle dot U+00B7, one space each side) while that value is at most 500
bytes, the limit the bridge applies to a create's title, and is otherwise sent unchanged, so the
prefix never makes a request the bridge refuses. A `managed-start` request cannot carry an empty
title, because `child.title` is required and non-blank, and `child.title` is itself at most 500
bytes, the same bound the bridge applies to a create's title: a title of fewer than 500 characters
whose UTF-8 encoding is longer is refused before the request is armed, so the request and the
bridge agree on the limit. The rule applies to the name the host is
given on `thread/start` and again when the engine renames the thread after an adopted standby. It
never rewrites the stored request or its fingerprint, so a repeat of the same request is still the
same replay.

A create's parameters carry the title, so a create that a runtime without this rule recorded
`not_attempted` conflicts when the same request is retried on a runtime with it. The runtime swap
that brings this rule in is made only while no managed request holds a `not_attempted` create.

The entry reserves the issue before asking the bridge to create a standby task. It then binds
the returned task and turn to the marker, registry, criteria and settings before sending the
business prompt. The standby prompt does no implementation work. A missing or mismatching live
worker policy refuses before creation; a later refusal retains the same task for recovery.
A request that names a `projectKey` also needs that project's parent to be bound already, to the
request's `parent` task (`linkage-bind --role parent`). The entry checks this twice before any
host effect: before it arms the reservation, and again right before it creates the task. A project
with no bound parent, or one that another task is the parent of, is refused as
`unregistered_scope` or `foreign_scope`, the same refusal registration gives, with no task
created. The first check leaves a reservation that was never armed. Bind the project and retry the
same request id and it creates its one task; if you release that reservation with
`managed-release` instead, its request id stays dead and the next attempt needs a new one. The
second check, which catches a binding that moved after the reservation was armed, leaves the
request armed and not releasable; retry it with the same request id once the project is bound again.
Three more refusals that registration or the settings record used to give only after the task existed
are given before the host is asked to create it, with the same reason, detail and exit code. They
come directly before the creation, after the readiness, scope and ledger checks that were always made
there: a `parent` task id or an `issueKey` that contains `|` (the field separator, as a plain error
naming `parent_task_id` or `issue_key`; the parent is named first when both do), a child host id that
contains `|` when the request names a `projectKey` (`unregistered_scope`; a request without a project is
not asked, as registration does not ask), and parent settings that differ from the settings already
recorded for that parent (`relationship_conflict`). A request refused this way is armed with no task and not
releasable; a settings refusal clears once the recorded settings agree with the request, and the
same request id then creates its task. A request that
is refused for two reasons answers the one that can be asked first, so a request that also names a
rival owner of its issue, a refusal that needs the created child, answers the settings refusal
instead of `duplicate_scope_owner`.
A request that names a `projectKey` holds that project's lock, shared, from before the readiness and
scope checks until its child is registered: `managed-start-project-<sha256 of the key>.lock` beside
the store. Starts do not hold each other up, and a start of another project uses another file. Every
writer of the project's parent binding takes the same lock exclusively before its transaction and
lets go when it returns: `linkage-bind --role parent`, `linkage-handover --role parent` and
`linkage-supervise`. Such a writer waits for every start inside that span, and a start waits for it
before it asks about the binding, so the binding cannot change between the last scope check, the
creation and the registration (which would refuse a child whose thread already exists). Writers of
other scopes (a child under an issue, a supervisor under an initiative) take no lock, and a writer or
a start of another project waits only for that project's own lock. The lock file is named after the
store's real path, so two spellings of one store (a linked file or directory) share it. The wait is
bounded at 30 seconds: a start or a writer that waits that
long answers the retryable `LockWaitExpired` host error (the host envelope, exit 3, detail
`LockWaitExpired: the project binding lock was not acquired within 30s; retry` for a writer) having
changed nothing; a start stays armed and the same request id continues it. That is the one new ending
of the three linkage commands; their refusals, exit codes and other output are unchanged. A retry
that already has the creation answer takes no lock; if the binding moved after a crash between the
creation and the registration, registration refuses as before.
Registration adds that retained child's ID to the declared recipients so the parent can
return revision requests to its own child. Replay and pre-start checks verify this derived
list; it does not authorize messages to an unrelated task.
The internal bridge uses the caller's snapshotted execution policy. Managed admission pins
its operation ledger to the explicit `--state` directory, independently of environment defaults.
The ledger's resolved path, device and inode are part of the request fingerprint and are
rechecked before host mutations. Replacing that file refuses recovery rather than creating
another child from an empty ledger.

Retry the **same request with the same paths and contents**. A fingerprint mismatch refuses
rather than rewriting the assignment; an uncertain delivery is reconciled against the
bridge's retained operation, never retried under a new identity, and an uncertain creation is
reconciled by observing the App Server under the same request (below). A running, missing or unknown
standby returns `incomplete` / `standby_incomplete`. When the recorded standby ends `interrupted`
or `failed`, a repeat may proceed to the business turn once the host is ready for direct input
(idle or not loaded, unarchived, with no paused or limited goal). It does not send another standby:
the standby forbids implementation, the business assignment is self-contained, and the original
`standbyTurnId` remains the registration and generation anchor. A known host hold returns its reason
without consuming the business operation; host read errors propagate. The worker policy and final
business guard still apply. A `completed` standby follows the existing path. This command does not
install a retry scheduler.
The daemon identifies the original standby by the attached managed request's child, generation,
dispatch request and turn. It excludes that inert anchor from polling and rechecks the association
under the settlement writer lock if another pending path or stale selection reaches it. It creates
no failure event or settlement for the standby, before admission, while business runs or afterwards;
it does not rewrite the terminal status. Ordinary/revision anchors and actual business failures
remain observed. This does not retract an observation a previous runtime already delivered.
`admitted` means the business turn was dispatched, not that the child claimed it, that its hook
fired, or that its issue passed review. Those remain separately observed facts.
If naming failed after a verified task was created and the bridge recorded that no first turn
was attempted, retry resumes that same task with a separately retained standby operation.
A creation whose outcome is unknown (the bridge receipt is `outcome_unknown`, or
`in_progress_or_unknown` when the process stopped inside it) is reconciled by the next retry of the same
request, which observes the App Server before it answers. A thread that exists with no turn is continued
under the same request (the standby turn under a standby operation of its own, then the title), after taking the project's lock and
asking the scope decision again, as a creation does. When no thread
is shown, the retry waits 2 minutes from the receipt's last update (`pending`, with `repeatAfter`) and then
creates again under the same request with a derived bridge operation id, at most three creations in all. A thread that has a turn,
a creation whose `turn/start` may have been sent, several threads that fit, an unobservable listing or another host error,
a non-abandonable standby refusal, or a creation with no recorded time stop with `creation_unknown` and a `creationReconciliation` object that says why (`state`,
`detail`, `attempt`, `attemptRequestId`, `thread`, `repeatAfter`); no replacement request is ever the answer.
dag-scheduler.md ([A creation whose outcome is unknown](dag-scheduler.md#a-creation-whose-outcome-is-unknown))
gives the table and what it does not establish. A named thread the host no longer knows or cannot serve, or an unloaded thread whose
resume was refused before a turn or verified resume, is abandoned and replaced under the same request. A transient resume error
can leave one usable orphan with no turn or writer. Earlier creation and recovery receipt IDs are excluded from later scans; UUIDv7
IDs outside the creation window (with one minute of clock slack) are not read.
An already armed profile-free request uses the child role's declared default during this managed start without changing its frozen
bytes or recorded settings. NEW and merely reserved requests still require an explicit profile. Unrelated later sends keep the
legacy record's absence of a profile; a role without a default cannot recover this way.
The original failed creation receipt is preserved. An unknown or attempted first turn does not
qualify for this recovery; its effects still need reconciliation.

    codex-session-relay --state /absolute/relay-state managed-show --request-id <id>

The retained request exposes its stage, fingerprint, revision and known identities, alongside
the last admission observation. `managed-release --request-id <id> --fingerprint <hash>
--revision <revision> --reason <reason>` releases only a reservation that has never been armed
for creation. Its tombstone prevents reuse. An armed or attached request cannot be released by
timeout or by assuming a missing response meant nothing happened.

In `managed-show` JSON, read `request.request_fingerprint`, `request.revision` and
`request.state`; only `state: "reserved"` is releasable. A released request stays dead: a later
authorized attempt uses a new request id after checking that the issue has no other owner.
`lastObservation` carries the last `state`, `stage`, `reason` and known dispatch identities.
An absent or unreadable store is reported through `readable`/`detail`, not as proof of absence.

A completed admission also carries `selectors` (`state`, `markerRoot`, `workspace`) derived from the
request identity. `reportingArgv` is present only when both `businessTurnId` and `childTaskId` are
known. It is a list of arguments for a later manual `reporting-show`, with the global `--state`
before the subcommand. An unknown turn or child omits that list; the result does not invent one.
Running it does not wake a parent, write a queue, or publish a new report.

| Observation | Recovery |
| --- | --- |
| `incomplete` / `standby_incomplete` | Retry the same request after a known end: `completed`, or `interrupted`/`failed` when the host is ready. Missing, unknown and running turns stay held; no second standby is sent. |
| worker policy absent or mismatched | Restore the declared serving policy, verify its reading, retry the same request. |
| paused/archived recipient, changed settings/scope/criteria | Preserve the hold; obtain the owning user's supported transition before retrying. |
| creation outcome unknown | Retry the same request: it observes the App Server and continues, creates again after the grace period, or stops with `creationReconciliation` saying why. Do not start a replacement request. |
| business outcome unknown | Inspect the retained bridge operation; keep the reservation and do not create a replacement. |
| request fingerprint conflict | Recover the original input and selectors; never overwrite them to force a replay. |

Before the business turn, authorization is checked again after resume. A known paused, archived,
busy or unreadable recipient is withheld. An external UI change can still race the final host
read and `turn/start`: the host offers no atomic conditional start. Raw bridge calls are outside
this managed admission boundary. Malformed JSON requests exit 4; missing CLI arguments are a
usage error, exit 2 on stderr. Refused or incomplete admission
exits 2; admitted requests exit 0. Transport failures retain the existing host-error behavior.

### Admission identity after an update

Explicit continuation admissions now retain the bound anchor of their exact generation.
The writer refuses an unknown or unbound generation inside the same transaction, so a typo
cannot become valid later merely because that generation opens. The daemon, receipt admission
and reporting reader share the same anchor-binding predicate.

Older `explicit_admission` rows remain stored but do not establish that binding. Do not infer
it from timestamps or bulk-upgrade them. After confirming the original assignment, its owner
can replay the same managed-start request or issue a supported `admit-turn` for the exact
relationship, generation and turn. This fresh admission upgrades that row without creating a
child, completion event or terminal settlement. Existing final receipts and acknowledgements
are retained. Until recovery, legacy continuations remain unmeasured; this is an explicit
compatibility boundary, not automatic migration.

Admission history is read for pending turns only. Each tick one query finds the admitted turns of the active
relationships that have no settlement, and the observation pass reads those (CRW-258). Settled and foreign rows
are never candidates, so a large shared store no longer slows observation: there is no raw rowid page and no
cursor to walk. Admission writers preserve existing rowids and never delete
rows. Historical terminal turns remain observable, but opening a new generation
supersedes all prior outcomes, including failures; observing an old failure does
not send it as a current-generation report or establish acceptance.

### Direct parent interventions

A parent can send the child a message the relay does not carry: over the thread bridge, outside `decision-reply` and outside a ruling's correction message. The relay cannot see such a message. It learns of it only when a turn is admitted for it, and an admission by the relationship's registered parent is then recorded as a **direct parent intervention**, in the transaction of the admission:

    codex-session-relay admit-turn --relationship <id> --generation <n> --turn <turn that received the message> --actor <the parent's task id> [--reason <why>]
    codex-session-relay intervention-show --relationship <id>

The record is a journal row of the kind `direct_parent_intervention` (subject the relationship id) naming the relationship, the generation, the turn, the anchor turn of that generation, the actor and the reason. `intervention-show` reads them back oldest first, each as `kind`, `generation`, `turn`, `anchorTurnId`, `actor`, `reason` (null when none was given) and `recordedAt`, beside the relationship's current generation and registered parent; an unknown relationship is refused `unregistered_relationship`. The same statement again (the same generation, turn, anchor, actor and reason) is not recorded twice, and the same turn with another reason is another statement.

What this does and does not say. `admit-turn` answers what it always answered and still stores its admission and its `turn_admitted` journal row, whoever the actor is; only an actor equal to the relationship's registered parent task is also recorded as intervening, so an operator's confirmation of a continuation, or the child's own claim, is an admission and no intervention. The relay does not authenticate `--actor` (as for a continuation claim): the record is the statement of whoever ran the command. And a message that is never admitted stays invisible to the relay, which is why the route a parent should use for a decision is `decision-reply`, and why this record is the fallback's trace and not a gate: nothing is refused for a message sent outside the route.

### Exact-turn reporting observation

`reporting-show` is a direct, manual, offline diagnosis of one already selected turn. It reads the
marker and the named store and prints JSON. It does not open a socket, call the host, wake a
parent, write a queue, or publish a report. Global `--state` is required and comes before the
subcommand, the same shape as `reportingArgv`:

    codex-session-relay --state /absolute/relay-state \
      reporting-show --marker-root /absolute/markers --workspace /absolute/workspace \
      --assignment <assignment> --session <session> --turn <turn>

The answer uses schema `reporting-observation/1`. `reportingState` may be `unreported`,
`reported`, `in_progress`, `unmanaged` (no selected marker), or `unmeasured`. A completed diagnosis, including `unmeasured`, exits 0.
Malformed identity arguments exit 4. Missing required flags are a usage error: exit 2 on stderr.
An absent store stays absent and is reported as missing evidence, not as a created database and
not as proof that nothing happened.

A Stop observation by itself is not a terminal assignment. `stopObservation` keeps that warning;
`terminalObservation` comes only from a persisted settlement for the same relationship, child and
turn. When that settlement is missing, the state is `unmeasured` with reason
`host_terminal_unobserved`. Evidence that cannot be read, or that conflicts, stays `unmeasured`
with its reason; it is not rewritten as success or absence. `relationshipStatus` is the stored
status, including `paused` or `cancelled`, and does not wake the owner.

This section describes the source command. An installed relay exposes `reporting-show` only after
that version is installed and measured. Source tests do not establish installed-host behavior.

`reporting-derive --relationship <id> [--turn <turn>] [--grace <seconds>]` gives the same reading
from the store alone, through the same predicate (`omitted.classify`): the relay's settlement and
admission, and the claim and turn declarations the child's own relay recorded in the store when
it ran `intent-claim` and `intent-disposition`. It is what the daemon's supervisor pass stages an
omission from. A child that claimed without that record is `unmeasured` /
`declarations_not_recorded`, and nothing is derived for it. Both readings carry `owed` beside
the diagnosis: false when the turn's own final receipt exists, when a later turn was admitted, or
inside the grace. See [the supervisor channel](supervisor-channel.md#an-omission-this-store-derives).

## Commands

Global options come BEFORE the subcommand:

    codex-session-relay [--state DIR] [--socket PATH] <command> [options]

| Command | Purpose |
|---|---|
| `register` | register a parent/child relationship with its authorized scope |
| `managed-start` | reserve, create a standby, register and dispatch one recoverable assignment |
| `managed-show` / `managed-release` | inspect a retained start; release only an unarmed reservation |
| `reporting-show` | offline exact-turn reporting diagnosis; reads, never wakes or writes a report |
| `reporting-derive` | the same reading from the store alone, the one the daemon stages an omission from |
| `register --project` | the same, and the issue's whole lower level in one transaction |
| `settings-record` / `settings-show` | record and inspect a task's authorized execution settings |
| `generation-open` / `generation-bind` | open a generation; bind its anchor to an exact dispatch turn |
| `admit-turn` | record an owner-confirmed continuation turn out of band; by the relationship's registered parent it is also a [direct parent intervention](#direct-parent-interventions) |
| `intervention-show` | the direct parent interventions recorded for a relationship, oldest first; a read |
| `relationship-status` / `relationship-resume` | pause, cancel, archive; resume only by restating generation and scope |
| `relationship-close-merged` | archive the live assignments that are merged with nothing owed; a dry run without `--apply` |
| `child-cleanup` | after the merge and the integration observation, archive the finished child and its loaded sub-threads on the App Server so their MCP helpers stop; refuses unless a merged mark counts and nothing can still be sent to the child; needs `--socket`, `--dry-run` plans |
| `linkage-supervise` | an initiative supervisor over a project parent, by execution or by reference |
| `linkage-bind` | claim one scope for one task at one level |
| `linkage-attach` | bind an existing assignment's issue to its project |
| `linkage-peer` | join two project parents, symmetrically and outside the hierarchy |
| `linkage-outstanding` | exactly the unfinished work a replacement owner must acknowledge |
| `linkage-handover` | replace a scope's owner, only by restating the owner and that work; merged assignments with nothing owed are closed by it |
| `linkage-directive` / `linkage-settle` | record an instruction by digest and origin, placed by its `--purpose` so one competing for a place already held is refused where it is recorded; settle one without erasing the other |
| `linkage-up` / `linkage-down` | walk the hierarchy either way, with its gaps and contention |
| `linkage-counterpart` | who a message is really addressing, and every problem with the reference |
| `emit` | emit a completion receipt over real artifacts |
| `deliver` | attempt eligible deliveries once |
| `reconcile` / `recover` | reconcile one attempt; recover everything after a restart |
| `claim` | duplicate-safe verification claim, so one event is verified once |
| `criteria-register` / `criteria-show` | the canonical criteria a delivery is judged against |
| `revision-head` | which revision this generation currently stands on, and why |
| `ack-proof` | compute the proof from your OWN turn id |
| `ack` | record the parent's acknowledgement |
| `verify-acks` | complete acknowledgements authored without a host |
| `verdict` | record a verdict; needs_changes routes a revision to the same child |
| `decision-reply` / `decision-show` | the parent's decision on a child's blocked_needs_input receipt, or an `answer` on a receipt the relay itself observed ending interrupted or failed, delivered to the child; read back |
| `assignment-show` / `assignment-find` / `assignment-mark` | one issue, one child, and where it stands |
| `sync-*` | the coordination-document outbox: target, next, claim, operation, reconcile, complete, fail, retry, status, progress |
| `status` | observable delivery, acknowledgement and verification state |
| `show` | the full record for one event: receipt, manifest, attempts, sent bytes, verdict |
| `daemon` | run the bounded reconciliation and delivery loop |
| `doctor` | environment and capability check, read-only by default (`--probe-write` measures writability by writing); optional `--require-worker-policy` readiness gate |

Every command prints JSON. Exit 0 success, 2 a refusal with a machine-readable `reason`, 3 a host
problem, 4 usage.

## The merge lane: landing a bundle

This is the in-flight pull-request lane. It serves the pull requests that were already open when the push-only procedure takes effect, and it stays until the push-only path of CRW-965 is installed; the repository's rule is in [In-flight pull requests (transition)](https://github.com/thisisjun786/codex-relay-workflow/blob/dev/plugins/crw/skills/crw-run/references/merge-readiness.md#in-flight-pull-requests-transition).

The merge lane's own commands are `merge-turn-*` (one candidate at a time) and, since CRW-768,
`merge-train-*` (a bundle of several verified candidates landing as one). A bundle exists because a
strict lane that merges members one by one needs a green `dev-gate` on every prefix tree, so k
members cost k CI runs and the runner limit caps the count; a bundle's one pull request gets **one**
full CI run on its single tree and lands as **one merge commit**, whatever the size. The parent
procedure is in the crw-run skill's
[merge-readiness](https://github.com/thisisjun786/codex-relay-workflow/blob/dev/plugins/crw/skills/crw-run/references/merge-readiness.md#merge-a-bundle).

| Command | Purpose |
|---|---|
| `merge-train-open` | the leader's holding turn opens a train over the members in the given order; one member is today's lane and opens no train |
| `merge-train-verify` | the leader reads the bundle pull request, this repository's `ci.yml` run, the first-parent chain and the head's own `.github/workflows/ci.yml` job set in the given checkout, and appends a verified event |
| `merge-train-land` | record that the bundle landed as one merge commit M and close every member turn landed; a member whose turn left the lane after the train opened is named in the landed event instead |
| `merge-train-close` | close the train done, or abandon it; abandoning moves no member turn (the leader still holds the lane turn, the other members still wait) |
| `merge-train-show` | the train's members in order, its event log, the state its newest event derives, and a reconcile reading of a lost landing |

The relay reads the pull request, the run, the jobs, the commits and the ancestry from the forge
itself, and the chain from the given checkout; the caller's values are compared and never trusted.
A bundle that disagrees or is out of order is `disposition_conflict` and a forge or git that cannot
answer is `merge_target_unreadable`, and a refusal writes no event. The five commands are offline
like the `merge-turn-*` ones.

The steps below are the in-flight lane's steps, for pull requests that were open before CRW-965 is installed. The steps, with the command names:

1. **Choose the members.** Every member must be a verified, accepted candidate: each member's own
   parent runs `dag-accept` on the member pull request's head before the leader opens the bundle, and
   the leader takes members whose `dag-ready` reads `done:accepted` on the head their turn holds.
   `merge-train-open` refuses the rest (a member with no active acceptance, or one whose acceptance
   stands on another head, is `disposition_conflict` naming the member, with no event); a member whose
   acceptance stands on a recorded base-refresh head is accepted. Among those, take the pull requests
   that merge onto the current dev in order without a conflict; related ones (the same package) first,
   and non-overlapping packages may ride together. Leave out a member that needs a base-refresh
   correction. Members may belong to different parents. There is no count cap. The order follows the
   plan's precedence edges.
2. **The leader.** `merge-train-open --turn <the leader's turn> --actor <leader> --base-sha <D>
   --member <pr>...`; then build the bundle branch `refs/heads/crw-train/<train_id>` from D by
   `git merge --no-ff` of each member head in order (no hand resolution); open the one bundle pull
   request through a file scanned by gitleaks and label it `crw-lane`; after its CI finishes,
   `merge-train-verify --train <id> --actor <leader> --bundle-pr <n> --head <H> --run <R> --repo
   <checkout>`; then `gh pr merge <bundle pr> --merge --match-head-commit <H>`; then
   `merge-train-land --train <id> --actor <leader> --landed-sha <M> --observed-base-sha <M>`; then
   check every member pull request the landed event did not exclude shows merged (comment "landed via
   bundle <merge sha>" and close it if not) and leave an excluded one alone; then `merge-train-close
   --train <id> --actor <leader> --state done --reason <...>` and delete the bundle branch.
3. **Each member's parent.** Once the landing is recorded, the member's own parent records
   `assignment-mark` (merged) and `dag-integration-observe` on its relationship — except for a member
   the landed event's `excluded` list names, whose work did not land and which gets neither mark.
4. **Failure handling.** A member touching a failed job's packages is removed and the rest re-bundled
   (the old train abandoned); when no member can be named the bundle is halved with a predecessor and
   its successors kept on the same side; a known flaky test's jobs are rerun once; a set that failed
   twice goes one by one; a removed member rides alone; a train whose base moved outside the lane is
   abandoned and reopened.
5. **The lane script** changes only after the bundle merge is in the runtime. The `plugin.json`
   version line is re-recorded mechanically.



## The normal flow

A child completing work from inside its own live turn:

    codex-session-relay --socket $SOCK emit \
      --relationship rel-... --generation 1 --attempt 1 \
      --outcome ready_for_review \
      --turn-thread <child task id> --turn-id <this turn> --turn-status inProgress \
      --artifact /abs/path/to/deliverable

A child that ran the independent code review adds `--independent-review <file>` to a `ready_for_review`
emit; the file is one JSON object, the `independentReview` item of the receipt contract. The relay
stores it as stated and reads nothing it names ([coordination](coordination.md#an-independent-review-beside-a-restatement)).

The turn status you pass is a claim, not proof. With `--socket` the relay reads the turn from the
host and uses what the host actually reports. **Offline, a readiness claim can only STAGE**: it is
stored and visible, and it becomes deliverable only once an independent observation sees that turn
end normally. A turn that ends failed or interrupted suppresses the claim instead of promoting it.

An emit refuses `unassigned_turn`, writing nothing, in two cases before the receipt is taken. A
`--turn-id` that does not have the Codex id form (36 lowercase hex digits in 8-4-4-4-12 groups) of a
`--turn-thread` that does is refused, with or without `--socket`; a thread that is not a Codex id is
not checked. That form check reads only the arguments, so it runs before emit opens the store: a
refused malformed turn id leaves no `schema_meta.socket_path` recorded, and a later emit with a
different `--socket` is not refused for a socket mismatch. Without `--socket`, and after that check,
emit opens the store and reads the turn read-only through `Adapter.ReadTurn` on the App Server socket
the store recorded (`schema_meta.socket_path`), and refuses the same way when an exhausted listing
does not hold the turn. That read keeps no operations ledger, so neither a refused nor a staged emit
leaves one behind. Everything else stages as it did: a listed turn, a store that records no socket,
and a host that could not be reached or read (a page budget that ran out is not evidence of absence).
With `--socket` only the form check is new; the host read is the one the flag already made.

A child that itself reports `failed` or `interrupted` from its own live turn states how the turn
ended: `--turn-status failed` or `--turn-status interrupted`, which is a claim and makes the receipt
final. `--turn-status` is `inProgress` when it is left out, a turn in progress cannot carry those two
outcomes, and the emit is refused `contradictory_observation`; the refusal's detail names the status
to pass. With `--socket` the relay reads the status from the host and ignores `--turn-status`, so a
live turn reads `inProgress` there: such an emit is made without `--socket`, with `--state` naming the
store this emit used. `blocked_needs_input` takes no `--turn-status` and stays staged.

A `ready_for_review` receipt is also judged on its lineage, inside the transaction that stores it:
the relay reads the generation with the receipt and without it, and refuses `revision_ambiguous`,
writing no event and no lineage row, when the generation reads one head without the receipt and no
single head with it (a second root, a cycle, an unknown predecessor or a disconnected revision).
The refusal's detail names the revision the generation reads now, so the child fixes it in the same
turn by naming that revision with `--supersedes-revision`. A first receipt, a duplicate, and a
generation that already reads no single head are answered as they were; recovering such a generation
is the route the refusal table of [Ruling an event that is already ruled](#ruling-an-event-that-is-already-ruled)
records for a plan node.

A loop spanning several turns completes on a turn that is not the anchor, and says so in the same
call:

    ... --turn-id <later turn> --continues-anchor <dispatch turn> \
        --continuation-actor <child task id> --continuation-reason 'cycle 3 of this execution'

Without that, a non-anchor turn that was not already admitted is refused. Host ordering can corroborate the claim and can
contradict it, but it never admits a turn on its own: ordering is not lineage.

The parent, from inside its own turn:

    codex-session-relay --socket $SOCK claim --event <eventId> --turn <my turn id>
    PROOF=$(codex-session-relay ack-proof --event <eventId> --turn <my turn id> | jq -r .ackProof)
    codex-session-relay --socket $SOCK ack --event <eventId> --ack-turn <my turn id> --ack-proof $PROOF
    codex-session-relay --socket $SOCK verdict --event <eventId> --verdict verified --verdict-turn <my turn id>

`--ack-proof` is required and is never computed during an acknowledgement. The proof is over the
parent's own turn id, which is absent from the delivered message, so quoting the message back
cannot produce it. `claim` is idempotent per event, which is what stops a duplicate delivery causing
a second verification.

## What a verdict is a claim about

A verdict says that one REVISION met one SET OF CRITERIA. Both halves are pinned, and both are
checked inside the transaction that writes the verdict rather than before it.

The revision must be the one the assignment currently stands on. A generation that advanced, a
revision that was superseded, an inactive relationship, or two competing revisions with no stated
supersession all refuse a `verified` or `needs_changes` verdict, each with its own reason. Which
revision is current is read from lineage the child declared with `emit --supersedes-revision`,
never from arrival order: knowing a digest shows acquaintance with a revision, not chronology, so
where the declared graph has a fork, a cycle, an unknown predecessor or a gap, the answer is
ambiguous and completion is withheld.

A receipt the relay suppressed (the turn that staged it ended failed or interrupted) is not a
revision, and the child that emitted it cannot tell after a restart. A revision that names one
is read as naming what that suppressed receipt itself replaced: nothing, so it has no predecessor,
or a revision that still counts, so it replaces that one, through any number of suppressed
receipts in a row. Emit accepts the naming and records it as stated; the head does the reading
(`registry.ReadThrough`, used by both head readers over the one statement that reads the
generation's reviewable receipts, suppressed ones included). The rest of the judgment is
unchanged: a revision of another generation, a revision nobody emitted, and a suppressed receipt
whose own predecessor nobody holds still read `unknown_predecessor`, and revisions left
unconnected read `fork`. One revision is one event (the event id is the generation and the
revision hash), so the same bytes emitted again are a duplicate of the suppressed receipt and
leave nothing that counts.

A `needs_changes` ruling opens a new generation and names the exact result to correct.
That result is the new generation's only permitted predecessor outside its own revisions:
the relay-owned request, ruling and generation must agree on the same relationship and
immediately preceding result. It is a lineage root, never a candidate for the new head.
Opening a generation manually does not grant this link. Unknown predecessors and competing
corrections remain ambiguous; the child can extend a correction with another declared revision.

The criteria must be the set the review was actually made against. `claim` binds the review to the
set in force at that moment, and a managed assignment refuses a verdict that is bound to nothing.
Editing a criterion's text afterwards, even keeping its id, invalidates that review rather than
passing it, and the assignment reports `re_review_needed` instead of `verified`.

There is no parameter that turns any of this off.

## Fixing the head a verified ruling handled

A `verified` ruling may carry the head it handled:

    codex-session-relay --socket $SOCK verdict --event <eventId> --verdict verified --verdict-turn <my turn id> --verified-head <40-hex commit id>

`--verified-head` takes the 40 lowercase hex digits of one commit id and is optional. When it is
given with `--verdict verified`, the ruling writes one `dag_verified_heads` row — the event, the
relationship, the generation the event belongs to, the verdict turn, the head, the recorder and the
time — in the same transaction that writes the ruling, so the two cannot disagree and a failure of
either rolls both back. The recorder is the relationship's registered parent, which is the only task
that can rule the event. This is the head a later `dag-accept` starts its parent-made-refresh proof
from, instead of the head the forge shows at accept time.

The ruling's own answer does not change: the record is read back from `dag_verified_heads`, never
returned, so no output field and no refusal reason is added.

| Case | Answer |
| --- | --- |
| `verified` with `--verified-head H`, first ruling | the ruling is recorded and one row holds `H` |
| `verified` with `--verified-head H` again | the replay of the recorded ruling: nothing is written, and the row still holds `H` |
| `verified` with `--verified-head H` after a `verified` ruling that fixed no head | refused `disposition_conflict`: the head is recorded with the ruling that fixes it, and a replay writes nothing |
| `verified` with `--verified-head H2` when the event already records `H` | refused `disposition_conflict`, and the row still holds `H`: one event keeps the head its verified ruling fixed |
| a verdict other than `verified` with `--verified-head` | refused `disposition_conflict`, and neither a verdict nor a row is written: a verified head is recorded only with a verified ruling |
| `verified` without `--verified-head` | exactly what it was before, and no row |

A re-review ruled `verified` under a re-registered criteria set keeps the head the event already
records; it does not write a second row, because one event has one verified head.

A value that is not 40 lowercase hex digits is a usage error (exit 4), like any other option value
this command refuses.

## Ruling an event that is already ruled

An event has one standing ruling, and a second `verdict` call on it is never answered with a ruling it was not asked for. What the call does depends on the verdict already recorded and the verdict asked:

| Recorded | Asked | Answer |
| --- | --- | --- |
| any verdict | the same verdict | a replay: the recorded record marked `_replay`, and nothing is written |
| `verified` | `needs_changes` | the ruling is replaced and the correction opens, while nothing rests on the verified ruling |
| `verified` | `unverified` or `aborted` | refused `disposition_conflict` |
| `needs_changes`, `unverified` or `aborted` | any other verdict | refused `disposition_conflict` |

No refusal reason is added: `disposition_conflict` already means a ruling that conflicts with the one on record, as in a re-review that is not allowed to replace a verified ruling with `unverified`. A refusal writes nothing, and the ruling on record stays in force. Its text names the recorded ruling, the verdict asked and the route that remains.

**Replacing a verified ruling.** A verified ruling is the parent's own judgement, and until something acts on it, changing it costs only the correction. The route a parent follows to send a candidate back after it ruled verified, for example because the base moved and conflicts before the plan accepted the result, is a `needs_changes` ruling on the same receipt, so that ruling works instead of being answered with the old one. The writer reads these in this order:

1. An open re-review is decided first, exactly as above (a criteria set that moved since the ruling), whether or not the head was accepted.
2. The transition table above.
3. Nothing may rest on the verified ruling: no plan acceptance of the event (`dag_acceptances`, whatever its state), no merged mark of it (`assignment_marks`), and no merge turn of the assignment that is merging, of unknown effect or landed. A turn that only waits for the lane or holds it does not count, because the parent that found the base conflict holds that very turn. The turn is read per assignment and not per head: a merge turn is recorded per assignment (`merge_turns`), while the head an event's verified ruling fixed is the head an acceptance proves against ([Fixing the head a verified ruling handled](#fixing-the-head-a-verified-ruling-handled)), which is a different question from whether a turn of the assignment is merging. A head recorded by a ruling that was later replaced stays as the record of what that ruling handled, so a reader of it checks the event's standing ruling before trusting it.
4. The existing path of a first `needs_changes` ruling: the event is the head of the generation the relationship stands on and the relationship is active (`stale_generation`, `superseded_revision`, `revision_ambiguous`, `relationship_not_active`), the finding marked `needs_changes` carries a note and the criteria set is the one the review is bound to, the child is an allowed recipient, and a declared restoration block can be carried.

When all four hold, the writer replaces the ruling in the transaction that opens the next generation and queues the revision request to the same child, exactly as a first `needs_changes` ruling does. The replaced record stays: the journal records `verdict_superseded` with the replaced record and `reason: ruling_changed` (a re-review's entry has no reason). The answer is the new ruling plus `_supersedes`, the verdict, verdict turn and time of the ruling it replaced; like `_replay` it is an annotation of the answer and is not stored. The summary owed to the coordination document is a new job, because its identity carries the verdict, and it counts the rulings (`ruling 2`).

When a step refuses, the refusal names what to do:

| Cause | Reason | Route |
| --- | --- | --- |
| recorded `verified`, asked `unverified` or `aborted` | `disposition_conflict` | rule `needs_changes`, or stop the assignment by changing the relationship's status |
| recorded `needs_changes` | `disposition_conflict` | the ruling opened a generation and is not withdrawn: rule the head of that generation when the child reports there |
| recorded `unverified` or `aborted` | `disposition_conflict` | the ruling is final for the event: open a fresh execution generation (`generation-open`, then `generation-bind`) and rule what the child reports there |
| a plan accepted the verified event | `disposition_conflict` | read the node's stale reading: when its action is `correct`, `dag-correct --prepare` prints the instruction and the dispatch request id, the generation is opened by hand and `dag-correct` binds it; `revalidate` and `hold` have their own steps; when the accepted result is current, a base that moved after the acceptance is recorded by `dag-base-refresh` (a generation opened by hand that only merges the base, ruled `verified`; the relay proves from git that its head is the accepted head plus merges of the base), and for any other current result this build records no correction route, so report it and open no generation that `dag-correct` will refuse |
| the work is marked merged, or a turn landed it | `disposition_conflict` | a merged result is corrected by new work, not by a second ruling |
| a merge turn is merging or of unknown effect | `disposition_conflict` | resolve the turn (`merge-turn-resolve` reads the branch), then rule again |
| the event is not the head | `stale_generation` or `superseded_revision` | rule the head the assignment shows |
| the head is ambiguous | `revision_ambiguous` | outside a plan, a fresh execution generation; for a plan node whose result is not accepted yet, the generation opened by hand: `dag-correct --prepare` prints the instruction and the dispatch request id, the generation is opened under that id (`generation-open`), the instruction line is sent to the child, the turn that carried it is bound (`generation-bind`) and `dag-correct --manifest-digest` records it as a correction, after which the child emits one receipt in that generation; a plan node whose accepted result is stale has its own route above (read `revision-head` first: a re-emit that named a suppressed receipt of its own generation may read a head) |
| the relationship is not active | `relationship_not_active` | `relationship-resume`, then rule again |

Two limits are part of the contract. The same verdict again is a replay even when its findings differ, so a `needs_changes` ruling that was already given cannot be given again with other words: the verdict does not resend (see [Return corrections to the existing task](../../plugins/crw/skills/crw-run/SKILL.md#return-corrections-to-the-existing-task)). And an installed relay older than this change answers a different verdict with the recorded ruling marked `_replay` and exit 0; read the answer (a ruling that is still `verified` and marked `_replay` changed nothing) and `assignment-show`, never the exit code.

## Replying to a blocked receipt

A child that cannot go on records a `blocked_needs_input` receipt and ends its turn. The receipt carries no artifact (a file attached to it is refused `manifest_forbidden`, and the refusal says to emit the outcome again without `--artifact`, keep the reason in the blocked file and the final message, and send a file that must travel with `ready_for_review`), so it is never the head revision of its generation and a verdict on it is refused `superseded_revision`: the parent's answer has no verdict to travel in. `decision-reply` is the relationship-level route for it. The parent returns a decision, the relay records it, and the delivery engine carries it to the child (held while the child is busy, waking it when it is idle) as a `revision_request` delivery rendered as its own message, `[codex-session-relay] parent decision`. A decision is never a verdict: it writes no ruling, and no ruling writes a decision. A `verified` or `needs_changes` verdict on a blocked receipt is still refused `superseded_revision` once the parent acknowledged it (the receipt is not the head revision), while `aborted` and `unverified` are not refused by the currency check. So the two exclude each other explicitly, each writer checking the other inside its own transaction: a receipt that carries a decision takes no first verdict, and a receipt that already has a verdict takes no decision, each refused `disposition_conflict` (a verdict on an unacknowledged receipt is refused `not_acknowledged` first, as for any verdict).

A second receipt takes a decision, and only one kind of it. A turn the relay itself saw end `interrupted` or `failed` without a receipt is recorded as a final, unsuppressed receipt of the child's generation with producer `daemon_observation` (the daemon's own observation). The child never asked a question, so there is nothing to answer and nothing to rule: the parent's `answer` continues the child in the same generation through the same delivery path, which reaches a `notLoaded` child by resuming it under the MCP profile its record states. `stop`, `split_approval` and `scope_change` are refused `disposition_conflict` for such a receipt, with a reason that says a turn the relay saw end is continued with answer. Every other producer and outcome pair stays refused exactly as before, a child's own `interrupted` or `failed` receipt included.

The daemon does not assert an observation for a turn that already holds its own final, unsuppressed child receipt with a delivery owed to the parent: the parent is told through that receipt's delivery, so the end of the turn is not news, and the turn is settled with no event. A receipt owed no delivery (emitted with `--no-enqueue`, or left behind by a refused enqueue), one a newer revision has replaced, and one that is not final are observed as they were.

    codex-session-relay decision-reply --event <blocked receipt> --decision <kind> --decision-turn <your turn id> --note <text|@file> [--criteria-digest <digest>]
    codex-session-relay decision-show --relationship <id>

| Decision | Generation | What the relay requires | What the child is told |
| --- | --- | --- | --- |
| `answer` | stays | a note; a child's `blocked_needs_input` receipt, or a receipt the relay itself observed ending `interrupted` or `failed` (there it is the only kind accepted) | the answer; continue under the criteria already registered, and for an observed end that the relay saw the turn end and to re-read the worktree, branch, commits and pull request and continue from where the child stopped |
| `stop` | stays | a note, on a child's `blocked_needs_input` receipt | stop work and change nothing more; report `interrupted` |
| `split_approval` | advances to g+1 | a note and `--criteria-digest`, equal to the set registered for the relationship | the criteria changed: here is the set (as registered when the decision was made); continue on the same node under it |
| `scope_change` | advances to g+1 | the same | the same |

The rule is fixed by the kind. `answer` and `stop` change nothing the child's attempt stands on, so generation g goes on. The child's next turn is not the generation's anchor, so the message prints the continuation claim that admits it (`--continues-anchor <anchor of g> --continuation-actor <child> --continuation-reason ...`); the anchor binding is not offered the reply's turn, which would be refused `anchor_already_bound` and reported on every tick. The relay admits that turn itself when the reply is dispatched into it: whether the send settles `dispatched` with the turn id the child's next turn got, or the daemon's reconciliation recovers that turn after a lost response, the same transaction writes the `generation_turns` admission (evidence `explicit_admission_bound:<anchor of g>`, actor `relay`, detail `decision reply <event id>`) and its `turn_admitted` journal row, so the daemon's census reads the continuation turn and observes an end that carries no receipt; it is the relay's own admission of the message it carried, not a parent intervention, and an existing admission of that turn is left as it is. The claim the message prints is still accepted, and is no longer required. `split_approval` and `scope_change` change the criteria the output is judged against, so they open generation g+1 (reason `decision_reply`, dispatch request id `decision-<event id>`) in the same transaction, the reply is an event of g+1, and the turn it opens becomes the anchor of g+1 through the same binding a correction uses (a generation that was bound by hand to another turn first is reported as `anchor_already_bound`, as it is for a correction). The child's first receipt there needs no claim and passes no `--supersedes-revision` (the generation holds no earlier revision to replace). The parent registers the new set first (`criteria-register`) and names its digest, so a reply that names a stale set is refused. The record keeps the set as it was registered when the decision was made, and the message prints it (the first ten criteria, an optional one marked, with a count of the rest), so the child works from what the parent approved even if the set is registered again before the message is sent; the child's output is still judged against the set registered when it is ruled, by the re-review rule. `stop` does not change the relationship status: `relationship-status` still pauses, cancels or archives. Opening g+1 marks the older undelivered deliveries `stale_generation`, the blocked receipt's delivery to the parent among them unless the parent acknowledged it.

**What a decision answers.** A final, unsuppressed receipt of the relationship's current generation, on an active relationship whose child is an allowed recipient, that is the newest final receipt the decision is weighed against (newest in the order the relay saw the receipts: when it first saw each, and for receipts first seen at one instant the order they were stored in, because an event id is a hash and not a sequence): the child's own `blocked_needs_input`, which every kind answers, or a receipt the relay itself observed ending `interrupted` or `failed` (producer `daemon_observation`), which `answer` alone answers and the other kinds are refused for. One decision per receipt: the same decision again (same kind, note and criteria digest) is a replay (the record marked `_replay`, nothing written); any other is refused `disposition_conflict`. The record is the event (outcome `decision_reply`, producer `relay`, id the first 32 hex characters of `sha256(relationship|receipt|decision_reply)`), its delivery, and a `decision_recorded` journal row; it carries `answersOutcome`, the outcome of the receipt it answers (`blocked_needs_input`, `interrupted` or `failed`). A reply that the child has moved past is not sent: a later final receipt of the same generation (the child's own, or the relay's own observation of a later end), later than the receipt that was answered (a receipt staged before the reply and made final after it counts), supersedes it (`superseded_revision`), as a newer generation does (`stale_generation`).

| Cause | Reason |
| --- | --- |
| no such event | `not_claimable` |
| the receipt is neither a final child `blocked_needs_input` nor a final receipt the relay itself observed ending `interrupted` or `failed`, or it is an observed end and the kind is not `answer` (a receipt with an artifact is ruled with a verdict), another decision or any verdict is on record | `disposition_conflict` |
| the relationship is not active | `relationship_not_active` |
| the receipt is not of the current generation | `stale_generation` |
| the child has reported again in this generation | `superseded_revision` |
| the child is not an allowed recipient | `recipient_not_authorized` |
| an unknown decision, no note, no turn, a criteria digest missing or given where none belongs | `malformed_receipt` |
| no criteria registered, or a digest other than the registered set's | `criteria_unregistered`, `criteria_set_changed` |
| a keeping decision on a generation with no bound anchor | `unbound_generation` |

No refusal reason is added, and a refusal writes nothing. The command line is checked first: an unknown `--decision` or a missing required option ends with the parser's own exit 2 and a message on stderr before the writer runs, an unreadable `--note @path` is a host problem (exit 3, as for `verdict --criteria`), and `malformed_receipt` is what the writer answers to a direct call or to a blank note, turn or digest. `decision-show` reads the decisions of a relationship back, oldest first, each with the state of its delivery.

Limits that are part of the contract. The relay records the decision and its delivery; it does not read the child's thread, so `dispatched` says the message was accepted, not that the child acted. A decision that opened g+1 is recorded against a DAG plan node by `dag-correct` ([the third way to open a generation](dag-scheduler.md#three-ways-to-open-the-generation)), which refuses until the decision was dispatched into the turn the generation is bound to; until it is recorded, `dag-accept` finds no execution for g+1 and refuses `stale_generation`. A message sent to the child outside this route leaves a record only when the registered parent admits its turn ([Direct parent interventions](#direct-parent-interventions)). The crw-run skill's need-input procedure follows this route. After an answer or a stop, the relationship still reads as blocked (`dispositions-show`) until the child reports again. `registry`'s `generation-open --reason` still accepts only its own two reasons: `decision_reply` is written by this command alone. An installed relay older than this change has no `decision-reply` and answers the command as an unknown subcommand.


## The coordination summary

A verdict enqueues the summary owed to its Linear document inside the verdict's own transaction,
so the two commit together. Nothing in this package performs the write: `sync-operation` returns
the exact operation, and whoever already holds an authenticated connector executes it.

The connector's document save takes no idempotency key, so a write can succeed and lose its
response. Read the document before retrying. A matching job block proves that the write landed;
an absent block does not prove non-delivery while an earlier write may still land. Initialize the
relationship's marked container once and reconcile an uncertain initialization before retrying.
Every job write, including its first insertion, conditionally replaces that container's exact
observed text. Repair a different, duplicated or malformed job block with the same conditional
replacement, never a bare append. `sync-complete` checks the readback's structured record and
summary together: "verified" inside "unverified", or fields split across blocks, is not proof.

A block is identity headers, a blank line, then the summary inside a FENCED literal region. The
fence is not decoration. Written as plain body text the summary came back from the real document
normalised: a bare filename autolinked, brackets and asterisks escaped, and the confirmation
correctly refused because the record no longer matched. Inside a fence the same summary returned
byte-for-byte. The fence is sized to exceed the longest backtick run in the summary, because
findings quote inline code and whole fenced blocks, and a `summarySha256` header describes exactly
the bytes written, so any residual alteration fails precisely instead of passing quietly.

Identity headers are read ONLY from the region between the start marker and that blank line, and
the end marker is located only after the fence closes. Both matter because a finding may
legitimately write `eventId: 0000` in prose or quote a relay marker as literal text, and neither
may become structure. A block written before this format stays readable and reconcilable, and is
repaired by the same conditional replacement rather than by regenerating anything.

A block that NAMES its format is held to that format. An unsupported `blockFormat` is refused
rather than reread under the older rules, a declared block owes a closed fenced summary, and only
whitespace may sit between that fence and the end marker, so text appended behind the fence cannot
ride along unread. Without those rules the more structure a block lost, the less it was checked.
Undeclared blocks keep the older treatment, since the connector's blank line and the blocks
written before this format still have to reconcile.

An external failure is retried on its own and never re-runs a verification or re-sends a
correction. A local failure inside the verdict's transaction rolls that transaction back, because
a durable enqueue that could be silently dropped would not be durable.

## Running the loop

    codex-session-relay --socket $SOCK --state $STATE daemon --max-ticks 200
    codex-session-relay --socket $SOCK --state $STATE daemon --deadline 3600

A run REQUIRES `--max-ticks` or `--deadline`. There is no unbounded mode. A second daemon on the same
state directory exits rather than racing, and stopping one is safe at any point: every transition
is committed before its side effect, and `recover` reconciles whatever was in flight without
resending anything.

Between ticks the run waits `RetryPolicy.poll_interval_seconds`, and that wait is clamped to the time
remaining, so a run never sleeps past its own deadline and a bounded run does not wait after its
final tick. The CLI supplies that wait; `RelayDaemon.run` waits only when it is given something to
wait with, and the sleeper stays injectable so the cadence tests drive it without real time
passing. Until this was wired, `daemon --max-ticks 3` returned in 0.155 seconds against a 20 second
interval and a deadline-only run busy-spun for its whole duration.

A bound that crosses a process boundary is handed over as an INSTANT rather than as a duration. A
supervisor that has already decided when a worker must stop passes `--deadline-monotonic`, read from
`CLOCK_MONOTONIC` on this host and this boot, and the worker converts it against its own clock the
moment it has one. What the worker spends getting there - fork, interpreter start, imports, argument
parsing, opening the store - then comes out of the segment instead of landing after it. As a duration
the count restarted at the worker, so the last segment of a bounded run overran by a whole startup;
earlier segments hid it, because each one's overspend was already deducted from its successor.
`service start --deadline N` does the same for the supervisor it launches, which means N now includes
that supervisor's own startup and a very short N can end a launch before it reports itself ready.

The instant is written by the launching process and is not meant to be typed. It names a time in one
boot on one host, and it fails open rather than closed if it escapes that: `CLOCK_MONOTONIC` restarts
near zero across a reboot, so a value carried into a later boot sits in that boot's future and names a
bound much later than anyone asked for. Nothing stores it — the launcher builds it and the child it
just exec'd reads it — which is why no boot identity is checked. A person writes `--deadline`, which
is a duration and starts counting where it lands.

A run that reaches its own clock after the instant it was given takes no tick and exits 5, and so
does one whose bound is gone by the time it has taken its locks and built its adapter. It neither
served nor failed: a supervisor reading it as a clean segment would reset its failure streak over a
worker that did nothing, and reading it as a crash would back off from one that did not break. A
supervisor that sees it stops replacing workers, and says so as `degraded` when its own bound still
had room - a segment shorter than a worker costs to start would otherwise churn processes forever
while reporting healthy segments.

Two end times are not a preference: `--deadline` together with `--deadline-monotonic` is refused, as
is any value no comparison can pass - `nan`, `inf`, or a negative one. `--deadline 0` is a bound with
nothing in it rather than the absence of one.

`--segment-seconds` is checked the same way, and has to be greater than zero, because it is not a
bound the supervisor keeps - it is the one it hands to every worker. A worker given a length it must
refuse exits before its first tick, and a supervisor reads that as an ordinary worker failure and
launches another, so the service would stay alive and serve nothing for as long as it was left
running. Only an absent value takes the policy default.

A bound stops a run from STARTING a tick, so a tick already under way finishes and may make several
deliveries. It is compared on two clocks on purpose. The deadline the daemon reads is a wall instant,
because that is what it has always read, and a wall clock that steps backwards would push that instant
away and hand the run time nobody granted it. So the run also carries a `stop` — the daemon's own
additional early exit — reading the monotonic bound this process was given. The guard is read once per
loop, so it bounds when a tick may begin to within one poll interval, whatever the wall clock does.

A supervisor whose bound was already gone when it first read a clock does not run recovery either.
Recovery makes real App Server calls for every unresolved attempt, and a run that cannot serve one
segment buys that work no segment to be useful in.

## Activation

Not installed, not started and not verified as a running service by this delivery, which has only
ever exercised the package offline and against injected transports. The unit below is a worked
example of the CLI's real shape, with the global options before the subcommand and an explicit
bound; whoever operates the host owns whether it is correct for that machine. It names the relay
through the runtime installer's pointer rather than through `PATH`, so it starts whichever runtime
the host has selected, and never a stale copy earlier on `PATH`.

    # ~/.config/systemd/user/codex-session-relay.service
    [Unit]
    Description=Codex session relay
    [Service]
    Environment=RELAY_SOCKET=%h/.codex/app-server-control/app-server-control.sock
    Environment=CODEX_THREAD_BRIDGE_EXECUTION_POLICY=%h/.codex/execution-policy.json
    ExecStart=%h/.local/share/crw-runtime/current/bin/codex-session-relay --socket ${RELAY_SOCKET} daemon --deadline 3600
    Restart=always
    RestartSec=5
    [Install]
    WantedBy=default.target

The deadline plus `Restart=always` is deliberate: the process is bounded, and the supervisor is what
makes it continuous. Where a user manager is unavailable, run the same command in the foreground.

Until todo 43 the Go runtime refused to open a store in the relay's default state directory unless
`CRW_ALLOW_LIVE_STATE=1` was set, and this unit runs the relay on that directory and sets no such
variable, so it served only a host whose pointer names the fence release's Python `env-*` runtime.
The guard now refuses only under test isolation
([the live-state guard](../../docs/port/cutover.md#the-live-state-guard-test-isolation-only)), and
the unit needs no variable for it. A host still moves its store to the Go runtime through
[the cutover](../../docs/port/cutover.md), rather than by pointing this unit at a Go runtime.

The execution policy is declared in the unit's own environment, not an interactive shell profile:
the worker resolves it in its own process at start, and a variable exported only in a shell leaves
the serving worker with no policy at all. After editing the policy file, restart the service (the
snapshot is republished at startup), run `doctor --require-worker-policy` with the same variable
set, and treat a matching digest as the recheck before relying on readiness.

Where this package's own `service` supervises instead of a unit, the declaration belongs to the
service for the same reason it belongs to the unit:

    codex-session-relay --state <dir> service declare --execution-policy <file>

It is read back before it is recorded, and every later `service start` and `service restart`
launches its daemon with it whatever the calling shell carries; the workers that supervisor
spawns inherit it. Without it, a restart typed from a shell that had not exported the variable
relaunched the service with no policy at all and withheld every role-bound delivery, while the
file, the installation and the owner's intent were unchanged.

A shell that names a different file is refused rather than preferred, before anything is stopped,
because choosing between two policy files by preference is that same failure spelled the other
way. `service status` reports the declaration as the input to the NEXT launch, beside the digest
the running worker actually published, so a declaration made while the service runs reads as the
pending change it is.

`service stop` holds a worker's pidfd while it reads its start ticks. If the identity
read gives no answer, or the recorded ticks are missing, a readable pidfd confirms
exit: stop reports the worker as `exited` and clears its recorded identity, so restart
can proceed past the stop. A running worker whose identity cannot be established stays
`unverifiable` and receives no signal. The stop deadline, 100 ms termination cadence
and launch-readiness snapshot remain unchanged (port decisions 27 and 40).

## How invocation actually becomes automatic

By POLLING, not by notification. The transport exposes no delivery-notification seam,
so the daemon detects a terminal turn by reading, on a
bounded cadence, and then dispatches the authorized wake itself. That is what makes it automatic:
saving an inbox item until somebody looks is not a wake, and is never reported as one.

A tick that learns nothing writes nothing. Reconciliation is invoked only when the evidence
actually changed, or when a previous failure recorded that work is owed.

## Capability limits

These are recorded because behaviour depends on them.

- Delivery terminal turns are found by bounded polling. The client has a private
  terminal observer only for [subscription release](subscriptions.md); it does not wake tasks.
- An interactive parent cannot be pushed to. Its event is stored and reported `stored_not_woken`.
- Path binding is proven; byte stability is enforced only with a read lease, which is opt-in
  because holding one blocks writers for the kernel's lease-break timeout.
- A later turn needs an explicit continuation admission. Ordering corroborates; it never admits.
- An attempt with no affirmative evidence stays held and reports what it is missing. There is no
  operator override.
- Archive state is resolved by exact task id, because a cwd-filtered listing can miss a task whose
  cwd changed. The App Server's default listing holds its interactive sources only, so a thread
  created by codex exec (a child on the official worktree path) is looked up in listings that name
  that source, beside the unchanged default ones. An inconclusive answer withholds rather than
  guessing, and assignment-show and dispositions-show then name that withhold on the correction it
  holds back. A thread from another source outside the default listing (appServer) still reads
  unknown.
- The JSON date-time format is not validated by the available validator; that is reported as
  unverified rather than implied.

## Documents

- `protocol-v1.md` — the wire and record protocol, derived from the frozen contract.
- `linkage.md` — the three-level execution linkage and peer links. Relay-owned records,
  outside the frozen contract, with the transaction protocol they are written under.
- `invariants.md` — every invariant and the code that enforces it.
- `faults.md` — the operational fault ledger: one breakage, one record, one Linear issue,
  closed only by reverification.
- `product-routing.md` — where a product's incident belongs: the current issue, an existing
  issue, a reopen, a follow-up, a new issue in its project, or one pending-classification record.
- `operations.md` — where the state lives, who owns the daemon, and how to read a
  stuck delivery. Each section says whether the behaviour is implemented or planned.
