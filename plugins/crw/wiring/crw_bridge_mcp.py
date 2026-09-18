#!/usr/bin/env python3
"""The task bridge, started the way a plugin-declared MCP server has to start it.

A plugin MCP server is spawned with none of a hook's conveniences. It gets no shell, so a
variable in its command line arrives as literal text; it inherits neither CODEX_HOME nor the
plugin root. What it does get is a working directory, and the declaration points that at the
installed package, which is how this file is reached by a ./ relative argument.

So the Codex home is derived rather than received. The cache layout an installation produces is
<CODEX_HOME>/plugins/cache/<marketplace>/<plugin>/<version>, and this file sits under that
version directory, which makes its own location the one fact it can rely on. An explicit
CODEX_HOME still wins when a caller set one.

The bridge itself is never bundled. The version cache is replaced wholesale on every install,
so an executable a running session depends on cannot live inside it. The record this reads
names the installed runtime through the installer's pointer, and replacing the package changes
this launcher without touching the runtime underneath it.

Unlike the Stop hook, this fails loudly. A hook that cannot answer must release the turn in
silence; a server that cannot start should say why, and the declaration marks it not required
so the session continues regardless.
"""

import json
import os
import sys
from pathlib import Path

RECORD_NAME = "crw-bridge-mcp.json"
PLUGIN_OWNER = "plugin"
# The record contract, mirrored from scripts/crw_runtime/bridgerecord.py, which this file
# cannot import. Reading a weaker shape than the writer guarantees is how a record written for
# a later contract gets started under this one: a changed version would be ignored, and a
# falsy non-list args would quietly become no arguments at all. Tests assert both halves still
# agree, here and with the server name wiring/mcp.json declares.
RECORD_VERSION = 1
DECLARED_SERVER = "codex-thread-bridge"
# version/wiring/this-file -> version -> plugin -> marketplace -> cache -> plugins -> home
CACHE_DEPTH = 6


def codex_home():
    """The Codex home and how it was decided, so a wrong answer names its own origin."""
    named = os.environ.get("CODEX_HOME")
    if named:
        return Path(named), "the CODEX_HOME environment variable"
    here = Path(__file__).resolve()
    if len(here.parents) > CACHE_DEPTH:
        candidate = here.parents[CACHE_DEPTH]
        if candidate.name == ".codex" or (candidate / "config.toml").exists():
            return candidate, "the installed package location " + str(here)
        # Reported rather than silently accepted: a package somewhere else entirely would
        # otherwise resolve to a directory that merely happens to be six levels up.
        return candidate, ("the installed package location " + str(here)
                           + ", which does not look like a Codex home")
    return Path.home() / ".codex", "the default home, because nothing else named one"


def fail(message):
    sys.stderr.write("crw bridge launcher: " + message + "\n")
    raise SystemExit(2)


def main():
    home, how = codex_home()
    record = home / RECORD_NAME
    try:
        document = json.loads(record.read_bytes().decode("utf-8"))
    except FileNotFoundError:
        fail("no record at " + str(record) + " (resolved from " + how + "). Which command"
             " writes it depends on who owns this server. If this host registers the bridge"
             " in its Codex configuration, run runtime_install.py register-mcp --apply and"
             " this launcher will stand down for that registration. If the package is to own"
             " it, add --owner plugin. Either way this package never installs a runtime.")
    except (OSError, UnicodeDecodeError, ValueError) as error:
        fail("the record at " + str(record) + " could not be read: " + str(error))
    if not isinstance(document, dict):
        fail("the record at " + str(record) + " is not an object")
    if document.get("recordVersion") != RECORD_VERSION:
        fail("the record at " + str(record) + " is version "
             + repr(document.get("recordVersion")) + ", and this package reads version "
             + str(RECORD_VERSION) + ". Rewrite it with the runtime_install.py that ships with"
             " this package rather than starting a runtime under a contract this launcher does"
             " not implement.")
    if document.get("owner") != PLUGIN_OWNER:
        fail("the record at " + str(record) + " names " + repr(document.get("owner"))
             + " as the owner of this server, so the Codex configuration registers it and this"
               " package must not start a second one")
    name = document.get("serverName")
    if name is not None and name != DECLARED_SERVER:
        fail("the record at " + str(record) + " names the server " + repr(name)
             + ", and this package declares " + repr(DECLARED_SERVER)
             + "; the record belongs to a registration this launcher does not start")
    executable = document.get("bridgeExecutable")
    if not isinstance(executable, str) or not os.path.isabs(executable):
        fail("the record at " + str(record) + " must name bridgeExecutable as an absolute path")
    arguments = document.get("args")
    if arguments is None:
        arguments = []
    # Checked before any defaulting. "args": false, 0, "" or {} would otherwise become an empty
    # list and start the runtime without the arguments the record was written to carry.
    if not isinstance(arguments, list) or not all(isinstance(word, str) for word in arguments):
        fail("the record at " + str(record) + " must list args as strings")
    try:
        # exec rather than spawn: the host speaks MCP over this process's stdio, and a relay in
        # the middle would have to copy every byte in both directions and get the shutdown right.
        os.execv(executable, [executable, *arguments])
    except OSError as error:
        fail("could not start " + executable + ": " + str(error)
             + ". The installer's pointer names the runtime; check that it is installed.")


if __name__ == "__main__":
    main()
