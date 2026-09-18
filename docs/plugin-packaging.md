# Plugin packaging

This repository publishes its skills as a versioned Codex plugin. The package is
instructions only: the task bridge, the session relay, the Python runtime and the
completion hook keep their own installer and are not bundled here.

## What the package is

| Path | Role |
| --- | --- |
| `.agents/plugins/marketplace.json` | Marketplace entry; its `source.path` names the plugin root |
| `plugins/crw/` | The plugin root, copied into the version cache as it stands |
| `plugins/crw/.codex-plugin/plugin.json` | Manifest: plugin name, version, and the declared skills path |
| `plugins/crw/skills/` | The seven skills, the only declared component |
| `plugins/crw/LICENSE` | The repository license, shipped with the package |
| `skills` | A link to `plugins/crw/skills`, kept for installations made before the move |

Edit the skills at `plugins/crw/skills/`; the root `skills` link is a compatibility
path, not a second copy, and it is a Git symlink, so a checkout without symlink
support turns it into a plain text file. The supported platform is Linux x86_64.

Only those three entries may sit in the plugin root. Installation copies that
directory verbatim, including untracked and ignored files, so anything left there
is published. `python3 scripts/ci/plugin.py` enforces the rule.

Everything that ships also has to sit inside the declared skills path, beside the
files under `.codex-plugin/` and `LICENSE`, because a file outside it would install
without ever being validated as a skill. The manifest itself may carry only the keys
the ingestion validator knows, and optional presentation fields are checked against
its shapes: URLs that begin with `https://`, a `#RRGGBB` brand colour, and `./`
relative asset paths the package actually ships.

The manifest declares `skills` and nothing else. On this Codex version a declared
component replaces default discovery rather than adding to it, so declaring
`hooks` or `mcpServers` would change what loads; wiring those belongs to its own
change.

That replacement behavior was measured on codex-cli 0.154.0: a plugin declaring a
non-default skills directory while also holding `./skills/` loaded only the declared
one. The plugin specification bundled with Codex describes the opposite, saying
declared components supplement default discovery. This package follows the measured
behavior and keeps every shipped component under the declared path.

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

The linked installation in [README](../README.md#install) still works and is
unchanged. Both installations read the same source: `scripts/install.py` links the
directory the manifest declares, and the repository root keeps `skills` as a link
to it so links created before the move still resolve.

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
once its commit is part of that revision. The installer test pins the current seven
names as a positive control, so a new skill belongs in that list too. Keep relative
links between skills pointing at siblings under the same parent; the cache preserves
that layout.

Bump `version` in the manifest when the change should reach installations, and
install the plugin again: a cached version changes only on installation, and a task
already running keeps the package its session started with.


## Update and roll back

The cache keeps one version per plugin, and installing a new version replaces the
previous directory instead of keeping both. Rolling back therefore means making
the source offer the earlier revision again and reinstalling it, not selecting an
older copy from the cache. Bump `version` in the manifest for a release; a new
task picks up the new package when its session starts, and work already running
keeps the version it started with.

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
