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

Every imported file is byte-identical to its source revision except for four, each
changed for a stated reason and recorded in the importing pull request.
`codex-session-relay/pyproject.toml` gained the `tool.uv.sources` entry that binds the
bridge to this workspace. Two of its tests had a maintainer path and a private document
URL replaced with synthetic values before this source became public. And
`codex-session-relay/src/codex_session_relay/currency.py` lost a blank line at end of
file, which `git diff --check` reports and this repository asks contributors to run.

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
