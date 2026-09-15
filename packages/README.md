# Packages

Two Python packages live here. They were developed in separate checkouts and were
imported into this repository so that one change can cross both of them and one
check can prove they still work together.

| Package | Module | CLI |
| --- | --- | --- |
| [codex-thread-bridge](codex-thread-bridge/README.md) | `codex_thread_bridge` | `codex-thread-bridge` |
| [codex-session-relay](codex-session-relay/README.md) | `codex_session_relay` | `codex-session-relay` |

Module names, CLI names, the wire protocol and the database paths and formats are
unchanged by the import. Each package keeps its own `pyproject.toml`, tests and
`.gitignore`. The uv workspace root is the repository root, so `uv.lock` is there
and the relay's optional bridge dependency resolves to `codex-thread-bridge` in this
checkout rather than to an index.

```sh
uv sync --locked --all-packages
python3 ../scripts/ci/packages.py
```

Having the source here does not install, upgrade or activate anything. An installed
bridge or relay, an MCP registration and a running service each remain separate
operations under [the repository policy](../POLICY.md); see
[CI operation](../docs/CI.md#packages) for what the check does and does not establish.

## Import provenance

| Package | Source revision | Imported paths |
| --- | --- | --- |
| codex-thread-bridge | `bb684f35b4919a82b09d2290dc26623716803d62` | `LICENSE`, `README.md`, `CONTRIBUTING.md`, `pyproject.toml`, `.gitignore`, `scripts/`, `src/`, `tests/` |
| codex-session-relay | `d3394038c108022dc0ff48d48ec878094c67e6df` | `README.md`, `pyproject.toml`, `.gitignore`, `docs/`, `src/`, `tests/` |

Every imported file is byte-identical to its source revision except for five, each
changed for a stated reason and recorded in the importing pull request:

| File | Change |
| --- | --- |
| `codex-session-relay/pyproject.toml` | the `tool.uv.sources` entry binding the bridge to this workspace |
| `codex-session-relay/tests/test_bridge_adapter.py` | a maintainer path replaced with a synthetic one, plus a regression test for the fix below |
| `codex-session-relay/tests/test_sync_outbox.py` | two private document URLs replaced with synthetic ones |
| `codex-session-relay/src/codex_session_relay/currency.py` | a blank line removed at end of file, which `git diff --check` reports |
| `codex-session-relay/src/codex_session_relay/bridge_adapter.py` | the transport no longer hands a caller its own suspended worker frame |

That last one is a pre-existing defect this repository's Python 3.11 job found, not
something the import caused: it reproduces on the source revision above. The transport
returns failures across a thread boundary, and a caller may clear the frames of what it
catches, which is exactly what `unittest.assertRaises` does. While the handed-over
traceback still began at the worker's own suspended coroutine frame, clearing it
finalized that worker on CPython 3.11, and every later call waited out its full timeout
against a loop that no longer read its inbox. Dropping that one frame keeps the
exception type, message and every frame from inside the operation.

Two bridge files were deliberately not imported. Its `uv.lock` is superseded by the
workspace lock at the repository root, where a member lock has no effect. Its
`.github/workflows/ci.yml` would have been an inert nested workflow; its steps were
folded into this repository's `packages` job instead.

Git history, virtual environments, build output, SQLite databases and their
write-ahead logs, operational state directories, logs and private task records were
not imported and must not be.

codex-thread-bridge is adapted from `saidelike/codex-thread-bridge` and keeps its own
[MIT notice](codex-thread-bridge/LICENSE). codex-session-relay carries no separate
license file: it is this repository's own work under the root [MIT license](../LICENSE).
