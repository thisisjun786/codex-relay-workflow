#!/usr/bin/env bash
set -euo pipefail
real="${GIT_REAL:?}"
if [[ "${1:-}" == push && "${2:-}" == origin && "${3:-}" == *:refs/heads/main ]]; then
  action=$(python3 - "$GH_STATE" <<'PY'
import json, sys
from pathlib import Path
data = json.loads(Path(sys.argv[1], "push.json").read_text())
if data.get("fail_main"):
    print("fail")
elif data.get("lie_main"):
    print("lie")
else:
    print("ok")
PY
)
  if [[ "$action" == fail ]]; then
    echo 'remote rejected main update' >&2
    exit 1
  fi
  if [[ "$action" == lie ]]; then
    "$real" "$@"
    # Report a different commit after a push that did update the ref.
    exit 0
  fi
fi
if [[ "${1:-}" == ls-remote && "${2:-}" == origin && "${3:-}" == refs/heads/main ]]; then
  action=$(python3 - "$GH_STATE" <<'PY'
import json, sys
from pathlib import Path
data = json.loads(Path(sys.argv[1], "push.json").read_text())
print("lie" if data.get("lie_main") else "ok")
PY
)
  if [[ "$action" == lie ]]; then
    printf '%s\trefs/heads/main\n' 0000000000000000000000000000000000000000
    exit 0
  fi
fi
exec "$real" "$@"
