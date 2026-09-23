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
| `plugins/crw/.codex-plugin/plugin.json` | Manifest: plugin name, the version that names the payload, and the declared skills path |
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

## The version names the payload

`codex plugin list` shows a name and a version, and the cache path is
`$CODEX_HOME/plugins/cache/<marketplace>/<plugin>/<version>`. Both of those are the version,
so a version several payloads can claim answers no question an operator actually has.
Revisions `fec0b69` and `174eacd` of this repository ship `plugins/crw` trees that differ in
21 files, and both manifests declared `0.4.0`: nothing on the host distinguished them, and
installing the second replaced the first in a directory of the same name.

So the version carries its payload's digest as semantic-version build metadata:

    "version": "0.4.0+640ccf7eadf4"

`0.4.0` remains the release version and stays the release owner's to choose. Semantic
versioning ignores build metadata when it orders two versions, so the suffix changes nothing
about precedence; it only makes the displayed version, and the directory named after it,
specific to the bytes underneath. Under this rule the two revisions above read
`0.4.0+344a3a6949c7` and `0.4.0+4bbc00f85a6e`.

The suffix is the first twelve hex characters of the digest of the payload, and it is derived
rather than maintained. `python3 scripts/ci/plugin.py --record-version` writes it into the
working-tree manifest; `python3 scripts/ci/plugin.py` re-derives it and refuses a version that
names other bytes, naming the value that should have been recorded. That refusal covers the
release payload, the working tree and an installed cache directory alike, so `--payload <dir>`
answers which bytes the directory in front of you holds rather than which name it was filed
under.

It reads the manifest in that directory and not the directory's own name. An install derives
both from one manifest, so they agree by construction; a cache renamed or assembled by hand
still declares the payload it holds, and comparing that version with the directory it sits in
is a separate reading this check does not make.

The payload is what installation copies: the roots the manifest declares, `.codex-plugin/` and
`LICENSE`, each file's path, mode and contents. It is the same payload `--json` reports a
digest for and the same one `plugin_transition.py check-declaration` reports as
`payloadDigest`, so what ships has one definition and the version is derived from that one.

One field is left out of the digest, and it has to be. The manifest ships inside the payload it
names, so digesting the bytes as they stand has no fixed point: recording the answer would
change the answer. The version's build metadata is elided from the manifest before the digest
is taken, which leaves a value that does not move once written. Every other byte of every
shipped file still reaches the digest, the rest of the manifest included, so the suffix is the
only difference between two release trees this rule cannot see. A shipped file that repeats the
suffix is refused for the same reason: two places holding one derived value could never be
updated to agree.

Measured on codex-cli 0.154.0, installing from a local marketplace into an isolated
`CODEX_HOME`: `codex plugin add` accepted `0.4.0+probe0a1b2c3d`, `codex plugin list` displayed
that version in full, and the cache directory was `plugins/cache/crw/crw/0.4.0+probe0a1b2c3d`.
`version` is a key the ingestion validator already reads; what this measured is that it accepts
this spelling of the value and carries it into the path. Whether a published marketplace
applies a further rule to build metadata was not measured.

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

   To have the bridge check role pairs, name the host's execution policy on the first of those two
   commands with `--execution-policy <file>`. The record then names the file and its digest, the
   launcher refuses to start the bridge when that file is gone or has changed, and the bridge reads
   it through `CODEX_THREAD_BRIDGE_EXECUTION_POLICY`. When crw is enabled and its package is
   already cached, register it only after that package ships a launcher reading that record and
   Codex has loaded it: `register-mcp` refuses while the enabled package's cached launcher is
   older, or while the cache cannot be looked at. With no crw package enabled or cached yet, the
   record is written and waits, inert, for the package that will read it. See
   [the execution policy the plugin bridge runs under](runtime-install.md#the-execution-policy-the-plugin-bridge-runs-under).
3. Trust the hook. Until it is trusted nothing fires, and no command in this repository
   grants that: installing writes no trust, and a session without it runs the hook zero
   times and says so nowhere.

Step 3 is what makes an installed hook look broken while it is working exactly as
installed. Steps 1 and 2 may run in either order; step 3 is last, because it is trust in
the hook as it then stands.

None of this starts a daemon. The server is a stdio process Codex spawns per session (one per
thread on Codex Desktop 0.154.0, observed), so a record written afterwards reaches the next thread
and not one already running. Replacing the package is the exception measured so far: the host
started bridges again from the new version directory, and each ran under the record as it stood
at that moment ([what one replacement measured](#what-one-replacement-measured)). The hook runs on
a Stop and exits, and the completion hook installs in `observe` mode, which classifies and
records and never holds a turn.

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

Bump `version` in the manifest when the change should reach installations, run
`python3 scripts/ci/plugin.py --record-version` so the suffix names the new payload, and
install the plugin again: a cached version changes only on installation, and a turn already
running keeps the skills directory it was given, which is the one the next install removes, until
that turn ends ([the cache lifetime](#the-cache-lifetime)). Adding a skill changes the payload, so
the suffix moves even when the release version does not, and the check refuses the commit
that leaves the old one in place.


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
| MCP start `cwd` and `args` | Yes | The host starts bridges again from the new version directory, and each runs under the bridge record as it stands at that moment: measured once, on the host. That the add caused the restart is inferred from its timing | The host |
| Skill reads | Yes | A turn already running keeps the removed directory as its skills root until it ends, and the thread's next turn is given the new one: measured on the host. A read against the removed directory was not observed | The host |

Two of those this package can answer for and two it cannot, and the difference is a host rule
rather than a preference. A hook command goes through a shell, so it can be written to resolve
its own program at run time. An MCP `command` must be a bare executable name or a contained
`./` path, and its `cwd` must be a contained `./` path, `${PLUGIN_ROOT}` or `${PLUGIN_DATA}`;
skills are read by the host from the directory the manifest names. Neither can be pointed
outside the version cache by anything this package declares.

### What one replacement measured

The two rows the host owns were measured on the user's host during one package replacement, from
`0.4.0` to `0.4.0+1ed13de2edbb` on 2026-09-23 with `codex plugin add` at 11:17:06Z, by reading
the App Server's child processes and the context each session recorded. The evidence is kept with
the task record, outside this repository.

About a minute after the add, while the Stop hook waited to be trusted again, the App Server started
twelve new bridge processes within three seconds. Each ran from the new version directory with the
six variables the App Server gives a plugin server, no bridge was left running from the old
directory, and the App Server itself was not restarted. So the MCP reference does not outlive the
directory the way a Stop command can: the host resolves the declaration again against the new
directory. That the add, or the configuration written around it, caused the restart is inferred
from the timing; nothing the host printed names a cause.

A restarted bridge runs under whatever the bridge record says at that moment, and the record lives
in the Codex home, outside the cache. At this replacement it was still version 1: the old package
was enabled and cached and its launcher predates version 2, so `register-mcp` could only write the
policy once the new package was in place, and it did so about thirty seconds after the restart. The
twelve restarted bridges checked no role pair, and ten of them were still running that way more
than an hour and a half later. Every bridge started after the record was written carried the
policy, one of them started when a thread was resumed.

A later replacement does not repeat that while the record is version 2 and the new launcher reads
it the way this one does. The launcher finds the Codex home six directories above its own file and
reads `crw-bridge-mcp.json` there. The record names the owner, the bridge executable, its arguments
and the policy file with its digest; no field ties it to a version directory or a payload, and the
executable the documented command registers is the installer's pointer, outside the cache. So a
launcher in any version directory under the same Codex home reads the same record. That was
measured in an isolated Codex home with codex-cli 0.154.0: one package installed and a version-2
record written, then a different payload installed in its place, which removed the first
directory. The new directory's launcher, started with its declared command, arguments and working
directory and the App Server's environment, handed the bridge the recorded policy,
`get_capabilities` reported its digest, and a `register-mcp` rerun answered `record_unchanged`. The
two payloads differed in a skill and shipped the same launcher bytes, so a launcher with other
bytes is outside what this showed; [updating safely](#updating-safely) probes every candidate
before it is added. Three states failed closed rather than open: no record, a policy file that no longer
matched its digest, and a rollback to a package whose launcher predates version 2 each made the
launcher exit 2 and start no bridge. Only a version-1 record started a bridge that checks no role,
which is what the host had. No App Server ran in that home, so it shows what a restarted bridge
reads, not whether the host restarts it.

Skills follow the turn. Three threads were in a turn when the package was replaced, one of them a
turn begun hours earlier, and each compacted afterwards with the removed directory still the root it
had been given. Three threads whose next turn began after the replacement were given the new
directory when that turn started, one of them on a resume. A compaction summary can still mention
the old directory as history, so the row speaks of the root a turn is given, not of every mention.
None of the six tried to read the removed directory in the forty minutes that followed, so what such
a read does was not observed.

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

| Task holding the old package reference | Stop | MCP bridge | Skill reads |
| --- | --- | --- | --- |
| Existing task, including an idle interval between turns | The fallback is available if installed; a quiet turn does not establish package-reference reload | Started again by the host from the new directory, under the record as it stood then: measured once | The next turn is given the new root: measured |
| Existing task during a turn, removed cache | Fallback invocation measured after removal | Started again during the turn, as above: measured once. What a tool call in flight across the restart sees was not measured | The turn keeps the removed root until it ends: measured. A read against it was not observed |
| Fresh task created after update | Invocation measured from the updated installation | Verify the new task's actual MCP call | Verify the new task's actual skill read |

The fallback protects the Stop launcher, and nothing measured shows a later turn resolving the Stop
command afresh: a task can remain alive across many turns, and for that command an idle interval is
not a session reload. The other two references were measured to behave differently. The skills root
is renewed when the next turn starts, and the bridge is started again by the host at the
replacement, under whatever the record says then.

### Updating safely

This is the order for an update that changes the payload, which is any update that moves the
version directory. It assumes the plugin owns the bridge and the Stop hook, as
[turning the wired surfaces on](#turning-the-wired-surfaces-on) sets them up, and it rests on
[what one replacement measured](#what-one-replacement-measured). Steps 1 to 7 are written for a
host whose bridge record is already version 2. A host whose record is version 1, or absent, is
making its first policy registration as well, and step 8 says what that changes.

1. Prefer a moment when no turn is running. A turn in progress keeps the removed skills root until
   it ends and has its bridge started again underneath it, and what a tool call in flight across
   that restart sees was not measured. Nothing here lists running turns for you.
2. Read the version-2 record without changing it: run `register-mcp` with the arguments it was
   written with and without `--apply`.

   ```sh
   python3 scripts/runtime_install.py register-mcp --owner plugin \
       --bridge-command <destination>/current/bin/codex-thread-bridge \
       --execution-policy <file>
   ```

   `record_unchanged` means the record still names that policy and the file still hashes to the
   recorded digest, and, with crw enabled and one version cached, that the cached package's declared
   server started a probe under such a record; nothing is written. Every bridge the host starts again during the add reads this record at that moment, so
   settle any other answer first: a policy file edited since it was registered, for one, would make
   every restarted bridge refuse to start.
3. Run `python3 scripts/runtime_install.py hook --adapter completion --owner plugin --apply`, so the
   fallback is current before the directory it backs up can disappear.
4. Probe the candidate before adding it, whatever its bytes. Install it into a throwaway Codex
   home and run the step 2 command against that home:

   ```sh
   CODEX_HOME=<throwaway> codex plugin marketplace add <source>
   CODEX_HOME=<throwaway> codex plugin add crw@<marketplace>
   python3 scripts/runtime_install.py register-mcp --owner plugin --codex-home <throwaway> \
       --bridge-command <destination>/current/bin/codex-thread-bridge \
       --execution-policy <file>
   ```

   Proceed only when all three hold: the add reports the one version directory it installed; the MCP
   declaration in that directory, the file its manifest's `mcpServers` names, declares
   `codex-thread-bridge`; and `register-mcp` answers `record_would_create`. With the server
   declared and one version cached, that answer means `register-mcp` started the candidate's declared
   command, arguments and working directory with a version-2 record naming a probe, and it passed.
   Without the first two, the same answer means no probe ran. Any other answer means the candidate
   was not shown to start the bridge under the policy, and `launcher_predates_policy` in particular
   means its bridge would start without the policy or not at all: do not add it until the answer is
   `record_would_create`. Both of those answers were measured in throwaway homes, and neither run
   left a record behind.
5. On a host that gates the bridge,
   [check the candidate's declaration](#before-you-add-or-update-on-a-host-that-gates-the-bridge).
   Then run `codex plugin add crw@<marketplace>`, and re-trust the hook once if its command
   changed. At the measured replacement the host started bridges again inside this step, before the
   trust was given.
6. Read back what landed:
   - the installed payload, with `check-declaration` or `python3 scripts/ci/plugin.py --payload <dir>`;
   - the step 2 dry run again, which now probes the server the new package declares and should
     still answer `record_unchanged`;
   - `get_capabilities` in a fresh task and in the tasks that were loaded during the add: its
     `executionPolicy.digest` is the digest the record names, and a bridge with no policy reports
     `presence_only`;
   - every running bridge, host-wide, read from `/proc` the way the measured replacement was read.
     This is an operator reading of your own processes, not a command this repository ships:

     ```sh
     python3 - <<'EOF'
     import json, os, pathlib
     home = pathlib.Path(os.environ.get("CODEX_HOME") or pathlib.Path.home() / ".codex")
     record = json.loads((home / "crw-bridge-mcp.json").read_text())
     want = (record.get("executionPolicy") or {}).get("digest")
     key = b"CODEX_THREAD_BRIDGE_EXECUTION_POLICY_DIGEST="
     for pid in sorted(filter(str.isdigit, os.listdir("/proc")), key=int):
         try:
             argv = open(f"/proc/{pid}/cmdline", "rb").read().split(b"\0")
             cwd = os.readlink(f"/proc/{pid}/cwd")
             env = open(f"/proc/{pid}/environ", "rb").read().split(b"\0")
         except OSError:
             continue
         if "/plugins/cache/crw/crw/" in cwd and any(a.endswith(b"codex-thread-bridge") for a in argv):
             have = next((e[len(key):].decode() for e in env if e.startswith(key)), None)
             state = "policy" if have and have == want else "OTHER POLICY" if have else "NO POLICY"
             print(pid, os.path.basename(cwd), state)
     EOF
     ```

     Each line is a bridge process, the version directory it runs from, and whether it carries the
     recorded policy.

   Preserve existing recovery evidence and compatibility paths until their readers are gone.
7. Tasks that were loaded before the replacement:
   - A turn that was running keeps the removed skills root it was given until it ends, and that
     directory is gone; what a later read against it does was not observed. The thread's next turn
     is given the new root.
   - Its bridge was started again at the replacement and runs under the record as it was at that
     moment. With a version-2 record and a candidate that passed step 4, that is the policy: measured
     in isolation, not yet at a host replacement. With a version-1 record it is no policy, as the
     host showed; with no record there is no bridge, as the isolated run showed. A bridge that step 6
     reports without the policy stays that way until the host starts that thread's bridge again. A
     resume did that at the measured replacement; nothing in this repository can.
   - A Stop whose command names the removed directory falls back to the copy step 3 placed.
8. The first policy registration. Step 4 is what shows the new package starts a bridge under a
   version-2 record, and it applies in every order below. With no record at all, run the step 2
   command. `record_would_create` there means no enabled, cached crw launcher on this host would
   refuse the record, which includes a host with no crw package cached: register with `--apply`
   before step 5 and continue as above. `launcher_predates_policy` means the cached one would:
   register with `--apply` right after step 5 instead; until then a launcher that finds no record
   starts no bridge (measured in isolation), so the threads whose bridges restart in between have no
   bridge tools until the host starts their bridges again. A version-1 record is not given a policy
   in place:
   `register-mcp --execution-policy` answers `record_differs` and names the repair, which is to
   move the record aside by hand and register again. And while the crw package that is enabled and
   cached ships a launcher older than version 2, `register-mcp` refuses the version-2 record itself
   ([turning the wired surfaces on](#turning-the-wired-surfaces-on)). Step 2 therefore answers with
   one of those refusals, and the choice is when the version-1 record leaves:
   - After step 5, as at the measured replacement: add the package, move the version-1 record aside,
     run the step 2 command with `--apply`, read back as in step 6, then have the host start again
     the bridges it reports without the policy. Every bridge the host restarts before the record
     moves checks no role.
   - Before step 5: move the version-1 record aside, add the package, then register with `--apply`
     and read back the same way. A launcher that finds no record starts no bridge (measured in
     isolation), so a thread whose bridge restarts in between has no bridge tools rather than
     unchecked ones, until the host starts its bridge again after the registration. What the host
     does after a bridge refuses to start was not measured.
   - With the old package disabled or removed first: move the record aside, register with `--apply`,
     which is accepted while no crw package is enabled or cached, then add the new package and read
     back. What disabling does to loaded threads, their skills, Stop hook and bridge included, was
     not measured.

`codex plugin add` removes the prior version cache. This procedure does not promise to retain
that directory automatically or prescribe copying it back after a gap as uninterrupted support.
If path continuity cannot be established before replacement, use the turn boundary in step 1; do
not proceed on an assumption that the old cache will remain.
`python3 scripts/plugin_transition.py swap-state` reports what a replacement actually left. It
reads the pointer and the host records, not the tasks holding references, so it cannot establish
that the last reader is gone; that needs evidence of its own.

A host carrying temporary compatibility files — an old cache path kept alive by hand after an
update went wrong — needs a record of its own, kept with the task record outside this
repository. Record the path, who made it, why, and the condition under which it may be removed.
Nothing here enumerates the tasks still holding a reference, so the evidence that the last reader
is gone has to come from the host; record which observation was used. Such a file is a repair, not
a guarantee that the next update will be survivable.

## Update and roll back

The cache keeps one version per plugin, and installing a new version replaces the
previous directory instead of keeping both. Rolling back therefore means making
the source offer the earlier revision again and reinstalling it, not selecting an
older copy from the cache. Bump `version` in the manifest for a release and record the
payload suffix under it; a new
task picks up the new package when its session starts. A task already running meets it piece by
piece: the host starts its bridge again, a turn in progress keeps the skills directory it was given
until it ends, and a Stop whose command names the removed directory falls back.
[The cache lifetime](#the-cache-lifetime) says which of those were measured.

Because the suffix follows the payload, an earlier revision reinstalls into its own directory
rather than over the one that replaced it, and `codex plugin list` says which of the two is
present.

A rollback past the bridge record's version installs, and then its launcher refuses the record. A
launcher that predates version 2 starts no bridge under a version-2 record: measured in isolation,
where the older package's `codex plugin add` succeeded and every start of its bridge exited 2. So
threads have no bridge tools after such a rollback until the record is replaced with a version-1
one, and a version-1 record starts bridges that check no role.

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

`--record-version` is the one command here that writes: it puts the derived suffix into the
working-tree manifest and stops without validating anything. Commit what it wrote, because the
release payload is read from a revision and not from that tree.

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
