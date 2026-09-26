"""Record the caller-visible words and prose assignment.py, dispositions.py and rolepolicy.py
emit outside errors.RefusalReason, straight from the Python modules.

  uv run --no-sync python internal/relay/registry/testdata/gen_words.py \
      > internal/relay/registry/testdata/python_words.json
"""
import json
import sys

from codex_session_relay import assignment as a, dispositions as d, rolepolicy as r
from codex_session_relay.settings import SETTINGS_DIFFER_AFTER_LOAD

json.dump({
    "NEXT_ACTION": a.NEXT_ACTION,
    "LIFECYCLE_WITHHOLD_SOURCE": a.LIFECYCLE_WITHHOLD_SOURCE,
    "PARENT_RECOVERY_THEN": a.PARENT_RECOVERY_THEN,
    "CORRECTION_ACTIONS": [a.CORRECTION_UNSENT_ACTION, a.CORRECTION_UNCONFIRMED_ACTION,
                           a.CORRECTION_HELD_ACTION, a.CORRECTION_ANSWERED_ACTION],
    "OBSERVATION_BY_STATE": d.OBSERVATION_BY_STATE,
    "OBSERVATION_DETAIL": d.OBSERVATION_DETAIL,
    "UNMEASURED_DETAIL": d.UNMEASURED_DETAIL,
    "RECIPIENT_UNMEASURED_DETAIL": d.RECIPIENT_UNMEASURED_DETAIL,
    "DISPOSITION_READING_LIMITS": d.DISPOSITION_READING_LIMITS,
    "EXECUTION_ONLY": list(d.EXECUTION_ONLY),
    "RECOVERY": r.RECOVERY,
    "SETTINGS_DIFFER_AFTER_LOAD": SETTINGS_DIFFER_AFTER_LOAD,
}, sys.stdout, indent=1, sort_keys=True)
sys.stdout.write("\n")
