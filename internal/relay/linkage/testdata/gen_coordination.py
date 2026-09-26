"""Python-source oracle for coordination.py, run only in the test-generation workflow."""
import json
import tempfile
from pathlib import Path
from codex_session_relay.coordination import exact, derive, Refusal, Conflicts, DOMAIN_MERGE_TARGET
from codex_session_relay.errors import CoordinationError, RefusalReason
from codex_session_relay.store import Store

values = ["alpha", " ", "", "a|b", "\u00e9", "a'b", "line\nbreak", 3, None]
answers = []
for value in values:
    try:
        answers.append({"input": value, "output": exact(value, "a target")})
    except CoordinationError as err:
        answers.append({"input": value, "reason": err.reason.value, "detail": err.detail})

with tempfile.TemporaryDirectory(dir="/dev/shm") as tmp:
    store = Store(Path(tmp) / "relay.sqlite3")
    conflicts = Conflicts(store)
    refusal = Refusal(RefusalReason.UNREGISTERED_SCOPE, "target must not contain '|'", domain=DOMAIN_MERGE_TARGET, subject="dev", challenger="other")
    refusal2 = Refusal(RefusalReason.UNREGISTERED_SCOPE, "new detail", domain=DOMAIN_MERGE_TARGET, subject="dev", challenger="other")
    with store.transaction() as db:
        conflicts.record_in(db, refusal, at="2023-11-14T22:13:20Z")
    with store.transaction() as db:
        conflicts.record_in(db, refusal2, at="2023-11-14T22:13:21Z")
    result = {"exact": answers, "ids": [derive("mrg", "owner/repo", "dev"), derive("mrg", "owner", "repo|dev"), derive("mrg", "\u00e9", "dev")], "record": refusal.to_record(), "error": {"reason": refusal.error().reason.value, "detail": refusal.error().detail}, "conflicts": conflicts.all(DOMAIN_MERGE_TARGET, "dev")}
    store.close()
print(json.dumps(result, ensure_ascii=True))
