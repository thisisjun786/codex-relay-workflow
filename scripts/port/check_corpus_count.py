#!/usr/bin/env python3
"""Account for every class-A test function in the contract notes (dev-only).

Exit 0 only when every ``*.json`` fixture parses, and every class-A ``test_`` function
(class methods included; one parametrised function is one function) appears in exactly
one note row as converted, kept, or blocked. A converted row needs a fixture whose
name starts with ``<stem>__<function>``. A kept row needs a reason. A blocked row
needs the missing kind named. Prints per-file converted/kept/blocked counts.
"""

# ─── How to run ───
# 1. Install uv (if not installed):
#      curl -LsSf https://astral.sh/uv/install.sh | sh
# 2. Run directly (no venv, no pip install needed):
#      uv run scripts/port/check_corpus_count.py
# 3. Or make executable and run:
#      chmod +x scripts/port/check_corpus_count.py && ./scripts/port/check_corpus_count.py
# ──────────────────
# /// script
# requires-python = ">=3.10"
# dependencies = []
# ///

from __future__ import annotations

import ast
import importlib.util
import json
import re
import sys
from collections import Counter
from dataclasses import dataclass
from pathlib import Path
from typing import Final, assert_never

_NOTES = Path(__file__).resolve().parent / "corpus_notes.py"
_SPEC = importlib.util.spec_from_file_location("corpus_notes", _NOTES)
if _SPEC is None or _SPEC.loader is None:
    raise RuntimeError(f"cannot load {_NOTES}")
_MODULE = importlib.util.module_from_spec(_SPEC)
sys.modules[_SPEC.name] = _MODULE
_SPEC.loader.exec_module(_MODULE)
Account = _MODULE.Account
accounts_of = _MODULE.accounts_of
cells = _MODULE.cells
heading_marks = _MODULE.heading_marks

ROOT: Final = Path(__file__).resolve().parents[2]
TEST_MAP: Final = ROOT / "docs" / "port" / "test-map.md"
NOTES: Final = ROOT / "contract" / "notes"
FIXTURES: Final = ROOT / "contract" / "fixtures"
COLUMNS: Final = ("path", "tests", "class")
STATUSES: Final = frozenset({"converted", "kept", "blocked"})
MIN_FIXTURE_PARTS: Final = 2
# Run and check kinds documented in contract/README.md; a letter alone is not a gap.
NAMED_KINDS: Final = frozenset({
    "cli", "mcp", "appserver", "git", "release", "stop", "agreement", "entry",
    "hook", "status", "install", "eq", "ne", "json_eq", "contains", "excludes",
    "truthy", "falsy", "length", "regex", "lt", "gt", "after", "same",
    "set_eq", "subset", "bytes", "ordered_calls", "absent_effects",
    "exception_code", "timeout", "signal", "stop-steps", "stop-transcript",
    "registrations", "guard-behaviour", "verifier", "record-mutation",
    "observe-glob", "console-script entry", "parser-handler-table",
    "parser-command-classification", "host-file setup", "worker-policy-service",
    "settings reader", "reader outcome", "record_outcome", "complaints()",
    "settings path", "cross-registration", "modes", "no_surface", "status_env",
    "symlink", "call_count", "expected_template",
})
GAP_CODE: Final = re.compile(r"\b[A-Z]\d+\b")


@dataclass(frozen=True, slots=True)
class CorpusError(Exception):
    """One coverage failure, already phrased for stdout."""

    detail: str

    def __str__(self) -> str:
        return self.detail


def class_a_files() -> list[Path]:
    """Class-A source paths from the test map, in table order."""
    lines = TEST_MAP.read_text(encoding="utf-8").splitlines()
    start = next(i for i, line in enumerate(lines) if tuple(cells(line)[:3]) == COLUMNS)
    found: list[Path] = []
    for line in lines[start + 2 :]:
        if not line.startswith("|"):
            break
        row = cells(line)
        if row[2] == "A":
            found.append(ROOT / row[0].strip("`"))
    return found


def test_functions(path: Path) -> list[str]:
    """``test_`` functions and methods in source order. Parametrised cases stay one function."""
    found: list[str] = []

    class Visitor(ast.NodeVisitor):
        def visit_FunctionDef(self, node: ast.FunctionDef | ast.AsyncFunctionDef) -> None:
            if node.name.startswith("test_"):
                found.append(node.name)

        visit_AsyncFunctionDef = visit_FunctionDef

    Visitor().visit(ast.parse(path.read_text(encoding="utf-8"), filename=str(path)))
    return found


def fixture_names() -> dict[str, list[str]]:
    """Parse every fixture. Group filenames by their ``<file>__<function>`` prefix."""
    grouped: dict[str, list[str]] = {}
    for path in sorted(FIXTURES.rglob("*.json")):
        try:
            json.loads(path.read_text(encoding="utf-8"))
        except json.JSONDecodeError as error:
            where = path.relative_to(ROOT)
            raise CorpusError(f"fixture does not parse: {where} ({error.msg})") from error
        parts = path.stem.split("__")
        if len(parts) >= MIN_FIXTURE_PARTS:
            grouped.setdefault(f"{parts[0]}__{parts[1]}", []).append(path.name)
    return grouped


def note_sections() -> dict[str, str]:
    """Map each class-A stem to the note text under the heading that names its file."""
    stems = [path.stem for path in class_a_files()]
    sections: dict[str, list[str]] = {stem: [] for stem in stems}
    for path in sorted(NOTES.glob("*.md")):
        lines = path.read_text(encoding="utf-8").splitlines()
        marks = heading_marks(lines, stems)
        for position, (start, stem) in enumerate(marks):
            following = [point for point, _ in marks[position + 1 :] if point != start]
            end = following[0] if following else len(lines)
            sections[stem].append("\n".join(lines[start + 1 : end]))
    return {stem: "\n".join(parts) for stem, parts in sections.items()}


def _reused_fixture(stem: str, detail: str, fixtures: dict[str, list[str]]) -> bool:
    """A converted row may point at another of this file's fixtures by name or parameter id."""
    owned = [name for key, names in fixtures.items() if key.startswith(f"{stem}__") for name in names]
    if any(name.removesuffix(".json") in detail for name in owned):
        return True
    words = set(re.findall(r"[A-Za-z_][A-Za-z0-9_]*", detail))
    return any(part in words for name in owned for part in name.removesuffix(".json").split("__")[2:])


def _problems(stem: str, function: str, rows: list[Account], fixtures: dict[str, list[str]]) -> list[str]:
    if len(rows) != 1:
        places = ", ".join(row.where for row in rows) or "nowhere"
        return [f"{stem}.py {function}: expected one account, found {len(rows)} ({places})"]
    return _status_problems(stem, function, rows[0], fixtures)


def _status_problems(stem: str, function: str, row: Account, fixtures: dict[str, list[str]]) -> list[str]:
    status = row.status
    if status == "converted":
        prefix = f"{stem}__{function}"
        if not fixtures.get(prefix, []) and not _reused_fixture(stem, row.detail, fixtures):
            return [f"{stem}.py {function}: converted but no fixture starts with {prefix}"]
        return []
    if status == "kept":
        if row.detail.strip() in {"", "-"}:
            return [f"{stem}.py {function}: kept without a reason"]
        return []
    if status == "blocked":
        if not GAP_CODE.search(row.detail) and not any(
            re.search(rf"(?<![A-Za-z0-9]){re.escape(kind)}(?![A-Za-z0-9])", row.detail)
            for kind in NAMED_KINDS
        ):
            return [f"{stem}.py {function}: blocked without the missing kind"]
        return []
    assert_never(status)


def main() -> int:
    """Check fixtures, then print one coverage line per class-A file."""
    try:
        fixtures = fixture_names()
    except CorpusError as error:
        print(error)
        return 1
    failures: list[str] = []
    sections = note_sections()
    for path in class_a_files():
        stem = path.stem
        functions = test_functions(path)
        rows, unreadable = accounts_of(sections.get(stem, ""), set(functions))
        for item in unreadable:
            failures.append(f"{stem}.py: notes {item.where} is not a readable account: {item.text}")
        counts: Counter[str] = Counter()
        for function in functions:
            found = rows.get(function, [])
            failures.extend(_problems(stem, function, found, fixtures))
            if len(found) == 1 and found[0].status in STATUSES:
                counts[found[0].status] += 1
        print(
            f"{path.relative_to(ROOT)}: converted={counts['converted']} "
            f"kept={counts['kept']} blocked={counts['blocked']}"
        )
    for failure in failures:
        print(failure, file=sys.stderr)
    return int(bool(failures))


if __name__ == "__main__":
    raise SystemExit(main())
