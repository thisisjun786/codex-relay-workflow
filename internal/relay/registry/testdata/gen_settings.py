"""Record settings.TaskSettings answers for a fixed table of rows (settings_cases.json).

  uv run --no-sync python internal/relay/registry/testdata/gen_settings.py \
      > internal/relay/registry/testdata/python_settings.json
Each case: {"row", optional "response", optional "transmitted"/"loadedBefore"}.
"""
import json
import sys

from codex_session_relay.errors import RelayError
from codex_session_relay.settings import TaskSettings

cases = json.load(open("internal/relay/registry/testdata/settings_cases.json"))
out = {}
for name, case in cases.items():
    settings = TaskSettings(case["row"])
    answer = {"missing": settings.missing()}
    try:
        settings.require_usable()
        answer["usable"] = None
    except RelayError as error:
        answer["usable"] = {"reason": error.reason.value, "detail": error.detail}
    if answer["usable"] is None:
        answer["resumeParams"] = settings.resume_params("t-1")
    if "response" in case:
        answer["mismatches"] = settings.mismatches(
            case["response"], transmitted=case.get("transmitted", True),
            loaded_before=case.get("loadedBefore", False))
        if case.get("narrowing"):
            answer["narrowing"] = settings.roots_narrowing(case["response"], status_before="idle")
    out[name] = answer
json.dump(out, sys.stdout, indent=1, sort_keys=True)
sys.stdout.write("\n")
