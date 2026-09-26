"""ORD-6..ORD-9 through the real Python adapter and supervisor channel. argv: <tree> <mode>."""
import json, sys, tempfile
TREE, MODE = sys.argv[1], sys.argv[2]
tempfile.mkdtemp = lambda prefix=None: TREE
from tests import test_bridge_adapter as seam
from tests.test_on_request_delivery import record_based, RealAdapterRoutes, OnRequestSupervisor
from codex_session_relay.transport import classify_operation_receipt
from codex_session_relay import supervisorchannel as channel_module
out = {}
def keep(receipt):
    return {k: receipt.get(k) for k in ("status", "rpcError", "settingsNotes", "settingsFindings", "error", "statusBeforeResume")}
if MODE in ("transmitted", "settings_free", "untrusted"):
    case = RealAdapterRoutes("test_untrusted_stays_stored_not_woken"); case.setUp()
    policy = "untrusted" if MODE == "untrusted" else "on-request"
    adapter, calls = case._adapter(resume=seam.authorized_resume_response(approvalPolicy=policy))
    settings = record_based() if MODE == "settings_free" else seam.AUTHORIZED
    receipt = adapter.send_message("del-a1c000000000-" + MODE[:2], "thread-1", "hi", settings)
    out["receipt"] = keep(receipt)
    out["methods"] = [m for m, _ in calls]
    out["resumes"] = [p for m, p in calls if m == "thread/resume"]
    out["delivery_state"] = classify_operation_receipt(receipt).delivery_state
    out["settings"] = settings.data
    out["settingsFree"] = settings.settings_free_resume
    out["resumed"] = seam.authorized_resume_response(approvalPolicy=policy)
    case.doCleanups()
else:
    case = OnRequestSupervisor("test_an_on_request_supervisor_receives_the_push"); case.setUp()
    from tests.test_supervisor_channel import SUPERVISOR
    if MODE == "sup_untrusted":
        case.adapter.threads[SUPERVISOR].approval_policy = "untrusted"
        _one, mid = case.staged()
        out["record"] = case.channel.attempt(mid, case.adapter)
        out["state"] = case.channel.get(mid)["state"]
    elif MODE == "sup_on_request":
        case.adapter.threads[SUPERVISOR].approval_policy = "on-request"
        _one, mid = case.staged()
        out["record"] = case.channel.attempt(mid, case.adapter)
        out["state"] = case.channel.get(mid)["state"]
    else:
        case.adapter.threads[SUPERVISOR].approval_policy = "on-request"
        existing = case.adapter.start_turn(SUPERVISOR, status="inProgress")
        out["turnStartedAt"] = existing.started_at
        case.clock.advance(600)
        _one, mid = case.staged()
        case.adapter.script("steer_existing")
        out["record"] = case.channel.attempt(mid, case.adapter)
        out["sentAt"] = case.store.one("SELECT transport_started_at FROM supervisor_attempts WHERE message_id = ? ORDER BY attempt_no DESC LIMIT 1", (mid,))["transport_started_at"]
        out["verified"] = case.read_back(mid, existing.turn_id)["verified"]
        out["state"] = case.channel.get(mid)["state"]
        before = len(case.adapter.sends); case.clock.advance(100000)
        case.channel.attempt(mid, case.adapter, now=case.clock.now())
        out["resent"] = len(case.adapter.sends) - before
    case.doCleanups()
print(json.dumps(out, default=repr))
