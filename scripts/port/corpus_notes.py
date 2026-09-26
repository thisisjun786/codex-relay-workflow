"""Read class-A coverage rows out of contract/notes markdown.

A row is a markdown table whose status cell is converted, kept, blocked, or partial,
or a list item whose first backtick is one function name under a heading or lead-in
sentence that states exactly one of those statuses. Anything else that names a test
is not a row.
"""

from __future__ import annotations

import re
from dataclasses import dataclass
from typing import Final

IDENT: Final = r"[A-Za-z_][A-Za-z0-9_]*"
FUNC: Final = re.compile(rf"test_{IDENT}")
QUALIFIED: Final = re.compile(rf"(?:{IDENT}::)?(test_{IDENT})\b")
GROUP: Final = re.compile(rf"({IDENT})\s*\(\d+:\s*((?:{IDENT})(?:\s*,\s*(?:{IDENT}))*)\)")
STATUS_WORD: Final = re.compile(r"\b(converted|kept|blocked|partial)\b", re.IGNORECASE)


@dataclass(frozen=True, slots=True)
class Account:
    """One note row's decision for one test function."""

    status: str
    detail: str
    where: str


@dataclass(frozen=True, slots=True)
class Unreadable:
    """A notes line that names a status the checker cannot turn into a row."""

    where: str
    text: str


def cells(line: str) -> list[str]:
    """Split one markdown table line into stripped cells."""
    return [cell.strip() for cell in line.strip().strip("|").split("|")]


def heading_marks(lines: list[str], stems: list[str]) -> list[tuple[int, str]]:
    """Headings that name one file, plus a shared first heading kept as each file's preamble."""
    raw = [
        (index, [stem for stem in stems if re.search(rf"\b{stem}\.py\b", line)])
        for index, line in enumerate(lines)
        if line.startswith("#")
    ]
    owned = {stem for _, hit in raw if len(hit) == 1 for stem in hit}
    marks = [
        (index, stem)
        for index, hit in raw
        for stem in hit
        if len(hit) == 1 or (hit and not owned.intersection(hit))
    ]
    if raw and len(raw[0][1]) > 1:
        return [(raw[0][0], stem) for stem in raw[0][1]] + marks
    return marks


def accounts_of(section: str, known: set[str]) -> tuple[dict[str, list[Account]], list[Unreadable]]:
    """Read status rows. A row the checker cannot bind is reported, not guessed."""
    found: dict[str, list[Account]] = {}
    unreadable: list[Unreadable] = []
    lines = section.splitlines()
    for index, line in enumerate(lines, start=1):
        row = _row_cells(line)
        if row is not None and len(row) >= 2:  # noqa: PLR2004
            _take_row(found, unreadable, row, known, index, line)
            continue
        _take_bullet(found, lines, line, index)
    _take_explanations(found, lines)
    _take_prose(found, lines, known)
    return found, unreadable


def _take_row(
    found: dict[str, list[Account]],
    unreadable: list[Unreadable],
    row: list[str],
    known: set[str],
    index: int,
    line: str,
) -> None:
    bound = _bound_row(row, known)
    if bound is None:
        if (FUNC.search(" ".join(row)) or GROUP.search(" ".join(row))) and any(
            _status_of(cell) is not None for cell in row
        ):
            unreadable.append(Unreadable(f"line {index}", line.strip()))
        return
    status, names, detail = bound
    for name in names:
        found.setdefault(name, []).append(Account(status, detail, f"line {index}"))


def _take_bullet(found: dict[str, list[Account]], lines: list[str], line: str, index: int) -> None:
    names = _bullet_functions(line)
    status = _lead_status(lines, index)
    if not names or status is None:
        return
    for name in names:
        found.setdefault(name, []).append(Account(status, line.strip(), f"line {index}"))


def _take_explanations(found: dict[str, list[Account]], lines: list[str]) -> None:
    """An explained list item counts only when no table row already accounts for the function."""
    for index, line in enumerate(lines, start=1):
        status = _lead_status(lines, index)
        if status is None:
            continue
        for name in _explained_functions(line):
            if name not in found:
                found.setdefault(name, []).append(Account(status, line.strip(), f"line {index}"))


def _take_prose(found: dict[str, list[Account]], lines: list[str], known: set[str]) -> None:
    """A prose sentence accounts for a backticked function only when no row already does."""
    for index, line in enumerate(lines, start=1):
        if line.strip().startswith(("-", "|")) or _status_of(line) is None:
            continue
        for name in {match for match in FUNC.findall(line) if match in known and match not in found}:
            found.setdefault(name, []).append(Account(_status_of(line) or "", line.strip(), f"line {index}"))


def _status_of(cell: str) -> str | None:
    """The row's status word. A cell that names two statuses is not a row."""
    found = {word.lower() for word in STATUS_WORD.findall(cell)}
    if len(found) != 1:
        return None
    word = next(iter(found))
    return "blocked" if word == "partial" else word


def _row_cells(line: str) -> list[str] | None:
    if not line.startswith("|"):
        return None
    row = cells(line)
    if not row or set(row[0]) <= {"-", ":"}:
        return None
    return row


def _functions_in(cell: str, known: set[str]) -> list[str]:
    """Names this cell accounts for: a qualified name, or one unique fragment per class member."""
    group = GROUP.search(cell)
    if group is None:
        return [match.group(1) for match in QUALIFIED.finditer(cell)]
    resolved: list[str] = []
    for fragment in (part.strip() for part in group.group(2).split(",")):
        hits = [name for name in known if _fragment_hits(fragment, name)]
        if len(hits) != 1:
            return []
        resolved.append(hits[0])
    return resolved


def _fragment_hits(fragment: str, name: str) -> bool:
    """A grouped abbreviation matches the function body, ignoring ``test_``, ``a_`` and ``an_``."""
    body = name.removeprefix("test_")
    for prefix in ("a_", "an_"):
        body = body.removeprefix(prefix)
    return all(part in body for part in fragment.split("_"))


def _bullet_functions(line: str) -> list[str]:
    """A list item whose first backtick is exactly one function name."""
    body = line.strip()
    if not body.startswith("- `"):
        return []
    quoted, _, rest = body[3:].partition("`")
    if rest[:1] in {":", ","} or FUNC.fullmatch(quoted) is None:
        return []
    return [quoted]


def _explained_functions(line: str) -> list[str]:
    """A list item that names one function and then explains it after a colon."""
    body = line.strip()
    if not body.startswith("- `"):
        return []
    quoted, _, rest = body[3:].partition("`")
    if rest[:1] != ":" or FUNC.fullmatch(quoted) is None:
        return []
    return [quoted]


def _lead_status(lines: list[str], index: int) -> str | None:
    """Status announced by the heading, or by the paragraph that introduces this list."""
    heading = _status_of(_nearest_heading(lines, index))
    if heading is not None:
        return heading
    for line in reversed(lines[: index - 1]):
        stripped = line.strip()
        if not stripped or stripped.startswith(("-", "#")):
            continue
        if stripped.startswith("|"):
            return None
        return _status_of(stripped)
    return None


def _nearest_heading(lines: list[str], index: int) -> str:
    """The nearest heading whose own text is a status, ignoring file-name headings."""
    for line in reversed(lines[:index]):
        if not line.startswith("#"):
            continue
        heading = line.lstrip("#").strip()
        if _status_of(heading) is not None:
            return heading
    return ""


def _bound_row(row: list[str], known: set[str]) -> tuple[str, list[str], str] | None:
    """Find the status cell and the function cell to its left. ``None`` when this is not a row."""
    for position, cell in enumerate(row):
        status = _status_of(cell)
        if status is None:
            continue
        for earlier in row[:position]:
            names = _functions_in(earlier, known)
            if names:
                return status, names, " | ".join(row[position + 1 :])
    if len(row) == 2 and re.search(r"[A-Za-z]", row[1]):  # noqa: PLR2004
        names = _functions_in(row[0], known)
        if names:
            return "blocked", names, row[1]
    return None
