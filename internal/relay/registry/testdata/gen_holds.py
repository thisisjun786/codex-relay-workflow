"""Record settings.settings_hold_recovery for every kind, code, source and revision.

  uv run --no-sync python internal/relay/registry/testdata/gen_holds.py \
      > internal/relay/registry/testdata/python_holds.json
"""
import json
import sys

from codex_session_relay import settings

KINDS = ["withheld", "capped", "channel_closed"]
SOURCES = ["attempt", "pre_send", "undetermined"]
CODES = [settings.SETTINGS_NOT_PRESERVED, settings.SETTING_UNOBSERVABLE, settings.ENVIRONMENTS_UNKNOWN,
         settings.UNVERIFIABLE_PERMISSION_PROFILE, settings.SETTINGS_DIFFER_AFTER_LOAD,
         settings.UNSUPPORTED_APPROVAL_POLICY, settings.SETTINGS_UNAVAILABLE, settings.SETTINGS_INCOMPLETE,
         "settings_mistyped", settings.UNSUPPORTED_SANDBOX_TYPE, "role_policy_unconfigured",
         "role_binding_mismatch", "settings_record_stale_for_role", None]
out = []
for kind in KINDS:
    for source in SOURCES:
        for code in CODES:
            for revision in (False, True):
                out.append({"kind": kind, "source": source, "code": code, "revision": revision,
                            "recovery": settings.settings_hold_recovery(kind, code, source, revision=revision)})
json.dump(out, sys.stdout, indent=1)
sys.stdout.write("\n")
