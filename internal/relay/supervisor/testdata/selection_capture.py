"""Read a test store through Python's real supervision.select without writing."""
import json
import sys
from codex_session_relay import supervision
from codex_session_relay.store import Store

store = Store(sys.argv[1])
try:
    mode = sys.argv[5] if len(sys.argv) > 5 else "select"
    obligation = None if mode in ("event", "select_event") else (
        json.loads(sys.argv[2]) if sys.argv[2] != "<none>" else [])
    recipient = None if sys.argv[3] == "<none>" else sys.argv[3]
    now = None if sys.argv[4] == "<none>" else float(sys.argv[4])
    if mode == "select_event":
        from codex_session_relay import report
        event_id = sys.argv[2]
        obligation = supervision.from_event(store, event_id, report.read(store, event_id))
        if obligation is None:
            answer = supervision.suppressed(event_id, "this event is not a completion, a new block or a decision the user owes")
        else:
            answer = {**supervision.select(store, obligation, recipient=recipient, now=now),
                      "obligation": obligation}
        print(json.dumps(answer))
    elif mode == "event":
        from codex_session_relay import report
        print(json.dumps(supervision.from_event(store, sys.argv[3], report.read(store, sys.argv[3]))))
    elif mode == "standing":
        print(json.dumps(supervision.standing_for(store, None, recipient, observations=obligation)))
    elif mode == "status":
        from codex_session_relay.assignment import AssignmentView
        from codex_session_relay.registry import Registry
        from codex_session_relay.linkage import Linkage
        from codex_session_relay.clock import SystemClock
        clock = SystemClock()
        linkage = Linkage(store, clock)
        print(json.dumps(supervision.status_answer(store, linkage,
            AssignmentView(store, Registry(store, clock), clock, linkage=linkage),
            recipient, observations=obligation)))
    elif mode == "envelope":
        from codex_session_relay import envelope
        try:
            value = supervision.envelope_for(
                store, obligation, sender="01parent-task", recipient="01supervisor-task",
                scope="project PRJ-1, issue REL-1",
                observed_at="2023-11-14T22:13:20.000000+00:00",
                decision=sys.argv[3] if sys.argv[3] != "<none>" else None)
            print(json.dumps({"value": value, "error": None}))
        except envelope.EnvelopeRefused as exc:
            print(json.dumps({"value": None, "error": str(exc)}))
    else:
        print(json.dumps(supervision.select(store, obligation, recipient=recipient, now=now)))
finally:
    store.close()
