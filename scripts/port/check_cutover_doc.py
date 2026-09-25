#!/usr/bin/env python3
"""Structural check for docs/port/cutover.md and docs/port/control-group.md (dev-only).

Exit 0 only when cutover.md is valid UTF-8, carries each required `## ` section exactly once,
and on every line that mentions `ttl` or `heartbeat`: each clause mentioning the term negates
it with an explicit pattern ("no ttl", "never ... heartbeat", "not by ttl", "ttl ... never"),
and each clause with authorising language (authori*, takeover, permit, allow, proceed) has a
negation that governs that verb ("never authorises", "does not permit", "no TTL or heartbeat
ever authorises"). Clauses are split on `;`, `:`, `.`, `,`, but/while/whereas/so/that/which,
and and/or when they open a new clause, so a negation in one clause cannot cover a positive
statement in the next. control-group.md must have `### CRW-116-<n>` and
`### CRW-124-<n>` headings numbered 1..N without gaps, each followed by a `Pass when:` line
before the next heading. Exit 1 otherwise, naming each problem.
"""

import re
import sys
from pathlib import Path
from typing import Final

ROOT: Final = Path(__file__).resolve().parents[2]
CUTOVER: Final = ROOT / "docs" / "port" / "cutover.md"
CONTROL: Final = ROOT / "docs" / "port" / "control-group.md"
SECTIONS: Final[tuple[str, ...]] = (
    "## Record", "## Lock order", "## Steps 0-7", "## Rollback", "## Commit point",
    "## Inbox", "## Hook budget", "## Retention", "## Retention scan surface",
)
ISSUES: Final[tuple[str, ...]] = ("CRW-116", "CRW-124")
TERM: Final = r"(?:ttls?|heartbeats?)"
TIMER: Final = re.compile(TERM, re.IGNORECASE)
# Clause boundaries: punctuation, subordinating and contrastive conjunctions, relative pronouns,
# and "and"/"or" when they open a new clause (followed by an article, determiner or pronoun)
# rather than join two nouns ("no TTL or heartbeat").
CLAUSE_SPLIT: Final = re.compile(
    r"[;:.,]|\b(?:but|while|whereas|so|that|which)\b"
    + r"|\b(?:and|or)\b(?=\s+(?:a|an|the|its|this|that|it|they|we|no|any|every|each)\b)",
    re.IGNORECASE,
)
# A clause that mentions the term is negated only when one of these explicit shapes surrounds it.
NEGATED: Final = re.compile(
    r"\bno\s+(?:\w+\s+){0,3}" + TERM  # "no TTL", "no expiry-based heartbeat"
    + r"|\bnever\b(?:\s+\w+){0,4}\s+" + TERM  # "never ... by heartbeat"
    + r"|\bnot\s+(?:by|through|via|from)\s+(?:\w+\s+){0,2}" + TERM  # "not by ttl"
    + r"|" + TERM + r"\b(?:\s+\w+){0,4}\s+(?:never|does not|do not|cannot|must not|will not)\b",
    re.IGNORECASE,
)
# Authorising language: any clause on a ttl/heartbeat line that says takeover happens.
AUTHORI: Final = re.compile(r"authori|takeover|permit|allow|proceed", re.IGNORECASE)
# The negation must govern the authorising verb itself: "never authorises", "does not permit",
# "no TTL or heartbeat ever authorises". A negation elsewhere in the clause ("never expires
# authorizes") does not count.
NEG_GOVERNS: Final = re.compile(
    r"\b(?:never|not|cannot|no\s+\w+(?:\s+(?:or|and|\w+)){0,3})\s+(?:ever\s+)?(?:authori|permit|allow|proceed|takeover)",
    re.IGNORECASE,
)
SCENARIO: Final = re.compile(r"^### (CRW-1(?:16|24))-(\d+):")


def check_cutover(text: str) -> list[str]:
    problems: list[str] = []
    lines = text.splitlines()
    headings = [line.strip() for line in lines if line.startswith("## ")]
    for section in SECTIONS:
        count = headings.count(section)
        if count == 0:
            problems.append(f"cutover.md: missing section {section!r}")
        elif count > 1:
            problems.append(f"cutover.md: section {section!r} appears {count} times; exactly once required")
    for number, line in enumerate(lines, 1):
        if not TIMER.search(line):
            continue
        for clause in CLAUSE_SPLIT.split(line):
            clause = clause.strip()
            if not clause:
                continue
            if AUTHORI.search(clause) and not NEG_GOVERNS.search(clause):
                problems.append(f"cutover.md:{number}: authorising clause on a ttl/heartbeat line is not governed by a negation: {clause!r}")
            elif TIMER.search(clause) and not NEGATED.search(clause):
                problems.append(f"cutover.md:{number}: ttl/heartbeat clause without an explicit negation: {clause!r}")
    return problems


def check_control(text: str) -> tuple[list[str], dict[str, int]]:
    problems: list[str] = []
    seen: dict[str, list[int]] = {issue: [] for issue in ISSUES}
    current: str | None = None
    has_pass = False
    for line in text.splitlines():
        match = SCENARIO.match(line)
        if match:
            if current and not has_pass:
                problems.append(f"control-group.md: {current} has no 'Pass when:' line")
            current = f"{match.group(1)}-{match.group(2)}"
            has_pass = False
            seen[match.group(1)].append(int(match.group(2)))
        elif line.startswith("## ") and current:
            if not has_pass:
                problems.append(f"control-group.md: {current} has no 'Pass when:' line")
            current = None
        elif current and "Pass when:" in line:
            has_pass = True
    if current and not has_pass:
        problems.append(f"control-group.md: {current} has no 'Pass when:' line")
    for issue, numbers in seen.items():
        if not numbers:
            problems.append(f"control-group.md: no ### {issue}-<n> headings")
        elif numbers != list(range(1, len(numbers) + 1)):
            problems.append(f"control-group.md: {issue} headings are not 1..{len(numbers)} in order: {numbers}")
    return problems, {issue: len(numbers) for issue, numbers in seen.items()}


def read_utf8(path: Path) -> tuple[str | None, str | None]:
    try:
        return path.read_bytes().decode("utf-8"), None
    except UnicodeDecodeError as error:
        return None, f"{path.relative_to(ROOT)}: not valid UTF-8 at byte {error.start}: {error.reason}"


def main() -> int:
    problems: list[str] = []
    for path in (CUTOVER, CONTROL):
        if not path.is_file():
            problems.append(f"missing file {path.relative_to(ROOT)}")
    if problems:
        for problem in problems:
            print(problem, file=sys.stderr)
        return 1
    cutover_text, error = read_utf8(CUTOVER)
    if error:
        problems.append(error)
    control_text, error = read_utf8(CONTROL)
    if error:
        problems.append(error)
    counts: dict[str, int] = {issue: 0 for issue in ISSUES}
    if cutover_text is not None:
        problems.extend(check_cutover(cutover_text))
    if control_text is not None:
        control_problems, counts = check_control(control_text)
        problems.extend(control_problems)
    for problem in problems:
        print(problem, file=sys.stderr)
    if problems:
        return 1
    print(f"sections={len(SECTIONS)} crw116={counts['CRW-116']} crw124={counts['CRW-124']}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
