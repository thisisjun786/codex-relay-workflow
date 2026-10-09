# dots contact: how a Linna request reaches a repository manager

This page records the control point between the Linna dots app and a repository management session.
It states what is verified, what is unknown, and how a request is identified, admitted, converged
and answered. It does not assume any dots API, webhook or internal extension.

Status: the admission rules in `internal/manage/dotscontact.go` are tested with fakes. No live
round trip through dots is observed by this change. The live check is the manager task at the end.

## Capabilities

One path is confirmed, and it runs in one direction: a Linna thread delegates work to the
repository management session, and the delegation carries a request id. The record is the CRW-549
issue, which states it. The delegation itself runs outside this repository, so this change did not
re-observe it. The repository side of that path is the receiving contract in `dotscontact.go`,
which decides what an arrived request may do.

Every dots-side capability below is unknown. Linna has been asked for the list of dots interfaces,
and the list is not in the repository or the inputs yet. Until it arrives, no row may be marked
supported.

| Capability | State | Evidence |
|---|---|---|
| Linna thread to manager delegation, with request id | verified (one direction) | CRW-549 issue record for the delegation; receiving contract in `internal/manage/dotscontact.go` |
| Read a dots conversation or message by id | unknown | no interface list received |
| Send a message from the manager back into dots | unknown | no interface list received |
| Receive a manager reply inside dots as a delivery receipt | unknown | no interface list received |
| Push webhook or event subscription from dots | unknown | not assumed; none known |
| Sender identity check on an inbound request | unknown | no interface list received |
| Application of a manager instruction | unknown | transport acceptance is never application |

A transport acceptance from the bridge is a receipt that the message was dispatched. It does not
show that the manager read it, applied it or finished the work. The round trip is proven only after
a manager turn shows the instruction in its own record.

## Identifiers

Each identifier lives in one field of the admission request or in an existing record. Nothing new
is added to the envelope.

| Identifier | Where it lives | Note |
|---|---|---|
| Source request id from dots | `RequestID`, the admission key and command id | A repeat with this id is a repeat. |
| Repository routing id | `Repository`, compared with the local repository | Routing only. It grants no scope ownership. |
| Named manager task | `Manager`, compared with the current manager | Empty when the request names none. |
| Purpose | `Purpose`: `instruction` or `approval` | Any other value is refused. |
| Authority class | `Authority`, which must be `user` for an approval | Matches the decision record's answer class. |
| Authority source | `AuthoritySource`, compared with the answer the relay recorded | The decision record's answer provenance, `AnsweredBy` and `AnsweredVia`. |
| Question id | `QuestionID`, compared with the open question | The decision record's `DecisionID`. |
| Revision | `Revision`, compared with the open question | The decision record's `Fingerprint`, computed over context, blocking items and options. |
| Action, target, scope | `Action`, `Target`, `Scope`, compared with the open question | The option the user chose, and the scope the decision record names. |
| Instruction text | `Text`, delivered and hashed, never copied into the ledger | Only the digest is kept. |
| Correlation and reply target | the logical id from `dotsContactLogicalID`, plus `QuestionID` for an approval | Each reply names the question it answers. |
| Logical id (outbox key) | `dotsContactLogicalID`, one path component under the bridge request limit | The same request id always gives the same logical id. |

The transport request id that `Deliver` (`internal/manage/deliver_send.go`) sends to the bridge is the
logical id on the first attempt. A retry after a `not_delivered` refusal, and a recovery resend for an id
the bridge never recorded or answered `not_delivered` for, takes `<logical-id>-rN` (`deliverRetryRequestID`,
N counted from the outbox record). A recovery resend for an id the bridge recorded and answered
`not_attempted` keeps the request id the outbox record holds (the unsuffixed logical id, or the `-rN` id
the record already moved to), so the bridge receipt stays correlated. The id is not the dots request id and
is never used as one. The relay's own `del-<event>-a<attempt>` form belongs to relay delivery and is not
the id `Deliver` sends.

## Delivery input

`dotsContactMessage` turns an admitted deliver verdict into the `Message` that `Deliver` takes. It
sets the logical id from the verdict, the thread, the role and the parent settings from the caller,
and a text that names the request, the purpose and, for an approval, the question, revision, action,
target and scope. Any other verdict yields no message, so nothing reaches the bridge without an
admission.

## Convergence

The ledger holds one entry per request id: the digest of its command, the logical id it was sent
under and the transport outcome. The digest covers every field that defines the command, including
purpose, authority, question binding and scope. Admission reads the ledger before anything else
that depends on it.

- A repeat with the same digest and an accepted outcome converges on the earlier logical id, and
  sends nothing. The reason is `accepted_not_applied`.
- A repeat with the same digest and an unknown outcome waits. The earlier record must be reconciled
  first, against the bridge receipt, before any send.
- A repeat with the same digest and a refused outcome stays refused.
- The same request id with any other digest is refused as `duplicate_conflict`.

Transport acceptance and application are recorded in separate fields. Only an applied record from a
manager turn counts as the instruction taking effect.

## Approvals

An approval answers one question under one revision, for one concrete action. Admission reads the
open question from the manager's own record, not from the request. Every value the open question
must hold (id, revision, action, target, scope and authority source) is checked first, and a blank
value in the open question refuses the approval, so a blank cannot match a blank.

The cancelled and paused states come from that same record. A reply cannot cancel or pause a
question, and an old reply cannot revive one. A reply that names an old revision is refused as
`stale_decision`. A reply whose action, target or scope differs from the question is refused as
`unbound_decision`. A reply without user authority, or with an authority source the relay did not
record, is refused as `no_permission`.

The relay's `ValidateAnswer` in `internal/relay/decisions` checks the state and the provenance of an
answer, but it does not compare the fingerprint or the action. Those comparisons are made at
admission, and no relay caller may rely on `ValidateAnswer` alone for them.

Quoted text from a message, or a status notice, carries no permission. Only a live answer under the
current revision does. A separate approval that a platform or a user policy requires stays required.

## Admission verdicts

| Reason | Action | Meaning |
|---|---|---|
| `manager_busy`, `manager_idle` | deliver | Send to the existing manager session. |
| `manager_unreachable` | wait | The manager is offline or unknown. Nothing is sent. |
| `accepted_not_applied` | converge | Earlier transport acceptance. Not a new send. |
| `outcome_unknown_reconcile_first` | wait | Reconcile the earlier record first. |
| `wrong_repository`, `previous_manager` | refuse | Routing does not match this manager. |
| `duplicate_conflict`, `previously_refused` | refuse | Same id with a different command, or a settled refusal. |
| `no_open_question`, `incomplete_decision` | refuse | The approval has no complete question to bind to. |
| `stale_decision`, `unbound_decision`, `cancelled`, `paused`, `no_permission` | refuse | The answer is not current, not bound or not authorized. |
| `invalid_encoding`, `no_request_id`, `unknown_purpose` | refuse | The request is malformed. |

## Test boundaries

`internal/manage/dotscontact_test.go` covers each boundary with a fake manager view and ledger:
busy and idle delivery, offline and unknown waiting, duplicate convergence (including an accepted approval
repeated after its question closed or moved on), a repeat that differs in
any field, unknown outcome reconciliation, wrong repository, previous manager, cancellation and pause
from the local view, stale and unbound answers, missing action, target, scope or authority, a blank
open question, invalid UTF-8, field boundaries in the digest, the path safety of the logical id, and
the delivery input. These tests are simulated. They prove the decision rules, not any live delivery.

## Manager task: the live round trip

The manager session runs this on the host. The child does not run it, because the child has no
network access and no MCP servers.

1. Ask Linna for the dots interface list. Mark each capability in the table above as verified or
   unsupported with its evidence path. Leave an unknown row unknown until that evidence exists.
2. Send one progress message, one completion, one block and one decision request through the
   existing path: `send-parent` for messages to the parent, and the relay's decision record for a
   decision request. Do not add a command.
3. For each message, record the transport receipt, then the receipt of the manager turn that shows
   the instruction, then the application, as three separate records.
4. Repeat one request with the same request id and confirm it converges. Interrupt one send and
   confirm that a restart reconciles it.
5. Record each result in one file named `live-roundtrip-<stage>.json`, where the stage is progress,
   completion, block, decision, converge or interrupt, and list those file names in the handoff. A
   transport receipt alone is not a pass.
