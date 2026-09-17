#!/usr/bin/env python3
"""The registered Stop hook command. Reads the host's payload, prints the guard's answer.

Everything this file does not do is the point of it.

It parses no arguments. The host reads exit 2 as the blocking code and takes stderr as the
continuation prompt, and argparse exits 2 on any usage error, so a stale flag in somebody's hook
file would otherwise become a hold on every ordinary turn with a usage message as the reason.
There is no argument parser here and nothing imported that has one.

It writes nothing to stderr and it always exits 0. A hook that times out, crashes or writes
invalid JSON was observed to fail open on this host, and that is the direction to fail in: an
unreliable detector should degrade into no detector, never into a stuck session.

It decides nothing. The only text that can reach stdout is a block the relay guard itself
produced and this adapter re-validated.

Rules: skills/crw-run/references/hook-contract.md. Logic: crw_runtime/completion.py.
"""

import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))


def main():
    try:
        payload = sys.stdin.buffer.read()
    except BaseException:
        # The payload could not be read at all. run() is still called, with nothing, so the
        # invocation is classified and recorded rather than vanishing.
        payload = None
    from crw_runtime import completion

    answer = completion.run(payload)
    if answer:
        sys.stdout.write(answer)


if __name__ == "__main__":
    try:
        main()
    except BaseException:
        # Deliberately bare and deliberately silent. Any escape here would be reported to the
        # host as a failed hook run at best, and as a blocking exit code at worst.
        pass
    raise SystemExit(0)
