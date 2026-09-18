#!/usr/bin/env python3
"""The Stop hook the CRW plugin package declares, and the least it can possibly do.

The package cannot carry a Python runtime: the version cache is replaced wholesale on every
install, so an executable a running process depends on must live outside it. What ships here
instead is the indirection. This file finds the settings the runtime installer wrote, checks
that the plugin is the owner of this registration, and hands the host's payload to the adapter
those settings name.

It inherits the adapter's three refusals, because a hook that breaks them costs turns rather
than reporting a fault.

It parses no arguments. The host reads exit 2 as the blocking code and argparse exits 2 on any
usage error, so there is no argument parser here and nothing imported that has one.

It writes nothing to stderr and exits 0 on every path, including the paths where it does
nothing at all. An unreliable detector must degrade into no detector, never into a stuck
session.

It decides nothing. The only bytes that can reach stdout are the ones the adapter produced.

And one rule of its own. A host can hold both registrations at once -- an installed plugin
declaring this hook and a hook file entry the runtime installer appended -- because installing
the plugin is not a command this repository runs and cannot be refused from here. Both would
fire on the same Stop. So this launcher stands down unless the settings name the plugin as the
owner, which makes the guarantee hold at run time as well as at install time.
"""

import json
import os
import subprocess
import sys
from pathlib import Path

SETTINGS_NAME = "crw-completion-hook.json"
SETTINGS_ENV = "CRW_COMPLETION_HOOK_CONFIG"
PLUGIN_OWNER = "plugin"
# Kept under the timeout this hook is registered with, so the host does not kill the adapter
# in the middle of recording why it could not answer.
MARGIN_SECONDS = 2
MAX_SECONDS = 9
# MAX_SECONDS is the ceiling this launcher puts on its own deadline, and it is the same number
# scripts/crw_runtime/completion.py calls LAUNCHER_CEILING_SECONDS. That module refuses to
# install plugin-owned settings whose guard budget reaches it, so this deadline always outlasts
# the adapter it runs. If one of the two moves, the other has to move with it; a test asserts
# they still agree, because this file cannot import that module.


def settings_path():
    """Where this hook's settings live, resolved the way the measured environment allows.

    A plugin-declared hook command runs through a shell and its process carries CODEX_HOME, so
    the home is read rather than guessed. The explicit override comes first because a caller
    that set it has already decided which file this hook reads.

    The override is settled to an absolute path before it is used. This hook runs with the
    session's own workspace as its directory, so a relative value would name one file where the
    install ran and a different one, or none, at every Stop -- and a Stop that cannot find its
    settings releases in silence, which is the failure that looks like nothing happening.
    """
    override = os.environ.get(SETTINGS_ENV)
    if override:
        return Path(os.path.abspath(str(Path(override).expanduser())))
    home = os.environ.get("CODEX_HOME")
    return Path(home if home else Path.home() / ".codex") / SETTINGS_NAME


def adapter_call(document):
    """The command to run, or None when these settings are not this launcher's to act on.

    Every reason to decline is a reason to decline silently. A launcher that reported them
    would be writing to a stream the host reads as a continuation prompt.
    """
    if not isinstance(document, dict) or document.get("owner") != PLUGIN_OWNER:
        return None
    interpreter = document.get("adapterInterpreter")
    entry_point = document.get("adapterEntryPoint")
    for value in (interpreter, entry_point):
        if not isinstance(value, str) or not value.strip() or not os.path.isabs(value):
            return None
    if not Path(entry_point).is_file():
        return None
    return [interpreter, entry_point]


def budget(document):
    value = document.get("timeoutSeconds")
    if not isinstance(value, (int, float)) or isinstance(value, bool) or value <= 0:
        return MAX_SECONDS
    return min(float(value) + MARGIN_SECONDS, MAX_SECONDS)


def main():
    try:
        payload = sys.stdin.buffer.read()
    except BaseException:
        payload = b""
    path = settings_path()
    try:
        document = json.loads(path.read_bytes().decode("utf-8"))
    except BaseException:
        # Absent, unreadable or not JSON. All three mean the same thing here: there is nothing
        # this launcher can act on, and a Stop it cannot judge is a Stop it must release.
        return
    call = adapter_call(document)
    if call is None:
        return
    try:
        finished = subprocess.run(call + [str(path)], input=payload,
                                  stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                  timeout=budget(document))
    except BaseException:
        return
    if finished.stdout:
        sys.stdout.buffer.write(finished.stdout)


if __name__ == "__main__":
    try:
        main()
    except BaseException:
        # Deliberately bare and deliberately silent, for the same reason the adapter's is.
        pass
    raise SystemExit(0)
