"""Stop adapter process and filesystem observations."""

import json
import os
import re
import stat
import subprocess
import sys
from pathlib import Path

from .core import ROOT


def observe(path):
    return {"exists": path.exists(), "bytes": path.read_bytes().hex() if path.is_file() else None,
            "mode": stat.S_IMODE(path.stat().st_mode) if path.exists() else None,
            "target": os.readlink(path) if path.is_symlink() else None,
            "entries": sorted(child.name for child in path.iterdir()) if path.is_dir() else None}


def isolated_env(tmp_path):
    return {**os.environ, "HOME": str(tmp_path), "XDG_STATE_HOME": str(tmp_path / "state"),
            "XDG_DATA_HOME": str(tmp_path / "data"), "XDG_CONFIG_HOME": str(tmp_path / "config"),
            "CODEX_HOME": str(tmp_path)}


def expand(value, tmp_path):
    if isinstance(value, str):
        return value.replace("${HOME}", str(tmp_path))
    if isinstance(value, list):
        return [expand(item, tmp_path) for item in value]
    if isinstance(value, dict):
        return {key: expand(item, tmp_path) for key, item in value.items()}
    return value


def run(case, tmp_path):
    given = case.get("given", {})
    for name, content in given.get("files", {}).items():
        path = tmp_path / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content.replace("${HOME}", str(tmp_path)), encoding="utf-8")
    for name, target in given.get("symlinks", {}).items():
        (tmp_path / name).symlink_to(expand(target, tmp_path))
    for name, mode in given.get("modes", {}).items():
        (tmp_path / name).chmod(mode)
    relay = tmp_path / "relay"
    if "relay" in given:
        response = given['relay']
        relay.write_text("#!/usr/bin/env python3\nimport sys,json,os,signal,time\n"
                         "from pathlib import Path\n"
                         "raw = sys.stdin.buffer.read()\n"
                         f"with Path({str(tmp_path / 'guard-calls')!r}).open('a') as log:\n"
                         "    log.write(json.dumps({'argv':sys.argv[1:], 'stdin':raw.decode()}) + '\\n')\n"
                         f"if {response.get('die_first', False)!r} and len(Path({str(tmp_path / 'guard-calls')!r}).read_text().splitlines()) == 1:\n"
                         "    os.kill(os.getppid(), signal.SIGKILL)\n"
                         f"time.sleep({response.get('delay', 0)!r})\n"
                         f"{'os.kill(os.getpid(), signal.SIGKILL)' if response.get('signal') else ''}\n"
                         f"sys.stdout.write({response.get('stdout', '')!r})\n"
                         f"raise SystemExit({response.get('exit', 0)})\n")
        relay.chmod(0o755)
    settings = tmp_path / "settings.json"
    if given.get("settings", True):
        settings.write_text(json.dumps({
            "configVersion": 1, "event": "Stop", "relayExecutable": str(relay),
            "markerRoot": str(tmp_path / "marker"), "dbPath": None, "mode": "observe",
            "timeoutSeconds": 5, "journalRoot": str(tmp_path / "journal"),
            "journalPolicy": "every_invocation", "installedBy": "CRW-115",
            "isolationAssertedBy": None, "owner": "plugin",
            "adapterInterpreter": sys.executable,
            "adapterEntryPoint": str(ROOT / "packages/codex-session-relay/src/codex_session_relay/stopadapter.py"),
            **expand(given.get("settings_overrides", {}), tmp_path)}))
    if given.get("transcript_r1"):
        source = ROOT / "packages/codex-session-relay/tests/fixtures/stop_event_r1.json"
        given["r1"] = json.loads(source.read_text())
    action = case["run"]
    adapter = ROOT / "packages/codex-session-relay/src/codex_session_relay/stopadapter.py"
    outcomes = []
    for step in action.get("steps", [action]):
        kind = step.get("kind", "stop")
        if "transcript_stop" in step:
            index = step["transcript_stop"]
            source = given["r1"]
            transcript = tmp_path / "transcript.jsonl"
            lines = source["transcriptLines"][:source["stops"][index]["linesAtStop"]]
            transcript.write_text("\n".join(line.replace("<CODEX_HOME>", str(tmp_path))
                                              for line in lines) + "\n")
            payload = source["stops"][index]["payload"]
            step = {**step, "stdin": {**payload, "transcript_path": str(transcript),
                                       "cwd": str(tmp_path)}}
        if kind == "verify":
            argv = [expand(arg, tmp_path) for arg in step.get("argv", [])]
            done = subprocess.run([sys.executable, str(ROOT / "scripts/stop_events.py"), *argv],
                                  capture_output=True, timeout=step.get("timeout", 60),
                                  env=isolated_env(tmp_path), check=False)
            outcomes.append({"exit": done.returncode, "stdout": done.stdout.decode(),
                             "stderr": done.stderr.decode(), "stdout_json": json.loads(done.stdout),
                             "signal": None, "timeout": False})
            continue
        if kind == "mutate":
            root = tmp_path / expand(step.get("root", "journal"), tmp_path)
            role = step["role"]
            patterns = {"host": "crw-completion-hook/stop-events/*.json",
                        "claim": "journal/accepted/[0-9a-f]*.json",
                        "outcome": "journal/accepted/*.outcome.json",
                        "accepted": "journal/[0-9]???????/*.json",
                        "duplicate": "journal/[0-9]???????/*.json",
                        "row": "journal/[0-9]???????/*.json"}
            matches = sorted(root.glob(patterns[role]))
            target = matches[step.get("index", 0)]
            match step["operation"]:
                case "set":
                    body = json.loads(target.read_text())
                    body[step["field"]] = expand(step["value"], tmp_path)
                    target.write_text(json.dumps(body))
                case "pop":
                    body = json.loads(target.read_text())
                    body.pop(step["field"])
                    target.write_text(json.dumps(body))
                case "unlink": target.unlink()
                case "symlink":
                    target.unlink()
                    target.symlink_to(expand(step["target"], tmp_path))
                case other: raise ValueError(f"unsupported mutation: {other}")
            continue
        data = expand(step.get("stdin", {}), tmp_path)
        raw = data if isinstance(data, str) else json.dumps(data)
        selected = step.get("settings", str(settings))
        command = [sys.executable, str(adapter), expand(selected, tmp_path)]
        if step.get("entry") == "console":
            command = [sys.executable, "-c", "from codex_session_relay import stopadapter; stopadapter.main()", expand(selected, tmp_path)]
        if step.get("entry") == "checkout":
            command = [sys.executable, str(ROOT / "scripts/completion_hook.py"), expand(selected, tmp_path)]
        if step.get("command"):
            command = expand(step["command"], tmp_path)
        env = isolated_env(tmp_path)
        if step.get("entry") == "console":
            env["PYTHONPATH"] = str(adapter.parents[1])
        processes = [subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                     stderr=subprocess.PIPE, env=env)
                     for _ in range(step.get("concurrency", 1))]
        for process in processes:
            try:
                stdout, stderr = process.communicate(raw.encode(), timeout=step.get("timeout", 60))
                outcomes.append({"exit": process.returncode, "stdout": stdout.decode(),
                                 "stderr": stderr.decode(), "signal": -process.returncode if process.returncode < 0 else None,
                                 "timeout": False})
            except subprocess.TimeoutExpired:
                process.kill()
                process.communicate()
                outcomes.append({"exit": process.returncode, "timeout": True, "signal": -process.returncode})
    files = {name: observe(tmp_path / name) for name in case["expect"].get("observe", [])}
    row_paths = [path for path in sorted((tmp_path / "journal").glob("*/*.json"))
                 if re.fullmatch(r"[0-9]{8}", path.parent.name)
                 and re.fullmatch(r"[0-9a-f]{32}\.json", path.name)]
    rows = [json.loads(path.read_text()) for path in row_paths]
    expected = case["expect"]
    ledger = tmp_path / "journal" / "accepted"
    claims = sorted(p for p in ledger.glob("*.json") if re.fullmatch(r"[0-9a-f]{64}\.json", p.name))
    ledger_outcomes = sorted(ledger.glob("*.outcome.json"))
    calls_path = tmp_path / "guard-calls"
    calls = [json.loads(line) for line in calls_path.read_text().splitlines()] if calls_path.exists() else []
    return {**outcomes[-1], "outcomes": outcomes, "rows": rows, "calls": calls,
            "acceptances": sorted(str(row.get("acceptance")) for row in rows),
            "claims": [json.loads(p.read_text()) for p in claims],
            "ledger_outcomes": [json.loads(p.read_text()) for p in ledger_outcomes],
            "row_files": [{"day": p.parent.name, "name": p.name, **observe(p)} for p in row_paths],
            "observed": files,
            "files": {name: (tmp_path / name).exists() for name in expected.get("files", {})},
            "stdout_json": json.loads(outcomes[-1]["stdout"]) if outcomes[-1].get("stdout") else None}


def agreement(case, tmp_path):
    """Run both installed and checkout adapters against independent identical fixtures."""
    sys.path.insert(0, str(ROOT / "scripts"))
    from crw_runtime import completion
    from codex_session_relay import stopadapter
    results = []
    for label, adapter in (("checkout", completion), ("packaged", stopadapter)):
        home = tmp_path / label
        home.mkdir()
        relay = home / "relay"
        response = case["given"]["relay"]
        relay.write_text("#!/usr/bin/env python3\nimport sys,os,signal,time,json\n"
                         "from pathlib import Path\n"
                         f"Path({str(home / 'call.json')!r}).write_text(json.dumps({{'argv':sys.argv[1:], 'stdin':sys.stdin.read()}}))\n"
                         f"time.sleep({response.get('delay', 0)!r})\n"
                         f"{'os.kill(os.getpid(), signal.SIGKILL)' if response.get('signal') else ''}\n"
                         f"sys.stdout.write({response.get('stdout', '')!r})\n"
                         f"raise SystemExit({response.get('exit', 0)!r})\n")
        relay.chmod(0o755)
        settings = completion.configuration(relay=str(relay), marker_root=str(home / "marker"),
                                            journal_root=str(home / "journal"), codex_home=str(home), issue="CRW-37")
        if label == "packaged":
            settings.update(owner="plugin", adapterInterpreter=sys.executable,
                            adapterEntryPoint=str(Path(stopadapter.__file__).resolve()))
        path = home / "settings.json"
        path.write_text(json.dumps(settings))
        data = case["run"].get("stdin", {})
        payload = None if data is None else json.dumps(expand(data, home)).encode()
        if label == "checkout":
            returned = adapter.run(payload, codex_home=str(home), settings=str(path))
        else:
            returned = adapter.run(payload, settings=str(path))
        records = [json.loads(p.read_text()) for p in sorted((home / "journal").glob("*/*.json"))]
        calls = json.loads((home / 'call.json').read_text()) if (home / 'call.json').exists() else None
        results.append((returned, records, calls))
    left, right = results
    excluded = {"configuration", "journalledAs", "at", "elapsedMs", "guardElapsedMs", "identityScanMs"}
    def normalized(records):
        return [{key: value for key, value in row.items() if key not in excluded}
                for row in records]
    assert left[0] == right[0] and normalized(left[1]) == normalized(right[1]), \
        "checkout and packaged adapters disagree"
    return {"exit": 0, "returned": left[0], "records": left[1],
            "calls": {"checkout": left[2], "packaged": right[2]}}

