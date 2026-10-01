# Plugin packaging

This repository publishes its skills as a versioned Codex plugin. The package also
declares the task-bridge MCP server and the completion Stop hook. Both reach the Go runtime
through the installer's pointer, `$HOME/.local/share/crw-runtime/current/bin/`: the hook
command names `crw` there directly and the server starts a three-line `sh` launcher that execs
its `codex-thread-bridge` link ([the native wiring](#the-native-wiring)). It carries no runtime: the runtime keeps its own
installer, and the package only points at what that installer left behind.

## What the package is

| Path | Role |
| --- | --- |
| `.agents/plugins/marketplace.json` | Marketplace entry; its `source.path` names the plugin root |
| `plugins/crw/` | The plugin root, copied into the version cache as it stands |
| `plugins/crw/.codex-plugin/plugin.json` | Manifest: plugin name, the version that names the payload, and the declared skills path |
| `plugins/crw/skills/` | The registered skills, one of the two declared components |
| `plugins/crw/wiring/` | The declared Stop hook and MCP server, and the `crw-bridge.sh` launcher the server starts. The two Python launchers the pre-native declarations started left the package in todo 43 ([the native wiring](#the-native-wiring)) |
| `plugins/crw/LICENSE` | The repository license, shipped with the package |
Edit the skills at `plugins/crw/skills/`. Until todo 44 the repository root also kept `skills`, a
Git symlink to that directory, for installations made before the move; nothing installed through
it any more, and it left, so a link made through it is dangling and `crw-dev skills link` reports
it as a CONFLICT to relink. The supported platform is Linux x86_64.

What may sit in the plugin root is whatever the manifest declares, plus
`.codex-plugin/` and `LICENSE`. Installation copies that directory verbatim,
including untracked and ignored files, so anything left there is published, and a
component nobody declared installs without ever loading. `crw-dev ci plugin`, in the
repository's development binary, derives the permitted roots from the manifest, refuses a
declaration naming a file the package does not ship, and checks the hook and server documents
against the shapes that
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
rather than maintained. `crw-dev ci plugin --record-version` writes it into the
working-tree manifest; `crw-dev ci plugin` re-derives it and refuses a version that
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
digest for, so what ships has one definition and the version is derived from that one.

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
`./` relative argument. `HOME` is the one variable both environments carry, which is why
the native wiring anchors the runtime pointer under it.

## The native wiring

| Surface | Declared as | What it runs |
| --- | --- | --- |
| Stop hook | `"$HOME/.local/share/crw-runtime/current/bin/crw" hook --plugin-launch; exit 0`, `timeout: 10` | `crw hook --plugin-launch` through the pointer, reading the host's payload on stdin |
| MCP server | `command: "sh"`, `args: ["./wiring/crw-bridge.sh"]`, `cwd: "."` | `exec "$HOME/.local/share/crw-runtime/current/bin/codex-thread-bridge" --plugin-launch "$@"` |

`--plugin-launch` is where the record contract the Python launchers carried now lives
([decision 26](port/decisions.md)). `codex-thread-bridge --plugin-launch` (`crw bridge
--plugin-launch` under the other name) reads `<CODEX_HOME>/crw-bridge-mcp.json`, finding the
Codex home as the Python launcher did, and refuses on stderr with exit 2 where that launcher did,
naming `crw install register-mcp --owner plugin` as the repair. That includes standing down for a
user-owned record. It then execs itself as the bridge with the record's arguments and, for a
version-2 record, the recorded execution policy in its environment, as the Python launcher execed
the recorded executable. The running bridge is therefore the process Codex started: its argv
ends in `codex-thread-bridge` and its environment carries the policy, which is what
[the `/proc` reading below](#updating-safely) looks for. `crw hook --plugin-launch` reads only
`<CODEX_HOME>/crw-completion-hook.json` and stands down in silence unless those settings name the
plugin as owner, so a host that holds both Stop registrations under user-owned settings evaluates
each Stop once. Under settings the plugin owns, a hook-file entry that runs `crw hook` with no
argument evaluates it too, because it reads no owner. The legacy entry forms - a settings path
after `crw hook`, the `crw-completion-hook` name and `CRW_COMPLETION_HOOK_CONFIG` - are retired
(decision 66): `crw hook` given any argument but `--plugin-launch` releases the Stop in silence.
The only guard is that `crw install hook --owner plugin` (and runtime_install.py, until todo 44
removed it) refuses to register a second owner, so the state takes a hand edit
([decision 26](port/decisions.md)).

The hook command does not `exec` and ends in `; exit 0`. When the pointer names nothing, as
mid-rollback or with `HOME` unset, the shell reports the missing program on stderr and the
command still exits 0, so the turn is released rather than held. Exit 2 is the host's blocking
code, and no status `crw hook` returns reaches the host. Neither command lives in the version
cache that is replaced on install. The hook's own file is the runtime, which sits under the
pointer. The server's launcher is in the cache, but `exec` replaces it with the runtime at
start, so after that the running bridge holds nothing in the cache.

The launcher is different on purpose. A server that cannot start should say why, so it `exec`s
the runtime, and a missing pointer shows up as a nonzero exit with the missing path on stderr.
The declaration keeps `required: false`, so the session continues regardless.

Known limits of this wiring:

- `XDG_DATA_HOME` is not honoured. Both commands name `$HOME/.local/share`, as the Python
  installer's default always has, and `crw install`'s one destination is
  `$HOME/.local/share/crw-runtime`.
- The record's `bridgeExecutable` is checked (present and absolute) and not executed: the
  runtime behind the pointer is the bridge.
- Nothing checks that the runtime behind the pointer can serve this payload. Both commands pass
  `--plugin-launch`, which only a runtime built with decision 26 reads, and `crw install rollback`
  points only at a Go runtime ([rolling back](runtime-install.md#rolling-back)); nothing in the
  package, `crw install` or `crw doctor` compares the two. If the pointer names a Python `env-*`
  runtime, there is no `bin/crw`: the shell reports the missing program on stderr and the hook
  exits 0 with no record. If the pointer names a Go runtime built before decision 26, the hook
  releases the Stop with no output at all. That `crw hook` takes `--plugin-launch` for a
  settings path relative to its working directory, finds none, exits 0 and writes no journal
  row. In both cases the bridge exits 2 from argument parsing, so the server does not start and
  no bridge runs without the recorded policy. That bridge answer tells the three runtimes apart
  without side effects (decision 26), but nothing runs it yet. The only guard is the order in
  step 1 of [turning the wired surfaces on](#turning-the-wired-surfaces-on). To confirm it, end a
  turn and read its journal row
  ([registration is not firing](runtime-install.md#registration-is-not-firing)).

`wiring/crw_stop_hook.py` and `wiring/crw_bridge_mcp.py`, the Python launchers the pre-native
declarations started, no longer ship: todo 43 retired them from the package after the cutover
commit, and the payload that dropped them names new bytes, so it carries a new version suffix. A
pre-native command names the version directory it was loaded from. A turn whose Stop command was
fixed before the native wiring therefore names a directory the install that brought the native
wiring already removed, whatever later payloads hold, and it falls back to
`<CODEX_HOME>/crw-stop-hook.py` ([the bootstrap a cached turn may still run](#the-bootstrap-a-cached-turn-may-still-run)).
A session that loaded the older server declaration and has its bridge started again from a
version directory without the launcher gets no bridge: `python3` finds no script, and the
declaration's `required: false` lets the session go on without the bridge tools until it loads the
native declaration. Whether the host ever starts an old declaration from a newer directory was not
measured. The repository kept both launchers, byte for byte, beside the pre-native declarations in
`internal/pluginwiring/testdata/pre-native-wiring` until todo 44 deleted them with the Python
implementation; the declarations stay there, and the Go record contract is compared with
goldens that began as the launcher's answers. Neither the package nor `crw install` ships them.

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

On a plugin host the package's declaration is the only thing gating `create_thread` and
`send_message_to_thread`: the `config.toml` table that gated them was handed over when the host
moved to the plugin install. Check the package you are about to install, not the one already there,
from a checkout:

```sh
go run -tags dev ./cmd/crw-dev ci plugin --payload <candidate> --json   # exits 1 if it drops or weakens a required gate
codex plugin add crw@crw
go run -tags dev ./cmd/crw-dev ci plugin --payload "$CODEX_HOME/plugins/cache/crw/crw/<version>" --json
```

Both reports carry the payload `digest`. Equal digests are what tie the first verdict to the bytes
that landed; a mismatch says the package changed between the check and the add. The same two steps
apply to `codex plugin update`, which is otherwise checked by nothing at the moment it replaces the
declaration. The check refuses a package that drops or weakens a gate on its list of required gates
(`requiredToolApprovals` in `internal/dev/ci/plugin.go`), so a gate added to the declaration and not
to that list is not protected. This is a gate you run: nothing in this repository invokes
`codex plugin add`, `update` or `remove`. See [approval policy](plugin-transition.md#approval-policy).

## Turning the wired surfaces on

Installing the package installs the skills, and registers nothing else that works on
its own. The declared MCP server and Stop hook both reach a runtime this package does
not carry, and each needs a step the installation cannot take for you.

1. Install the Go runtime, if this host has none, where both declared commands look for it: run
   `crw install install --from <crw_<version>_<os>_<arch>.tar.gz> --socket <app-server-socket>`
   (or `--release <tag>` in place of `--from`) with the `crw` from that release; without
   `--socket` the relay half of the install's exercise has no App Server to reach, and nothing is
   promoted. It installs under `~/.local/share/crw-runtime`, its only destination (it has no
   `--dest`). A runtime the Python installer (runtime_install.py, removed in todo 44) placed under
   another `--dest`,
   or a pointer that still names a Python `env-*` runtime, is one neither command starts: the hook
   then releases every Stop without a word, and the server exits as it starts. `crw install`
   refuses a host record whose pointer names such another link, naming the repair, and
   `crw install status` reports it as `destinationAgrees`.

   The runtime also has to be a build that reads `--plugin-launch`
   ([decision 26](port/decisions.md)), and the pointer has to name it before the plugin cache
   takes a payload that declares the native wiring (one whose `wiring/hooks/*.json` command runs
   `crw hook --plugin-launch`). Install or update the runtime first, with `crw install install`
   or `crw install update`. With a local marketplace, that means before the checkout the
   marketplace names moves onto such a payload. A Go runtime built before decision 26 fails
   silently. Its `crw hook` reads `--plugin-launch` as a relative settings path and finds no
   settings there. It exits 0 with nothing on stdout or stderr and writes no journal row, so
   every Stop is released unrecorded. Its bridge refuses the flag with exit 2. A rollback goes
   in the other order ([update and roll back](#update-and-roll-back)).
2. Write the two records the runtime reads under `--plugin-launch`. Neither registers anything
   itself: `crw install register-mcp --owner plugin` and `crw install hook --owner plugin`. Each
   refuses when the same surface is already registered the other way, because the two together
   would run two bridges, or two hooks on every Stop.

   To have the bridge check role pairs, name the host's execution policy on the first of those two
   commands with `--execution-policy <file>`. The record then names the file and its digest, the
   launcher refuses to start the bridge when that file is gone or has changed, and the bridge reads
   it through `CODEX_THREAD_BRIDGE_EXECUTION_POLICY`. See
   [the execution policy the plugin bridge runs under](runtime-install.md#the-execution-policy-the-plugin-bridge-runs-under).
   A host on the Python runtime had these records from the Python fence installer instead
   ([the Python fence installer](runtime-install.md#the-python-fence-installer), removed in todo
   44), whose `register-mcp` also probed the enabled package's cached launcher before it wrote a
   policy record. `crw install register-mcp` probes nothing: the launcher it would probe is the runtime
   itself.
3. Trust the hook. Until it is trusted nothing fires, and no command in this repository
   grants that: installing writes no trust, and a session without it runs the hook zero
   times and says so nowhere.

Step 3 is what makes an installed hook look broken while it is working exactly as
installed. Steps 1 and 2 may run in either order; step 3 is last, because it is trust in
the hook as it then stands.

None of this starts a daemon. The server is a stdio process Codex spawns per session (one per
thread on Codex Desktop 0.154.0, observed), so a record written afterwards reaches the next thread
and not one already running. Replacing the package can reach a running thread too: at one of two
measured replacements the host started bridges again from the new version directory, each under
the record as it stood at that moment, and at the other none was seen to start until a thread resumed
([what two replacements measured](#what-two-replacements-measured)). The hook runs on
a Stop and exits, and the completion hook installs in `observe` mode, which classifies and
records and never holds a turn.

The linked installation in [README](../README.md#install) still works. Both
installations read the same source: `crw-dev skills link` links the directory the manifest
declares (links created before the move went through the root `skills` link, which todo 44
retired).

A host carrying a manual install of the bridge or the hook beside the plugin runs two of
everything. [Moving a manual install to the plugin install](plugin-transition.md) records how
this repository's hosts became plugin hosts. The tool that did it is retired: its commands that
change a host refused once the payload declared the native wiring, and it has since been deleted.

## Skill names

A linked installation exposes the skills as `crw-run`, `crw-plan`, and so on. A
plugin installation namespaces them under the plugin name, so the same skills are
offered as `crw:crw-run`, `crw:crw-plan`, and so on. The documents keep the
unprefixed names and the mapping lives in
[the shared integration guide](../plugins/crw/skills/crw-plan/references/integrations.md),
so one revision reads correctly under either installation.

`crw-dev ci plugin` derives the namespaced names from the revision rather than
from a list in prose:

```sh
go run -tags dev ./cmd/crw-dev ci plugin --json
```

Whether a client resolves a `$`-prefixed invocation token for a plugin skill, and
whether the per-skill `agents/openai.yaml` interface metadata is read under a
plugin installation, were not measured for this change.

## Adding a skill

Create the directory under `plugins/crw/skills/` with its `SKILL.md` and
`agents/openai.yaml`. The manifest lists no skills: the package ships whatever the
declared path holds at the release revision, and `crw-dev ci plugin` derives the
namespaced names from that revision, so a skill developed in parallel is included
once its commit is part of that revision. The installer test pins the registered
names as a positive control, so a new skill belongs in that list too. Keep relative
links between skills pointing at siblings under the same parent; the cache preserves
that layout.

Bump `version` in the manifest when the change should reach installations, run
`crw-dev ci plugin --record-version` so the suffix names the new payload, and
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
| Native Stop command | No, it names the runtime pointer under `$HOME` | Nothing | `crw install install` |
| Stop launcher, first candidate (legacy bootstrap a cached turn may still hold) | Yes | Falls through to the second candidate | No one: no payload ships it since todo 43, and a turn holding the bootstrap names a pre-native version directory an earlier install removed |
| Stop launcher, second candidate at `<CODEX_HOME>/crw-stop-hook.py` | No | Nothing | Placed by the Python fence installer; `crw install` leaves it as it is, and the operator removes it by hand once no turn that could still run the Python bootstrap is running ([retention](port/cutover.md#retention)) |
| Stop settings at `<CODEX_HOME>/crw-completion-hook.json` | No | Nothing | `crw install hook --owner plugin`; the document names the runtime through the pointer, and no promotion or rollback rewrites it |
| Adapter, relay and bridge executables | No, they sit under the installer pointer | Nothing | `crw install install` |
| Hook document path in the run identifier | Yes | Held as an identifier and never re-read | The host |
| MCP start `cwd` and `args` | Yes | At one measured replacement the host started bridges again from the new version directory; at the other none was seen to start: the bridges already running kept running from the removed directory, and a thread that resumed was given one from the new directory. Either way a bridge started from the new directory runs under the bridge record as it stands then. Both measured on the host; what decides between the two is not | The host |
| Skill reads | Yes | A turn already running keeps the removed directory as its skills root until it ends, and the thread's next turn is given the new one: measured on the host. A read against the removed directory was not observed | The host |

Two of those this package can answer for and two it cannot, and the difference is a host rule
rather than a preference. A hook command goes through a shell, so it can be written to resolve
its own program at run time. An MCP `command` must be a bare executable name or a contained
`./` path, and its `cwd` must be a contained `./` path, `${PLUGIN_ROOT}` or `${PLUGIN_DATA}`;
skills are read by the host from the directory the manifest names. Neither can be pointed
outside the version cache by anything this package declares.

### What two replacements measured

The two rows the host owns, and how the Stop hook came through, were measured on the user's host,
whose App Server was codex 0.154.0, at two package replacements: from `0.4.0` to
`0.4.0+1ed13de2edbb` on 2026-09-23, with `codex plugin add` at 11:17:06Z, and from
`0.4.0+1ed13de2edbb` to `0.4.0+e9f724fde562` on 2026-09-24, with the add at 00:52:09Z. They were
read from the App Server's child processes, the Stop journal and the context each session recorded.
The evidence is kept with the task record, outside this repository.

The two replacements treated running bridges differently. At the first, the Stop hook's declaration
had changed and had to be trusted again, and about a minute after the add, before the new trust was
found in place, the App Server started twelve new bridge processes within three seconds. Each ran
from the new version directory with the six variables the App Server gives a plugin server, no
bridge was left running from the old directory, and the App Server itself was not restarted. At the
second, the declaration was byte-for-byte unchanged, no approval was given, and no bridge was seen
to start at the add: right after it all ten crw bridges were still running from the removed
directory, nine of them still were half an hour later (the tenth had exited), and a thread that
resumed twenty minutes after the add was given a bridge from the new directory. So the MCP reference
can outlive its directory, as at the second replacement, or the host can resolve the declaration
again against the new one, as at the first. The one restart seen came before a changed declaration's
new trust was found in place, and at the replacement that needed no new trust none was seen to
start. The approval itself was not timed, only found done about ten seconds after the restart, and
two replacements that differ in more than this do not establish a cause: what restarts the bridges
is not measured.

A bridge started from the new directory runs under whatever the bridge record says at that moment,
and the record lives in the Codex home, outside the cache. At the first replacement it was still
version 1: the old package was enabled and cached and its launcher predates version 2, so
`register-mcp` could only write the policy once the new package was in place, and it did so about
thirty seconds after the restart. The twelve restarted bridges checked no role pair, and ten of them
were still running that way more than an hour and a half later. Every bridge started after the
record was written whose state could be read carried the policy, one of them started when a thread
was resumed; one short-lived process exited before it could be classified. At the second replacement
the record was already version 2. The bridges left running from the removed directory kept the
policy each had started with: eight carried the recorded one, one carried none, a leftover of the
first replacement, and one carried a policy file registered before the current one. The bridge the
resumed thread got from the new directory carried the recorded policy, and a `get_capabilities` call
from that thread reported its digest.

What cannot repeat while the record is version 2, and the new launcher reads it the way the current
one does, is the first replacement's bridges starting without the policy; whether the host restarts
bridges at all is the unmeasured part. The launcher finds the Codex home six directories above its
own file and reads `crw-bridge-mcp.json` there. The record names the owner, the bridge executable,
its arguments and the policy file with its digest; no field ties it to a version directory or a
payload, and the executable the documented command registers is the installer's pointer, outside the
cache. So a launcher in any version directory under the same Codex home reads the same record. That
was measured in an isolated Codex home with codex-cli 0.154.0: one package installed and a version-2
record written, then a different payload installed in its place, which removed the first directory.
The new directory's launcher, started with its declared command, arguments and working directory and
the App Server's environment, handed the bridge the recorded policy, `get_capabilities` reported its
digest, and a `register-mcp` rerun answered `record_unchanged`. The two payloads differed in a skill
and shipped the same launcher bytes, so a launcher with other bytes is outside what this showed.
The native wiring's launcher carries no record logic of its own, because the runtime behind the
pointer reads the record ([the native wiring](#the-native-wiring)), and
[updating safely](#updating-safely) still looks at every candidate before it is added. Three states failed
closed rather than open: no record, a policy file that no longer matched its digest, and a rollback
to a package whose launcher predates version 2 each made the launcher exit 2 and start no bridge.
Only a version-1 record started a bridge that checks no role, which is what the host had at the
first replacement. No App Server ran in that home, so it shows what a bridge started from a new
directory reads, not whether or when the host starts one.

The Stop hook came through the second replacement on both counts. A turn begun before it ended
thirteen seconds after the replacement finished, when the directory its Stop command named was gone,
and recorded its Stop once; the journal does not name the candidate that answered, but the packaged
one no longer existed. And with no approval given, a new ordinary task started after the add
recorded its Stop, which a hook without trust does not do. Subagent turns recorded no Stop at all on
this host, before the replacement or after it, so they are no signal either way.

Skills follow the turn. At the first replacement three threads were in a turn when the package was
replaced, one of them a turn begun hours earlier, and each compacted afterwards with the removed directory still the root it
had been given. Three threads whose next turn began after the replacement were given the new
directory when that turn started, one of them on a resume. A compaction summary can still mention
the old directory as history, so the row speaks of the root a turn is given, not of every mention.
None of the six tried to read the removed directory in the forty minutes that followed, so what such
a read does was not observed.

### The turn-command cache

Measured on codex-cli 0.154.0 on 2026-09-30, in an isolated Codex home against a local stub model
provider, with `codex app-server` driven over stdio. A probe plugin's Stop hook wrote which
declaration ran: its own command text, the `${PLUGIN_ROOT}` it was given and whether that directory
still existed. `codex plugin add` then replaced it with a version whose command text and version
differed, which removed the old version directory. Hook trust came from the per-thread config
override `bypass_hook_trust` on `thread/start` and `thread/resume`; `--dangerously-bypass-hook-trust`
on the App Server's command line left the hook untrusted. The evidence is kept with the task record,
outside this repository.

| Turn | Declaration its Stop ran |
| --- | --- |
| Started before the replacement, ended after it | The old one. `PLUGIN_ROOT` named the removed directory: the command's shell still ran, and what it named inside that directory was missing |
| The next turn of the same thread, still loaded in the same App Server | The new one |
| The thread unsubscribed and resumed in that process, or resumed in a new App Server process | The new one |
| A new thread, in that process or a new one | The new one |

So the turn-command cache lives for one turn. A hook command is resolved from the version installed
when a turn starts and held until that turn ends; a thread staying loaded, an idle interval and the
App Server's own lifetime do not extend it. A thread can still run a hook command from a replaced
version only while a turn that started before the replacement is running. Read through another App
Server process, such a turn is `interrupted` with no `completedAt`, as is a turn whose process died,
while a user's interrupt records a `completedAt`. Row 7 of the retention scan applied this rule
until `crw doctor retention-scan` was retired ([retention](port/cutover.md#retention-scan-surface)).

### Why the hook resolves the pointer

A hook command is fixed when a turn starts, with the plugin root already resolved into it, and
the whole turn reuses that string, including every Stop re-fire. Replace the package while a turn
still holds that command and anything it names inside the version cache is gone until that turn
ends; the task's next turn resolves the declaration afresh
([the turn-command cache](#the-turn-command-cache)). So the native command names nothing in
the cache. `"$HOME/.local/share/crw-runtime/current/bin/crw" hook --plugin-launch; exit 0` resolves
the runtime through the pointer when the Stop fires, and a cache replacement, before a turn or
during one, changes nothing it reaches. When the pointer names nothing the shell reports the
missing program and `; exit 0` still releases the turn, because 2 is the blocking code
([the native wiring](#the-native-wiring)).

Changing the command text changes the hook’s `trusted_hash`, so an update that changes it needs
one re-trust per installed hook identity, and Codex asks for it; until it is given nothing fires.
An update of the runtime changes no declaration, because the command names the pointer rather than
a version, so it needs none. The move to the native wiring changed the command text once, and so
needed the hook trusted again once. Trust is keyed to the declaration content and not to
the version path, so an update that leaves the command alone keeps its trust. Both halves were
seen on the host. After the first measured replacement, whose declaration had changed, the stored
value stayed as it was until the new declaration was trusted; after the second, whose declaration
was byte-identical, no approval was given and the hook still fired in a task started
afterwards. The stored value reads the same in both cases until someone approves, so it cannot tell
them apart; [updating safely](#updating-safely) compares the declarations instead.

### The bootstrap a cached turn may still run

This subsection is history from before the cutover. It describes the Python bootstrap the package
declared before [the native wiring](#the-native-wiring), which a turn that started then could still
run. The launchers it opens left the package in todo 43, after the cutover commit, and the
repository in todo 44; the last copy, `<CODEX_HOME>/crw-stop-hook.py`, is the operator's to remove
once no turn can still hold such a command ([retention](port/cutover.md#retention)). It stays as
the record of why that bootstrap named two candidates, for a host that may still hold such a turn.

That bootstrap named a file inside the version cache. Replace the package while a task still holds
it and it names a file that no longer exists. `python3` exits **2** for a
missing script, and 2 is the hook protocol’s blocking code, so the host feeds the error back
to the model and fires Stop again. Measured on the user's host: one removed directory, eleven
repeated Stop prompts in a single turn of one task and eight in a single turn of a second, and
neither turn able to finish while that path was absent. Both completed once a compatibility path
was restored there, at 06:35:13Z and 06:35:59Z. An isolated reproduction of the same manoeuvre
produced thirty-seven in one turn.

So the declaration names two candidates and opens the first one it can read:

1. `${PLUGIN_ROOT}/wiring/crw_stop_hook.py` — the packaged copy in the version directory the turn
   was loaded from, always that version's own, so a fallback left by an older install could never
   outrank it. A pre-native directory lasts only until its marketplace's package is next
   installed, and no payload ships this copy since todo 43, so once a host has installed a native
   payload from each marketplace it uses, this candidate is always gone.
2. `<CODEX_HOME>/crw-stop-hook.py` — the copy the Python fence installer placed, which
   `crw install` leaves as it is. Reached only when the first one is already gone. The operator
   removes it once no turn can still hold the bootstrap; a turn that still
   held the bootstrap after that would have neither candidate, the case the next paragraph
   describes.

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

### The supported range

| Task holding the old package reference | Stop | MCP bridge | Skill reads |
| --- | --- | --- | --- |
| Existing task, including an idle interval between turns | The native command names no cache path, so a replacement does not reach it. A turn still holding the Python bootstrap falls back to the launcher copy if one was placed. The next turn is given the declaration installed then, loaded or resumed: measured in an isolated home ([the turn-command cache](#the-turn-command-cache)) | At the first measured replacement, started again by the host from the new directory under the record as it stood then; at the second, left running from the removed directory, while a thread that resumed was given one from the new directory. Both measured on the host | The next turn is given the new root: measured |
| Existing task during a turn, removed cache | The same for the native command. For the Python bootstrap, fallback invocation measured after removal; on the host, a turn begun before the second replacement recorded its Stop once after it | As above; which running bridge belonged to a thread in mid-turn was not mapped. What a tool call in flight across a restart sees was not measured | The turn keeps the removed root until it ends: measured. A read against it was not observed |
| Fresh task created after update | Invocation measured from the updated installation | Verify the new task's actual MCP call | Verify the new task's actual skill read |

The native Stop command needs no protection from a replacement, and the fallback protects the
Python bootstrap a cached turn may still hold. That turn is the only holder: the Stop command, like
the skills root, is renewed when the next turn starts, whether the thread stayed loaded or was
resumed ([the turn-command cache](#the-turn-command-cache)). The bridge was measured to behave
differently. It is either started again by the host at the replacement, under whatever the record
says then, or left running from the removed directory with the policy it started under while a
thread loaded again gets one from the new directory; both happened on the host.

### Updating safely

This is the order for an update that changes the payload, which is any update that moves the
version directory. It assumes the plugin owns the bridge and the Stop hook, as
[turning the wired surfaces on](#turning-the-wired-surfaces-on) sets them up, and it rests on
[what two replacements measured](#what-two-replacements-measured). Steps 1 to 7 are written for a
host whose bridge record is already version 2. A host whose record is version 1, or absent, is
making its first policy registration as well, and step 8 says what that changes.

1. Prefer a moment when no turn is running. A turn in progress keeps the removed skills root until
   it ends and may have its bridge started again underneath it, and what a tool call in flight
   across such a restart sees was not measured. Nothing here lists running turns for you.
2. Read the version-2 record without changing it: run `register-mcp` with the arguments it was
   written with and `--dry-run`.

   ```sh
   crw install register-mcp --owner plugin --execution-policy <file> --dry-run
   ```

   `record_unchanged` means the record still names that policy and the file still hashes to the
   recorded digest; nothing is written. Every bridge the host starts from the new directory, at the
   add or when a thread resumes, reads this record at that moment, so settle any other answer
   first: a policy file edited since it was registered, for one, would make every such bridge refuse
   to start. If the record is version 1 or absent, read step 8 now, because it decides what happens
   around step 5.
3. Read the Stop settings the same way: `crw install hook --owner plugin --dry-run`, with any option
   they were written with, such as `--socket`, answers `config_unchanged`. Nothing about the Stop
   hook moves with the payload: the native command names the pointer and the settings live in the
   Codex home. A turn that still holds the Python bootstrap falls back to `<CODEX_HOME>/crw-stop-hook.py`
   once its version directory is gone, and that copy stays where the Python installer put it until
   the operator removes it, once no turn can still hold the bootstrap ([retention](port/cutover.md#retention)).
4. Look at the candidate before adding it, whatever its bytes. Install it into a throwaway Codex home:

   ```sh
   CODEX_HOME=<throwaway> codex plugin marketplace add <source>
   CODEX_HOME=<throwaway> codex plugin add crw@<marketplace>
   go run -tags dev ./cmd/crw-dev ci plugin --payload <candidate-dir> --json
   ```

   `<candidate-dir>` is the one version directory the add reports under
   `<throwaway>/plugins/cache/<marketplace>/crw/`. Proceed only when the add reports that one
   directory, the check passes, and the candidate declares the native wiring: its MCP declaration
   starts `codex-thread-bridge` through `sh ./wiring/crw-bridge.sh`, and its Stop declaration, in a
   file its manifest names under `hooks`, runs `crw hook --plugin-launch`. Such a candidate carries
   no bridge or hook logic of its own: both start the runtime the pointer names, which reads the
   record itself, so what it starts under is decided by the runtime and the record rather than by
   the payload's bytes. A candidate that still declares the Python launchers is a rollback, and it
   goes in the order [update and roll back](#update-and-roll-back) gives.

   The same directory tells whether this update will need the hook trusted again. Trust belongs to
   the hook declaration rather than to the version, so compare the declaration files the two
   manifests name under `hooks`, and only those: a hook file the manifest does not name never loads
   ([how hooks and MCP servers load](#how-hooks-and-mcp-servers-load)). The package check's report
   reads them as the host does, from the checkout: `stopHooks` counts the hooks those files list
   under `hooks.Stop`, and `hooksDigest` covers which files the manifest names, in its order, with
   their modes and bytes. `<installed-dir>` is the one installed now,
   `$CODEX_HOME/plugins/cache/<marketplace>/crw/<version>`:

   ```sh
   candidate=<candidate-dir> installed=<installed-dir>
   # The package check's report on a payload; empty when the check refuses the payload.
   report() { go run -tags dev ./cmd/crw-dev ci plugin --payload "$1" --json; }
   # One top-level field of such a report, which prints each on its own line, two spaces in.
   field() { printf '%s\n' "$1" | sed -nE "s/^  \"$2\": (.*[^,]),?\$/\1/p"; }
   new=$(report "$candidate") old=$(report "$installed")
   case $(field "$new" stopHooks) in
       '' | 0) echo "no Stop hook declared" ;;
       *) if [ "$(field "$new" hooksDigest)" = "$(field "$old" hooksDigest)" ]; then
              echo unchanged
          else
              echo changed
          fi ;;
   esac
   ```

   `no Stop hook declared` means the files the candidate's manifest names under `hooks` list no
   hook under `hooks.Stop`, so after the add the completion hook would stop firing; that is not an
   update this procedure covers, so do not add it. The package check does not refuse every such
   candidate: it passes one whose manifest has no `hooks`, one whose Stop entry sits in a file the
   manifest does not name, and one whose Stop list sits in a declared file outside its `hooks`
   object, and the host runs none of those entries.
   `unchanged` means both manifests name the same files in the same order and each holds the same
   bytes under the same mode, so the stored trust carries over, as it did at the second measured
   replacement.
   `changed` means expect to trust the hook again in Codex after step 5, as at the first; that was
   measured for a change to the command text, what a change elsewhere in the file does was not, and
   step 6's firing check settles it either way. The check prints no report for a payload it refuses,
   so a refused candidate reads as declaring no Stop, the side that stops the add, and a refused
   installed directory reads as `changed`.
   The trust entry also names the marketplace, so adding the package from a marketplace of another
   name is inferred to need a trust of its own whatever this prints. The trust entry in the Codex
   configuration cannot decide any of this: its value stays the same after the add in both cases
   until someone approves.
5. On a host that gates the bridge,
   [check the candidate's declaration](#before-you-add-or-update-on-a-host-that-gates-the-bridge),
   passing the version directory step 4's throwaway add reported as `--payload`.
   Then run `codex plugin add crw@<marketplace>`, and trust the hook again in Codex if step 4
   found its declaration changed. At the first measured replacement the host started bridges again
   before that trust was found in place; at the second, which needed none, none was seen to start.
6. Read back what landed:
   - the installed payload: `go run -tags dev ./cmd/crw-dev ci plugin --payload <dir> --json` on
     the version directory the add installed checks it against the package rules and reports a
     `digest` that should equal the one the same command reports for the throwaway directory from
     step 4;
   - the step 2 dry run again, which should still answer `record_unchanged`;
   - `get_capabilities` in a fresh task and in the tasks that were loaded during the add: its
     `executionPolicy.digest` is the digest the record names, and a bridge with no policy reports
     `presence_only`;
   - whether the hook fires: end a turn in an ordinary task started after the add and find that
     session and turn in the Stop journal
     ([registration is not firing](runtime-install.md#registration-is-not-firing)). A subagent's
     turn is no signal: on the measured host subagent turns recorded no Stop at all;
   - every running crw bridge, from `/proc`, the way both measured replacements were read. Run it as
     the user the App Server runs as; a bridge process whose directory or environment you may not
     read is listed as `UNREADABLE` rather than skipped. This is an operator reading, not a command
     this repository ships:

     ```sh
     home="${CODEX_HOME:-$HOME/.codex}"
     cache="$(cd "$home/plugins/cache" && pwd -P)"
     want="$(sed -n 's/.*"digest": *"\([0-9a-f]\{64\}\)".*/\1/p' "$home/crw-bridge-mcp.json")"
     for dir in /proc/[0-9]*; do
         pid="${dir#/proc/}"
         tr '\0' '\n' < "$dir/cmdline" 2>/dev/null | grep -q 'codex-thread-bridge$' || continue
         if ! cwd="$(readlink "$dir/cwd")" || ! env="$(tr '\0' '\n' < "$dir/environ")"; then
             echo "$pid UNREADABLE"; continue
         fi
         case "$cwd" in "$cache"/*) ;; *) continue ;; esac
         rel="${cwd#"$cache"/}"; marketplace="${rel%%/*}"; rest="${rel#*/}"
         [ "${rest%%/*}" = crw ] || continue
         have="$(printf '%s\n' "$env" | sed -n 's/^CODEX_THREAD_BRIDGE_EXECUTION_POLICY_DIGEST=//p')"
         if [ -z "$have" ]; then state="NO POLICY"
         elif [ "$have" = "$want" ]; then state=policy
         else state="OTHER POLICY"; fi
         echo "$pid $marketplace/${rest#*/} $state"
     done
     ```

     Each line is a bridge process, the marketplace and version directory it runs from, and whether
     it carries the recorded policy. A bridge still running from a removed directory shows
     `(deleted)` after the version, as all ten did right after the second measured replacement.

   Preserve existing recovery evidence and compatibility paths until their readers are gone.
7. Tasks that were loaded before the replacement:
   - A turn that was running keeps the removed skills root it was given until it ends, and that
     directory is gone; what a later read against it does was not observed. The thread's next turn
     is given the new root.
   - Its bridge was either started again at the replacement or left running from the removed
     directory while a thread that resumed was given a new one, and what decides between the two was
     not measured. A bridge left running keeps the
     policy it started with. A bridge started from the new directory, at the replacement or when the
     thread resumes, runs under the record as it is at that moment: with a version-2 record that is
     the policy, measured on the host at the second replacement and in isolation; with a version-1
     record it is no policy, as the first replacement showed; with no record there is no bridge, as
     the isolated run showed. A bridge that step 6 reports without the recorded policy stays that way
     until the host starts that thread's bridge again. A resume did that at both measured
     replacements; nothing in this repository can.
   - A Stop whose command is the native one reaches the runtime whatever the cache holds; one whose
     command is the Python bootstrap naming the removed directory falls back to the launcher copy.
8. The first policy registration. With no record at all, `crw install register-mcp --owner plugin
   --execution-policy <file>` writes a version-2 record. It probes no cached package, so choose its
   moment yourself: a cached package that declares the native wiring starts the runtime, which
   reads a version-2 record, and one that still ships a Python launcher older than version 2 refuses
   the record and starts no bridge (measured in isolation) until the new package is added. With such
   a package cached, register right after step 5; otherwise register before it. A version-1 record is
   not given a policy in place: `register-mcp --execution-policy` answers `record_differs` and names
   the repair, which is to move the record aside by hand and register again. The choice is then when
   the version-1 record leaves:
   - After step 5, as at the first measured replacement: add the package, move the version-1
     record aside, register, read back as in step 6, then have the host start again the bridges it
     reports without the policy. Every bridge the host restarts before the record moves checks no
     role.
   - Before step 5: move the version-1 record aside, add the package, then register and read back
     the same way. A launcher that finds no record starts no bridge (measured in isolation), so a
     thread whose bridge restarts in between has no bridge tools rather than unchecked ones, until
     the host starts its bridge again after the registration. What the host does after a bridge
     refuses to start was not measured.
   - With the old package disabled or removed first: move the record aside, register, then add the
     new package and read back. What disabling does to loaded threads, their skills, Stop hook and
     bridge included, was not measured.

`codex plugin add` removes the prior version cache. This procedure does not promise to retain
that directory automatically or prescribe copying it back after a gap as uninterrupted support.
If path continuity cannot be established before replacement, use the turn boundary in step 1; do
not proceed on an assumption that the old cache will remain.
`crw install status` reports what a replacement actually left: what the host record selects, what
the owned pointer names and whether they agree, and every runtime directory with its claim. It reads the pointer and the host records, not the tasks holding references, so
it cannot establish that the last reader is gone; that needs evidence of its own.

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
piece: the host may start its bridge again or leave it running from the removed directory, a thread
that resumes gets one from the new directory, a turn in progress keeps the skills directory it was
given until it ends, and a Stop whose command names the removed directory falls back.
[The cache lifetime](#the-cache-lifetime) says which of those were measured.

Because the suffix follows the payload, an earlier revision reinstalls into its own directory
rather than over the one that replaced it, and `codex plugin list` says which of the two is
present.

An update that brings in the native wiring, or a rollback that takes it out, moves the package
and the runtime in a fixed order. Going forward, the runtime moves first. The pointer has to
name a runtime that reads `--plugin-launch` before the cache takes a payload whose commands
pass it. Going back, the package moves first. Reinstall a revision whose Stop declaration is the
Python bootstrap before `crw install rollback` points at a Go runtime built before decision 26,
which does not read that flag; no rollback rewrites the one Stop settings document
([one Stop settings document](runtime-install.md#one-stop-settings-document)). In the other order,
every Stop in between is released without a record ([the native wiring](#the-native-wiring)), and
nothing checks it. `crw install rollback` no longer points at a Python `env-*` runtime at all
(decision 61).

A rollback past the bridge record's version installs, and then its launcher refuses the record. A
launcher that predates version 2 starts no bridge under a version-2 record: measured in isolation,
where the older package's `codex plugin add` succeeded and every start of its bridge exited 2. So
a bridge started after such a rollback, at the add or when a thread resumes, is refused, and that
thread has no bridge tools. A version-1 record in place of the version-2 one lets bridges started
after it run, and they check no role; whether the host starts a refused bridge again was not
measured. A bridge already running may keep going from the removed directory, as all of them did at
the second measured replacement, with the policy it started under.

Removing the plugin deletes the cached version directory and the plugin entry in
`config.toml`. It leaves the marketplace registration, so removing that is a
separate step, and it does not touch the relay store, the bridge ledger, the hook
journal or the runtime installation.

## Verify

Structural checks run offline, from a checkout, and are part of CI:

```sh
go run -tags dev ./cmd/crw-dev ci plugin     # package shape, hygiene, release digest
go run -tags dev ./cmd/crw-dev ci validate   # skill metadata, local links, Python syntax
go test ./internal/runtime/install/...       # the wiring through the pointer, among the installer's tests
```

`scripts/ci/plugin.py` and `scripts/ci/validate.py`, the checks' Python twins, answer the same
with the same flags and output until todo 48 removes them; they are developer tools
([CI operation](CI.md)). The Python installer's wiring tests left with it in todo 44.

`crw-dev ci plugin` builds the release payload from a Git revision rather than from the
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

`crw-dev ci plugin` checks it, and it is the only signal a mistake here ever produces. Measured:
an invalid `approval_mode` in a plugin declaration makes `codex plugin add` exit 0 and
`codex mcp list` exit 0 with **zero** entries. The server disappears and no `disabled_reason` is
recorded, because there is no entry left to carry one. The same mistake in a user configuration is
a loud error naming the valid set.

So the check fails closed: the accepted values are `auto`, `prompt`, `writes` and `approve`,
measured from the host's own rejection text; `approval_mode` is the only key allowed inside a tool
gate; an empty `tools` object is refused because nothing measured says what a host does with one;
and `codex-thread-bridge` must keep gating `create_thread` and `send_message_to_thread` with
`approve`, because that gate is what the user configuration handed over when the transition removed
its table. See [approval policy](plugin-transition.md#approval-policy).
