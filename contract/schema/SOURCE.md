# Frozen contract schemas

Byte copies of the five schemas from the cross-session communication contract v1. The Python
relay carried the same five under `packages/codex-session-relay/src/codex_session_relay/schema/`
with this note until todo 44 removed its source; the copies here are the ones that remain.

| File | sha256 |
|---|---|
| acknowledgement.json | 193c2a1dbdb6197f852aaa38c6b8b4e55ba804ffc66e7b2a73925365b133eb7c |
| completion-receipt.json | 8111438e60b46b209a33902dd9080426953dfaaf2a7025cb47c2336d72b49317 |
| delivery-attempt.json | ad856f98872952ffc2235acc12ccc5942bbd6e5cae0df1771f063dc74526d24b |
| relationship.json | c8ebaf4559fa1ac6df26d98c4214938caf78bd8c61659bce026da6a3595a4b90 |
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
