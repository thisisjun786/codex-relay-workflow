#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.13"
# dependencies = []
# ///
# How to run: from the repository root, uv run --no-sync python scripts/port/make_ledger_fixture.py
from __future__ import annotations

import json
import shutil
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "packages/codex-thread-bridge/src"))
from codex_thread_bridge.ledger import Ledger  # noqa: E402


def main() -> None:
    destination = ROOT / "contract/fixtures/ledger-fingerprint"
    destination.mkdir(parents=True, exist_ok=True)
    values = [
        {}, {"a": 1, "b": 2}, {"b": 2, "a": 1}, {"nested": {"z": [1, {"b": True, "a": None}]}},
        {"text": "café 🐈"}, {"text": "\u2028\u2029\b\f\n\t\r"},
        {"text": "<>&/"}, {"float": 1.5}, {"float": 1e-7}, {"float": -0.0},
        {"float": 1e20}, {"float": 1e-5}, {"float": 1.0}, {"integer": 9007199254740991},
        {"array": [[], [1, 2, 3]]}, {"nested": {"ü": "é", "a": "ω"}},
        {"deep": {"c": {"b": {"a": 3}}}}, {"quote": "\\\""},
        {"boolean": False}, {"null": None}, {"list": ["🧪", "汉字", "ascii"]},
        {"float": 0.0001}, {"float": 1e16}, {"float": 1e-6}, {"float": 1e15},
        {"float": 999999999999999.9}, {"float": -1e15},
        {"text": "\u007f"}, {"text": "\u0000\u0001\u001f"},
        {"text": "𐀀🚀"}, {"text": "é\u007f🚀\u0000中"},
        {"integer": -0}, {"float": json.loads("1e400")},
        {"text": "\u0080\uffff"},
        # Lone surrogates: json.loads keeps them, and json.dumps writes them back as escapes.
        {"text": "\ud800"}, {"text": "a\udc00b"}, {"text": "\udc00\ud800"},
        {"text": "\ud800\u00e9"}, {"\udbff": ["\udfff", "\ud83d\ude80"]},
    ]
    pairs = [
        {"method": "create_thread", "params": params,
         "hash": Ledger._fingerprint(f"golden-{index}", "create_thread", params)}
        for index, params in enumerate(values)
    ]
    encoded = json.dumps(pairs, indent=2, ensure_ascii=True)
    # Keep Python's Infinity fingerprint but supply the equivalent JSON numeric literal to Go.
    (destination / "goldens.txt").write_text(encoded.replace('"float": Infinity', '"float": 1e400') + "\n")
    with tempfile.TemporaryDirectory() as temp:
        ledger = Ledger(Path(temp) / "operations.sqlite3")
        fresh, receipt = ledger.begin("retained-create", "create_thread", {"cwd": "/checkout"})
        assert fresh
        ledger.save({**receipt, "status": "accepted", "threadId": "python-thread"})
        ledger.close()
        shutil.copyfile(Path(temp) / "operations.sqlite3", destination / "operations.sqlite3")
    print(f"generated {len(pairs)} Python ledger fingerprints and retained SQLite receipt")


if __name__ == "__main__":
    main()
