#!/usr/bin/env python3
"""Compute a project parent's task title from the product family its project actually carries.

../../crw-plan/references/integrations.md#set-the-app-presentation-and-record owns the rule: a
project parent's Codex task title leads with the linked project's product-family label in
brackets, spelled exactly as Linear spells it. The label is data, so nothing here maps, expands or
case-folds it, and there is no table of known families to drift.

decide    read one request as JSON on stdin and print the decision. Exit 2 when the request
          itself is unreadable, because a malformed call must not look like a settled title.
readback  classify a rename readback: verified, mismatch or unread.
replay    run every fixture against its recorded expectation, and fail when a decision this
          module can reach has no fixture reaching it.

The caller supplies the family candidates already scoped to the product-family label group. That
is deliberate: the group is not exposed by the project read, so a helper counting labels would
prefix confidently from another group whenever a project with no family carried exactly one label.
Membership is evidence the caller has to bring; a count is not membership.

What this does not do. It writes no title and reads no host, so a decision here is a proposal and
never evidence that a task is named anything. It settles no binding either: every answer below
assumes the caller has already matched this task to this project by their stable IDs, and the one
thing it does about that is refuse to proceed when the caller says that check has not been made.
replay proves this module agrees with its recorded expectations, and proves nothing about a title
having been written, displayed, or read back from a real host.
"""

import argparse
import ast
import json
from pathlib import Path
import re
import sys

FIXTURES = Path(__file__).resolve().parent / "fixtures" / "titles"
# A leading bracket is a prefix whether or not anything follows it: "[CRW]" on its own is
# already this family's prefix, and adding a second one would break idempotence.
BRACKET = re.compile(r"^(\[([^\]]*)\])(\s*)")
# A bare label counts as a separable prefix only when whitespace surrounds the separator.
# Without that rule "CRW-137 · 제목" reads as the label CRW followed by "-", and the issue code
# loses its number.
SEPARATORS = ("\u00b7", ":", "-", "\u2014", "|", "/")
ROLES = ("parent", "supervisor", "child")
USER_TITLE = ("none", "descriptive", "fixed")
BRACKET_ACTIONS = ("body", "replace")
READBACK = ("verified", "mismatch", "unread")
# Several inputs reach one decision: a title with no prefix, one already carrying this family,
# and one whose foreign bracket was kept or replaced all end at the same return. Naming the
# structural branch keeps the fixture denominator from counting them as one covered case.
MATCHES = ("none", "bracket_family", "bracket_body", "bracket_replaced", "bare_label")


class RequestError(ValueError):
    """The request could not be read as a decision request."""


def settle(decision, reason, title=None, prefix=None, body=None, stripped=None, requires=None,
           matched=None):
    """One decision, named the same way every caller and every fixture names it."""
    return {
        "decision": decision,
        "reason": reason,
        "title": title,
        "prefix": prefix,
        "body": body,
        "stripped": stripped,
        "matched": matched,
        "requires": list(requires or ()),
    }


def _text(request, key):
    value = request.get(key)
    if value is None:
        return None
    if not isinstance(value, str):
        raise RequestError(f"{key} must be a string or null")
    return value


def _names(request, key):
    value = request.get(key, [])
    if not isinstance(value, list) or not all(isinstance(item, str) for item in value):
        raise RequestError(f"{key} must be a list of strings")
    return value


def bracket_prefix(title):
    """Return (lexeme, inner, removed, body) when the title opens with a bracketed prefix.

    The lexeme is the bracket alone, which is what a caller sees and therefore what a
    disposition names. The whitespace after it is kept in @removed so that stripping stays
    exact without making the caller reproduce an invisible trailing space.
    """
    match = BRACKET.match(title)
    if not match:
        return None
    lexeme, inner, gap = match.group(1), match.group(2), match.group(3)
    return lexeme, inner, lexeme + gap, title[match.end():]


def bare_prefix(title, family):
    """Return (removed, body) when the title opens with the family as a separable token.

    Separable means label, whitespace, one separator, whitespace, then something left. A label
    glued to what follows it is part of the sentence: "CRW를 설치형 …" is a subject with a
    particle and "CRW-137" is an issue code, and neither is a prefix to remove.
    """
    if not title.startswith(family):
        return None
    rest = title[len(family):]
    if not rest[:1].isspace():
        return None
    rest = rest.lstrip()
    if not rest or rest[0] not in SEPARATORS:
        return None
    tail = rest[1:]
    if not tail[:1].isspace():
        return None
    body = tail.lstrip()
    if not body:
        return None
    return title[: len(title) - len(body)], body


def decide(request):
    """Apply the rule to one request and return its single decision."""
    if not isinstance(request, dict):
        raise RequestError("the request must be a JSON object")

    role = _text(request, "role")
    if role not in ROLES:
        return settle("invalid", "malformed_role",
                      requires=["role as one of " + ", ".join(ROLES)])
    if role != "parent":
        # The prefix names the project parent. A child keeps ISSUE-ID · title and a supervision
        # task keeps its own name, so the title comes back untouched rather than reformatted.
        observed = _text(request, "observed_title")
        return settle("unchanged", "role_out_of_scope", title=observed, body=observed)

    if request.get("binding_verified") is not True:
        # Ordered first on purpose. A prefix written before the record matches this task ID to
        # this project ID would be a guess wearing the project's label.
        return settle("withhold", "binding_unverified",
                      title=_text(request, "observed_title"),
                      requires=["management record matching this task ID to this project ID"])

    observed = _text(request, "observed_title")
    summary = _text(request, "summary")
    source = observed or summary
    if not source:
        return settle("invalid", "no_title_source",
                      requires=["observed_title read by task ID, or summary for a new task"])

    user_title = request.get("user_title", "none")
    if user_title not in USER_TITLE:
        return settle("invalid", "malformed_user_title",
                      requires=["user_title as one of " + ", ".join(USER_TITLE)])
    if user_title == "fixed":
        # The user fixed this exact string, or forbade the rename. Presentation rules do not
        # outrank that. The title returned is the source, so a task being created with a fixed
        # title still gets one back rather than a null.
        return settle("unchanged", "user_fixed_title", title=source, body=source)

    candidates = _names(request, "family_candidates")
    project_labels = _names(request, "project_labels")
    if not candidates:
        return settle("withhold", "no_family_label", title=observed,
                      requires=["the project's product-family label, or confirmation it has none"])
    if len(candidates) > 1:
        return settle("withhold", "ambiguous_family", title=observed,
                      requires=["which of " + ", ".join(candidates) + " is the product family"])
    family = candidates[0]
    if family not in project_labels:
        return settle("withhold", "family_unverified", title=observed,
                      requires=["the candidate read back among this project's current labels"])

    stripped = None
    body = source
    matched = "none"
    bracketed = bracket_prefix(source)
    if bracketed:
        lexeme, inner, removed, rest = bracketed
        if inner == family and not rest.strip():
            # The whole title is this prefix. There is nothing to add to it.
            return settle("unchanged", "already_prefixed", title=source, prefix=family,
                          body="", matched="bracket_family")
        if inner != family and not rest.strip():
            # A bracket alone that is not this family names nothing that could survive a
            # decision, so there is no title to propose either way.
            return settle("withhold", "foreign_prefix", title=observed,
                          requires=["a title body beside " + lexeme])
        if inner == family:
            body = rest
            matched = "bracket_family"
        else:
            disposition = request.get("bracket_disposition")
            action = None
            if isinstance(disposition, dict) and disposition.get("bracket") == lexeme:
                action = disposition.get("action")
            if action not in BRACKET_ACTIONS:
                # A bracket that is not this family may be an obsolete family or the user's own
                # words. Nothing here can tell those apart, and stacking a second bracket to
                # avoid deciding would put two classifications on one title. A disposition
                # naming some other bracket is not an answer about this one.
                return settle("withhold", "foreign_prefix", title=observed,
                              requires=["bracket_disposition naming " + lexeme
                                        + " as body or replace"])
            if action == "replace":
                stripped, body = removed, rest
                matched = "bracket_replaced"
            else:
                matched = "bracket_body"
    else:
        bare = bare_prefix(source, family)
        if bare:
            stripped, body = bare
            matched = "bare_label"

    title = "[" + family + "] " + body
    if observed is not None and title == observed:
        return settle("unchanged", "already_prefixed", title=title, prefix=family, body=body,
                      matched=matched)
    if stripped:
        return settle("apply", "prefix_replaced", title=title, prefix=family, body=body,
                      stripped=stripped, matched=matched)
    return settle("apply", "prefix_added", title=title, prefix=family, body=body,
                  stripped=stripped, matched=matched)


def classify_readback(requested, observed):
    """Say what a rename readback established, and never more than it did."""
    if observed is None:
        return "unread"
    if observed == requested:
        return "verified"
    return "mismatch"


def reachable_reasons(path=None):
    """Derive the reason vocabulary from this module rather than restating it.

    An enumeration kept by hand is the thing that silently stops matching the code. Reading the
    settle() calls out of the syntax tree makes the fixture denominator the module itself.
    """
    source = Path(path or __file__).read_text(encoding="utf-8")
    found = {}
    for node in ast.walk(ast.parse(source)):
        if not isinstance(node, ast.Call):
            continue
        name = getattr(node.func, "id", None) or getattr(node.func, "attr", None)
        if name != "settle" or len(node.args) < 2:
            continue
        decision, reason = node.args[0], node.args[1]
        if isinstance(decision, ast.Constant) and isinstance(reason, ast.Constant):
            found[reason.value] = decision.value
    return found


def load_fixtures(directory):
    fixtures = []
    for path in sorted(Path(directory).glob("*.json")):
        fixtures.append((path, json.loads(path.read_text(encoding="utf-8"))))
    return fixtures


def command_decide(args):
    try:
        request = json.loads(sys.stdin.read())
    except json.JSONDecodeError as error:
        print(f"Unreadable request: {error}", file=sys.stderr)
        return 2
    try:
        result = decide(request)
    except RequestError as error:
        print(f"Unreadable request: {error}", file=sys.stderr)
        return 2
    print(json.dumps(result, ensure_ascii=False, indent=2, sort_keys=True))
    return 2 if result["decision"] == "invalid" else 0


def command_readback(args):
    try:
        request = json.loads(sys.stdin.read())
    except json.JSONDecodeError as error:
        print(f"Unreadable request: {error}", file=sys.stderr)
        return 2
    if not isinstance(request, dict) or not isinstance(request.get("requested_title"), str):
        print("Unreadable request: requested_title must be a string", file=sys.stderr)
        return 2
    observed = request.get("observed_title")
    if observed is not None and not isinstance(observed, str):
        print("Unreadable request: observed_title must be a string or null", file=sys.stderr)
        return 2
    result = classify_readback(request["requested_title"], observed)
    print(json.dumps({"readback": result}, ensure_ascii=False, indent=2, sort_keys=True))
    return 0 if result == "verified" else 1


def command_replay(args):
    fixtures = load_fixtures(args.fixtures)
    if not fixtures:
        print(f"No fixtures under {args.fixtures}; nothing was checked", file=sys.stderr)
        return 1

    failures = []
    reached_reasons, reached_readback, reached_matches = set(), set(), set()
    for path, fixture in fixtures:
        name = path.name
        subcommand = fixture.get("subcommand", "decide")
        expected = fixture.get("expected")
        if not isinstance(expected, dict):
            failures.append(f"{name}: no recorded expectation")
            continue
        if subcommand == "decide":
            try:
                got = decide(fixture.get("input"))
            except RequestError as error:
                failures.append(f"{name}: request rejected: {error}")
                continue
            reached_reasons.add(got["reason"])
            if got["matched"] is not None:
                reached_matches.add(got["matched"])
        elif subcommand == "readback":
            payload = fixture.get("input") or {}
            got = {"readback": classify_readback(payload.get("requested_title"),
                                                 payload.get("observed_title"))}
            reached_readback.add(got["readback"])
        else:
            failures.append(f"{name}: unknown subcommand {subcommand}")
            continue
        for key, want in expected.items():
            if got.get(key) != want:
                failures.append(f"{name}: {key} expected {want!r}, got {got.get(key)!r}")

    missing = sorted(set(reachable_reasons()) - reached_reasons)
    missing_readback = sorted(set(READBACK) - reached_readback)
    missing_matches = sorted(set(MATCHES) - reached_matches)

    print(f"Replayed {len(fixtures)} title fixtures against their recorded expectations.")
    for failure in failures:
        print(failure, file=sys.stderr)
    if missing:
        print("No fixture reaches: " + ", ".join(missing), file=sys.stderr)
    if missing_readback:
        print("No fixture reaches readback: " + ", ".join(missing_readback), file=sys.stderr)
    if missing_matches:
        print("No fixture reaches the branch: " + ", ".join(missing_matches), file=sys.stderr)
    print("Replay compares this module with its fixtures. It is not evidence that any title "
          "was written, displayed or read back on a host.")

    if failures:
        return 1
    if (missing or missing_readback or missing_matches) and not args.allow_unreached:
        return 1
    return 0


def main():
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)

    decide_parser = sub.add_parser("decide", help="Decide one parent title from stdin JSON")
    decide_parser.set_defaults(func=command_decide)

    readback_parser = sub.add_parser("readback", help="Classify a rename readback")
    readback_parser.set_defaults(func=command_readback)

    replay = sub.add_parser("replay", help="Check every fixture against its recorded expectation")
    replay.add_argument("--fixtures", default=str(FIXTURES))
    replay.add_argument("--allow-unreached", action="store_true",
                        help="Report unreached decisions without failing; for deliberate subset "
                             "runs only. Fixture mismatches are never waived.")
    replay.set_defaults(func=command_replay)

    args = parser.parse_args()
    return args.func(args)


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except OSError as exc:
        print(f"Title check failed: {exc}. Nothing was written.", file=sys.stderr)
        raise SystemExit(3)
