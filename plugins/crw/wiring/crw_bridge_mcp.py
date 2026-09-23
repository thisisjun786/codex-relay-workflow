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

The same bare environment is why the record can name the host's execution policy. Codex hands
this process no CODEX_THREAD_BRIDGE_EXECUTION_POLICY, so a bridge started from it would read no
policy and check no role. A version-2 record names the policy file and the digest register-mcp
read it under; this launcher refuses a file it cannot read or whose bytes no longer hash to that
digest, and otherwise hands both to the bridge, which checks the digest again against the bytes
it actually parses. A version-1 record names no policy and starts exactly as it always did.
"""

import hashlib
import json
import os
import re
import stat
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
POLICY_RECORD_VERSION = 2
POLICY_FIELD = "executionPolicy"
POLICY_KEYS = ("digest", "path")
# The two variables the bridge reads, mirrored from codex_thread_bridge.execution for the same
# reason, and asserted equal by the same tests.
POLICY_VARIABLE = "CODEX_THREAD_BRIDGE_EXECUTION_POLICY"
DIGEST_VARIABLE = "CODEX_THREAD_BRIDGE_EXECUTION_POLICY_DIGEST"
DIGEST = re.compile(r"[0-9a-f]{64}")
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


def canonical(path):
    """One spelling of a file, for comparing two of them. The relay compares the same way."""
    try:
        return str(Path(path).expanduser().absolute().resolve())
    except (OSError, RuntimeError, ValueError):
        return str(path)


def inherited(name):
    """A variable this process was given, stripped the way the bridge strips it; empty is unset."""
    return (os.environ.get(name) or "").strip() or None


def policy_environment(record, reference):
    """The environment the bridge starts under, or a refusal. Never a start without the policy.

    Everything that can be checked here is checked before exec, so a refusal names the record and
    the command that repairs it, which the bridge cannot do. The bridge remains the authority on
    what the file says: it parses it, refuses one it cannot use, and checks the digest again
    against the bytes it actually read.
    """
    repair = (" Run runtime_install.py register-mcp --owner plugin --execution-policy <file>"
              " again after moving " + str(record) + " aside, so the record names the policy as"
              " it now stands.")
    if not isinstance(reference, dict) or sorted(reference) != sorted(POLICY_KEYS):
        fail("the record at " + str(record) + " is version " + str(POLICY_RECORD_VERSION)
             + " and must name " + POLICY_FIELD + " as an object with exactly "
             + " and ".join(POLICY_KEYS))
    path, digest = reference["path"], reference["digest"]
    # Checked on the value itself rather than on a normalised one. The bridge strips the variable,
    # so a padded path would be verified here as one file and opened there as another.
    if (not isinstance(path, str) or not path or path != path.strip()
            or any(ord(character) < 32 or ord(character) == 127 for character in path)
            or not os.path.isabs(path)):
        fail("the record at " + str(record) + " must name the execution policy as an absolute"
             " path with no surrounding whitespace or control characters, found " + repr(path))
    if not isinstance(digest, str) or not DIGEST.fullmatch(digest):
        fail("the record at " + str(record) + " must name the execution policy digest as 64"
             " lowercase hexadecimal characters")
    named = inherited(POLICY_VARIABLE)
    if named is not None and canonical(named) != canonical(path):
        # Two files are not a preference. Choosing one by precedence is how the policy a process
        # runs on became a property of whoever started it; the relay refuses the same disagreement.
        fail("the record at " + str(record) + " names the execution policy " + repr(path)
             + " and this process was started with " + POLICY_VARIABLE + "=" + repr(named)
             + ". Unset the variable, or register the other file")
    expected = inherited(DIGEST_VARIABLE)
    if expected is not None and expected != digest:
        fail("the record at " + str(record) + " names the policy digest " + digest
             + " and this process was started with " + DIGEST_VARIABLE + "=" + expected
             + ". Unset the variable, or register the policy it names")
    try:
        found = os.stat(path)
        if not stat.S_ISREG(found.st_mode):
            fail("the execution policy the record at " + str(record) + " names, " + path
                 + ", is not a regular file, so the bridge is not started without it." + repair)
        with open(path, "rb") as handle:
            actual = hashlib.sha256(handle.read()).hexdigest()
    except OSError as error:
        fail("the execution policy the record at " + str(record) + " names could not be read ("
             + path + ": " + str(error) + "). The bridge is not started without it, because it"
             " would then check no role." + repair)
    if actual != digest:
        fail("the execution policy at " + path + " now hashes to " + actual + ", and the record"
             " at " + str(record) + " was written when it hashed to " + digest + ". It changed"
             " after it was registered, so the bridge is not started under a policy nobody"
             " registered." + repair)
    environment = dict(os.environ)
    environment[POLICY_VARIABLE] = path
    environment[DIGEST_VARIABLE] = digest
    return environment


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
    version = document.get("recordVersion")
    if version not in (RECORD_VERSION, POLICY_RECORD_VERSION):
        fail("the record at " + str(record) + " is version "
             + repr(version) + ", and this package reads versions " + str(RECORD_VERSION)
             + " and " + str(POLICY_RECORD_VERSION) + ". Rewrite it with the runtime_install.py"
             " that ships with this package rather than starting a runtime under a contract this"
             " launcher does not implement.")
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
    environment = None
    if version == RECORD_VERSION:
        if POLICY_FIELD in document:
            fail("the record at " + str(record) + " is version " + str(RECORD_VERSION)
                 + " and names an execution policy, which only a version "
                 + str(POLICY_RECORD_VERSION) + " record carries. Starting it as version "
                 + str(RECORD_VERSION) + " would start the bridge without that policy.")
    else:
        environment = policy_environment(record, document.get(POLICY_FIELD))
    try:
        # exec rather than spawn: the host speaks MCP over this process's stdio, and a relay in
        # the middle would have to copy every byte in both directions and get the shutdown right.
        # A version-1 record starts with the environment this process was given, exactly as it
        # always has; only a record naming a policy adds to it.
        if environment is None:
            os.execv(executable, [executable, *arguments])
        else:
            os.execve(executable, [executable, *arguments], environment)
    except OSError as error:
        fail("could not start " + executable + ": " + str(error)
             + ". The installer's pointer names the runtime; check that it is installed.")


if __name__ == "__main__":
    main()
