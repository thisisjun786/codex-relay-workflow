#!/usr/bin/env python3
"""Check a start-policy record's two closed-vocabulary fields against the contract.

../references/start-policy.md declares `run_mode` and `observation_path` as closed
vocabularies and pins their legal combinations in one table. Four readers of that document on
2026-09-21 wrote a paraphrase into both fields anyway, and a fifth restored a paraphrased record
as though it were canonical. Asking for the literals in prose, offering them as a list to copy,
and requiring a consumer to reject anything outside the set were each measured, and none of the
three changed a reader's behaviour. So the enumeration is available here as something a producer
and a consumer can run, rather than something either has to recall correctly.

vocabulary  print the declared values and the legal pairings.
check       read `field: value` lines and report each field and then the pairing. Exit
            non-zero when anything failed.
selftest    assert the parsed vocabulary is the declared one and run the recorded paraphrases
            through check as negative cases.

The table in the contract is the source of truth and this script parses it rather than carrying
its own copy, so a literal renamed in the document cannot leave the two disagreeing silently.

What this does not do: it does not decide whether a path is available. The five readiness facts
own that, and a record this script calls well formed can still be a record that should not have
been written. It also proves nothing about a reader who never runs it.
"""

import argparse
from pathlib import Path
import re
import sys

CONTRACT = Path(__file__).resolve().parents[1] / "references" / "start-policy.md"
FIELDS = ("run_mode", "observation_path")
CELL = re.compile(r"`([^`]+)`")


class ContractError(RuntimeError):
    """The contract could not be read as the source of the vocabulary."""


def parse_contract(text):
    """Return (run_modes, observation_paths, legal) from the pairing table.

    The table is recognised by its empty leading header cell followed by backticked column
    names; its row labels are the run modes. A cell is illegal when it says so.
    """
    lines = text.splitlines()
    for index, line in enumerate(lines):
        stripped = line.strip()
        if not stripped.startswith("| |"):
            continue
        columns = [CELL.search(cell) for cell in stripped.split("|")[2:-1]]
        if not columns or not all(columns):
            continue
        paths = [match.group(1) for match in columns]
        modes, legal = [], {}
        for row in lines[index + 2:]:
            row = row.strip()
            if not row.startswith("|"):
                break
            cells = [cell.strip() for cell in row.split("|")[1:-1]]
            label = CELL.search(cells[0]) if cells else None
            if not label or len(cells) != len(paths) + 1:
                break
            mode = label.group(1)
            modes.append(mode)
            for path, cell in zip(paths, cells[1:]):
                legal[(mode, path)] = not re.search(r"\billegal\b", cell)
        if modes:
            return modes, paths, legal
    raise ContractError(f"no pairing table found in {CONTRACT}")


def load():
    try:
        return parse_contract(CONTRACT.read_text(encoding="utf-8"))
    except OSError as error:
        raise ContractError(f"cannot read {CONTRACT}: {error}") from error


def read_record(lines):
    """Pull `field: value` pairs out of record text.

    Markdown decoration around the value is formatting, not a different literal, so a bullet,
    backticks or quotes are removed before the value is compared. The value itself is compared
    exactly: that is the whole point of the check.
    """
    found = {}
    for line in lines:
        text = line.strip().lstrip("-*").strip()
        if ":" not in text:
            continue
        name, _, value = text.partition(":")
        name = name.strip().strip("`*_").strip()
        if name not in FIELDS or name in found:
            continue
        found[name] = value.strip().strip("`'\"").strip().rstrip(".,;")
    return found


def check(record, vocabulary, out):
    modes, paths, legal = vocabulary
    declared = {"run_mode": modes, "observation_path": paths}
    failed = False
    for field in FIELDS:
        allowed = " | ".join(declared[field])
        if field not in record:
            out.append(f"{field}: missing -> declared: {allowed}")
            failed = True
        elif record[field] in declared[field]:
            out.append(f"{field}: {record[field]} -> ok")
        else:
            out.append(f"{field}: {record[field]} -> not in set; declared: {allowed}")
            failed = True
    if failed:
        out.append("pairing: not checked, a field is unreadable")
        out.append("An unreadable field is re-adjudicated exactly as a missing one is.")
        return False
    pair = (record["run_mode"], record["observation_path"])
    if legal.get(pair):
        out.append(f"pairing: {pair[0]} + {pair[1]} -> legal")
        return True
    out.append(f"pairing: {pair[0]} + {pair[1]} -> illegal; this is a record to repair")
    return False


def show_vocabulary(vocabulary, out):
    modes, paths, legal = vocabulary
    out.append("run_mode: " + " | ".join(modes))
    out.append("observation_path: " + " | ".join(paths))
    out.append("legal pairings, each one two lines to copy:")
    for mode in modes:
        for path in paths:
            if legal.get((mode, path)):
                out.append("")
                out.append(f"  run_mode: {mode}")
                out.append(f"  observation_path: {path}")


SELFTEST = (
    ("the declared default", ["run_mode: goal-free-run", "observation_path: event-driven-idle"], True),
    ("a transitional record", ["run_mode: goal-free-run", "observation_path: blocked"], True),
    ("backticked and bulleted", ["- `run_mode`: `loop`", "- `observation_path`: `active-observation`"], True),
    ("parent G", ["run_mode: relay_only", "observation_path: relay"], False),
    ("parent H", ["run_mode: goal_free", "observation_path: relay"], False),
    ("parent I", ["run_mode: run_only", "observation_path: relay"], False),
    ("parent J", ["run_mode: relay_only", "observation_path: relay"], False),
    ("a parked parent given a running parent's path", ["run_mode: blocked", "observation_path: event-driven-idle"], False),
    ("nothing recorded", ["scope: this-run"], False),
)


def selftest(vocabulary, out):
    modes, paths, _ = vocabulary
    expected = (
        ["goal-free-run", "loop", "blocked"],
        ["event-driven-idle", "active-observation", "blocked"],
    )
    if (modes, paths) != expected:
        out.append(f"vocabulary drifted: parsed {modes} and {paths}, expected {expected[0]} and {expected[1]}")
        return False
    out.append(f"vocabulary: {len(modes)} run modes and {len(paths)} observation paths, as declared")
    ok = True
    for name, lines, want in SELFTEST:
        got = check(read_record(lines), vocabulary, [])
        if got != want:
            ok = False
        out.append(f"{'ok' if got == want else 'FAILED'}: {name} -> {'accepted' if got else 'rejected'}")
    return ok


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    sub = parser.add_subparsers(dest="mode", required=True)
    sub.add_parser("vocabulary", help="print the declared values and the legal pairings")
    checker = sub.add_parser("check", help="check a record's two closed-vocabulary fields")
    checker.add_argument("record", nargs="?", help="file to read; omit to read stdin")
    sub.add_parser("selftest", help="check the vocabulary and the recorded negative cases")
    args = parser.parse_args(argv)

    try:
        vocabulary = load()
    except ContractError as error:
        print(error, file=sys.stderr)
        return 2

    out = []
    if args.mode == "vocabulary":
        show_vocabulary(vocabulary, out)
        ok = True
    elif args.mode == "selftest":
        ok = selftest(vocabulary, out)
    else:
        text = Path(args.record).read_text(encoding="utf-8") if args.record else sys.stdin.read()
        ok = check(read_record(text.splitlines()), vocabulary, out)
    print("\n".join(out).rstrip())
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(main())
