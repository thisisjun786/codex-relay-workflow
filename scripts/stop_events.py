#!/usr/bin/env python3
"""Whether every Stop event recorded under the given journal roots was accepted exactly once.

Read-only. This is the per-event reading CRW-212 put in place of the per-(session, turn) count: a
turn can end several times, and each of those Stops is its own event, while two registrations
answering one Stop are one event handled twice. The reading lives in crw_runtime.completion, beside
the names of the files it reads, so the adapter and this command cannot disagree about them.

    python3 scripts/stop_events.py --journal-root <root> [--journal-root <root>]...
        [--codex-home <home>]... [--since <YYYY-MM-DDTHH:MM:SSZ>] [--until <YYYY-MM-DDTHH:MM:SSZ>]
        [--session <id>] [--turn <id>]

Give every journal root the host's registrations write to, and the host's Codex home. The
registrations of one host meet in one file under its Codex home before they claim in their own
roots, so an event is accepted in one root. The host ledgers the claims name are read without being
asked for; --codex-home adds a host whose ledger no claim names yet. Every record of an event the
window reaches is checked whatever its own time. A duplicate or host file whose accepted record is
in a root not given reads UNREADABLE, and an event accepted in two roots (registrations of different
Codex homes) reads FALSE when both are given. TRUE means every invocation in the window was judged:
one answered without an event identity or an owner, or a row from before event identity, keeps the
window from TRUE.

Prints the reading as JSON. Exit 0 TRUE, 1 FALSE, 3 UNREADABLE; 2 is a usage error.
"""

import argparse
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from crw_runtime import completion  # noqa: E402

EXIT = {completion.TRUE: 0, completion.FALSE: 1, completion.UNREADABLE_VERDICT: 3}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--journal-root", action="append", required=True,
                        help="a journal root a registration writes to; repeat for each")
    parser.add_argument("--since", type=completion.window_bound,
                        help="records at or after this UTC time, YYYY-MM-DDTHH:MM:SSZ")
    parser.add_argument("--until", type=completion.window_bound,
                        help="records before this UTC time, YYYY-MM-DDTHH:MM:SSZ")
    parser.add_argument("--session", help="only this session")
    parser.add_argument("--turn", help="only this turn")
    parser.add_argument("--codex-home", action="append", default=[],
                        help="a Codex home whose host ledger is read as well; repeat for each")
    arguments = parser.parse_args(argv)
    answer = completion.stop_events(arguments.journal_root, since=arguments.since,
                                    until=arguments.until, session=arguments.session,
                                    turn=arguments.turn, hosts=arguments.codex_home)
    print(json.dumps(answer, indent=2, sort_keys=True))
    return EXIT[answer["verdict"]]


if __name__ == "__main__":
    raise SystemExit(main())
