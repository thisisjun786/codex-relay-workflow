# Frozen contract schemas

Byte copies of the five schemas from the cross-session communication contract v1.

| File | sha256 |
|---|---|
| acknowledgement.json | 193c2a1dbdb6197f852aaa38c6b8b4e55ba804ffc66e7b2a73925365b133eb7c |
| completion-receipt.json | 8111438e60b46b209a33902dd9080426953dfaaf2a7025cb47c2336d72b49317 |
| delivery-attempt.json | e821647e35bee9179321332d8b7df06f0fc61f2da49c7b74fb6128b8802650d1 |
| relationship.json | c8ebaf4559fa1ac6df26d98c4214938caf78bd8c61659bce026da6a3595a4b90 |
| verification-verdict.json | 0b3f8f4b061cff2992fc60a7c1f45dec6f803116894735751c40df3a8d356af9 |

Contract bundle revision c37d332e2daba95c9ef47adf00a82bc9c6a539ab62538f0ad857561e470d2989.

These are the child-to-parent direction. The parent-to-child revision request is a relay-owned
record with no schema here, because contract v1 defines none for that direction; see
docs/protocol-v1.md.
