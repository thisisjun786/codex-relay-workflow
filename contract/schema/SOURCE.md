# Frozen contract schemas

Byte copies of the five schemas from the cross-session communication contract v1. The Python
relay carried the same five under `packages/codex-session-relay/src/codex_session_relay/schema/`
with this note until todo 44 removed its source; the copies here are the ones that remain.

| File | sha256 |
|---|---|
| acknowledgement.json | 193c2a1dbdb6197f852aaa38c6b8b4e55ba804ffc66e7b2a73925365b133eb7c |
| completion-receipt.json | fd6498fdca80c7e8f4d97e37ceec12935bf1afce10211826fdc232593b9fb0d5 |
| delivery-attempt.json | ad856f98872952ffc2235acc12ccc5942bbd6e5cae0df1771f063dc74526d24b |
| relationship.json | 6a994cad6400a07d3e2faaf2161a16818ac7784be651cdcb2d6c9b9439988cbd |
| verification-verdict.json | 0b3f8f4b061cff2992fc60a7c1f45dec6f803116894735751c40df3a8d356af9 |

Contract bundle revision c37d332e2daba95c9ef47adf00a82bc9c6a539ab62538f0ad857561e470d2989.

These are the child-to-parent direction. The parent-to-child revision request is a relay-owned
record with no schema here, because contract v1 defines none for that direction; see the relay's
[protocol-v1.md](../../docs/relay/protocol-v1.md).

The delivery-attempt copy now allows optional `runtime: {build, executable}` attribution.
Its current digest is listed above; the bundle revision records the original import.
Old attempts without the key still validate. The 16 existing attempt cases were re-judged
against the revised bytes with jsonschema 4.19.2 Draft7Validator and kept their verdicts;
12 additional recorded cases cover presence, absence, unknown executable and malformed
attribution. This changes the stored JSON contract, not the SQLite schema.

The completion-receipt copy now allows an optional `independentReview` item (what the child states
about its independent code review: the review artifact's path and sha256, status and reason, the
count of unusable reviewer calls, an optional `headPatchId`, and one disposition per finding), only
on a `ready_for_review` receipt. Old receipts without it still validate. The 18 existing receipt cases
were re-judged against the revised bytes with jsonschema 4.19.2 Draft7Validator and kept their
verdicts; 18 additional recorded cases cover the item stated, absent, on an execution-only receipt and
malformed. This changes the stored JSON contract, not the SQLite schema.

The relationship copy now allows one more `generations[].reason`: `accepted_result_correction`,
the reason a coordinator gives `generation-open` when it corrects a result that was accepted and is
still current (the relay's verdict writer refuses a second ruling on an accepted head, so the
generation is opened by hand and its reason is what notes the route). The enum previously allowed
only `initial_assignment` and `needs_changes_revision`, so a relationship opened by that route
serialized a reason the shipped contract refused. Old relationships without it still validate; a
generation with no reason (a returning tenure) still validates as null. The 6 existing relationship
cases were re-judged against the revised bytes with jsonschema 4.19.2 Draft7Validator and kept their
verdicts; 6 additional recorded cases cover each reason the registry accepts, the empty reason, and
an unknown and a mistyped one. This changes the stored JSON contract, not the SQLite schema.
