"""Record the Python relay CLI's whole dispositions-show answer for every cli-shape fixture of
test_dispositions that needs only a store (no emit, no host).

  uv run --no-sync python internal/relay/registry/testdata/gen_dispositions.py \
      > internal/relay/registry/testdata/python_dispositions.json

Each fixture runs through contract/runner/cli.run (the real Python Store for given.sql_seed, the
real `python -m codex_session_relay.cli` for each step). Recorded per step: exit, stdout with the
case directory replaced by <HOME>. The Go test replays the same fixture through the built crw.
"""
import json
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, ".")
from contract.runner.cli import run  # noqa: E402

FIXTURES = Path("contract/fixtures/cli-shape")
out = {}
for path in sorted(FIXTURES.glob("test_dispositions__*.json")):
    case = json.loads(path.read_text())
    steps = case["run"].get("steps", [case["run"]])
    if "host" in case.get("given", {}) or any(s["argv"][0] != "dispositions-show" for s in steps):
        continue
    with tempfile.TemporaryDirectory() as raw:
        home = Path(raw)
        actual = run(case, home)
        out[path.name] = {key: {"exit": value["exit"], "stdout": value["stdout"].replace(str(home), "<HOME>")}
                          for key, value in actual["steps"].items()}
json.dump(out, sys.stdout, indent=1, sort_keys=True)
sys.stdout.write("\n")
