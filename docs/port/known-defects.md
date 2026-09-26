# Python defects not carried over

- **Python defect not carried over:** `daemon._gate` fingerprint omits the receipt turn id (`daemon.py:~993`), so an accepted receipt that gains a turn id later is never reconciled; Go includes it (`TestReconcilePass_receipt_gains_turn_id`).

- **Python defect not carried over:** `registry.bind_anchor` (`registry.py:790-822`) reads the pending anchor before beginning its transaction. Concurrent binders can overwrite a generation's dispatch turn after one has already succeeded, violating the never-rebind invariant. Go reads and decides inside the write transaction; `TestBindAnchor_concurrent_turns_never_replace_the_winner` proves only one distinct turn binds and the loser is refused.
- **Python defect not carried over:** `ack.bind_dispatched_revision` (`ack.py:1026-1035`) rejects acknowledged revisions even though `ack.bind_pending_anchors` (`ack.py:1037-1060`) selects them for recovery. This violates the invariant that every dispatched revision's pending anchor can recover after acknowledgement. Go accepts both states; `TestBindPendingAnchors_recovers_acknowledged_revision` proves the recovery binds and is idempotent.
