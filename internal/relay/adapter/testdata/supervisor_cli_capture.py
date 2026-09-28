import json
import secrets
import sys

from codex_session_relay import cli, supervisorchannel
from codex_session_relay.clock import FakeClock

# Match the built Go fixture's deterministic process seams while retaining the real
# Python CLI, BridgeHostAdapter, RPC transport, store, and supervisor channel.
_original_init = cli.Services.__init__
def _init(self, args):
    _original_init(self, args)
    self.clock = FakeClock(1700000000)
cli.Services.__init__ = _init
secrets.token_hex = lambda count: "00" * count
supervisorchannel.relay_program = lambda: tuple(json.loads(sys.argv[1]))
raise SystemExit(cli.main(json.loads(sys.argv[2])))
