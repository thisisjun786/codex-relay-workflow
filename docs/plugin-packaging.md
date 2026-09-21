# Plugin packaging

This repository publishes its skills as a versioned Codex plugin. The package also
declares the task-bridge MCP server and the completion Stop hook, and ships the two
small launchers that start them. It carries no runtime: the bridge, the session
relay, the Python environment and the completion adapter keep their own installer,
and the launchers only point at what that installer left behind.

## What the package is

| Path | Role |
| --- | --- |
| `.agents/plugins/marketplace.json` | Marketplace entry; its `source.path` names the plugin root |
| `plugins/crw/` | The plugin root, copied into the version cache as it stands |
| `plugins/crw/.codex-plugin/plugin.json` | Manifest: plugin name, version, and the declared skills path |
| `plugins/crw/skills/` | The registered skills, one of the two declared components |
| `plugins/crw/wiring/` | The declared Stop hook and MCP server, and the two launchers they start |
| `plugins/crw/LICENSE` | The repository license, shipped with the package |
| `skills` | A link to `plugins/crw/skills`, kept for installations made before the move |

Edit the skills at `plugins/crw/skills/`; the root `skills` link is a compatibility
path, not a second copy, and it is a Git symlink, so a checkout without symlink
support turns it into a plain text file. The supported platform is Linux x86_64.

What may sit in the plugin root is whatever the manifest declares, plus
`.codex-plugin/` and `LICENSE`. Installation copies that directory verbatim,
including untracked and ignored files, so anything left there is published, and a
component nobody declared installs without ever loading. `python3 scripts/ci/plugin.py`
derives the permitted roots from the manifest, refuses a declaration naming a file the
package does not ship, and checks the hook and server documents against the shapes that
were measured to load. The manifest itself may carry only the keys
the ingestion validator knows, and optional presentation fields are checked against
its shapes: URLs that begin with `https://`, a `#RRGGBB` brand colour, and `./`
relative asset paths the package actually ships.

The manifest declares every component the package ships. A declared component replaces
default discovery rather than adding to it, measured on codex-cli 0.154.0: a plugin declaring a
non-default skills directory while also holding `./skills/` loaded only the declared
one. The plugin specification bundled with Codex describes the opposite, saying
declared components supplement default discovery. This package follows the measured
behavior and keeps every shipped component under a declared path.

## How hooks and MCP servers load

Measured on codex-cli 0.154.0 by installing probe plugins into isolated Codex homes and
ending real turns against a local stub model provider, so the readings below are what a
hook and a server actually did rather than what an installer accepted. The evidence is
kept with the task record, outside this repository.

| Reading | Result |
| --- | --- |
| `hooks` as an array of file paths | Loads, and the hooks fire |
| `hooks` as one string path | Loads, and the hook fires |
| `hooks` as an inline document | Does not load |
| No `hooks` key, with a `./hooks/` directory present | Does not load: there is no default discovery for hooks |
| Firing without persisted hook trust | Nothing fires. `--dangerously-bypass-hook-trust` is what made a probe fire |
| `[hooks.state]` after installing, after read-only commands, after a session | Empty every time; `codex exec` never recorded trust |
| Variables in a hook command | Expanded: the command goes through a shell and the process carries `CODEX_HOME`, `PLUGIN_ROOT`, `CLAUDE_PLUGIN_ROOT` and `CLAUDE_PLUGIN_DATA` |
| The same variables for a hook registered in `$CODEX_HOME/hooks.json` | `PLUGIN_ROOT` is absent; that environment belongs to plugin-declared hooks |
| `skills`, `hooks` and `mcpServers` declared together | All three load |
| A relative command in an MCP declaration | Resolves only when `cwd` is set; a `cwd` of `.` resolves to the installed version directory |
| An MCP declaration with no `cwd` | `cwd` stays null and the server never starts |
| A plugin-root variable in an MCP argument | Delivered literally, never expanded, and the server never starts |
| What an MCP server inherits | `HOME` and its working directory. Not `CODEX_HOME`, not `PLUGIN_ROOT` |

Those two environments are opposites, and the wiring is built around the difference. A
hook command can name the plugin root and the Codex home through shell variables because
a shell expands them. An MCP command can do neither, so it sets `cwd` to `.` with a
`./` relative argument, and the program it starts derives the Codex home from its own
location: the cache layout is
`$CODEX_HOME/plugins/cache/<marketplace>/<plugin>/<version>`.

Hooks ship as an array with one event per file. A single file carrying several events
works too, but a hook's identity is positional, so adding an event to a shared file
renumbers the ones after it and detaches the trust Codex recorded against them.

Installing the package does not make its hooks run. Trust is a separate, explicit step,
and until it is given the declared hooks are inert. That is why installation activates
nothing on its own, and why the installation flow has to say so rather than leave an
operator waiting for a hook that is working exactly as installed.

## Install

```sh
codex plugin marketplace add thisisjun786/codex-relay-workflow --ref dev
codex plugin add crw@crw
```

For development, register a local checkout as its own marketplace so it stays
separate from the published one:

```sh
codex plugin marketplace add /path/to/codex-relay-workflow
codex plugin add crw@crw
```

Installing creates no credential. The marketplace entry sets
`authentication: ON_USE`, so Linear and repository access are checked when a skill
needs them, and a skill says so and stops when they are missing.

### Before you add or update, on a host that gates the bridge

A host whose `config.toml` gates `create_thread` or `send_message_to_thread` keeps that gate only
because the package declares the same one. Check the package you are about to install, not the one
already there:

```sh
python3 scripts/plugin_transition.py check-declaration --package <candidate>   # exits 1 if it would not preserve
codex plugin add crw@crw
python3 scripts/plugin_transition.py check-declaration --package "$CODEX_HOME/plugins/cache/crw/crw/<version>"
```

Both receipts carry `payloadDigest`. Equal digests are what tie the first verdict to the bytes that
landed; a mismatch says the package changed between the check and the add. The same two steps apply
to `codex plugin update`, which is otherwise checked by nothing at the moment it replaces the
declaration. This is a gate you run: nothing in this repository invokes `codex plugin add`,
`update` or `remove`. See [approval policy](plugin-transition.md#approval-policy).

## Turning the wired surfaces on

Installing the package installs the skills, and registers nothing else that works on
its own. The declared MCP server and Stop hook both reach a runtime this package does
not carry, and each needs a step the installation cannot take for you.

1. Install the runtime, if this host has none:
   `python3 scripts/runtime_install.py install --dest <destination> --apply`.
2. Write the two records the launchers read. Neither registers anything itself:
   `python3 scripts/runtime_install.py register-mcp --owner plugin --bridge-command <destination>/current/bin/codex-thread-bridge --apply`
   and
   `python3 scripts/runtime_install.py hook --adapter completion --owner plugin --dest <destination> --apply`.
   Each refuses when the same surface is already registered the other way, because the
   two together would run two bridges, or two hooks on every Stop.
3. Trust the hook. Until it is trusted nothing fires, and no command in this repository
   grants that: installing writes no trust, and a session without it runs the hook zero
   times and says so nowhere.

Step 3 is what makes an installed hook look broken while it is working exactly as
installed. Steps 1 and 2 may run in either order; step 3 is last, because it is trust in
the hook as it then stands.

None of this starts a daemon. The server is a stdio process Codex spawns per session,
the hook runs on a Stop and exits, and the completion hook installs in `observe` mode,
which classifies and records and never holds a turn.

The linked installation in [README](../README.md#install) still works and is
unchanged. Both installations read the same source: `scripts/install.py` links the
directory the manifest declares, and the repository root keeps `skills` as a link
to it so links created before the move still resolve.

A host carrying both runs two of everything. [Moving a manual install to the plugin
install](plugin-transition.md) is how one becomes the other, and it owns the update, the failed
update, the disable and the removal that follow.

## Skill names

A linked installation exposes the skills as `crw-run`, `crw-plan`, and so on. A
plugin installation namespaces them under the plugin name, so the same skills are
offered as `crw:crw-run`, `crw:crw-plan`, and so on. The documents keep the
unprefixed names and the mapping lives in
[the shared integration guide](../plugins/crw/skills/crw-plan/references/integrations.md),
so one revision reads correctly under either installation.

`scripts/ci/plugin.py` derives the namespaced names from the revision rather than
from a list in prose:

```sh
python3 scripts/ci/plugin.py --json
```

Whether a client resolves a `$`-prefixed invocation token for a plugin skill, and
whether the per-skill `agents/openai.yaml` interface metadata is read under a
plugin installation, were not measured for this change.

## Adding a skill

Create the directory under `plugins/crw/skills/` with its `SKILL.md` and
`agents/openai.yaml`. The manifest lists no skills: the package ships whatever the
declared path holds at the release revision, and `scripts/ci/plugin.py` derives the
namespaced names from that revision, so a skill developed in parallel is included
once its commit is part of that revision. The installer test pins the registered
names as a positive control, so a new skill belongs in that list too. Keep relative
links between skills pointing at siblings under the same parent; the cache preserves
that layout.

Bump `version` in the manifest when the change should reach installations, and
install the plugin again: a cached version changes only on installation, and a task
already running may still hold cache-bound references to the version its session started
with — the same directory the next install removes.


## The cache lifetime

Installing a version replaces the cache directory whole. `codex plugin add` removes the
previous version directory, and so does a rollback to an earlier one. Anything a running
session still points into that directory stops resolving at that moment, and the question for
each declared surface is whether its reference outlives the directory it names.

| Reference | Bound to the cache | What a replacement does to it | Owner |
| --- | --- | --- | --- |
| Stop launcher, first candidate | Yes | Falls through to the second candidate | This package |
| Stop launcher, second candidate at `<CODEX_HOME>/crw-stop-hook.py` | No | Nothing | `runtime_install.py hook --owner plugin` |
| Stop settings at `<CODEX_HOME>/crw-completion-hook.json` | No | Nothing | The same command |
| Adapter, relay and bridge executables | No, they sit under the installer pointer | Nothing | `runtime_install.py install` |
| Hook document path in the run identifier | Yes | Held as an identifier and never re-read | The host |
| MCP start `cwd` and `args` | Yes | A server already running survives, because it has already replaced itself with the installed bridge. A restart inside that session is expected to fail: inferred from the `cwd` the host holds, not measured here | The host |
| Skill reads | Yes | A read against the removed directory is expected to fail: inferred from where the host reads skills, not measured here | The host |

Two of those this package can answer for and two it cannot, and the difference is a host rule
rather than a preference. A hook command goes through a shell, so it can be written to resolve
its own program at run time. An MCP `command` must be a bare executable name or a contained
`./` path, and its `cwd` must be a contained `./` path, `${PLUGIN_ROOT}` or `${PLUGIN_DATA}`;
skills are read by the host from the directory the manifest names. Neither can be pointed
outside the version cache by anything this package declares.

### Why the Stop hook is declared as a bootstrap

A hook command is fixed when a turn starts, with the plugin root already resolved into it, and
the whole turn reuses that string — including every Stop re-fire. Replace the package while a
task still holds that command and it names a file that no longer exists; nothing measured shows
a later turn of the same task resolving it afresh. `python3` exits **2** for a
missing script, and 2 is the hook protocol’s blocking code, so the host feeds the error back
to the model and fires Stop again. Measured on the user's host: one removed directory, eleven
repeated Stop prompts in a single turn of one task and eight in a single turn of a second, and
neither turn able to finish while that path was absent. Both completed once a compatibility path
was restored there, at 06:35:13Z and 06:35:59Z. An isolated reproduction of the same manoeuvre
produced thirty-seven in one turn.

So the declaration names two candidates and opens the first one it can read:

1. `${PLUGIN_ROOT}/wiring/crw_stop_hook.py` — the packaged copy. Always the current version, so a
   fallback left by an older install can never outrank it.
2. `<CODEX_HOME>/crw-stop-hook.py` — the copy `runtime_install.py` places. Reached only when
   the first one is already gone.

If neither can be opened it exits 0 and prints nothing. That is not error suppression: the
launcher’s own contract has always been that a Stop it cannot judge is a Stop it releases, and
the one failure outside that contract was the interpreter failing to open its own argument.
A candidate that opens and then fails while running is a different thing and is reported, as
exit 1, which the host reads as an ordinary failure rather than as a hold. Once a candidate has
been read it owns that Stop: the second one is not tried, because a launcher that raised after
doing half its work has already acted on the turn.

A launcher that exits non-zero deliberately is reported the same way. The launcher's own
contract is to exit 0 on every path, so a copy that breaks it is saying something, and turning
that into a success would hide the one failure it went out of its way to report. It surfaces as
exit 1 rather than as the code the launcher chose, because 2 is the blocking code and no path
through this declaration may produce it.

Which exits count as success follows the interpreter rather than the code's truthiness.
`SystemExit.code` is not restricted to integers: CPython exits 0 only for `None` and an integer
zero, and every other object exits 1 even when it is falsey. An empty string and `0.0` are
therefore failures, and the declaration treats them as failures.

Changing the command text changes the hook’s `trusted_hash`, so an update that changes it needs
one re-trust per installed hook identity. Trust is keyed to the declaration content and not to
the version path, so an update that leaves the command alone keeps its trust.

### The supported range

| Task holding the old package reference | Stop | MCP restart | Skill reads |
| --- | --- | --- | --- |
| Existing task, including an idle interval between turns | The fallback is available if installed; a quiet turn does not establish package-reference reload | Cache-bound reference may be stale; not measured as safe | Cache-bound reference may be stale; not measured as safe |
| Existing task during a turn, removed cache | Fallback invocation measured after removal | Failure is inferred from the retained cache-bound reference, not independently measured here | Failure is inferred from the retained cache-bound reference, not independently measured here |
| Fresh task created after update | Invocation measured from the updated installation | Verify the new task's actual MCP call | Verify the new task's actual skill read |

The fallback protects the Stop launcher. It does not make every old package reference survive
replacement. A task can remain alive across many turns; an idle interval is not a session reload.

### Updating safely

1. Identify tasks that still hold the version being replaced. Finish and replace those tasks
   through the supported new-task path, or establish a supported way to keep every referenced
   path available continuously. Merely observing no active turn is insufficient.
2. Run `python3 scripts/runtime_install.py hook --adapter completion --owner plugin --apply`
   first, so the fallback is current before the directory it backs up can disappear.
3. Run `codex plugin add crw@<marketplace>`.
4. Re-trust the hook once if its command changed.
5. Read back the installed payload and validate each required surface in a fresh task.
   Preserve existing recovery evidence and compatibility paths until their readers are gone.

`codex plugin add` removes the prior version cache. This procedure does not promise to retain
that directory automatically or prescribe copying it back after a gap as uninterrupted support.
If path continuity cannot be established before replacement, use the finished-task boundary
in step 1; do not proceed on an assumption that the old cache will remain.
`python3 scripts/plugin_transition.py swap-state` reports what a replacement actually left. It
reads the pointer and the host records, not the tasks holding references, so it cannot establish
that the last reader is gone; that needs evidence of its own.

A host carrying temporary compatibility files — an old cache path kept alive by hand after an
update went wrong — needs a record of its own, kept with the task record outside this
repository. Record the path, who made it, why, the condition under which it may be removed, and
the command that shows whether anything still references it. Such a file is a repair, not a
guarantee that the next update will be survivable.

## Update and roll back

The cache keeps one version per plugin, and installing a new version replaces the
previous directory instead of keeping both. Rolling back therefore means making
the source offer the earlier revision again and reinstalling it, not selecting an
older copy from the cache. Bump `version` in the manifest for a release; a new
task picks up the new package when its session starts, and work already running may still hold
cache-bound references to the version it started with, which is the directory the new install
removes.

Removing the plugin deletes the cached version directory and the plugin entry in
`config.toml`. It leaves the marketplace registration, so removing that is a
separate step, and it does not touch the relay store, the bridge ledger, the hook
journal or the runtime installation.

## Verify

Structural checks run offline and are part of CI:

```sh
python3 scripts/ci/plugin.py            # package shape, hygiene, release digest
python3 scripts/ci/validate.py          # skill metadata, local links, Python syntax
python3 -m unittest discover -s scripts/ci/tests
```

`plugin.py` builds the release payload from a Git revision rather than from the
working tree, so the bytes it validates are the ones a clone publishes. `--json`
prints that payload digest, and `--payload <dir>` applies the same rules to an
installed cache directory, which is how an installed tree is compared against its
source.

A passing check is evidence about this source. It is not evidence that a plugin
installed, that a skill loaded on any host, or that a running workflow changed.
Those need their own observation of `codex plugin list`, `codex debug prompt-input`
and the behavior itself.

### Declared approval policy

`wiring/mcp.json` may gate individual tools of a declared server:

    "tools": { "create_thread": { "approval_mode": "approve" } }

`scripts/ci/plugin.py` checks it, and it is the only signal a mistake here ever produces. Measured:
an invalid `approval_mode` in a plugin declaration makes `codex plugin add` exit 0 and
`codex mcp list` exit 0 with **zero** entries. The server disappears and no `disabled_reason` is
recorded, because there is no entry left to carry one. The same mistake in a user configuration is
a loud error naming the valid set.

So the check fails closed: the accepted values are `auto`, `prompt`, `writes` and `approve`,
measured from the host's own rejection text; `approval_mode` is the only key allowed inside a tool
gate; an empty `tools` object is refused because nothing measured says what a host does with one;
and `codex-thread-bridge` must keep gating `create_thread` and `send_message_to_thread` with
`approve`, because that gate is what the user configuration hands over when the transition removes
its table. See [approval policy](plugin-transition.md#approval-policy).
