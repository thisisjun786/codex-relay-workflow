# dots contact: how a Linna request reaches a repository manager

This page records the control point between the Linna dots app and a repository management session.
It states what is verified, what is unknown, and how a request is identified, admitted, converged
and answered. It does not assume any dots API, webhook or internal extension.

Status: the admission rules in `internal/manage/dotscontact.go` are tested with fakes. No live
round trip through dots is observed by this change. The live check is listed at the end as a
manager task.

## Capabilities

The one confirmed path runs in one direction: a Linna thread delegates work to the repository
management session, and the delegation carries a request id. The issue record for CRW-549 states
it, and the relay's assignment record for this child shows it arriving. This change did not
re-observe that delegation on a live host.

Every dots-side capability below is unknown. Linna has been asked for the list of dots interfaces.
Until that list arrives, no row may be marked supported.

| Capability | State | Basis |
|---|---|---|
| Linna thread to manager session delegation, with request id | verified (one direction) | CRW-549 issue record; relay assignment record |
| Read a dots conversation or message by id | unknown | no interface list received |
| Send a message from the manager back into dots | unknown | no interface list received |
| Receive a manager reply inside dots as a delivery receipt | unknown | no interface list received |
| Push webhook or event subscription from dots | unknown | not assumed; none known |
| Sender identity check on an inbound request | unknown | no interface list received |
| Delivery and application of a manager instruction | unknown | transport accepted is never application |

A transport acceptance from the bridge is a receipt that the message was dispatched. It does not
show that the manager read it, applied it or finished the work. The round trip is proven only after
a manager turn shows the instruction in its own record.

## Identifiers

Each identifier has one existing field to live in. Nothing new is added to the envelope.

| Identifier | Where it lives | Note |
|---|---|---|
| Source conversation or request id from dots | `dotsContactRequest.RequestID` | The admission key. It makes a repeat a repeat. |
| Repository routing id | `dotsContactRequest.Repository` compared to the local repository | Routing only. It grants no scope ownership. |
| Named manager task | `dotsContactRequest.Manager` compared to the current manager | Empty when the request names none. |
| Command id | the request id; no separate field | The logical id is derived from the request id. |
| Logical id (outbox key) | `dotsContactLogicalID`, one path component under the bridge request limit | Same request id, same logical id, every time. |
| Purpose and authority | `Purpose`, `Authority` | Approval needs authority `user`. |
| Correlation and reply target | the logical id, plus the decision DecisionID for an approval | A reply names the question it answers. |
| Payload | `Payload`, hashed to a digest | The ledger keeps the digest, not the text. |

The transport request id on a delivery attempt (`del-<event>-a<attempt>`) changes on every
retry. It is not the dots request id and is never used as one.

## Convergence

The ledger holds one entry per request id: the payload digest, the logical id and the transport
outcome. Admission reads it before anything else.

- A repeat with the same digest and an accepted outcome converges on the earlier logical id, and
  sends nothing. The reason is `accepted_not_applied`.
- A repeat with the same digest and an unknown outcome waits. The earlier record must be reconciled
  first, with the bridge receipt, before any send.
- A repeat with the same digest and a refused outcome stays refused.
- The same request id with a different digest is refused as `duplicate_conflict`.

Transport acceptance and application are recorded in separate fields. Only an applied record from a
manager turn counts as the instruction taking effect.

## Approvals

An approval answers one question under one revision. The admission checks the question id, the
revision the manager holds and the authority. A reply that names an old revision is refused as
`stale_decision`. A cancelled question is refused as `cancelled`. A reply without user authority
is refused as `no_permission`.

The revision is the decision record's fingerprint, which the relay computes from the context, the
blocking items and the options. `ValidateAnswer` in `internal/relay/decisions` checks the state and
the provenance of an answer, but it does not compare the fingerprint. The comparison is therefore
done at admission, and a relay caller must not rely on `ValidateAnswer` alone.

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
| `duplicate_conflict`, `previously_refused` | refuse | Same id, different request or a settled refusal. |
| `stale_decision`, `cancelled`, `no_permission` | refuse | The answer is not current or not authorized. |
| `invalid_encoding`, `no_request_id`, `unknown_purpose` | refuse | The request is malformed. |

## Test boundaries

`internal/manage/dotscontact_test.go` covers each boundary with a fake manager view and ledger:
busy and idle delivery, offline and unknown waiting, duplicate convergence, conflicting payloads,
unknown outcome reconciliation, wrong repository, previous manager, stale and cancelled answers,
missing authority, invalid UTF-8, delimiter ambiguity in the digest, and the path safety of the
logical id. These tests are simulated. They prove the decision rules, not any live delivery.

## Manager task: the live round trip

This is the part the manager session must run on the host. The child does not run it, because the
child has no network access and no MCP servers.

1. Confirm the dots interface list from Linna. Mark each capability in the table above as verified
   or unsupported with its evidence path. Do not change any unknown row without that evidence.
2. Send one progress request, one completion, one block and one decision request through the
   existing path. Use `crw manage deliver` or `send-parent` and the relay decision commands. Do not
   add a command.
3. For each message, record the transport receipt, then the receipt of the manager turn that shows
   the instruction, then the application. Keep these three as separate records.
4. Repeat one request with the same request id and confirm it converges. Interrupt one send and
   confirm that restart reconciles it.
5. Record the evidence file name and the result in the handoff. A transport receipt alone is not a
   pass.
