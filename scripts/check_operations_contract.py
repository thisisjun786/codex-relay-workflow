#!/usr/bin/env python3
"""Replay the operations fixtures against the contract they claim to follow.

This checks structure and referential integrity, which is the honest limit of what a fixture can
prove: that every clause a fixture cites exists, that every normative clause is cited by a fixture,
that each scenario states all four of its parts with actual content, and that the example records
carry the fields the contract declares.

It does not execute the workflow, it does not read the fixtures for meaning, and it is not evidence
of runtime behaviour. A scenario that cites the right clause and prescribes the wrong action passes
here, so this is a guard against drift between the contract and its fixtures, not a correctness
proof of either.
"""

import argparse
import json
import re
import sys
from pathlib import Path

TICK = chr(96)
CLAUSE = re.compile(r"OPS-\d+(?:\.\d+)?")
HEADING = re.compile(r"^#{2,3}\s+(OPS-\d+(?:\.\d+)?)\b")
NORMATIVE = re.compile(r"^###\s+(OPS-\d+\.\d+)\b")
REGISTER_ROW = re.compile(r"^\|\s*(OPS-\d+\.\d+)\s*\|")
TICKED = re.compile(TICK + r"([A-Za-z][A-Za-z0-9_]*)" + TICK)
SCENARIO = re.compile(r"^##\s+(S\d+)\s+(.+)$")
REQUIRED_PARTS = ("Observed:", "Clauses:", "Action:", "Preserved:")
# The narrative parts must actually say something. The clause list is short by nature and is
# checked by the clause pattern instead.
MIN_PART_CHARACTERS = {"Observed:": 40, "Clauses:": 7, "Action:": 80, "Preserved:": 40}
FIELD_VALUES = {"verified", "not_verified", "unknown", "not_applicable"}
POINT_KEYS = ("interpreter", "codexCli", "appServer", "host", "date", "measuredBy", "method")


def read(path):
    return path.read_text(encoding="utf-8")


def contract_clauses(text):
    """Every clause id the contract defines, and the subset that must be exercised."""
    defined, normative = set(), set()
    for line in text.splitlines():
        heading = HEADING.match(line)
        if heading:
            defined.add(heading.group(1))
        normative_match = NORMATIVE.match(line)
        if normative_match:
            normative.add(normative_match.group(1))
        row = REGISTER_ROW.match(line)
        if row:
            defined.add(row.group(1))
    defined |= {clause.split(".")[0] for clause in defined}
    return defined, normative


def declared_check_fields(contract_text):
    """The field names the OPS-6.1 table names, read from the contract itself."""
    names, inside = [], False
    for line in contract_text.splitlines():
        if line.startswith("### OPS-6.1"):
            inside = True
            continue
        if inside and line.startswith("###"):
            break
        if inside and line.startswith("| ") and not line.startswith("| Field") and "---" not in line:
            found = TICKED.findall(line.split("|")[1])
            if found:
                names.append(found[0])
    return set(names)


def check_result_record(contract_text, record, problems):
    declared = declared_check_fields(contract_text)
    present = set(record.get("fields", {}))
    if not declared:
        problems.append("OPS-6.1 declares no fields, so the check-result example cannot be verified")
    elif declared != present:
        problems.append(
            "check-result fields " + str(sorted(present))
            + " do not match OPS-6.1 " + str(sorted(declared))
        )
    for name, field in record.get("fields", {}).items():
        for key in ("value", "evidence", "command", "actor", "measuredAt"):
            if key not in field:
                problems.append("check-result field " + name + " is missing " + key)
        if field.get("value") not in FIELD_VALUES:
            problems.append("check-result field " + name + " has an undeclared value")


def compatibility_record(record, problems):
    components = record.get("components", [])
    if not components:
        problems.append("the compatibility example records no component")
    if len({tuple(sorted(component)) for component in components}) > 1:
        problems.append("compatibility components do not share one field set")
    for component in components:
        name = component.get("component", "<unnamed>")
        for key in ("source", "revision", "tree", "version", "requiresPython", "installs"):
            if not component.get(key):
                problems.append(name + " is missing a non-empty " + key)
        # OPS-1.3 lets a component legitimately hold no measured point: a combination nobody has
        # exercised is unmeasured, and the honest record says so with an empty list rather than by
        # promoting an inventory observation. The key itself is still required, because silence
        # about evidence is not the same as stating there is none.
        if "measuredPoints" not in component:
            problems.append(name + " does not state measuredPoints, not even as an empty list")
        elif not isinstance(component.get("measuredPoints"), list):
            problems.append(name + " states measuredPoints as something other than a list")
        # OPS-1.1 names cleanliness and the checkout path as required inputs, and OPS-2.1 classifies
        # from the entry point and the resolved package location. A record silent about any of them
        # cannot be classified, so silence is refused rather than read as a default.
        if "workingTreeClean" not in component:
            problems.append(name + " does not state workingTreeClean, which OPS-2.1 needs as a signal")
        if not component.get("source", {}).get("checkout"):
            problems.append(name + " has no source.checkout path")
        if len(str(component.get("revision", ""))) != 40:
            problems.append(name + " revision is not a full 40 character commit id")
        if len(str(component.get("tree", ""))) != 40:
            problems.append(name + " tree is not a full 40 character tree id")
        if "remote" not in component.get("source", {}):
            problems.append(name + " does not state a remote, not even as none")
        for install in component.get("installs", []):
            if install.get("installMode") not in {"editable", "copied"}:
                problems.append(name + " has an install with an undeclared installMode")
            if len(str(install.get("integrity", ""))) != 64:
                problems.append(name + " has an install without a 64 character integrity digest")
            for key in ("environment", "location", "entryPoint"):
                if not install.get(key):
                    problems.append(name + " has an install with no " + key + ", so OPS-1.1 cannot separate the environment from the imported package")
        points = component.get("measuredPoints")
        for point in points if isinstance(points, list) else []:
            # A malformed entry is reported rather than raised, because a checker that crashes on bad
            # input tells the reader less than one that names what is wrong.
            if not isinstance(point, dict):
                problems.append(name + " has a measured point that is not an object")
                continue
            for key in POINT_KEYS:
                if key not in point:
                    problems.append(name + " has a measured point missing " + key)
    if "unmeasured" not in record:
        problems.append("the compatibility example does not say what is unmeasured, which invites a range claim")


def scenarios(text, problems):
    blocks, current = {}, None
    for line in text.splitlines():
        match = SCENARIO.match(line)
        if match:
            current = match.group(1)
            blocks[current] = []
        elif current is not None:
            blocks[current].append(line)
    if len(blocks) < 6:
        problems.append("only " + str(len(blocks)) + " scenarios found, the contract requires at least the six named situations")
    for name, lines in sorted(blocks.items()):
        body = "\n".join(lines)
        positions = {}
        for part in REQUIRED_PARTS:
            index = body.find(part)
            if index < 0:
                problems.append("scenario " + name + " is missing its " + part.rstrip(":") + " part")
            else:
                positions[part] = index
        # A present but empty part is the failure a heading check misses, so measure each part's
        # own text up to the next part rather than trusting the label.
        ordered = sorted(positions.items(), key=lambda item: item[1])
        for offset, (part, index) in enumerate(ordered):
            end = ordered[offset + 1][1] if offset + 1 < len(ordered) else len(body)
            written = "".join(body[index + len(part):end].split())
            minimum = MIN_PART_CHARACTERS[part]
            if len(written) < minimum:
                problems.append(
                    "scenario " + name + " states its " + part.rstrip(":")
                    + " part in fewer than " + str(minimum) + " characters"
                )
        if not CLAUSE.search(body):
            problems.append("scenario " + name + " cites no clause")
    return blocks


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parent.parent)
    args = parser.parse_args()
    references = args.root / "skills" / "linear-run" / "references"
    contract_path = references / "operations.md"
    fixtures = references / "operations"

    missing = [path for path in (contract_path, fixtures) if not path.exists()]
    if missing:
        for path in missing:
            print("MISSING " + str(path), file=sys.stderr)
        return 1

    problems = []
    contract_text = read(contract_path)
    defined, normative = contract_clauses(contract_text)

    scenario_text = read(fixtures / "scenarios.md")
    plan_text = read(fixtures / "installation-plan.example.md")
    blocks = scenarios(scenario_text, problems)

    # Every fixture is scanned, including the JSON records: a clause id cited in an example
    # record is as capable of going stale as one cited in prose.
    cited = set()
    for fixture in sorted(fixtures.iterdir()):
        if fixture.is_file():
            cited |= set(CLAUSE.findall(read(fixture)))
    for clause in sorted(cited - defined):
        problems.append("fixtures cite " + clause + ", which the contract does not define")
    for clause in sorted(normative - cited):
        problems.append(clause + " is never exercised by a fixture")

    try:
        compatibility = json.loads(read(fixtures / "compatibility-record.example.json"))
        check_result = json.loads(read(fixtures / "check-result.example.json"))
    except json.JSONDecodeError as error:
        problems.append("an example record is not valid JSON: " + str(error))
    else:
        compatibility_record(compatibility, problems)
        check_result_record(contract_text, check_result, problems)

    print("contract: " + str(len(defined)) + " clause ids, " + str(len(normative)) + " normative")
    print("fixtures: " + str(len(blocks)) + " scenarios, " + str(len(cited)) + " distinct clause citations")
    if problems:
        print("", file=sys.stderr)
        for problem in problems:
            print("FAIL " + problem, file=sys.stderr)
        print("\n" + str(len(problems)) + " problem(s). Nothing was modified.", file=sys.stderr)
        return 1
    print(
        "OK every clause cited by any fixture exists, every normative clause is cited, every "
        "scenario states all four parts, and both example records match the fields the contract "
        "declares. This is citation and shape only: it does not read any fixture for meaning."
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
