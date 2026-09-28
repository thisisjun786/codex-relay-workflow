import json
import sys
from codex_session_relay.manifest import verify_frozen_detailed

try:
    result = verify_frozen_detailed(sys.argv[1], json.loads(sys.argv[2]) if len(sys.argv) > 2 else None)
    print(json.dumps({"result": result}))
except Exception as error:
    print(json.dumps({"error": type(error).__name__, "detail": str(error)}))
