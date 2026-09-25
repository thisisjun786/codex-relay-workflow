"""Replay a Python-written SQLite ledger through the real bridge Ledger API."""

import shutil
from pathlib import Path

from codex_thread_bridge.ledger import Ledger


def run(case: dict, tmp_path: Path) -> dict:
    """Return the retained receipt without modifying the committed database."""
    source = Path(__file__).resolve().parents[1] / "fixtures/ledger-fingerprint" / case["given"]["fixture"]
    target = tmp_path / "operations.sqlite3"
    shutil.copyfile(source, target)
    ledger = Ledger(target)
    try:
        fresh, receipt = ledger.begin(
            case["run"]["request_id"], case["run"]["method"], case["run"]["params"]
        )
        return {"exit": 0, "fresh": fresh, "receipt": receipt}
    finally:
        ledger.close()
