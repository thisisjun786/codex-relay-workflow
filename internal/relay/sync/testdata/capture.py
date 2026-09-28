"""Live sync.py oracle. Inputs, clock and claim tokens are shared with Go, not its outputs."""
import json
import sys
from pathlib import Path

root = Path(__file__).resolve().parents[4]
sys.path[:0] = [str(root / "packages/codex-session-relay/src"), str(root / "packages/codex-thread-bridge/src"), str(root / "packages/codex-session-relay")]
from codex_session_relay.store import Store
from codex_session_relay import sync
from codex_session_relay.errors import AckRefused

class Clock:
    def now(self): return 1700000000.0
    def iso(self): return "2023-11-14T22:13:20.000000+00:00"

def main():
    actions = json.loads(sys.argv[2])
    store = Store(Path(sys.argv[1]) / "relay.sqlite3")
    outbox = sync.SyncOutbox(store, Clock())
    replies = []
    identifiers = []
    tokens = []
    token_count = 0
    def token_hex(_):
        nonlocal token_count
        token_count += 1
        return f"{token_count:032x}"
    sync.secrets.token_hex = token_hex
    def document(a, row):
        block = sync.render_block(row)
        mode = a.get("document", "block")
        if mode == "absent": return "# Coordination\n\nnothing here yet\n"
        if mode == "duplicate": return block + "\n" + block
        if mode == "malformed": return block.rsplit("<!-- /relay-sync:", 1)[0]
        if mode == "split":
            head, _, tail = block.partition("revisionHash:")
            return head + "revisionHash: \n<!-- /relay-sync:" + row["sync_id"] + " -->\nsome other block mentioning revisionHash:" + tail
        if mode == "no-summary": return block.replace(row["summary"], "")
        if mode == "missing-header": return "\n".join(l for l in block.split("\n") if not l.startswith("relationshipId:"))
        if mode == "tampered-summary": return block.replace(row["summary"], "something else entirely")
        if mode == "legacy": block = block.replace("blockFormat: v2\n", ""); block = "\n".join(l for l in block.split("\n") if not l.startswith("summarySha256:")); block = block.replace("```text\n", "").replace("\n```\n", "\n")
        if mode == "fenced-legacy": block = block.replace("blockFormat: v2\n", ""); block = "\n".join(l for l in block.split("\n") if not l.startswith("summarySha256:"))
        for old, new in a.get("replace", []): block = block.replace(old, new)
        return block
    for a in actions:
        try:
            op = a["op"]
            identifier = identifiers[a.get("job", -1)] if identifiers else "unknown"
            if op == "target": result = outbox.set_target("rel-1", "coordination_document", a["ref"])
            elif op == "enqueue":
                with store.transaction() as db:
                    result = outbox.enqueue_in(db, relationship_id="rel-1", issue_key="REL-1", subject_kind=a.get("kind", "verdict"), summary=a.get("summary", "REL-1 · child · verified\ngeneration 1, revision r1"), event_id=a.get("event", "e1"), generation=1, revision=a.get("revision", "r1"), verdict=a.get("verdict", "verified"), criteria_digest=a.get("criteria"), ruling=a.get("ruling"))
                if result is not None: identifiers.append(result)
            elif op == "claim":
                result = outbox.claim(identifier, owner=a.get("owner", "main"), now=a.get("now", Clock().now()))
                tokens.append(result["claimToken"])
            elif op == "fail": result = outbox.fail(identifier, claim_token=tokens[a.get("token", -1)], error=a.get("error", "connector timed out"), now=a.get("now", Clock().now()))
            elif op == "retry": result = outbox.retry(identifier)
            elif op == "complete": result = outbox.complete(identifier, claim_token=tokens[a.get("token", -1)], target_ref=a.get("ref", "https://linear.app/example/document/coordination-000000000000"), readback=document(a, outbox.get(identifier)), external_ref=a.get("external"))
            elif op == "reconcile": result = outbox.reconcile(identifier, document(a, outbox.get(identifier)))
            elif op == "operation": result = outbox.operation(identifier)
            elif op == "parse": result = sync.parse_document(document(a, outbox.get(identifier)))
            elif op == "next": result = outbox.next(now=a.get("now", Clock().now()))
            elif op == "snapshot": result = outbox.snapshot()
            elif op == "real":
                from tests.linear_readback_fixtures import SYNC_ID, RAW_SUMMARY, MANGLED_BLOCK, CONFIRMED_BLOCK
                fields = sync.parse_document(CONFIRMED_BLOCK)["blocks"][SYNC_ID]["fields"]
                with store.transaction() as db:
                    db.execute("UPDATE sync_outbox SET sync_id=?,issue_key=?,relationship_id=?,event_id=?,execution_generation=?,revision_hash=?,verdict=?,identity_digest=?,summary=? WHERE sync_id=?", (SYNC_ID, fields["issueKey"],fields["relationshipId"],fields["eventId"],int(fields["executionGeneration"]),fields["revisionHash"],fields["disposition"],fields["identityDigest"],RAW_SUMMARY,identifier))
                identifiers[-1] = SYNC_ID
                block = MANGLED_BLOCK if a.get("mangled") else CONFIRMED_BLOCK
                for old,new in a.get("replace",[]): block=block.replace(old,new)
                result = {"reconcile":outbox.reconcile(SYNC_ID,block),"parse":sync.parse_document(block),"render":sync.render_block(outbox.get(SYNC_ID))}
            else: raise ValueError(op)
            replies.append(result)
        except AckRefused as error:
            replies.append({"error":"refused","reason":error.reason.value,"detail":str(error)})
    tables = {}
    # Every table written by these commands, including the shared journal.
    for table in ("sync_targets", "sync_outbox", "journal"):
        tables[table] = [dict(r) for r in store.all(f"SELECT * FROM {table}")]
    print(json.dumps({"replies":replies,"tables":tables}))
    store.close()

if __name__ == "__main__": main()
