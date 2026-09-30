#!/usr/bin/env python3
"""Check docs/port/test-map.md against the test files it claims to classify (dev-only).

Exit 0 only when the table's path column is set-equal to every `test_*.py` under
packages/*/tests and scripts/ci/tests, each path appears once, every class is A, B or C, every
`tests` cell equals `grep -c 'def test_'` of its file, every destination names a known kind,
every C row names its coupling and every drop names its reason, and the stated totals agree
with both the rows and the files (119 files, 6,041 tests when this map was written). Prints
per-class totals; exit 1 otherwise, naming each offending path on stderr.

Todo 44 deletes the files the map classifies, so the map states the revision it is measured at
(`Map revision: <sha>`, the last dev revision that still held every one of them) and the files are
read there with git, not from the working tree.

With --final (todo 44's acceptance) the map must also state that revision, and every destination
must name where its property lives now: each `corpus: <domain>` a domain under contract/fixtures,
which internal/contracttest replays, and each `go-test:` part a directory of this checkout.
"""

import argparse
import re
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path, PurePosixPath
from typing import Final

ROOT: Final = Path(__file__).resolve().parents[2]
TEST_MAP: Final = ROOT / "docs" / "port" / "test-map.md"
COLUMNS: Final = ("path", "tests", "class", "family", "fixtures", "owner", "destination",
                  "coupling")
CLASSES: Final = ("A", "B", "C")
DESTINATION: Final = re.compile(r"(corpus|go-test|inventory-check|drop): \S.*")
EMPTY: Final = frozenset({"", "-"})
STATED: Final = re.compile(r"^(Files|Tests): (\d+)$", re.MULTILINE)
STATED_CLASS: Final = re.compile(r"^Class ([ABC]): files=(\d+) tests=(\d+)$", re.MULTILINE)
MAP_REVISION: Final = re.compile(r"^Map revision: `?([0-9a-f]{40})`?$", re.MULTILINE)


@dataclass(frozen=True, slots=True)
class Row:
    """One map row, reduced to the cells this check judges."""

    path: str
    tests: str
    klass: str
    destination: str
    coupling: str


def count(data: bytes) -> int:
    """`grep -c 'def test_'` of one file's bytes."""
    return sum(b"def test_" in line for line in data.split(b"\n"))


def collected(path: str) -> bool:
    """Whether a repository path is a test_*.py the suites collected (the globs below)."""
    parts = PurePosixPath(path).parts
    return (parts[-1].startswith("test_") and parts[-1].endswith(".py")
            and (len(parts) == 4 and parts[0] == "packages" and parts[2] == "tests"
                 or parts[:-1] == ("scripts", "ci", "tests")))


def git(*args: str) -> bytes:
    """One git command's stdout in this checkout."""
    return subprocess.run(["git", *args], cwd=ROOT, check=True, capture_output=True).stdout


def test_files(revision: str | None = None) -> dict[str, int]:
    """Every test_*.py the suites collect, with its `grep -c 'def test_'` count: in the working
    tree, or at revision when the map names one."""
    if revision is None:
        paths = [*ROOT.glob("packages/*/tests/test_*.py"), *ROOT.glob("scripts/ci/tests/test_*.py")]
        return {path.relative_to(ROOT).as_posix(): count(path.read_bytes()) for path in paths}
    listed = git("ls-tree", "-r", "--name-only", "-z", revision, "--", "packages", "scripts/ci/tests")
    names = sorted(name for name in listed.decode().split("\0") if name and collected(name))
    return {name: count(git("show", f"{revision}:{name}")) for name in names}


def cells(line: str) -> list[str]:
    """Split one markdown table line into stripped cell texts."""
    return [cell.strip() for cell in line.strip().strip("|").split("|")]


def table_rows(text: str) -> list[Row]:
    """Read the rows of the one table whose header is COLUMNS."""
    lines = text.splitlines()
    start = next(i for i, line in enumerate(lines) if tuple(cells(line)) == COLUMNS)
    rows: list[Row] = []
    for line in lines[start + 2:]:
        if not line.startswith("|"):
            break
        values = cells(line)
        if len(values) != len(COLUMNS):
            rows.append(Row(values[0].strip("`"), "", "", "", ""))
            continue
        path, tests, klass, _, _, _, destination, coupling = values
        rows.append(Row(path.strip("`"), tests, klass, destination, coupling))
    return rows


def row_problems(row: Row, files: dict[str, int]) -> list[str]:
    """Name what is wrong with one row on its own."""
    found: list[str] = []
    if row.path in files and row.tests != str(files[row.path]):
        found.append(f"tests {row.tests!r} != grep -c 'def test_' {files[row.path]}: {row.path}")
    if row.klass not in CLASSES:
        found.append(f"class {row.klass!r} is not A, B or C: {row.path}")
    parts = [part.strip() for part in row.destination.split(" + ")]
    if not all(DESTINATION.fullmatch(part) for part in parts):
        found.append(f"destination {row.destination!r} has an unknown kind: {row.path}")
    if any(part == "drop:" or part.startswith("drop: -") for part in parts):
        found.append(f"drop names no reason: {row.path}")
    if row.klass == "C" and row.coupling in EMPTY:
        found.append(f"C row names no coupling: {row.path}")
    return found


def total_problems(text: str, rows: list[Row], files: dict[str, int]) -> list[str]:
    """Compare the stated totals with the rows' classes and the files' counts."""
    found: list[str] = []
    stated = {match.group(1): int(match.group(2)) for match in STATED.finditer(text)}
    actual = {"Files": len(files), "Tests": sum(files.values())}
    for name, value in actual.items():
        if stated.get(name) != value:
            found.append(f"stated {name} {stated.get(name)} != measured {value}")
    stated_class = {match.group(1): (int(match.group(2)), int(match.group(3)))
                    for match in STATED_CLASS.finditer(text)}
    for klass in CLASSES:
        members = [row.path for row in rows if row.klass == klass and row.path in files]
        measured = (len(members), sum(files[path] for path in members))
        if stated_class.get(klass) != measured:
            found.append(f"stated class {klass} {stated_class.get(klass)} != rows {measured}")
    return found


GO_PATH: Final = re.compile(r"(?:internal|cmd)/[A-Za-z0-9_./{},-]*")
CORPUS: Final = re.compile(r"corpus: ([A-Za-z0-9-]+)")


def go_directory(part: str) -> str | None:
    """The first directory a `go-test:` part names: the first internal/ or cmd/ path in it, cut at
    its first component that is a file (`.go`) or a brace group, trailing punctuation dropped."""
    match = GO_PATH.search(part)
    if match is None:
        return None
    components = []
    for component in match.group(0).rstrip(".,;").split("/"):
        if not component or "{" in component or component.endswith(".go"):
            break
        components.append(component)
    return "/".join(components)


def final_problems(text: str, rows: list[Row]) -> list[str]:
    """Name every destination that does not name where its property lives in this checkout."""
    found: list[str] = []
    if MAP_REVISION.search(text) is None:
        found.append("the map states no `Map revision: <sha>`")
    domains = {path.name for path in (ROOT / "contract" / "fixtures").iterdir() if path.is_dir()}
    for row in rows:
        for part in (part.strip() for part in row.destination.split(" + ")):
            if part.startswith("corpus:"):
                match = CORPUS.match(part)
                if match is None or match.group(1) not in domains:
                    found.append(f"corpus {part[:60]!r} is not a domain under contract/fixtures: {row.path}")
            elif part.startswith("go-test:"):
                directory = go_directory(part)
                if not directory or not (ROOT / directory).is_dir():
                    found.append(f"go-test {part[:60]!r} names no directory of this checkout: {row.path}")
    return found


def problems(text: str, rows: list[Row], files: dict[str, int]) -> list[str]:
    """Name every disagreement between the map and the files; empty when they agree."""
    listed = [row.path for row in rows]
    found = [f"duplicated row: {path}"
             for path in sorted({path for path in listed if listed.count(path) > 1})]
    found += [f"missing from test map: {path}" for path in sorted(set(files) - set(listed))]
    found += [f"not a collected test file: {path}" for path in sorted(set(listed) - set(files))]
    for row in rows:
        found += row_problems(row, files)
    return found + total_problems(text, rows, files)


def main(argv: list[str] | None = None) -> int:
    """Print the totals, name each problem on stderr, and exit 1 when there is any."""
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--final", action="store_true",
                        help="also require every destination to name where its property lives")
    args = parser.parse_args(argv)
    text = TEST_MAP.read_text(encoding="utf-8")
    rows = table_rows(text)
    revision = MAP_REVISION.search(text)
    files = test_files(revision.group(1) if revision else None)
    found = problems(text, rows, files)
    if args.final:
        found += final_problems(text, rows)
    for problem in found:
        print(problem, file=sys.stderr)
    for klass in CLASSES:
        members = [row.path for row in rows if row.klass == klass and row.path in files]
        print(f"class {klass}: files={len(members)} tests={sum(files[p] for p in members)}")
    print(f"rows={len(rows)} files={len(files)} tests={sum(files.values())}")
    return 1 if found else 0


if __name__ == "__main__":
    sys.exit(main())
