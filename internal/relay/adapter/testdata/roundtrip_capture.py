import json
import sys
from unittest.mock import patch
from codex_session_relay import cli
from codex_session_relay.clock import FakeClock
spec=json.load(sys.stdin)
with patch.object(cli,'SystemClock',FakeClock),patch('codex_thread_bridge.ledger.time.time',return_value=1700000000.0):
    raise SystemExit(cli.main(spec['argv']))
