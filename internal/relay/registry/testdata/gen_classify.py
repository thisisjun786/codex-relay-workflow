"""Record transport.classify_operation_receipt for every resume refusal code.

  uv run --no-sync python internal/relay/registry/testdata/gen_classify.py \
      > internal/relay/registry/testdata/python_classify.json
"""
import json
import sys

from codex_session_relay.transport import classify_operation_receipt

out = {}
for code in ("settings_not_preserved", "setting_unobservable", "environments_unknown",
             "unverifiable_permission_profile", "settings_differ_after_load",
             "unsupported_approval_policy", "unknown", "thread_busy"):
    facts = classify_operation_receipt({
        "requestId": "del-000000000000-a1", "status": "failed",
        "resumed": {"approvalPolicy": "never"}, "error": f"thread/resume: {code}",
        "rpcError": {"code": code, "message": code}})
    out[code] = {"deliveryState": facts.delivery_state, "sendAttempted": facts.send_attempted,
                 "retrySafe": facts.retry_safe, "failedOperation": facts.failed_operation}
json.dump(out, sys.stdout, indent=1, sort_keys=True)
sys.stdout.write("\n")
