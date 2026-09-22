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
| identity, manifest, scope | implemented | tests/test_identity.py, tests/test_manifest_scope.py |
| durable store | implemented | tests/test_store.py |
| registry, generations, turn admission | implemented | tests/test_registry.py, tests/test_delivery.py |
| receipts and outcome classification | implemented | tests/test_receipts.py |
| delivery, transport classification, bounds | implemented | tests/test_delivery.py |
| acknowledgement, verdicts, revision routing | implemented | tests/test_ack_reconcile.py |
| reconciliation and restart recovery | implemented | tests/test_ack_reconcile.py |
| bridge host adapter | implemented | tests/test_bridge_adapter.py |
| bounded daemon | implemented | tests/test_daemon.py |
| CLI | implemented | tests/test_cli.py |
| frozen-schema conformance | implemented | tests/test_schema_conformance.py |

## Install

The core has no third-party dependency. The real host adapter needs the transport bridge.

    pip install -e .
    pip install -e '.[bridge]'      # only to talk to a live App Server

The wheel carries the five JSON schemas as package data, from the package selection alone. An
explicit `force-include` for that directory would map it a second time and the build fails on the
duplicate archive path, so there is deliberately no such table in `pyproject.toml`.

Build and install checks cover the wheel contents, schema data, imports and CLI. JUN-93 also
exercised a dedicated installed environment against a real App Server: child completion, automatic
parent review, a correction to the same child, busy-recipient deferral, accepted-response loss and
recovery without resending. Host versions, exact revisions, remaining cases and activation/stop
procedures are maintained in the canonical Linear project record. These observations do not
activate a service on another host or establish that an earlier test process is still running.

## State

Runtime state lives outside any repository, in
`$XDG_STATE_HOME/codex-session-relay/<endpoint-hash>/` (mode 0700), holding `relay.sqlite3`,
`daemon.lock`, `daemon.pid` and `daemon.log`. Precedence, highest first: `--state`,
`CODEX_SESSION_RELAY_STATE`, `XDG_STATE_HOME`, then `~/.local/state`.

**Every process must point at the same state directory.** The child emitting, the parent
acknowledging and the daemon delivering share one store; a mismatched `--state` means they simply do
not see each other. The endpoint hash is derived from the socket path, so passing the same
`--socket` is enough.

`doctor` reports which rule won, the database it resolved to and the access this process really
has. To prove two participants share one store rather than two copies of one, take
`store-identity` on one side and write a nonce with `store-challenge --write`, then check both
from the other side: `doctor --expect-inode <device>:<inode> --expect-nonce <nonce>`. Each half
is necessary. An identifier is copied along with the file; a nonce is copied too when the copy
is taken after the challenge was written; and a device and inode that agree do not say both
processes opened the same pathname for that inode. Anything short of the pair is reported
`unproven` and exits non-zero rather than being read as yes. See
[docs/operations.md](docs/operations.md).

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

One further rule applies where the host reports a recipient as `notLoaded`. A resume transmits the
recorded settings and may apply them to a thread the host has to load first, so a pair the policy
did not derive — a supervisor's, whose pair is the user's own selection, or one admitted by an
exception — is refused rather than transmitted, because it could restore a value the user has
since changed. `doctor` reports the policy digest this process resolved; the bridge's own is
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
      "model": "anthropic/claude-opus-5",
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
| resume returns a different sandbox, cwd, roots, model or effort | withheld, `settings_not_preserved`, no turn started |
| resume returns no value for one of them | withheld, `setting_unobservable`, no turn started |
| resume returns `environments: null` | withheld, `environments_unknown` |
| resume returns an approval policy other than `never` | `inbox_only`: stored, not woken |
| resume returns no approval policy at all | withheld, `setting_unobservable`, still retryable |
| resume matches | the turn begins, carrying no overrides |

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
      --require-worker-policy '[{"role": "parent", "model": "devin/swe-2", "reasoningEffort": "max"}]'
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
existing full settings record, including environments, approval policy and sandbox. Each
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
      "model": "devin/swe-2", "reasoningEffort": "max",
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
      "model": "anthropic/claude-opus-5", "reasoningEffort": "xhigh",
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

The entry reserves the issue before asking the bridge to create a standby task. It then binds
the returned task and turn to the marker, registry, criteria and settings before sending the
business prompt. The standby prompt does no implementation work. A missing or mismatching live
worker policy refuses before creation; a later refusal retains the same task for recovery.
Registration adds that retained child's ID to the declared recipients so the parent can
return revision requests to its own child. Replay and pre-start checks verify this derived
list; it does not authorize messages to an unrelated task.
The internal bridge uses the caller's snapshotted execution policy. Managed admission pins
its operation ledger to the explicit `--state` directory, independently of environment defaults.
The ledger's resolved path, device and inode are part of the request fingerprint and are
rechecked before host mutations. Replacing that file refuses recovery rather than creating
another child from an empty ledger.

Retry the **same request with the same paths and contents**. A fingerprint mismatch refuses
rather than rewriting the assignment; uncertain creation or delivery is reconciled against the
bridge's retained operation, never retried under a new identity. A still-running standby returns
`incomplete`; the caller may retry when it ends. This command does not install a retry scheduler.
`admitted` means the business turn was dispatched, not that the child claimed it, that its hook
fired, or that its issue passed review. Those remain separately observed facts.
If naming failed after a verified task was created and the bridge recorded that no first turn
was attempted, retry resumes that same task with a separately retained standby operation.
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
| `incomplete` / `standby_incomplete` | Wait for that standby to complete, then retry the same request. |
| worker policy absent or mismatched | Restore the declared serving policy, verify its reading, retry the same request. |
| paused/archived recipient, changed settings/scope/criteria | Preserve the hold; obtain the owning user's supported transition before retrying. |
| creation or business outcome unknown | Inspect the retained bridge operation; keep the reservation and do not create a replacement. |
| request fingerprint conflict | Recover the original input and selectors; never overwrite them to force a replay. |

Before the business turn, authorization is checked again after resume. A known paused, archived,
busy or unreadable recipient is withheld. An external UI change can still race the final host
read and `turn/start`: the host offers no atomic conditional start. Raw bridge calls are outside
this managed admission boundary. Malformed JSON requests exit 4; missing CLI arguments follow
argparse's exit 2 on stderr. Refused or incomplete admission
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

Admission history is read in bounded raw rowid pages. The cursor retains a finite
insertion boundary and advances only past a consumed prefix. Settled and foreign
rows use scan slots but cannot become this assignment's candidates; large shared
stores can therefore increase observation latency, without increasing per-tick
row materialization. Admission writers preserve existing rowids and never delete
rows. Historical terminal turns remain observable, but opening a new generation
supersedes all prior outcomes, including failures; observing an old failure does
not send it as a current-generation report or establish acceptance.

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
Malformed identity arguments exit 4. Missing required flags follow argparse and exit 2 on stderr.
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

## Commands

Global options come BEFORE the subcommand:

    codex-session-relay [--state DIR] [--socket PATH] <command> [options]

| Command | Purpose |
|---|---|
| `register` | register a parent/child relationship with its authorized scope |
| `managed-start` | reserve, create a standby, register and dispatch one recoverable assignment |
| `managed-show` / `managed-release` | inspect a retained start; release only an unarmed reservation |
| `reporting-show` | offline exact-turn reporting diagnosis; reads, never wakes or writes a report |
| `register --project` | the same, and the issue's whole lower level in one transaction |
| `settings-record` / `settings-show` | record and inspect a task's authorized execution settings |
| `generation-open` / `generation-bind` | open a generation; bind its anchor to an exact dispatch turn |
| `admit-turn` | record an owner-confirmed continuation turn out of band |
| `relationship-status` / `relationship-resume` | pause, cancel, archive; resume only by restating generation and scope |
| `linkage-supervise` | an initiative supervisor over a project parent, by execution or by reference |
| `linkage-bind` | claim one scope for one task at one level |
| `linkage-attach` | bind an existing assignment's issue to its project |
| `linkage-peer` | join two project parents, symmetrically and outside the hierarchy |
| `linkage-outstanding` | exactly the unfinished work a replacement owner must acknowledge |
| `linkage-handover` | replace a scope's owner, only by restating the owner and that work |
| `linkage-directive` / `linkage-settle` | record an instruction by digest and origin; settle one without erasing the other |
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
| `assignment-show` / `assignment-find` / `assignment-mark` | one issue, one child, and where it stands |
| `sync-*` | the coordination-document outbox: target, next, claim, operation, reconcile, complete, fail, retry, status, progress |
| `status` | observable delivery, acknowledgement and verification state |
| `show` | the full record for one event: receipt, manifest, attempts, sent bytes, verdict |
| `daemon` | run the bounded reconciliation and delivery loop |
| `doctor` | environment and capability check; optional `--require-worker-policy` readiness gate |

Every command prints JSON. Exit 0 success, 2 a refusal with a machine-readable `reason`, 3 a host
problem, 4 usage.

## The normal flow

A child completing work from inside its own live turn:

    codex-session-relay --socket $SOCK emit \
      --relationship rel-... --generation 1 --attempt 1 \
      --outcome ready_for_review \
      --turn-thread <child task id> --turn-id <this turn> --turn-status inProgress \
      --artifact /abs/path/to/deliverable

The turn status you pass is a claim, not proof. With `--socket` the relay reads the turn from the
host and uses what the host actually reports. **Offline, a readiness claim can only STAGE**: it is
stored and visible, and it becomes deliverable only once an independent observation sees that turn
end normally. A turn that ends failed or interrupted suppresses the claim instead of promoting it.

A loop spanning several turns completes on a turn that is not the anchor, and says so in the same
call:

    ... --turn-id <later turn> --continues-anchor <dispatch turn> \
        --continuation-actor <child task id> --continuation-reason 'cycle 3 of this execution'

Without that, a non-anchor turn is refused. Host ordering can corroborate the claim and can
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

## Activation

Not installed, not started and not verified as a running service by this delivery, which has only
ever exercised the package offline and against injected transports. The unit below is a worked
example of the CLI's real shape, with the global options before the subcommand and an explicit
bound; whoever operates the host owns whether it is correct for that machine.

    # ~/.config/systemd/user/codex-session-relay.service
    [Unit]
    Description=Codex session relay
    [Service]
    Environment=RELAY_SOCKET=%h/.codex/app-server-control/app-server-control.sock
    Environment=CODEX_THREAD_BRIDGE_EXECUTION_POLICY=%h/.codex/execution-policy.json
    ExecStart=%h/.local/bin/codex-session-relay --socket ${RELAY_SOCKET} daemon --deadline 3600
    Restart=always
    RestartSec=5
    [Install]
    WantedBy=default.target

The deadline plus `Restart=always` is deliberate: the process is bounded, and the supervisor is what
makes it continuous. Where a user manager is unavailable, run the same command in the foreground.

The execution policy is declared in the unit's own environment, not an interactive shell profile:
the worker resolves it in its own process at start, and a variable exported only in a shell leaves
the serving worker with no policy at all. After editing the policy file, restart the service (the
snapshot is republished at startup), run `doctor --require-worker-policy` with the same variable
set, and treat a matching digest as the recheck before relying on readiness.

## How invocation actually becomes automatic

By POLLING, not by notification. The transport bridge answers every server-initiated message with
an error and exposes no subscription seam, so the daemon detects a terminal turn by reading, on a
bounded cadence, and then dispatches the authorized wake itself. That is what makes it automatic:
saving an inbox item until somebody looks is not a wake, and is never reported as one.

A tick that learns nothing writes nothing. Reconciliation is invoked only when the evidence
actually changed, or when a previous failure recorded that work is owed.

## Capability limits

These are recorded because behaviour depends on them.

- No notification subscription exists, so terminal turns are found by bounded polling.
- An interactive parent cannot be pushed to. Its event is stored and reported `stored_not_woken`.
- Path binding is proven; byte stability is enforced only with a read lease, which is opt-in
  because holding one blocks writers for the kernel's lease-break timeout.
- A later turn needs an explicit continuation admission. Ordering corroborates; it never admits.
- An attempt with no affirmative evidence stays held and reports what it is missing. There is no
  operator override.
- Archive state is resolved by exact task id, because a cwd-filtered listing can miss a task whose
  cwd changed. An inconclusive answer withholds rather than guessing.
- The JSON date-time format is not validated by the available validator; that is reported as
  unverified rather than implied.

## Documents

- `docs/protocol-v1.md` — the wire and record protocol, derived from the frozen contract.
- `docs/linkage.md` — the three-level execution linkage and peer links. Relay-owned records,
  outside the frozen contract, with the transaction protocol they are written under.
- `docs/invariants.md` — every invariant and the code that enforces it.
- `docs/operations.md` — where the state lives, who owns the daemon, and how to read a
  stuck delivery. Each section says whether the behaviour is implemented or planned.
