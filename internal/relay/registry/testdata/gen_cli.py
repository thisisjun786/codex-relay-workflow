"""Record the Python relay CLI's answers for the registry commands (cli_cases.json).

  uv run --no-sync python internal/relay/registry/testdata/gen_cli.py \
      > internal/relay/registry/testdata/python_cli.json
Each case is a list of steps {"argv": [...], optional "files": {name: text}, optional "sql"}.
Every step runs `python -m codex_session_relay.cli --state <case>/state <argv>` with HOME and
the XDG/CODEX dirs inside the case directory. ${HOME} in argv and files is the case directory.
Timestamps and the case directory are replaced by <T> and <HOME> in the recorded stdout.
"""
import json
import os
import re
import sqlite3
import subprocess
import sys
import tempfile

STAMP = re.compile(r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{6}\+00:00")
cases = json.load(open("internal/relay/registry/testdata/cli_cases.json"))
out = {}
for name, steps in cases.items():
    home = tempfile.mkdtemp(dir=os.environ["TMPDIR"])
    env = dict(os.environ, HOME=home, XDG_STATE_HOME=home + "/xs", XDG_DATA_HOME=home + "/xd",
               XDG_CONFIG_HOME=home + "/xc", CODEX_HOME=home + "/ch")
    env.pop("CODEX_THREAD_BRIDGE_EXECUTION_POLICY", None)
    results = []
    for step in steps:
        for fname, text in step.get("files", {}).items():
            with open(os.path.join(home, fname), "w", encoding="utf-8") as handle:
                handle.write(text.replace("${HOME}", home))
        if "sql" in step:
            db = sqlite3.connect(os.path.join(home, "state", "relay.sqlite3"))
            db.executescript(step["sql"])
            db.commit()
            db.close()
            continue
        argv = [a.replace("${HOME}", home) for a in step["argv"]]
        done = subprocess.run([sys.executable, "-m", "codex_session_relay.cli", "--state",
                               home + "/state", *argv], capture_output=True, text=True, env=env,
                              timeout=60)
        text = STAMP.sub("<T>", done.stdout).replace(home, "<HOME>")
        results.append({"exit": done.returncode, "stdout": text})
    out[name] = results
json.dump(out, sys.stdout, indent=1, sort_keys=True)
sys.stdout.write("\n")
