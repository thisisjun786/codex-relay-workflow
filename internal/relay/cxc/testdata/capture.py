"""Whole-output oracle for the public cxc seam, not a replacement implementation."""
import dataclasses
import json
import sys
from codex_session_relay import cxc

results = []
for case in json.load(sys.stdin):
    try:
        if case['function'] == 'constants':
            result = {key: getattr(cxc, key) for key in case['args'][0]}
        else:
            result = getattr(cxc, case['function'])(*case.get('args', []), **case.get('kwargs', {}))
        results.append({'result': result})
    except Exception as error:
        results.append({'error': type(error).__name__, 'message': str(error)})
print(json.dumps(results, default=lambda value: dataclasses.asdict(value)))
