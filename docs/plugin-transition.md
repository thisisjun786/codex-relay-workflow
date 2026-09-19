# Moving a manual install to the plugin install

The [linked installation](../README.md#install) and the [plugin installation](plugin-packaging.md)
can both be present on one host, and on that host two things run for every one that should. This
page is how a host with a manual install becomes a host with a plugin install, and what owns the
update, the failure, the disable and the removal afterwards.

`scripts/plugin_transition.py` performs it. It prints one JSON document per run, it writes nothing
without `--apply`, and it never installs a runtime, registers a plugin, grants hook trust, stops a
service, or deletes an operational database, journal, receipt or assignment.

## The three surfaces, and who owns each

| Surface | Manual install | Plugin install |
| --- | --- | --- |
| Skills | `$CODEX_HOME/skills/crw-*` symlinks into a checkout | the plugin cache, offered as `crw:crw-run` and so on |
| Task bridge | `[mcp_servers.codex-thread-bridge]` in `config.toml`, plus a record naming owner `user` | `wiring/mcp.json` plus a record naming owner `plugin` |
| Completion hook | an entry in `$CODEX_HOME/hooks.json` running `<checkout>/scripts/completion_hook.py` | the package's declared Stop hook plus `crw-completion-hook.json` settings naming owner `plugin` |

## Why there is a standdown step

`completion.run()` validates the settings, including who owns the registration, and then calls the
guard regardless of that owner. It does not stand down on it. Both readers also read the same
settings file by default: the packaged launcher reads `$CODEX_HOME/crw-completion-hook.json` and
nothing else, and the user-owned command carries that same path. So there is no content you can put
in that file that leaves one of them running and stops the other. With valid settings naming the
plugin, both fire on every Stop.

The only thing that stops the old registration is that registration no longer being there, or no
longer being trusted. So the transition removes it, and removal is where the care goes.

## What it will and will not remove

It removes only what it can prove runs this repository's own code:

| Surface | What is proven | Otherwise |
| --- | --- | --- |
| skill link | a symlink resolving to a `crw-*` skill directory inside a CRW checkout | left byte-identical, and named in the output |
| hook entry | the command is exactly what `completion.command_for` emits for the words it names, and its script sits in a CRW checkout | left in place, the transition refuses, the hand edit is printed |
| `config.toml` table | the bytes equal what `codexconfig.render` produces for the registration found there | left in place, the transition refuses |
| bridge record | it passes the record's own shape check and names owner `user` | left in place, the transition refuses |

One limit worth stating plainly: a hook file carries no provenance, so nobody can prove from it who
wrote a registration. What is proven is that the registration runs this repository's adapter. That
is the property that matters, because a registration running our adapter beside the plugin's
declaration is the double fire this exists to prevent, whoever created it.

## The order, and the two windows

    preflight
    1 retire the settings the registration names
    2 remove the registration that runs our adapter
    3 write the plugin-owned settings, recording the adapter under the destination pointer
    4 retire the user-owned bridge record
    5 remove the config.toml table
    6 write the plugin-owned bridge record
    7 remove the CRW-owned skill links

Part of that order is forced, and the forced part is what prevents doubles: the registration
goes before the new settings, the old settings go before the new ones, and the table goes before the
plugin record. Steps 4 to 6 are held under the same ownership lock `register-mcp` takes, because a
user-owned registration landing in the middle would put the record back and leave the host with no
bridge.

The settings are retired before the registration is removed, and that is deliberate. A custom
settings path is recorded only in the hook command, so removing the command first and stopping there
leaves a file the next run cannot rediscover, and a host with no completion hook. Retiring first
costs a window in which the old registration runs against settings that are no longer there, which
it answers by releasing in silence, and costs no window in which two adapters run, because the
plugin-owned settings are not installed until step 3.

Three windows follow, and all three are printed by the run:

- between 1 and 2 the old registration runs with no settings to read and releases without recording;
- between 2 and 3 no completion hook fires at all;
- between 5 and 6 a session that starts finds no bridge registered.

## Renumbering and trust

Codex records hook trust positionally. Removing an entry shifts the index of every later hook in the
same event and detaches the trust recorded against those positions, so those hooks need trusting
again. The transition refuses to do that until `--accept-hook-renumbering` says it may, and it names
every identity that would shift. When the entry is the only hook in its matcher group, the group is
left in place and empty, which renumbers nothing.

Trust for the plugin's own hook is reported, never asserted. A `[hooks.state]` entry records a hash
for the hook as it stood when trust was given, and nothing here can compute the hash Codex compares
it against, so a stale or fabricated record is indistinguishable from a current one. An untrusted
declared hook fires zero times, which would turn the window above into a host with no completion
hook at all. So the transition reports the key it found, reports that the hash was not compared, and
requires `--accept-hook-trust-gap` on every run. Trust the hook and confirm it fires first.

## Preflight refuses rather than half-finishing

    python3 scripts/plugin_transition.py inspect
    python3 scripts/plugin_transition.py transition            # a dry run
    python3 scripts/plugin_transition.py transition --apply

Before anything is removed, preflight requires: the plugin registered and enabled in `config.toml`;
a cache version that passes `scripts/ci/plugin.py --payload`, because an empty hook document and an
empty `mcp.json` satisfy a file census and leave nothing working; the cached package carrying every
skill the links being removed provide; the destination pointer resolving, and the adapter, its
interpreter and the bridge each existing as executable files, because a dangling pointer still reads
as a link; and every registration proven, including any table that starts the same bridge under
another name, because `register-mcp` takes `--name` and leaving an alias would start two
bridges.

Work in flight is reported rather than judged. The relay is never asked: its status subcommand takes
no store argument, asking about an absent store would create one, and an idle relay answers with a
nonempty object. The marker root is listed instead, and a marker is created once and outlives the
work it recorded, so those entries are history. Whether a turn is running right now is not
establishable from these records, and the run says so rather than refusing on a reading that was
never about liveness.

## Re-running, and an interrupted run

Every step decides from what is on disk, so nothing depends on a previous step's result in memory. A
second run reports `already_done` for each step, writes nothing, and exits zero. A run interrupted
anywhere converges on the next run.

## Update

Nothing here migrates a session. The settings record the adapter, the interpreter and the relay
through `<destination>/current`, so replacing the version behind that pointer changes what new work
resolves while a process already running keeps the one it started with. The records are not rewritten
by an update, which is the point of recording the pointer rather than a resolved version.

## A failed version replacement

    python3 scripts/plugin_transition.py swap-state

It reports the pointer state, the pointer target, whether that target resolves, and whether a host
record was read, as separate facts. What it does not do is invent the failed run's own residue:
`residualPaths`, `residualOwnership` and `recoveryRequires` exist only in the result of the run that
failed and cannot be recovered from any later reading. The retry is the owner's own command,
`runtime_install.py install --apply`. Nothing in this page moves a pointer, writes a host record or
touches the store.

## Disable and remove

    python3 scripts/plugin_transition.py disable --apply
    python3 scripts/plugin_transition.py remove --apply

`disable` retires the two records the packaged launchers read. New adapter invocations stop, because
the launcher finds no settings and returns; new bridge starts stop, because the launcher has no
record. What does not stop: a bridge already spawned in a running session, a turn already inside the
adapter, and the relay service if one runs. Excluding a shared service is the operator's own action,
and this tool never performs or claims it.

`remove` additionally removes the CRW-owned skill links. Out of its scope, and printed as such: the
plugin cache and its `config.toml` entry, which `codex plugin remove` owns; the marketplace
registration; the runtime installation; and the relay store, the bridge ledger, the hook journal and
every receipt. There is deliberately no purge flag.

## What none of this establishes

Written, registered, trusted and fired are four claims. These commands can establish the first two.
That a hook fired on a trusted path, that a promoted pointer serves a real installation, and that a
round trip completed are separate observations with their own task.
