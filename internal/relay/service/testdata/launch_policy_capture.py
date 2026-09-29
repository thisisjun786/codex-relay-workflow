"""Capture, once, Python's launch-policy resolution for every declaration the Go test builds.

Not run by any test: its output, launch_policy.json beside it, is the oracle
TestLaunchPolicy_resolution_is_pythons_for_every_declaration reads, so default CI never starts
Python for it. Regenerate after a change to service.py LaunchPolicy.read,
resolve_launch_policy, canonical_policy_path or rolepolicy._resolve with

    PYTHONDONTWRITEBYTECODE=1 .venv/bin/python internal/relay/service/testdata/launch_policy_capture.py

run from the repository root. Each case is a declarative tree under a scratch root R: the
launch declaration S/launch-policy.json (text, bytes, or a special file), policy files and
links beside it, and the value of CODEX_THREAD_BRIDGE_EXECUTION_POLICY. The relay CLI's
`service status` resolves it with R as its working directory; its launchPolicy block, less
the three keys status adds, is recorded with R spelled <R>. The Go test builds the same tree
and compares ResolveLaunchPolicyAt's answer byte for byte.
"""
import json
import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[3]
RELAY = REPO / ".venv" / "bin" / "codex-session-relay"
OUT = HERE / "launch_policy.json"

POLICY = '{"roles":{"parent":{"model":"m","reasoningEffort":"high"},"child":{"model":"m","reasoningEffort":"high"}}}'
OTHER = POLICY.replace('"m"', '"n"')


def generate(kind, depth):
    """The deep documents, generated rather than stored (they are tens of kilobytes)."""
    array = lambda n: "[" * n + "]" * n  # noqa: E731
    obj = lambda n: '{"a":' * n + "1" + "}" * n  # noqa: E731
    return {
        "array": lambda: array(depth),
        "object": lambda: obj(depth),
        "declaration-array": lambda: '{"path": "<R>/good.json", "x": %s}' % array(depth - 1),
        "policy-array": lambda: '{"roles": {}, "x": %s}' % array(depth - 1),
        "policy-object": lambda: '{"roles": {}, "x": %s}' % obj(depth - 1),
        "policy-object-in-arrays": lambda: '{"roles": {}, "x": %s{}%s}' % ("[" * (depth - 2), "]" * (depth - 2)),
        "policy-duplicate-then-deep": lambda: '{"x": {"a": 1, "a": 2}, "y": %s}' % array(depth),
    }[kind]()


def declaration(path):
    return json.dumps({"path": path})


CASES = {
    "absent": {},
    "record": {"launch": {"text": declaration("<R>/good.json")}},
    "record-missing-policy": {"launch": {"text": declaration("<R>/nope.json")}},
    "record-dotted": {"launch": {"text": declaration("<R>//x/./../nope.json/")}},
    "record-relative": {"launch": {"text": declaration("rel/./p.json")}},
    "record-trailing-slash": {"launch": {"text": declaration("<R>/good.json/")}},
    "record-nul": {"launch": {"text": '{"path": "<R>/a\\u0000b"}'}},
    "record-directory-policy": {"launch": {"text": declaration("<R>")}},
    "record-trailing-separator": {"launch": {"text": '{"path": "<R>/good.json\\u001f"}'}},
    "record-values": {"launch": {"text": '{"path": "<R>/good.json", "declaredAt": 1e400, "declaredBy": 123456789012345678901234567890}'}},
    "record-nan": {"launch": {"text": '{"path": "<R>/good.json", "declaredAt": NaN, "declaredBy": [1, {"b": -0.0}]}'}},
    "record-negative-zero": {"launch": {"text": '{"path": "<R>/good.json", "declaredAt": -0.0, "declaredBy": -Infinity}'}},
    "record-duplicate-keys": {"launch": {"text": '{"path": "/first", "path": "<R>/good.json"}'}},
    "record-escapes": {"launch": {"text": '{"path": "<R>/good.json", "declaredBy": "\\u00e9\\n\\"q"}'}},
    "record-big-integer": {"launch": {"text": '{"path": "<R>/good.json", "declaredAt": %s}' % ("9" * 4301)}},
    "record-deep-9998": {"launch": {"generate": "declaration-array", "depth": 9998}},
    "record-deep-9999": {"launch": {"generate": "declaration-array", "depth": 9999}},
    "unreadable-eacces": {"launch": {"text": declaration("<R>/good.json"), "mode": 0}},
    "unreadable-directory": {"launch": {"special": "directory"}},
    "unreadable-fifo": {"launch": {"special": "fifo"}},
    "unreadable-dangling": {"launch": {"special": "dangling"}},
    "unreadable-loop": {"launch": {"special": "loop"}},
    "utf8-invalid-start": {"launch": {"hex": "ff7b7d"}},
    "utf8-invalid-continuation": {"launch": {"hex": "7b2270617468223a2022c328227d"}},
    "utf8-end-of-data": {"launch": {"hex": "7b2270617468223a2022e282"}},
    "utf8-surrogate": {"launch": {"hex": "7b2270617468223a2022eda080227d"}},
    "utf8-overlong": {"launch": {"hex": "c080"}},
    "json-empty": {"launch": {"text": ""}},
    "json-open": {"launch": {"text": "{"}},
    "json-bom": {"launch": {"hex": "efbbbf7b7d"}},
    "json-extra": {"launch": {"text": "{}x"}},
    "json-control": {"launch": {"text": '{"path": "\x1f"}'}},
    "json-array-9998": {"launch": {"generate": "array", "depth": 9998}},
    "json-array-9999": {"launch": {"generate": "array", "depth": 9999}},
    "json-object-9999": {"launch": {"generate": "object", "depth": 9999}},
    "names-no-file-list": {"launch": {"text": "[]"}},
    "names-no-file-string": {"launch": {"text": '"x"'}},
    "names-no-file-null": {"launch": {"text": "null"}},
    "names-no-file-path-null": {"launch": {"text": '{"path": null}'}},
    "names-no-file-path-number": {"launch": {"text": '{"path": 1}'}},
    "names-no-file-path-blank": {"launch": {"text": '{"path": "  "}'}},
    "names-no-file-path-separators": {"launch": {"text": '{"path": "\\u001f \\u3000"}'}},
    "policy-deep-array": {"launch": {"text": declaration("<R>/policy.json")}, "files": {"policy.json": {"generate": "policy-array", "depth": 9999}}},
    "policy-array-fits": {"launch": {"text": declaration("<R>/policy.json")}, "files": {"policy.json": {"generate": "policy-array", "depth": 9998}}},
    "policy-deep-object": {"launch": {"text": declaration("<R>/policy.json")}, "files": {"policy.json": {"generate": "policy-object", "depth": 9997}}},
    "policy-object-fits": {"launch": {"text": declaration("<R>/policy.json")}, "files": {"policy.json": {"generate": "policy-object", "depth": 9996}}},
    "policy-object-in-arrays": {"launch": {"text": declaration("<R>/policy.json")}, "files": {"policy.json": {"generate": "policy-object-in-arrays", "depth": 9997}}},
    "policy-object-in-arrays-fits": {"launch": {"text": declaration("<R>/policy.json")}, "files": {"policy.json": {"generate": "policy-object-in-arrays", "depth": 9996}}},
    "policy-duplicate-before-depth": {"launch": {"text": declaration("<R>/policy.json")}, "files": {"policy.json": {"generate": "policy-duplicate-then-deep", "depth": 10005}}},
    "policy-byte-order-mark": {"launch": {"text": declaration("<R>/policy.json")}, "files": {"policy.json": {"hex": "efbbbf" + POLICY.encode().hex()}}},
    "policy-no-roles": {"launch": {"text": declaration("<R>/policy.json")}, "files": {"policy.json": {"text": '{"roles": {}}'}}},
    "environment": {"environment": "<R>/good.json"},
    "environment-relative": {"environment": "./good.json"},
    "environment-missing": {"environment": "<R>/gone.json"},
    "environment-blank": {"environment": "  "},
    "environment-separator": {"environment": "\x1f"},
    "environment-home": {"environment": "~/p.json"},
    "conflict": {"launch": {"text": declaration("<R>/good.json")}, "environment": "<R>/other.json"},
    "same-file-through-alias": {"launch": {"text": declaration("<R>/good.json")}, "environment": "<R>/alias.json"},
    "same-file-relative": {"launch": {"text": declaration("<R>/good.json")}, "environment": "./good.json"},
    "unreadable-record-beats-environment": {"launch": {"hex": "ff"}, "environment": "<R>/good.json"},
}


def build(root, case):
    """The case's tree under root; the Go test's buildLaunchCase is this function's twin."""
    def expand(text):
        return text.replace("<R>", str(root))

    def content(spec):
        if "generate" in spec:
            return expand(generate(spec["generate"], spec["depth"])).encode()
        if "hex" in spec:
            return bytes.fromhex(spec["hex"])
        return expand(spec["text"]).encode()

    (root / "home").mkdir(parents=True)
    (root / "good.json").write_text(POLICY)
    (root / "other.json").write_text(OTHER)
    (root / "home" / "p.json").write_text(POLICY)
    os.symlink(root / "good.json", root / "alias.json")
    state = root / "S"
    state.mkdir()
    for name, spec in case.get("files", {}).items():
        (root / name).write_bytes(content(spec))
    launch = case.get("launch")
    target = state / "launch-policy.json"
    if launch is not None:
        special = launch.get("special")
        if special == "directory":
            target.mkdir()
        elif special == "fifo":
            os.mkfifo(target)
        elif special == "dangling":
            os.symlink(state / "nothing", target)
        elif special == "loop":
            os.symlink(target, target)
        else:
            target.write_bytes(content(launch))
            if "mode" in launch:
                os.chmod(target, launch["mode"])
    return state


def resolve(root, state, case):
    env = {key: value for key, value in os.environ.items() if key != "CODEX_THREAD_BRIDGE_EXECUTION_POLICY"}
    env.update(HOME=str(root / "home"), CODEX_SESSION_RELAY_STATE="", PYTHONDONTWRITEBYTECODE="1")
    if case.get("environment") is not None:
        env["CODEX_THREAD_BRIDGE_EXECUTION_POLICY"] = case["environment"].replace("<R>", str(root))
    answer = subprocess.run([str(RELAY), "--state", str(state), "service", "status"], env=env, cwd=root,
                            capture_output=True, check=True)
    block = json.loads(answer.stdout)["launchPolicy"]
    for key in ("appliesTo", "runningDigest", "matchesRunning"):
        del block[key]
    return json.dumps(block, indent=2).replace(str(root), "<R>")


def main():
    if os.geteuid() == 0:
        sys.exit("run as an ordinary user: root reads a mode-0 file")
    captured = {"policy": POLICY, "other": OTHER, "cases": {}}
    for name, case in CASES.items():
        root = Path(tempfile.mkdtemp(prefix="launch-policy-"))
        try:
            state = build(root, case)
            captured["cases"][name] = dict(case, resolution=resolve(root, state, case))
        finally:
            for path in (root / "S" / "launch-policy.json",):
                if path.exists() and not path.is_symlink():
                    os.chmod(path, 0o600)
            shutil.rmtree(root)
    OUT.write_text(json.dumps(captured, indent=1, sort_keys=True) + "\n")
    print(OUT, len(captured["cases"]))


if __name__ == "__main__":
    main()
