#!/usr/bin/env python3
"""Move a manual CRW installation to the plugin installation, and own what follows it.

Five commands, one JSON document each, so a run leaves a receipt that can be diffed:

    inspect     read-only: both installs, the owner of each surface, in-flight work
    transition  manual -> plugin, in order, --apply to act and a dry run otherwise
    disable     stop new calls without deleting anything
    remove      delete the records this repository wrote, and only those
    swap-state  what a version replacement left, actual state reported apart from recorded

What none of them do: install a runtime, register a plugin, grant hook trust, stop a service, or
delete an operational database, journal, receipt or assignment. Written, registered, trusted and
fired are four claims, and this tool can establish the first two at most.

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

from crw_runtime import completion  # noqa: E402
from crw_transition import inventory, steps  # noqa: E402


def completion_event():
    return completion.EVENT

# The same three the runtime installer uses, so a caller reading both does not have to learn two
# meanings for one number.
EXIT_OK, EXIT_REFUSED, EXIT_USAGE = 0, 1, 2


def emit(document):
    json.dump(document, sys.stdout, indent=2, sort_keys=True, default=str)
    sys.stdout.write("\n")


def host_of(args):
    home = Path(args.codex_home or os.environ.get("CODEX_HOME") or Path.home() / ".codex")
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
    return inventory.snapshot(home, repo_root=ROOT, destination=destination,
                              event=getattr(args, "event", None))


def options_of(args):
    return {
        "apply": bool(args.apply),
        "accept_hook_renumbering": bool(getattr(args, "accept_hook_renumbering", False)),
        "accept_hook_trust_gap": bool(getattr(args, "accept_hook_trust_gap", False)),
    }


def verdict(results):
    """Zero when every step is settled or already done, nonzero the moment one refused."""
    if any(item["outcome"] in (steps.REFUSED, steps.BUSY) for item in results):
        return EXIT_REFUSED
    return EXIT_OK


def cmd_inspect(args):
    host = host_of(args)
    emit({"command": "inspect", "host": host,
          "note": "read-only. No record was written and nothing was removed."})
    return EXIT_OK


def cmd_transition(args):
    host = host_of(args)
    options = options_of(args)
    results = steps.transition(host, options, apply=bool(args.apply))
    emit({"command": "transition", "applied": bool(args.apply), "results": results,
          "destination": host.get("destination"),
          "destinationFrom": host.get("destinationDerivedFrom"),
          "preserved": steps.preserved_paths(host),
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
          "note": "nothing was deleted. Every path under preserved was left exactly as it is."
                  " stops names only what is in effect on this host now; wouldStop is what an"
                  " --apply would stop and stillLive is every surface a reader must not read as"
                  " stopped, with the reason it is not."})
    return verdict(results)


def cmd_remove(args):
    host = host_of(args)
    results = steps.remove(host, options_of(args), apply=bool(args.apply))
    claims = steps.stop_claims(results)
    emit({"command": "remove", "applied": bool(args.apply), "results": results,
          "stops": claims["stopped"],
          "wouldStop": claims["wouldStop"],
          "stillLive": claims["stillLive"],
          "outOfScope": ["the plugin cache and its config.toml entry, which codex plugin remove"
                         " owns", "the marketplace registration",
                         "the runtime installation under the destination",
                         "the relay store, the bridge ledger, the hook journal and every receipt"],
          "preserved": steps.preserved_paths(host),
          "note": "there is deliberately no purge flag."})
    return verdict(results)


def cmd_swap_state(args):
    emit(steps.swap_state(host_of(args), options_of(args)))
    return EXIT_OK


def build():
    parser = argparse.ArgumentParser(prog="plugin_transition.py", description=__doc__)
    parser.add_argument("--codex-home")
    parser.add_argument("--dest", help="the install destination whose pointer names the runtime;"
                                       " derived from the recorded relayExecutable when omitted")
    parser.add_argument("--event", default=None)
    sub = parser.add_subparsers(dest="command", required=True)

    inspect = sub.add_parser("inspect")
    inspect.set_defaults(handler=cmd_inspect, apply=False)

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
