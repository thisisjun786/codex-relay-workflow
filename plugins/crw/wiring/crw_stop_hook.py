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

It is not reached directly. The declaration runs a fixed bootstrap that opens the first of two
candidates it can read: this file under the version cache, and a copy the runtime installer
places at <CODEX_HOME>/crw-stop-hook.py. The cache copy comes first so the current version
always wins and a stale copy can never outrank it; the installed copy exists for the one case
the cache cannot answer, which is a plugin update landing in the middle of a turn. The turn's
hook command is fixed when the turn starts, so a cache directory removed underneath it leaves
an absolute path to a file that is gone, and python3 exits 2 for a missing script -- the same
number the hook protocol reads as "block this turn". That collision is what turned one missing
file into a termination loop, and it is why the declaration no longer names only a cache path.

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
PLUGIN_OWNER = "plugin"
# What marks a copy of this launcher as CRW's to replace or remove. It says whose file this is
# and nothing more: it does not establish who wrote it, and it does not establish that the bytes
# around it are intact. Those are separate questions, and the installer answers the second one
# by reporting this file's digest beside the checkout's rather than by trusting the marker.
LAUNCHER_MARKER = "crw-stop-hook/1"
# The settings contract this launcher implements, mirrored from scripts/crw_runtime/completion.py
# CONFIG_VERSION, which this file cannot import. An installed copy outlives the package that
# wrote it, so it can meet a document written for a later contract. Absent reads as the first
# contract, because a host that installed before the key existed holds a document without it.
# Anything else is a contract this copy does not implement, and the answer to that is to stand
# down rather than to act on a document it would be guessing about.
CONFIG_VERSION = 1
# Kept under the timeout this hook is registered with, so the host does not kill the adapter
# in the middle of recording why it could not answer.
MARGIN_SECONDS = 2
MAX_SECONDS = 9
# MAX_SECONDS is the ceiling this launcher puts on its own deadline, and it is the same number
# scripts/crw_runtime/completion.py calls LAUNCHER_CEILING_SECONDS. The deadline outlasts the
# adapter only while the recorded budget stays at or under MAX_SECONDS - MARGIN_SECONDS: above
# that the cap eats the margin, and at a budget just under MAX_SECONDS this deadline arrives
# while the adapter is still writing the record of its own timeout. Settings that record such a
# budget are refused where they are written -- scripts/crw_transition/steps.py, which derives its
# limit from these two numbers -- rather than here, because this launcher cannot wait longer than
# the hook it is registered under. If one of these numbers moves, that limit has to move with it;
# a test asserts they still agree, because this file cannot import that module.


def settings_path():
    """Where this hook's settings live: the one place the installer is made to write them.

    A plugin-declared hook command runs through a shell and its process carries CODEX_HOME, so
    the home is read rather than guessed.

    What this deliberately does NOT read is the settings override the rest of this repository
    honours. A plugin declaration carries no settings argument, so nothing ties the environment
    an install ran under to the environment a session starts under, and those are two different
    environments: refusing the override at installation only settles the first one. A session
    that inherits the variable from a shell profile or a legacy user-owned setup would be sent
    somewhere the installer never wrote, and a Stop that cannot find its settings releases in
    silence -- the failure that looks exactly like nothing happening.

    So the install refuses an override and this reads one path. Together those make the
    location an invariant instead of a value two sides have to agree about.
    """
    home = os.environ.get("CODEX_HOME")
    return Path(home if home else Path.home() / ".codex") / SETTINGS_NAME


def adapter_call(document):
    """The command to run, or None when these settings are not this launcher's to act on.

    Every reason to decline is a reason to decline silently. A launcher that reported them
    would be writing to a stream the host reads as a continuation prompt.
    """
    if not isinstance(document, dict) or document.get("owner") != PLUGIN_OWNER:
        return None
    version = document.get("configVersion")
    if version is not None and version != CONFIG_VERSION:
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
