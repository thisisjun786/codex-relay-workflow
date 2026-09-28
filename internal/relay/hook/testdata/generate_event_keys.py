"""Regenerate byte-goldens, including lone surrogates, from Python event_key."""
import itertools
import json
from pathlib import Path

from codex_session_relay.stopadapter import EVENT_KEY_TAG, event_key

values = ['s', '', '\uD55C\uAE00', '\U0001D11E\U0001F600', 'a\x00b', '\\"\n', '<>&',
          '\u007f', '\u2028', 'r\u00e9sum\u00e9']
rows = []
for index, (session, turn) in enumerate(itertools.product(values, repeat=2)):
    parts = [session, turn, index % 2 == 0, values[(index + 3) % len(values)] + str(index)]
    rows.append({'values': parts,
                 'bytes': json.dumps([EVENT_KEY_TAG, *parts], separators=(',', ':')),
                 'digest': event_key(*parts)})
# High and low surrogates at each boundary, mixed with paired astral characters.
# json.loads combines valid surrogate pairs; Python strings below contain genuine
# lone code points, not the six literal characters of an escape spelling.
for surrogate in ['\ud800', '\udbff', '\udc00', '\udfff']:
    for value in [surrogate, surrogate + 'tail', 'mid' + surrogate + 'dle',
                  'head' + surrogate, '\U0001f600' + surrogate + '\U0001d11e']:
        for field in [0, 1, 3]:
            parts = ['session', 'turn', False, 'item']
            parts[field] = value
            rows.append({'values': parts,
                         'bytes': json.dumps([EVENT_KEY_TAG, *parts], separators=(',', ':')),
                         'digest': event_key(*parts)})
Path(__file__).with_name('event_keys.json').write_text(
    json.dumps(rows, ensure_ascii=True, indent=2) + '\n', encoding='utf-8')
