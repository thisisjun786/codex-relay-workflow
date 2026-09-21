#!/usr/bin/env python3
"""Move a manual CRW installation to the plugin installation, and own what follows it.

Six commands, one JSON document each, so a run leaves a receipt that can be diffed:

    inspect     read-only: both installs, the owner of each surface, in-flight work
    check-declaration
                read-only: whether adding or updating a package would keep the per-tool approval
                policy this host grants. Judges the package named by --package, which is the one
                about to be installed rather than the one already there, and carries its payload
                digest so the same check run afterwards can be tied to the bytes that landed
    transition  manual -> plugin, in order, --apply to act and a dry run otherwise
    disable     stop new calls without deleting anything
    remove      delete the records this repository wrote, and only those
    swap-state  what a version replacement left, actual state reported apart from recorded

What none of them do: install a runtime, register a plugin, grant hook trust, stop a service, or
delete an operational database, journal, receipt or assignment. Written, registered, trusted and
fired are four claims, and this tool can establish the first two at most.

check-declaration is a gate an operator runs, not one that intercepts anything: nothing here
invokes codex plugin add, update or remove, and a host serves whatever declaration is installed
whether or not this was asked first.

Standard library only, and it never calls the runtime installer's install, hook or register-mcp
commands: it reads that package's readers and writers directly, so the diagnosis those commands
own keeps answering about a host this tool did not reshape underneath it.
"""

import argparse
import json
import os
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / "scripts"))

from crw_runtime import completion, reading  # noqa: E402
from crw_transition import inventory, steps  # noqa: E402


def completion_event():
    return completion.EVENT

# The same three the runtime installer uses, so a caller reading both does not have to learn two
# meanings for one number.
EXIT_OK, EXIT_REFUSED, EXIT_USAGE = 0, 1, 2
# A fourth answer, for the same reason runtime_install.py declares one: "nothing happened" and
# "it happened and did not stay in effect" are two results and one status cannot carry both.
# A step that removed its file and then found the surface live again is not a refusal -- bytes
# were removed -- and it is not success either, because the operation did not remain done.
EXIT_INCOMPLETE = 3


def emit(document):
    json.dump(document, sys.stdout, indent=2, sort_keys=True, default=str)
    sys.stdout.write("\n")


def policy_in_effect(host):
    """Which per-tool approval policy this host is under right now, and where it comes from.

    Measured: while the user table exists it wins, so that is the answer whenever the table is
    there, and the installed declaration only answers once the table is gone. Reported on every
    read-only command because an operator deciding whether to run codex plugin add, or codex plugin
    remove, has no other way to see which of the two is actually gating the bridge.
    """
    mcp = host["mcp"]
    if mcp.get("table") == reading.PRESENT:
        policy = mcp.get("policy") or {}
        return {"from": "the " + inventory.SERVER_NAME + " table in " + str(mcp.get("configPath")),
                "state": policy.get("state"), "tools": policy.get("tools") or {},
                "detail": policy.get("detail")}
    # A cached directory is not a loaded plugin. Reading the declaration on the strength of the
    # cache alone reported both tools as gated on a host that had kept the cache, lost its
    # [plugins."crw@crw"] entry and had no table either -- a policy labelled in effect while
    # nothing served the bridge at all.
    plugin = host.get("plugin") or {}
    version = plugin.get("cacheVersion")
    if plugin.get("configEntry") != reading.PRESENT:
        why = "no plugin entry in " + str(inventory.config_path(host["codexHome"])) + " (" \
              + str(plugin.get("configEntry")) + "), so Codex loads no declaration here"
    elif plugin.get("enabled") is not True:
        why = "the plugin entry " + str(plugin.get("entryKey")) + " does not record enabled =" \
              " true (" + repr(plugin.get("enabled")) + "), so whether Codex loads its" \
              " declaration was not established"
    elif not version:
        why = "no single installed plugin version could be named" \
              + ((": " + str(plugin["detail"])) if plugin.get("detail") else "")
    else:
        declared, unread = steps.declared_policy(version, inventory.SERVER_NAME)
        if declared is None:
            # Unreadable is not absent. Reporting an empty tool set for a declaration nobody
            # could read states as fact the very thing that was not established.
            return {"from": "the installed plugin declaration under " + str(version),
                    "state": reading.UNREADABLE, "tools": {}, "detail": unread}
        return {"from": "the installed plugin declaration under " + str(version),
                "state": reading.PRESENT if declared else reading.ABSENT,
                "tools": declared or {}, "detail": unread}
    return {"from": None, "state": reading.ABSENT, "tools": {},
            "detail": "this host has no bridge table, and " + why + ", so nothing established"
                      " here gates the bridge's tools"}


def host_of(args):
    exported = os.environ.get("CODEX_HOME")
    if exported and not Path(exported).is_absolute():
        # Judged on its own, whether or not --codex-home was passed. A relative CODEX_HOME means
        # something different to every process that reads it, and the launchers this transition
        # hands the surfaces to read that same variable from the installed package's working
        # directory rather than this one. The flag moves where THIS run writes; it does not reach
        # a launcher, so the flag cannot settle the variable. Letting it excuse the value wrote
        # the records under one home and left a later session's launchers looking under another,
        # with the manual hook and the bridge table already removed and the run reporting success.
        #
        # As written, without expanding ~: the packaged launchers build Path(CODEX_HOME) directly
        # and never expand it, so a tilde-spelled value sends them looking under a directory
        # literally named "~" while this command would have expanded it to somewhere real.
        raise ValueError("CODEX_HOME is exported as " + repr(exported) + ", which is relative,"
                         " and a relative home resolves against each reader's own working"
                         " directory. The packaged launchers read that same variable from the"
                         " installed package, never this command's --codex-home, so this run"
                         " will not decide what it means. Unset it or make it absolute; passing"
                         " --codex-home does not settle it. A ~ spelling counts as relative here,"
                         " because the launchers do not expand one")
    # Settled for the same reason --dest is, and for one more. Every path in the snapshot is
    # derived from this one, and the settings candidate is ALSO spelled by completion, which
    # resolves what it is given: a relative home left as it was produced two spellings of one
    # file, the de-duplication is lexical, and an applied run then took the same lock twice and
    # waited itself out -- every transition on a relative home answering busy at the first step.
    home = Path(args.codex_home or exported or Path.home() / ".codex").expanduser().resolve()
    named = getattr(args, "event", None)
    if named and named != completion_event():
        # The package declares one event. Transitioning another would remove a registration on it
        # and hand ownership to a declaration that does not cover it, leaving nothing registered.
        raise ValueError("this package declares only the " + completion_event() + " hook, so "
                         + repr(named) + " is not an event this transition can move")
    # Settled here, once. Everything downstream is derived from it -- the adapter, its interpreter
    # and the fallback bridge path -- and a plugin-owned record has to name an absolute path, so a
    # relative --dest would pass preflight resolving from this command's working directory and be
    # refused at the record write with the settings retired and the registration already gone.
    destination = Path(args.dest).expanduser().resolve() if args.dest else None
    found = inventory.snapshot(home, repo_root=ROOT, destination=destination,
                               event=getattr(args, "event", None))
    # Named rather than chosen between. The flag decides where THIS run reads and writes; the
    # packaged launchers read the exported variable and never see the flag, so when the two name
    # different homes a session started in this environment will not find what this run wrote.
    # That is a mistake worth seeing and a supported way to work on another home, which is why it
    # is reported on every command rather than refused.
    found["exportedHome"] = {
        "exported": exported,
        "using": str(home),
        "agrees": (not exported) or Path(exported).expanduser().resolve() == home,
    }
    return found


def options_of(args):
    return {
        "apply": bool(args.apply),
        "accept_hook_renumbering": bool(getattr(args, "accept_hook_renumbering", False)),
        "accept_hook_trust_gap": bool(getattr(args, "accept_hook_trust_gap", False)),
    }


def verdict(results):
    """Zero when every step is settled or already done, nonzero the moment one refused.

    A cleanup that failed after a surface really stopped is nonzero too, without moving the stop
    itself into the refusals: the surface IS stopped and something was left behind that another
    cooperating writer will trip over, and an operator who reads only the exit code has to learn
    that from it.

    And live_again is nonzero on its own status. It means this run did what it was asked and
    something put the surface back around it, so an operator reading only the exit code would
    otherwise run the next cleanup step against a host where the adapter is callable again.
    Reported apart from a refusal because bytes really were removed.
    """
    if any(item["outcome"] in (steps.REFUSED, steps.BUSY) for item in results) \
            or any(item.get("lockCleanupFailed") for item in results):
        return EXIT_REFUSED
    if any(item["outcome"] == steps.LIVE_AGAIN for item in results):
        return EXIT_INCOMPLETE
    return EXIT_OK


def cmd_inspect(args):
    host = host_of(args)
    emit({"command": "inspect", "host": host,
          "policyInEffect": policy_in_effect(host),
          "note": "read-only. No record was written and nothing was removed."})
    return EXIT_OK


def cmd_check_declaration(args):
    """Whether adding or updating a package would keep the approval policy this host grants."""
    host = host_of(args)
    package = Path(args.package).expanduser().resolve() if args.package \
        else ROOT / "plugins" / "crw"
    answer = steps.declaration_check(host, package)
    refused = bool(answer["refusals"] or answer["packageErrors"])
    emit({"command": "check-declaration", "outcome": "refused" if refused else "preserves",
          **answer,
          "policyInEffect": policy_in_effect(host),
          "note": "read-only, and about the package named under package, not about whatever is"
                  " installed now. Run it again against the installed version afterwards and"
                  " compare payloadDigest: equal digests are what tie this verdict to the bytes"
                  " that landed. This does not run codex plugin add or codex plugin update and"
                  " cannot stop them; nothing in this repository invokes either."})
    return EXIT_REFUSED if refused else EXIT_OK


def cmd_transition(args):
    host = host_of(args)
    options = options_of(args)
    results = steps.transition(host, options, apply=bool(args.apply))
    emit({"command": "transition", "applied": bool(args.apply), "results": results,
          "destination": host.get("destination"),
          "destinationFrom": host.get("destinationDerivedFrom"),
          "preserved": steps.preserved_paths(host),
          # Before, not in effect. On an applied run the table is gone by the time this is
          # printed, so a value labelled "in effect" would be wrong on exactly the receipt that
          # matters most. What the run removed, if it removed anything, is on the standdown's own
          # answer as removedPolicy.
          "policyBefore": policy_in_effect(host),
          "windows": [
              "between the settings retire and the hook standdown the old registration still"
              " fires and finds no settings to read, so it releases the turn without recording"
              " anything for that Stop",
              "between the hook standdown and the new settings no completion hook fires, so no"
              " observe-mode record is written for a Stop in that window",
              "between the table removal and the plugin record a session that starts finds no"
              " bridge registered",
          ],
          "note": "installing the records is not trusting the hook and not proof anything fired."})
    return verdict(results)


def cmd_disable(args):
    host = host_of(args)
    results = steps.disable(host, options_of(args), apply=bool(args.apply))
    claims = steps.stop_claims(results)
    emit({"command": "disable", "applied": bool(args.apply), "results": results,
          "stops": claims["stopped"],
          "wouldStop": claims["wouldStop"],
          "stillLive": claims["stillLive"],
          "doesNotStop": ["a bridge already spawned in a running session",
                          "a turn already inside the adapter",
                          "the relay service, if one runs. Excluding a shared service is the"
                          " operator's own action and this tool never performs or claims it"],
          "preserved": steps.preserved_paths(host),
          "policyInEffect": policy_in_effect(host),
          "note": "nothing was deleted. Every path under preserved was left exactly as it is."
                  " stops names only what is in effect on this host now; wouldStop is what an"
                  " --apply would stop and stillLive is every surface a reader must not read as"
                  " stopped, with the reason it is not."})
    return verdict(results)


def cmd_remove(args):
    host = host_of(args)
    results = steps.remove(host, options_of(args), apply=bool(args.apply))
    # remove's own claim set: it is the command that runs the stable-launcher step, and disable
    # is not, so the fallback is a surface only this caller can answer for.
    claims = steps.stop_claims(results, steps.REMOVE_CLAIMS)
    emit({"command": "remove", "applied": bool(args.apply), "results": results,
          "stops": claims["stopped"],
          "wouldStop": claims["wouldStop"],
          "stillLive": claims["stillLive"],
          "outOfScope": ["the plugin cache and its config.toml entry, which codex plugin remove"
                         " owns", "the marketplace registration",
                         "the runtime installation under the destination",
                         "the relay store, the bridge ledger, the hook journal and every receipt"],
          "preserved": steps.preserved_paths(host),
          "policyInEffect": policy_in_effect(host),
          "note": "there is deliberately no purge flag."})
    return verdict(results)


def cmd_swap_state(args):
    host = host_of(args)
    emit({**steps.swap_state(host, options_of(args)),
          "policyInEffect": policy_in_effect(host)})
    return EXIT_OK


class _Parser(argparse.ArgumentParser):
    """An argument parser that keeps this command's promise when the arguments are wrong.

    Every command here answers with one JSON document, and argparse answers a bad invocation with
    usage text on stderr and exit 2. A caller parsing the output then has nothing to parse at the
    one moment it most needs to know what happened, so the usage failure is emitted in the shape
    the caller was promised, with the usage text carried inside it.
    """

    def error(self, message):
        emit({"command": None, "outcome": "refused", "error": str(message),
              "usage": self.format_usage().strip(),
              "note": "the arguments were not usable, so nothing was read and nothing was"
                      " written."})
        raise SystemExit(EXIT_USAGE)


def build():
    parser = _Parser(prog="plugin_transition.py", description=__doc__)
    parser.add_argument("--codex-home")
    parser.add_argument("--dest", help="the install destination whose pointer names the runtime;"
                                       " derived from the recorded relayExecutable when omitted")
    parser.add_argument("--event", default=None)
    sub = parser.add_subparsers(dest="command", required=True)

    inspect = sub.add_parser("inspect")
    inspect.set_defaults(handler=cmd_inspect, apply=False)

    declaration = sub.add_parser("check-declaration")
    declaration.add_argument("--package", help="the package root about to be added or updated;"
                                              " this checkout's plugins/crw when omitted")
    declaration.set_defaults(handler=cmd_check_declaration, apply=False)

    move = sub.add_parser("transition")
    move.add_argument("--apply", action="store_true")
    move.add_argument("--accept-hook-renumbering", action="store_true",
                      help="proceed when removing the registration shifts the index of a later"
                           " hook, which detaches the trust Codex recorded against it")
    move.add_argument("--accept-hook-trust-gap", action="store_true",
                      help="proceed when no [hooks.state] entry records trust for the plugin's"
                           " hook, accepting that nothing fires until it is trusted")
    move.set_defaults(handler=cmd_transition)

    off = sub.add_parser("disable")
    off.add_argument("--apply", action="store_true")
    off.set_defaults(handler=cmd_disable)

    gone = sub.add_parser("remove")
    gone.add_argument("--apply", action="store_true")
    gone.set_defaults(handler=cmd_remove)

    swap = sub.add_parser("swap-state")
    swap.set_defaults(handler=cmd_swap_state, apply=False)
    return parser


def main(argv=None):
    """Run one command, and never let an operational failure leave a traceback.

    A bounded failure contract, like the runtime installer's. Every command here promises a JSON
    receipt, and a receipt is exactly what an operator has left when a step failed: a traceback on
    stdout is unparseable, says nothing about what was written before it, and cannot be diffed
    against the run that came before. A defect still fails the command; it fails it in the shape
    the caller was promised.
    """
    parser = build()
    args = parser.parse_args(argv)
    try:
        return args.handler(args)
    except ValueError as error:
        emit({"command": getattr(args, "command", None), "outcome": "refused",
              "error": str(error), "note": "nothing was read and nothing was written."})
        return EXIT_USAGE
    except KeyboardInterrupt:
        emit({"command": getattr(args, "command", None), "outcome": "interrupted",
              "note": "the run was interrupted. Nothing further was written; rerun to decide"
                      " against the host as it now stands."})
        return EXIT_REFUSED
    except OSError as error:
        emit({"command": getattr(args, "command", None), "outcome": "failed",
              "error": type(error).__name__ + ": " + str(error),
              "note": "an operational failure, reported as a receipt rather than a traceback."
                      " Steps already settled stay settled: rerun to converge from here."})
        return EXIT_REFUSED
    except Exception as error:  # noqa: BLE001 - a defect is reported in the promised shape
        emit({"command": getattr(args, "command", None), "outcome": "internal_error",
              "error": type(error).__name__ + ": " + str(error),
              "note": "a defect in this command. Nothing about the host is claimed by this"
                      " receipt; treat the run as having stopped where it stopped."})
        return EXIT_REFUSED


if __name__ == "__main__":
    raise SystemExit(main())
