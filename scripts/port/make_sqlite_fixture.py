#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10"
# dependencies = []
# ///
# Usage: uv run --no-sync python scripts/port/make_sqlite_fixture.py
"""Create the committed compatibility database using the real Python Store."""

from __future__ import annotations

from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "packages/codex-session-relay/src"))
from codex_session_relay.store import Store  # noqa: E402


def main() -> None:
    path = ROOT / "contract/fixtures/sqlite-ddl/python-store.sqlite3"
    if path.exists():
        raise FileExistsError(path)
    store = Store(path, socket_path="/fixture/python.sock")
    store.db.close()


if __name__ == "__main__":
    main()
