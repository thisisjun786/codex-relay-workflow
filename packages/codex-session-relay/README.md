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
`daemon.lock`, `daemon.pid` and `daemon.log`. `CODEX_SESSION_RELAY_STATE` overrides the directory.

**Every process must point at the same state directory.** The child emitting, the parent
acknowledging and the daemon delivering share one store; a mismatched `--state` means they simply do
not see each other. The endpoint hash is derived from the socket path, so passing the same
`--socket` is enough.

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
| resume returns a different sandbox, cwd, roots, model or effort | withheld, `settings_not_preserved`, no turn started |
| resume returns `environments: null` | withheld, `environments_unknown` |
| resume returns an approval policy other than `never` | `inbox_only`: stored, not woken |
| resume matches | `turn/start` carries the same settings, and the turn begins |

A withheld send is retry-safe in the only sense that matters: nothing was sent, so nothing can be
delivered twice. It is not a permanent hold either, because settings that were never recorded can
be recorded and the next pass decides again.

**One observation limit, stated plainly.** `TurnStartResponse` defines only `turn`, and `Turn`
carries `id`, `items`, `status`, `startedAt`, `completedAt`, `durationMs` and `error`. There is no
settings echo anywhere in the start response, so the guarantee this offers is the checked resume
response plus the settings explicitly carried on the supported start call, which the protocol
scopes to that turn and subsequent turns. That is an ordinary supported-call guarantee, not a
confirmation from the backend, and the missing echo is never treated as a failure.

A sandbox type with no `ThreadResumeParams.sandbox` mode, such as `externalSandbox`, is refused as
`unsupported_sandbox_type` rather than approximated with a mode that means something else.

## Commands

Global options come BEFORE the subcommand:

    codex-session-relay [--state DIR] [--socket PATH] <command> [options]

| Command | Purpose |
|---|---|
| `register` | register a parent/child relationship with its authorized scope |
| `settings-record` / `settings-show` | record and inspect a task's authorized execution settings |
| `generation-open` / `generation-bind` | open a generation; bind its anchor to an exact dispatch turn |
| `admit-turn` | record an owner-confirmed continuation turn out of band |
| `relationship-status` / `relationship-resume` | pause, cancel, archive; resume only by restating generation and scope |
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
| `doctor` | environment and capability check |

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
    ExecStart=%h/.local/bin/codex-session-relay --socket ${RELAY_SOCKET} daemon --deadline 3600
    Restart=always
    RestartSec=5
    [Install]
    WantedBy=default.target

The deadline plus `Restart=always` is deliberate: the process is bounded, and the supervisor is what
makes it continuous. Where a user manager is unavailable, run the same command in the foreground.

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
- `docs/invariants.md` — every invariant and the code that enforces it.
