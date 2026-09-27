import json, sys
from codex_session_relay import omitted

facts=json.loads(sys.argv[1])
print(json.dumps(omitted.classify(facts),sort_keys=True,separators=(",",":")))
