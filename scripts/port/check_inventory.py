#!/usr/bin/env python3
"""Check docs/port/inventory.md against the Python files it claims to inventory (dev-only).

Exit 0 only when the table's path column is set-equal to every non-test .py file under
packages/, scripts/ and plugins/ (the same selection as `find packages scripts plugins -name
'*.py' -not -path '*/tests/*' -not -path '*/__pycache__/*'`), every line count equals `wc -l`,
the stated total equals the sum, every row names an owning issue among CRW-150..161, and every
retire-with-evidence row carries its consumer search and removal trigger. A file that is gone
is listed once, under "Files deleted with evidence", with its consumer search and what removed
it, and never in the table of files that exist. Exit 1 otherwise, naming each offending path.
"""

import re
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Final

ROOT: Final = Path(__file__).resolve().parents[2]
INVENTORY: Final = ROOT / "docs" / "port" / "inventory.md"
SEARCHED: Final = ("packages", "scripts", "plugins")
COLUMNS: Final = ("path", "lines", "invoked", "runs", "owner", "disposition",
                  "consumer_search", "removal_trigger")
DISPOSITIONS: Final = frozenset({"port", "retire-with-evidence", "keep-as-data"})
OWNER: Final = re.compile(r"CRW-1(5[0-9]|6[01])")
TOTAL: Final = re.compile(r"^Total non-test lines: (\d+)$", re.MULTILINE)
EMPTY: Final = frozenset({"", "-"})
DELETED_COLUMNS: Final = ("path", "lines", "invoked", "owner", "consumer_search", "removed by")


@dataclass(frozen=True, slots=True)
class Row:
    """One inventory row, reduced to the cells this check judges."""

    path: str
    lines: str
    owner: str
    disposition: str
    consumer_search: str
    removal_trigger: str


def python_files() -> dict[str, int]:
    """Every non-test .py file under the searched roots, with its newline count (`wc -l`)."""
    found: dict[str, int] = {}
    for top in SEARCHED:
        for path in (ROOT / top).rglob("*.py"):
            relative = path.relative_to(ROOT).as_posix()
            if "/tests/" in relative or "/__pycache__/" in relative:
                continue
            found[relative] = path.read_bytes().count(b"\n")
    return found


def cells(line: str) -> list[str]:
    """Split one markdown table line into stripped cell texts."""
    return [cell.strip() for cell in line.strip().strip("|").split("|")]


def table_lines(text: str, columns: tuple[str, ...]) -> list[list[str]]:
    """The cell texts of each row of the one table whose header is columns."""
    lines = text.splitlines()
    start = next(i for i, line in enumerate(lines) if tuple(cells(line)) == columns)
    rows: list[list[str]] = []
    for line in lines[start + 2:]:
        if not line.startswith("|"):
            break
        rows.append(cells(line))
    return rows


def table_rows(text: str) -> list[Row]:
    """Read the rows of the one table whose header is COLUMNS."""
    rows: list[Row] = []
    for values in table_lines(text, COLUMNS):
        if len(values) != len(COLUMNS):
            rows.append(Row(values[0].strip("`"), "", "", "", "", ""))
            continue
        path, lines_cell, _, _, owner, disposition, search, trigger = values
        rows.append(Row(path.strip("`"), lines_cell, owner, disposition, search, trigger))
    return rows


def deleted_problems(text: str, listed: list[str]) -> list[str]:
    """Name every row of the deleted-files table that is malformed, still present, listed twice or
    also listed as existing, or that lacks its consumer search or what removed it."""
    found: list[str] = []
    paths = []
    for values in table_lines(text, DELETED_COLUMNS):
        path = values[0].strip("`")
        paths.append(path)
        if len(values) != len(DELETED_COLUMNS):
            found.append(f"deleted row has {len(values)} cells, not {len(DELETED_COLUMNS)}: {path}")
            continue
        if (ROOT / path).exists():
            found.append(f"listed as deleted but present: {path}")
        if path in listed:
            found.append(f"listed both as present and as deleted: {path}")
        if values[4] in EMPTY or values[5] in EMPTY:
            found.append(f"deleted row lacks consumer_search or removed by: {path}")
    found += [f"duplicated deleted row: {path}"
              for path in sorted({path for path in paths if paths.count(path) > 1})]
    return found


def problems(rows: list[Row], files: dict[str, int], stated_total: int | None) -> list[str]:
    """Name every disagreement between the table and the files; empty when they agree."""
    found: list[str] = []
    listed = [row.path for row in rows]
    duplicated = sorted({path for path in listed if listed.count(path) > 1})
    found += [f"duplicated row: {path}" for path in duplicated]
    found += [f"missing from inventory: {path}" for path in sorted(set(files) - set(listed))]
    found += [f"not a non-test .py file: {path}" for path in sorted(set(listed) - set(files))]
    for row in rows:
        if row.path in files and row.lines != str(files[row.path]):
            found.append(f"line count {row.lines!r} != wc -l {files[row.path]}: {row.path}")
        if not OWNER.fullmatch(row.owner):
            found.append(f"owner {row.owner!r} is not one of CRW-150..161: {row.path}")
        if row.disposition not in DISPOSITIONS:
            found.append(f"disposition {row.disposition!r} unknown: {row.path}")
        if row.disposition == "retire-with-evidence" and (
                row.consumer_search in EMPTY or row.removal_trigger in EMPTY):
            found.append(f"retire row lacks consumer_search or removal_trigger: {row.path}")
    actual_total = sum(files.values())
    if stated_total != actual_total:
        found.append(f"stated total {stated_total} != wc -l total {actual_total}")
    return found


def main() -> int:
    """Print the counts, name each problem on stderr, and exit 1 when there is any."""
    text = INVENTORY.read_text(encoding="utf-8")
    rows = table_rows(text)
    files = python_files()
    total = TOTAL.search(text)
    found = problems(rows, files, int(total.group(1)) if total else None)
    found += deleted_problems(text, [row.path for row in rows])
    for problem in found:
        print(problem, file=sys.stderr)
    print(f"rows={len(rows)} files={len(files)} lines={sum(files.values())}")
    return 1 if found else 0


if __name__ == "__main__":
    sys.exit(main())
