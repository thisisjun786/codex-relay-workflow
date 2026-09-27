"""Run the real CLI with deterministic boundary inputs, without changing output."""
import sys
from codex_session_relay import cli, faults, faultsweep
from codex_session_relay.clock import FakeClock

cli.SystemClock = lambda: FakeClock(start=100000)
faults.secrets.token_hex = lambda size=None: bytes(range(32 if size is None else size)).hex()
faultsweep._now = lambda store: FakeClock(start=100000).iso()
raise SystemExit(cli.main(sys.argv[1:]))
