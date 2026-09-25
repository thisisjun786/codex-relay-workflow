"""Checkout completion hook driver."""

import json
import os
import re
import stat
import subprocess
import time
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
FIXTURES = ROOT / "contract" / "fixtures"
sys.path.insert(0, str(ROOT / "scripts"))
from crw_runtime import completion

from .core import STOP


def run_scenario(path, tmp_path):
    """Drive the checkout hook with a case-local fake relay and isolated Codex home."""
    case = json.loads(path.read_text(encoding="utf-8"))
    ident = path.stem
    given = case["given"]
    relay = tmp_path / "codex-session-relay"
    if "relay" in given:
        response = given["relay"]
        relay.write_text(
            "#!/usr/bin/env python3\nimport sys, json\n"
            "from pathlib import Path\n"
            f"Path({str(tmp_path / 'seen.json')!r}).write_text(json.dumps({{'argv': sys.argv[1:], 'stdin': sys.stdin.read()}}))\n"
            f"import time, os, signal\n"
            f"time.sleep({response.get('delay', 0)!r})\n"
            f"{'os.kill(os.getpid(), signal.SIGKILL)' if response.get('signal') else ''}\n"
            f"sys.stdout.write({response.get('stdout', '')!r})\n"
            f"raise SystemExit({response.get('exit', 0)!r})\n", encoding="utf-8")
        relay.chmod(relay.stat().st_mode | stat.S_IXUSR)
    if given.get("settings", True):
        settings = completion.configuration(relay=str(relay), marker_root=str(tmp_path / "marker"),
                                            journal_root=str(tmp_path / "journal"),
                                            codex_home=str(tmp_path), issue="CRW-37")
        settings.update({key: value.replace("${HOME}", str(tmp_path)) if isinstance(value, str) else value
                         for key, value in given.get("settings_overrides", {}).items()})
        completion.configuration_path(tmp_path).write_text(json.dumps(settings), encoding="utf-8")
    for relative, contents in given.get("files", {}).items():
        contents = contents.replace("${PYTHON}", sys.executable).replace("${ENTRY}", str(ROOT / "scripts" / "completion_hook.py")).replace("${HOME}", str(tmp_path))
        destination = tmp_path / relative
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_text(contents, encoding="utf-8")
    for relative, target in given.get("symlinks", {}).items():
        destination = tmp_path / relative
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.symlink_to(target.replace("${HOME}", str(tmp_path)))
    for relative, mode in given.get("modes", {}).items():
        (tmp_path / relative).chmod(mode)
    action = case["run"]
    payload = action.get("stdin", STOP)
    raw = json.dumps(payload).encode() if not isinstance(payload, str) else payload.encode()
    env = {**os.environ, "HOME": str(tmp_path), "XDG_STATE_HOME": str(tmp_path / 'state'),
           "XDG_DATA_HOME": str(tmp_path / 'data'), "XDG_CONFIG_HOME": str(tmp_path / 'config'),
           "CODEX_HOME": str(tmp_path), **action.get("env", {})}
    argv = [arg.replace("${HOME}", str(tmp_path)) for arg in action.get("argv", [])]
    cwd = action.get("cwd", str(tmp_path)).replace("${HOME}", str(tmp_path))
    started = time.monotonic()
    if action["kind"] == "entry":
        done = subprocess.run([sys.executable, str(ROOT / "scripts" / "completion_hook.py"),
                               *argv], input=raw, capture_output=True,
                              timeout=60, check=False, env=env, cwd=cwd)
        exit_code, stdout, stderr = done.returncode, done.stdout.decode(), done.stderr.decode()
    elif action["kind"] == "hook":
        answer = completion.run(raw, codex_home=str(tmp_path), environ=env,
                                settings=action.get("settings"))
        exit_code, stdout, stderr = 0, answer or "", ""
    elif action["kind"] == "status" and action.get("document"):
        document = completion.configuration(relay=action.get("relay", "/opt/relay"),
                                            marker_root=action.get("marker_root", "/markers"),
                                            codex_home=str(tmp_path), environ=env,
                                            socket=action.get("socket"))
        exit_code, stdout, stderr = 0, json.dumps(document), ""
    elif action["kind"] == "status":
        exit_code, stdout, stderr = 0, "", ""
    elif action["kind"] == "install":
        done = subprocess.run([sys.executable, str(ROOT / "scripts" / "runtime_install.py"),
                               "hook", "--codex-home", str(tmp_path), *argv],
                              capture_output=True, timeout=60, check=False, env=env, cwd=cwd)
        exit_code, stdout, stderr = done.returncode, done.stdout.decode(), done.stderr.decode()
    else:
        pytest.fail(f"{ident}: unknown run kind {action['kind']}")
    elapsed = time.monotonic() - started
    records = [json.loads(entry.read_text(encoding="utf-8"))
               for day in sorted((tmp_path / "journal").glob("*"))
               if day.is_dir() and re.fullmatch(r"[0-9]{8}", day.name)
               for entry in sorted(day.glob("*.json"))
               if re.fullmatch(r"[0-9a-f]{32}\.json", entry.name)]
    actual: dict = {"exit": exit_code, "stdout": stdout, "stderr": stderr, "rows": records,
              "elapsed": elapsed, "stdout_json": json.loads(stdout) if stdout else None,
              "status": completion.status(codex_home=str(tmp_path), environ=env)
              if action.get("status") or action["kind"] == "status" else {},
              "files": {name: (tmp_path / name).exists() for name in case["expect"].get("files", {})}}
    if (tmp_path / "seen.json").exists():
        actual["call"] = json.loads((tmp_path / "seen.json").read_text(encoding="utf-8"))
    else:
        actual["call"] = None
    expected = case["expect"]
    actual["observed"] = {name: {"exists": (tmp_path / name).exists(),
        "bytes": (tmp_path / name).read_bytes().hex() if (tmp_path / name).is_file() else None,
        "mode": stat.S_IMODE((tmp_path / name).stat().st_mode) if (tmp_path / name).exists() else None,
        "target": os.readlink(tmp_path / name) if (tmp_path / name).is_symlink() else None}
        for name in expected.get("observe", [])}
    assert exit_code == expected["exit"], f"{ident}: expected exit {expected['exit']}, got {exit_code}"
    if "stdout_json" in expected:
        assert json.loads(stdout) == expected["stdout_json"], f"{ident}: stdout JSON mismatch"
    for name, present in expected.get("files", {}).items():
        assert actual["files"][name] == present, f"{ident}: file {name} presence mismatch"
    from .core import evaluate
    evaluate(actual, expected.get("checks", []), ident)
