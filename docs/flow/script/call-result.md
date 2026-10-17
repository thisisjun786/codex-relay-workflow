
## CallResult

`agent.call` resolves with a frozen object with separate fields:

| Field | Meaning |
| --- | --- |
| `callId` | The journal's call id. |
| `handle` | Opaque `crwh1:<run>:<call_id>`; non-null once a thread exists for the call. It resolves through the journal to the call's thread and last turn. |
| `status` | `completed`, `null_model`, `held`, `refused`, `unknown` or `superseded`. |
| `output` | The schema-valid object (or the text of a call without a schema); non-null only when `status` is `completed`. |
| `reason` | Closed set, `null` when completed: `schema_invalid_after_repair`, `context_window_exceeded`, `host_infra_<kind>`, `host_config_<kind>`, `host_policy_<kind>`, `host_interrupted_external`, `engine_paused`, `budget_exhausted`, `pair_refused`, `policy_refused`, legacy `independence_refused`, `missing_step_decision`, `schema_not_strict`, `foreign_handle`, `dispatch_unknown`, `superseded_by_attempt`, `superseded_by_resume`, `cancelled`, `worktree_failed`, `restore_snapshot_mismatch`, `accepted_node_changed`, `recovery_hold`. |
| `meta` | `role`, `requestedPair`, `configuredPair`, `threadId`, `turnIds`, `repairUsed`, `tokens`, `startSeq`, `endSeq` (the last two are absent under `crw flow run`, see above). |

A failed or interrupted turn, and an RPC error to the thread or turn request, is classified by its `codexErrorInfo`: the
quota, rate, overload, connection, stream, rollback, steering and `other` kinds are `held` (`host_infra_<kind>`; a usage or
rate limit also pauses the run), `sessionBudgetExceeded` is `held` with `budget_exhausted`, `badRequest`, `unauthorized` and
`sandboxError` are `refused` (`host_config_<kind>`), `cyberPolicy` and `misalignmentPolicyViolation` are `refused`
(`host_policy_<kind>`), `contextWindowExceeded` is `null_model`, and a failure with no `codexErrorInfo` or one the table does
not list is `held` with `host_infra_unknown`. An interrupt the engine did not send is `held` with `host_interrupted_external`. A turn the App Server backend did not send because the
flow binding of its thread could not be written (the spawn hook's file, `~/.local/state/crw/flow-bound/<thread_id>.jsonl`; F14) is `held`
with `host_config_flow_binding`: the flow-bound directory is the current user's, mode 0700 and no symlink, and the thread id has the form
of a thread id (lower-case hexadecimal and `-`).
A request that failed without an answer (a lost connection, a timeout) ends `unknown` with `dispatch_unknown`; on the fake
backend the call writes `outcome_unknown`, and on the App Server backend it writes no terminal row at all, so its last row
stays the intent that was sent and the reconcile pass decides it ([Reconcile and recovery](recovery.md#reconcile-and-recovery)). The node
of such a call is in `recovery_hold`: every later call of the node (a fresh call, a continuation of the unknown call's handle)
ends `held` with `recovery_hold` and sends nothing, and `crw.tierStep` for the node throws. Only `null_model` can be
evidence for a tier step.

A turn the engine interrupts - the turns in flight when a usage or rate limit pauses the run - is journaled first
(`interrupt_requested`, reason `engine_paused`) and ends `held` with reason `engine_paused`, whatever the host reports for
the interrupted turn (failed, interrupted or completed with a valid answer); a turn the host reports interrupted with no such row is `held` with `host_interrupted_external`. A
usage or rate limit also writes one flow row `escalation` with reason `quota_or_rate`.

### Schemas

An `outputSchema` is sent as a strict `json_schema`, so a schema is checked against the strict structured-output rules
before any thread starts: the root is an object; every object has `additionalProperties: false` and lists every one of its
properties in `required` (an optional field is a required nullable union such as `type: ['string', 'null']`); only `type`,
`properties`, `required`, `additionalProperties`, `items`, `enum`, `const`, `anyOf`, `$defs`, `$ref`, `description` and the
`pattern`, `format`, `minimum`, `maximum`, `exclusiveMinimum`, `exclusiveMaximum`, `multipleOf`, `minItems` and `maxItems`
constraints appear (no `oneOf`, `allOf`, `not`, `if`/`then`, `patternProperties` or `unevaluatedProperties`); and the schema
stays within 10 levels of nesting, 5000 properties and 1000 enum values.

- Every export of `crw:lib/schemas` is checked when a script imports the module (directly or through another helper); a
  violation is a load error.
- A schema passed to `agent.call` that fails the check ends the call `refused` with reason `schema_not_strict`, before any
  thread starts.
- The result of every call with a schema is validated in Go (`google/jsonschema-go`). A message is valid only if it is one JSON
  document with nothing after it (a closing bracket, a second value or text after the value is invalid). An invalid result
  gets one repair turn on the same thread with the same pair; a result that is still invalid is `null_model` with reason
  `schema_invalid_after_repair`. A third turn is never sent.
