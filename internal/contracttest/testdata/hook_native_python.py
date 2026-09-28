"""Run the original fixture through the real Python adapter for native parity."""
import json
from pathlib import Path
import sys

fixture, home, root = map(Path, sys.argv[1:])
sys.path.insert(0, str(root))
from contract.runner import files

case = json.loads(fixture.read_text())
# The completion-hook fixture's default Stop is the hook runner's, while the
# stop-adapter fixture drives its captured transcript explicitly.
if case['run']['kind'] == 'hook' and 'stdin' not in case['run']:
    from contract.runner.core import STOP
    case['run']['stdin'] = STOP
case['run']['settings'] = str(home / 'settings.json')
result = files.run(case, home)
for outcome in result['outcomes']:
    outcome['stdout_json'] = json.loads(outcome['stdout']) if outcome.get('stdout') else None
result['host_claims'] = {p.name: json.loads(p.read_text())
                         for p in sorted((home / 'crw-completion-hook/stop-events').glob('*.json'))}
# The two runners' settings filenames differ; replay with one literal path shape
# for the only record field that names the settings file, not arbitrary prose.
for row in result['rows']:
    row['configuration'] = str(home / 'crw-completion-hook.json')
print(json.dumps(result))
