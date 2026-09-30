# Moving a manual install to the plugin install

This page is a historical note. `scripts/plugin_transition.py` moved a host with a manual install
of CRW to the [plugin installation](plugin-packaging.md), and it is retired: this repository's
host completed that move, and nothing in the repository runs the tool any more. Its commands that
change a host (`transition`, `disable` and `remove`) were retired first, when the plugin payload
began declaring the native wiring ([decision 26](port/decisions.md)). Its Stop hook and bridge run
the Go runtime's `crw` and `codex-thread-bridge` behind `$HOME/.local/share/crw-runtime/current`.
The steps handed the surfaces only to a Python runtime (preflight requires
`<dest>/current/bin/python3` and a cached payload equal to the checkout's). So on every host they
accepted, an applied transition removed the working Stop registration and bridge table, reported
every step settled, and left a Stop hook and a bridge that could not run there. From then until the
tool was deleted, those commands refused with exit 2 and one line on stderr, and changed nothing.
The full reference it had is this page at a revision where the tool still ran; see
[a host that still needs it](#a-host-that-still-needs-it).

## What it did

A host could carry both installations, and then two things ran for every one that should:

| Surface | Manual install | Plugin install |
| --- | --- | --- |
| Skills | `$CODEX_HOME/skills/crw-*` symlinks into a checkout | the plugin cache, offered as `crw:crw-run` and so on |
| Task bridge | `[mcp_servers.codex-thread-bridge]` in `config.toml`, plus a record naming owner `user` | `wiring/mcp.json` plus a record naming owner `plugin` |
| Completion hook | an entry in `$CODEX_HOME/hooks.json` running `<checkout>/scripts/completion_hook.py` | the package's declared Stop hook plus `crw-completion-hook.json` settings naming owner `plugin` |

`transition` removed the manual surfaces, and only the bytes it could prove ran this repository's
own code, in a fixed order: place the fallback Stop launcher `<CODEX_HOME>/crw-stop-hook.py`,
retire the settings the manual registration named, remove that registration, write the
plugin-owned settings, retire the user-owned bridge record, remove the `config.toml` table, write
the plugin-owned bridge record, and remove the CRW-owned skill links. `inspect`,
`check-declaration`, `disable`, `remove` and `swap-state` read a host, judged a candidate package,
and retired or removed the plugin records afterwards. Every command printed one JSON document and
wrote nothing without `--apply`.

## Why it was retired

The one host that had a manual install is a plugin host now: its skills come from the plugin cache,
its `hooks.json` and `config.toml` hold no CRW registration, and its settings and bridge record
name owner `plugin`. No skill, workflow, CI script or cutover document runs the tool. The Go
runtime writes only plugin-owned registrations (`crw install hook` and `register-mcp` take
`--owner plugin` alone), so it cannot create a manual install that would need the move again.

What the tool did that is still needed has another home:

| Was | Now |
| --- | --- |
| `check-declaration --package` | `crw-dev ci plugin --payload <dir> --json`, before and after `codex plugin add`; see [before you add or update](plugin-packaging.md#before-you-add-or-update-on-a-host-that-gates-the-bridge) |
| `swap-state` | `crw install status` |
| `disable` as the repair for a refused bridge record | move the record aside by hand, then register again |
| the launcher half of `remove` | `install.RemoveLauncher` in `internal/runtime/install`: it deletes `<CODEX_HOME>/crw-stop-hook.py` only while the file carries the launcher marker, proves the marker again under the launcher's own lock, and never touches the settings. The cutover calls it once the retention scan is clear |
| making the skill links | `crw-dev skills link --apply` (see [README](../README.md#install)) |

`scripts/crw_transition/inventory.py` stayed until todo 44 removed the Python installer, because
`runtime_install.py register-mcp --execution-policy` read the enabled plugin through it.

## A host that still needs it

A host this repository cannot see may still carry a manual install. Run the tool from a revision
before the native wiring, where it still runs and this page is still the full reference. The
revisions between the native wiring and the tool's deletion carry it, but their `transition`,
`disable` and `remove` refuse. `53caad67` on `dev` is one such earlier revision:

```sh
git worktree add --detach /path/to/transition 53caad67
cd /path/to/transition && python3 scripts/plugin_transition.py inspect
```

Its preflight requires `<dest>/current/bin/python3` and a cached payload equal to that revision's,
so it moves only a host still on the Python runtime and the pre-native payload.

## Approval policy

A Codex configuration can gate individual tools of an MCP server:

    [mcp_servers.codex-thread-bridge.tools.create_thread]
    approval_mode = "approve"

Measured on an isolated home: while that table exists it wins over the plugin declaration, so
installing the plugin does not drop the gate, and removing the table does. There is no overlay: a
configuration that keeps the `tools` sub-tables and drops `command` and `args` fails to load at all.
The package declares the same gate, so removing the table hands the gate over instead of dropping
it:

    "tools": { "create_thread": { "approval_mode": "approve" },
               "send_message_to_thread": { "approval_mode": "approve" } }

The modes are compared for equality and never ranked. The measured set is `auto`, `prompt`,
`writes` and `approve`, and `prompt` against `approve` was never measured. On a plugin host the
declaration is the only gate left, so `crw-dev ci plugin` refuses a package that drops or weakens a
required gate, and `runtime_install.py register-mcp` refuses to write a user table that would shadow
a server an installed package declares.
