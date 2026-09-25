"""Case-local contract execution and observable assertions."""

import json
import re
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
FIXTURES = ROOT / "contract" / "fixtures"


STOP = {"cwd": "/tmp/workspace", "hook_event_name": "Stop",
        "last_assistant_message": "I finished the task.", "model": "test-model",
        "permission_mode": "default", "session_id": "01a0b109-1ea5-7fb3-9adc-87f45ed83688",
        "stop_hook_active": False, "transcript_path": "/tmp/transcript.jsonl", "turn_id": "turn-1"}


def scenarios(domains):
    return [path for domain in domains for path in sorted((FIXTURES / domain).glob("*.json"))]


def evaluate(actual, checks, scenario_id):
    for check in checks:
        value = actual
        for key in check["path"]:
            value = value[key]
        if check.get("decode_json"):
            value = json.loads(value)
        expected = check.get("value")
        kind = check["kind"]
        match kind:
            case "eq": passed = value == expected
            case "ne": passed = value != expected
            case "json_eq": passed = json.loads(value) == expected
            case "contains": passed = expected in value
            case "excludes": passed = expected not in value
            case "truthy": passed = bool(value)
            case "falsy": passed = not value
            case "length": passed = len(value) == expected
            case "regex": passed = re.search(expected, value) is not None
            case "lt": passed = value < expected
            case "gt": passed = value > expected
            case "after": passed = value[value.index(check["flag"]) + 1] == expected
            case "same":
                other = actual
                for key in check["other"]:
                    other = other[key]
                passed = value == other
            case "set_eq": passed = set(value) == set(expected)
            case "subset": passed = set(expected) <= set(value)
            case "bytes": passed = bytes.fromhex(value) == bytes.fromhex(expected)
            case "ordered_calls": passed = value == expected
            case "absent_effects": passed = not value
            case "exception_code": passed = value == expected
            case "timeout": passed = value == expected
            case "signal": passed = value == expected
            case _: pytest.fail(f"{scenario_id}: unsupported assertion {kind}")
        assert passed, f"{scenario_id}: {check}: {value!r}"


def run_scenario(path, tmp_path):
    case = json.loads(path.read_text(encoding="utf-8"))
    ident = path.stem
    run = case["run"]
    kind = run["kind"]
    match kind:
        case "entry" | "hook" | "status":
            from .hook import run_scenario as run_hook
            return run_hook(path, tmp_path)
        case "cli":
            from .cli import run
        case "stop":
            from .files import run
        case "agreement":
            from .files import agreement as run
        case "git":
            from .git import run
        case "mcp":
            from .mcp import run
        case "release":
            from .release import run
        case "appserver":
            from .appserver import run
        case "ledger":
            from .ledger import run
        case _: pytest.fail(f"{ident}: unknown run kind {kind}")
    actual = run(case, tmp_path)
    expected = case["expect"]
    for key in ("exit", "stdout_json", "files"):
        if key in expected:
            assert actual[key] == expected[key], f"{ident}: {key}: {actual[key]!r} != {expected[key]!r}"
    evaluate(actual, expected.get("checks", []), ident)
