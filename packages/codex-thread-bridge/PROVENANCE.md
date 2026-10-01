# codex-thread-bridge: provenance

The bridge `crw` runs as `codex-thread-bridge` began as an imported Python package. Todo 44
removed that package's source; this file and the [MIT notice](LICENSE) beside it are what
stay, because the notice is still owed for the port and ships in every release archive
(`.goreleaser.yaml`, `internal/runtime/definition`).

## Upstream

| | |
| --- | --- |
| Repository | <https://github.com/saidelike/codex-thread-bridge> |
| Revision | `bb684f35b4919a82b09d2290dc26623716803d62` |
| Licence | MIT, "Copyright (c) 2026 codex-thread-bridge contributors" ([LICENSE](LICENSE), kept byte for byte) |

The import brought source, not history, so the upstream tree hash is not derivable here and
was never recorded.

## The import

Commit `9bbb3cb4` ("Import the bridge and relay packages and require a packages check",
2026-09-16), merged by pull request #4 (merge `97860a6a`), imported from that revision:
`LICENSE`, `README.md`, `CONTRIBUTING.md`, `pyproject.toml`, `.gitignore`, `scripts/`, `src/` and
`tests/`, each byte-identical to the source revision. Two files were deliberately not imported:
the bridge's `uv.lock`, superseded by the workspace lock at the repository root, where a member
lock has no effect; and its `.github/workflows/ci.yml`, which would have been an inert nested
workflow and whose steps informed this repository's `packages` job instead. Git history, virtual
environments, build output, SQLite databases and their write-ahead logs, operational state
directories, logs and private task records were not imported.

The same pull request imported the session relay from its originating checkout at
`d3394038c108022dc0ff48d48ec878094c67e6df` (no remote exists for that checkout, so its own
revision is the provenance anchor): `README.md`, `pyproject.toml`, `.gitignore`, `docs/`, `src/`
and `tests/`. The relay is this repository's own work under the root [MIT license](../../LICENSE)
and carries no separate notice. Its documents are [docs/relay](../../docs/relay/README.md).

Every imported file was byte-identical to its source revision except five, each changed for a
stated reason and recorded in the importing pull request:

| File | Change |
| --- | --- |
| `codex-session-relay/pyproject.toml` | the `tool.uv.sources` entry binding the bridge to this workspace |
| `codex-session-relay/tests/test_bridge_adapter.py` | a maintainer path replaced with a synthetic one, plus a regression test for the fix below |
| `codex-session-relay/tests/test_sync_outbox.py` | two private document URLs replaced with synthetic ones |
| `codex-session-relay/src/codex_session_relay/currency.py` | a blank line removed at end of file, which `git diff --check` reports |
| `codex-session-relay/src/codex_session_relay/bridge_adapter.py` | the transport no longer hands a caller its own suspended worker frame |

That last one was a pre-existing defect this repository's Python 3.11 job found, not something
the import caused: it reproduced on the source revision above. The transport returned failures
across a thread boundary, and a caller may clear the frames of what it catches, which is exactly
what `unittest.assertRaises` does. While the handed-over traceback still began at the worker's
own suspended coroutine frame, clearing it finalized that worker on CPython 3.11, and every later
call waited out its full timeout against a loop that no longer read its inbox. Dropping that one
frame kept the exception type, message and every frame from inside the operation.

## In this repository

After the import, 73 commits changed `packages/codex-thread-bridge` (71 of them not merges;
`git log 9bbb3cb4..origin/dev -- packages/codex-thread-bridge` before todo 44). The last one to
change its Python source was `c887e45e` ("feat(relay): fenced single-writer ownership in both
runtimes", 2026-09-29). The last revision holding that source is the parent of the todo 44 commit
that deleted it; `git log --diff-filter=D -- packages/codex-thread-bridge/src` names that commit.

The Go port is the bridge now:

| Python module | Go |
| --- | --- |
| `bridge.py` | `internal/bridge` |
| `rpc.py`, `effects.py` | `internal/bridge/appserver` (its fake App Server in `internal/bridge/appserver/fakehost`) |
| `execution.py`, `roles.py` | `internal/bridge/execution` |
| `ledger.py` | `internal/bridge/ledger` |
| `server.py` | `internal/bridge/mcp` (`crw bridge`, `codex-thread-bridge`) |
| `settings.py` | `internal/bridge/settings` |
| `worktrees.py` | `internal/bridge/worktrees` |
| `scripts/check_connection.py` | `internal/runtime/exercise` (the exercise `crw install` runs) |

Its tests carried the Python suite's properties ([test map](../../docs/port/test-map.md)), and
the Python answers they were compared with are the goldens beside them
(`testdata/golden`), which began as those answers.
