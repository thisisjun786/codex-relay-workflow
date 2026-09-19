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

Every step decides from the host as it stands at that step, not from the snapshot the run opened
with. The bridge surface is re-read inside the ownership lock, the hook file is re-read and its
registrations re-proved inside the hook lock, the bridge table's span and its proof are re-derived
before a byte is removed, and the skills directory is inventoried again at step 7 and once more
after it. That last one is a weaker guarantee than the other two and is named as such: the
directory has no lock, so a link arriving during the removals is reported rather than prevented,
and the run refuses instead of reporting success over it.

Step 7 removes nothing until every link it would remove has been proved, and then proves each one
again in the moment before it is unlinked. Neither pass is a lock. The first stops a refusal from
leaving half a manual installation behind; the second stops a link replaced during the removals
from being deleted as though it were still ours, and narrows that window to the gap between a
read and the call after it. Closing it entirely needs a lock that `scripts/install.py` takes too,
which is a change to a tool other flows use and is not made here.

One limitation of the locks themselves, stated rather than papered over. The ownership lock is
`hostrecord.Locked`, because that is the lock `register-mcp` takes and a lock only excludes those
who take the same one. `Locked` treats a lock file older than 300 seconds as stale and removes it,
so a step that stays inside the lock for longer than that -- a cross-filesystem archive of a large
document, say -- can have its lock taken by a waiter while it is still working. Taking a different
lock here would not fix it; it would silently stop excluding the other owner, which is worse. The
repair belongs to `hostrecord` and to every command that takes that lock, and `hostrecord` already
carries the corrected mechanism it would use: `Exclusive`, which holds an advisory lock on a file
that is never unlinked and expires only when its holder dies.

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

It also requires the relay under that pointer, which is easy to forget because the adapter does not
answer a Stop by itself: it runs the relay as a subprocess for the guard decision, and a host
missing it reaches a working adapter on every Stop and releases with `guard_unreachable`.

## The cached package has to be the replacement, not merely a valid package

`scripts/ci/plugin.py --payload` answers whether the installed package is well formed. Well formed
is not the question standing in front of a working hook and a working bridge registration about to
be removed: its hook check accepts any nonempty event and command, and its server check any
nonempty name. So the transition also compares what the cached package DECLARES with what this
checkout ships, and refuses when they differ.

What is compared, and why each part is there:

- the documents the cached MANIFEST names, not the files at the paths this repository happens to
  use, because Codex loads what the manifest declares and ignores everything else in the package;
- the interpreter and the script together, as one pair, because half a launcher is not a launcher:
  a versioned name this checkout does not ship is a valid spelling of a Python that need not exist
  on the host;
- the script positionally, the way an interpreter resolves it -- the first non-option argument, with
  only the options that leave the next word alone skipped -- because `-c` takes source text and
  `-m` takes a module name, and a command that merely mentions the launcher does not run it;
- the matcher and the timeout beside the command, because the right launcher under a restrictive
  matcher fires on some turns and not others, and a one-second timeout is killed before the
  adapter's own budget can answer;
- the whole set, counted: a document declaring our launcher twice fires two adapters on every Stop,
  and an `mcp.json` carrying the expected entry plus another name starting the same launcher loads
  two bridges. Addition defeats a one-owner handoff as surely as substitution does;
- a declaration whose shape cannot be read is counted as exactly that rather than skipped, because
  a skipped declaration is one Codex still runs and this comparison cannot see.

What this deliberately is not: a shell parser, and not a resolution of the declared interpreter
against a PATH this command does not control. An unreadable shape is refused rather than
interpreted, and the interpreter that IS resolved is the one the settings record, through the same
probe that asks it to be a Python before recording it.

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

`disable` retires the two records the packaged launchers read, and decides each one under the lock
that serialises its own write: the settings under that file's lock, and the bridge record under the
same ownership lock `register-mcp` takes. Both owners are read again inside the lock, so a record
that became user-owned while this command ran is refused rather than archived as if it were ours.

The receipt reports what is in effect, not what was intended. `stops` names only the surfaces this
run actually left unable to serve a new call, `wouldStop` is what an `--apply` would stop on a dry
run, and `stillLive` names every surface a reader must not read as stopped, carrying the reason it
is not. A stopped settings record means new adapter invocations stop, because the launcher finds no
settings and returns; a stopped bridge record means new bridge starts stop, because the launcher has
no record. What does not stop: a bridge already spawned in a running session, a turn already inside
the adapter, and the relay service if one runs. Excluding a shared service is the operator's own
action, and this tool never performs or claims it.

`remove` additionally removes the CRW-owned skill links. Out of its scope, and printed as such: the
plugin cache and its `config.toml` entry, which `codex plugin remove` owns; the marketplace
registration; the runtime installation; and the relay store, the bridge ledger, the hook journal and
every receipt. There is deliberately no purge flag.

## What none of this establishes

Written, registered, trusted and fired are four claims. These commands can establish the first two.
That a hook fired on a trusted path, that a promoted pointer serves a real installation, and that a
round trip completed are separate observations with their own task.
