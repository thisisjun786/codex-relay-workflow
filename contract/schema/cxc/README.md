# CXC v0.2.40 behaviour corpus

The CRW Go port absorbs CodexClaw (CXC) as-is (Linear CRW-189; decisions in the Wave 0 analysis).
Before any Go code for it exists, this corpus freezes what the real CXC v0.2.40 Node build does,
so each later port issue can replay the same inputs against `crw` and compare. It is the CXC
counterpart of the Python-recorded scenarios beside it in `contract/fixtures/` (CRW-279).

- Oracle: CodexClaw tag `v0.2.40` = `3c1459acadeb1906d97c00a598e1457327ae372d`, extracted with
  `git archive v0.2.40`. Its committed `dist/` is in sync with `src/` (all 192 compiled files
  recompiled in memory with the build's own `compileSource` and compared byte for byte; none
  stale, none missing), so nothing is built.
- Scope: only what the v0.2.40 manifest registers and what the CLI table reaches. Out of scope
  (decision 9): messenger-bridge, `remote`, the GUI, Windows/WSL, the nine dead modules, the six
  hook events no manifest file registers, `scan show`, `release`.
- Fixtures stay raw v0.2.40: CXC names (`codexclaw`, `cxc`, `.codexclaw/`, `CODEXCLAW_HOME`) are
  recorded as the oracle printed them. `name-substitution.json` is applied by the replayer, never
  to these files.

## Layout

| Path | What |
| --- | --- |
| `contract/schema/cxc/specs/<group>.json` | The scenarios: what to set up and what to run. Authored by hand; the only input of the recorder. |
| `contract/fixtures/cxc/<id>.json` | One recorded fixture per scenario: the spec's `given` and steps, and the oracle's normalised answer under `expect`. Written only by the recorder. |
| `contract/schema/cxc/hook-declarations.json` | Contract K1: the 32 registered hook legs (event, matcher, command, timeout, statusMessage), extracted from the oracle. |
| `contract/schema/cxc/injected-text.json` | Contract K5: the exported injected-text constants of the modules contracts.md names, extracted from the oracle. |
| `contract/schema/cxc/normalisation.json` | The normalisation rules, as data. |
| `contract/schema/cxc/name-substitution.json` | The CXC to CRW rename table, as data. |
| `contract/schema/cxc/coverage.json` | Every contract item and how the corpus holds it: recorded (with its fixtures), extracted, pending (with the exact spec to record), or not recordable (with the reason). |

## Spec grammar

```json
{"group": "pabcd-hooks", "scenarios": [{
  "id": "hook__stop-checking-pabcd-continuation__active_phase_b_blocks",
  "covers": ["K3/stop-checking-pabcd-continuation", "A/pabcd-state/hook.test.ts"],
  "note": "why this case exists",
  "given": {
    "files": {"ws/codexclaw.json": "{\"pabcd\":{\"enabled\":true}}"},
    "json": {"ws/.codexclaw/sessions/rec-s1.json": {"phase": "B"}},
    "dirs": ["ws/devlog"], "modes": {"ws/x": 384}, "symlinks": {"ws/link": "${WS}/x"},
    "sqlite": {"codex/goals_1.sqlite": ["CREATE TABLE thread_goals (...)", "INSERT ..."]},
    "env": {"CODEX_THREAD_ID": "rec-s1"},
    "stubs": {"ocx": {"stdout": "{}", "exit": 0}, "gh": null,
              "codex": {"stdout": "<listing>", "exit": 0, "cases": [{"argv": ["features", "enable"], "stderr": "no", "exit": 1}]}},
    "git": {"dir": "ws", "commit": "initial", "origin": "https://example.invalid/r.git", "worktrees": [{"path": "tmp/wt", "branch": "work"}]},
    "mtimes": {"cxc/skill-cache/jaw-aHR0cHM6Ly9yYXcuZ2l0aHVi.cache": "2025-12-31T00:00:00Z"},
    "fetch": {"https://example.invalid/catalog.json": {"status": 200, "body": "{}"}}
  },
  "steps": [
    {"hook": "stop-checking-pabcd-continuation", "stdin": {"hook_event_name": "Stop", "session_id": "rec-s1", "cwd": "${WS}"}},
    {"cli": ["orchestrate", "status", "--session", "rec-s1", "--json"], "env": {"X": "y"}, "unset": ["Y"], "cwd": "ws"},
    {"cli": ["--help"], "payload": true},
    {"node": ["plugins/codexclaw/components/recall/dist/cli.js", "chat", "index"]},
    {"mcp": [{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {}}]},
    {"write": {"ws/src/parser.ts": "export const x = 2;\n"}},
    {"hook": "pre-tool-use-guarding-memory-write", "stdin": {"hook_event_name": "PreToolUse"}, "stdin_pad": 4194400}
  ],
  "observe": ["ws", "codex/codexclaw/hook-observations"]
}]}
```

- Every path in `given` is relative to the case root and starts with one of the five roots
  `ws` (the workspace, every process's cwd and the payloads' `cwd`), `codex` (`CODEX_HOME` and
  `CODEX_SQLITE_HOME`), `cxc` (`CODEXCLAW_HOME`), `home` (`HOME`) and `tmp` (`TMPDIR`).
- Text in `files`, `json`, `symlinks`, `env`, `stdin` and argv may use the placeholders of
  `normalisation.json` (`${WS}`, `${CODEX_HOME}`, `${CXC_HOME}`, `${HOME}`, `${TMP}`,
  `${PLUGIN_ROOT}` ...); the recorder expands them to the case's absolute paths.
- `json` values are written compact with a trailing newline; `files` are written verbatim.
  Every given entry gets the frozen clock's mtime (2026-01-01T00:00:00Z).
- `sqlite` creates databases with the listed statements (through `node:sqlite`); `mtimes`
  then ages named entries (RFC 3339) for staleness cases.
- `git` makes `dir` (default `ws`) a repository; with `commit`, every file under it is committed
  once with the fixed identity and dates, so commit ids are stable; `worktrees` adds linked
  worktrees on new branches.
- `stubs`: `codex`, `ocx`, `gh`, `uv` and `sg` are always on `PATH` as fake programs that log
  `{cmd, argv, cwd}` and exit 127 unless scripted; a scripted stub prints its `stdout`/`stderr`
  and exits with `exit`, unless one of its `cases` (an argv prefix) matches first; `null`
  removes it (the program is absent). Other names add a stub (`python3`, `ps` ...).
- The network is closed in every oracle process: `fetch`, TCP connects and DNS lookups fail as
  an unreachable host would and are logged as calls (`fetch`, `connect`, `dns`); `fetch` in
  `given` scripts replies for named URLs. `git` is the real program behind a wrapper that logs
  its argv as a call.
- A step is exactly one of: `hook` (a leg of `hook-declarations.json`, run as declared, with
  `PLUGIN_ROOT` set), `cli` (the repository dispatcher `bin/codexclaw.mjs`, which is the `cxc`
  on a user's PATH; `"payload": true` runs the plugin's own `bin/cxc.mjs`), `node` (a file under
  the oracle root), or `mcp` (the declared MCP server, each request written as one line, then
  stdin closed). `stdin` is a JSON value (written compact) or a JSON string (written raw);
  `stdin_pad` appends that many spaces (an oversized payload that is still one JSON document).
  A `write` step runs nothing: the recorder writes those files between invocations, standing in
  for an agent's edit, and records the step with `"action": "write"`.
- `observe` lists the roots or subtrees to record afterwards; the default is all five roots
  without `codex/codexclaw/hook-observations`, the diagnostic record every hook invocation
  writes. Naming that path observes it too.
- Scenario IDs are `<surface>__<subject>__<case>` in `[a-z0-9._-]`: `hook__<leg>__...`,
  `cli__<verb>__...`, `cli-help__<verb>__...`, `mcp__...`. Session ids in payloads start with
  `rec-`, and the one native thread id (which must be a UUID) is
  `0190cafe-0279-7000-8000-000000000279`, so a recording can be told apart from any real
  session.

## Fixture grammar

A fixture holds `oracle`, `covers`, `note`, the spec's `given`, `run` (`{"kind": "cxc", "steps",
"observe"}`) and `expect`:

- `exit` is the last step's exit status; `steps[i]` holds each step's `exit` (`-1` with `signal`
  or `timeout` when it did not exit), `stderr` (normalised, Node's own warning lines removed) and
  its stdout by `stdout_form`: `empty`; `json` or `json-line` (one compact document as
  `JSON.stringify` prints it, without or with a trailing newline) in `stdout_json`;
  `json-pretty-nonl` or `json-pretty` (two-space indented, as `JSON.stringify(v, null, 2)`) in
  `stdout_json`; `jsonl` (one compact document per line) in `stdout_jsonl`; `text` verbatim in
  `stdout`. The stored JSON keeps the oracle's key order, so the exact bytes are rebuilt by
  compacting (or two-space indenting) `stdout_json` and appending the newline the form names.
- `tree` maps each observed path (normalised) to `type` (`file`, `dir`, `symlink`), `mode`
  (octal, umask 022) and, for files, the content by the same forms (`json`, `jsonl`, `text`),
  `sqlite` (a database's schema text and rows, in `json`), `sqlite-sidecar` (a `-wal`, `-shm`
  or `-journal` file, presence only) or `binary` with `sha256` and `size`; symlinks carry
  `target`. A `.git` directory is listed but not walked.
- `calls` is every stub invocation, `git` call and network attempt in order, with normalised
  `argv` and `cwd`.

## Isolation

Every process of a recording runs under an empty environment (Go `exec` with an explicit `Env`;
nothing is inherited) with only: `PATH=<case>/stubs:<case>/bin` (`bin` holds `node` and a
logging wrapper around the real `git`, nothing else), `HOME`, `TMPDIR`, `CODEX_HOME`, `CODEX_SQLITE_HOME`, `CODEXCLAW_HOME`
(absolute), `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_STATE_HOME`, `XDG_CACHE_HOME`,
`XDG_RUNTIME_DIR` and `GIT_CONFIG_GLOBAL` all inside a fresh case root, `GIT_CONFIG_NOSYSTEM=1`,
fixed git identity and dates, `TZ=UTC`, `LANG=C.UTF-8`, the fake-clock preload in
`NODE_OPTIONS`, `PLUGIN_ROOT` for hooks, and the scenario's own `env`. The recorder refuses a
scratch directory inside `~/.codex`, `~/.codexclaw`, the CRW runtime or state directories, the
home directory itself, or any git checkout, and refuses an oracle whose manifest is not
v0.2.40. Each step runs in its own process group, killed when the step ends, and the case root
is removed afterwards (`--keep` keeps it).

## Normalisation and determinism

`normalisation.json` is applied to every stdout, stderr, stub argument, observed path and file
text: placeholders for every bound absolute path (longest first, as given and symlink-resolved),
then the text rules (timestamps, stamps, epoch milliseconds, pids, durations, payload digests,
the version), then aliases (UUIDs, nonces, background ids, spawn grants become `<UUID_1>`,
`<NONCE_1>` ..., numbered by first appearance across the scenario so equality between steps
survives). The oracle also runs under a frozen clock, so derived names and local dates are
stable. `crw-dev cxc record` records every scenario twice, in two fresh case roots, and refuses
to write a fixture unless both normalised recordings are byte-identical; `crw-dev cxc check`
records again and compares with the committed fixtures.

Directory listings are recorded sorted by normalised path; where the oracle prints a listing in
`readdir` order the fixture holds that order, and a replay must sort before comparing.

Two dependencies a replay has to reproduce or exclude:

- The frozen clock. A few values are deterministic only under it: launch ids and backup names
  minted from the time (`r1-20260101000000`, `config.toml.codexclaw-2026-01-01T00-00-00-000Z.bak`),
  `[age: Nd]` labels and "1h ago" relative to file mtimes, recency-weighted recall scores. A
  replay runs the Go build with the same epoch (or compares those values through the
  normalisation only), and a fixture whose stdin carries such a value (the review-round
  sign-off names the launch id) needs the same clock to mean the same thing.
- Path lengths. Case roots are `<scratch>/cxc-rec-<16 hex>` and the oracle sits at
  `/var/tmp/cxc-v0.2.40-rec`, so a byte count or a cut over an expanded path is the same in
  every recording. `coverage.json` lists the fixtures that depend on it
  (`path_length_dependent`): recall's 160-character recovery line, which is cut inside the
  plugin path, and recall index `files.size` of rollouts holding `${WS}`.

## Replaying against the Go build

The Go contract runner does not replay this domain yet: `internal/contracttest` lists `cxc` as a
pending domain and skips it by name, with the reason, until a Go implementation exists. A replay
issue then:

1. Maps each step to its CRW entry: `hook` legs through the CRW hook registration that
   replaces them (`crw hook <event>`), `cli` argv through `cli-names.md` (Wave 0) to `crw ...`,
   `mcp` to the crw MCP server. Each fixture that is replayed is tagged identical or
   intentionally changed in the replay's notes (absorbed surfaces: bg-wake, session binding,
   helper roles, doctor).
2. Builds the same `given` with the CRW names: `name-substitution.json` is applied to every path
   and text of `given` (`.codexclaw/` becomes `.crw/`, `codexclaw.json` becomes `crw.json`,
   `CODEXCLAW_HOME` becomes `CRW_HOME` bound to the `${CXC_HOME}` root).
3. Runs the Go build with the same isolation, normalises its output with `normalisation.json`
   (binding `${PLUGIN_ROOT}` to the crw plugin root), and applies `name-substitution.json` to the
   fixture's expected text, tree paths and tree contents, in rule order (regex rules with Go
   `regexp`; `resolver` rules replace the oracle's invocation prefix, recorded as
   `node "${PLUGIN_ROOT}/bin/cxc.mjs"` or `cxc`, with the placeholder `{CRW}` on both sides;
   `rewrite` rules are not textual, their note says what replaces the text).
4. Compares: exit, stdout by form (bytes rebuilt from the form), stderr, tree and calls.

## Coverage

`coverage.json` lists each contract item of the Wave 0 analysis (`contracts.md` K1-K14, the
CLI exit codes, and every class-A test file of `tests.md`) once. `recorded` items list the
fixtures whose `covers` name them (the recorder keeps the lists in sync, `lint` checks them);
`extracted` items name their data file; `pending` items carry the exact scenarios to add to a
spec file (or, for an item that is not a scenario, the extraction method); `not-recordable`
items say why.

## Commands

```sh
git -C <codexclaw clone> archive v0.2.40 | tar -x -C /var/tmp/cxc-v0.2.40-rec
go run -tags dev ./cmd/crw-dev cxc declarations --oracle /var/tmp/cxc-v0.2.40-rec
go run -tags dev ./cmd/crw-dev cxc constants --oracle /var/tmp/cxc-v0.2.40-rec
go run -tags dev ./cmd/crw-dev cxc record --oracle /var/tmp/cxc-v0.2.40-rec [--only REGEX]
go run -tags dev ./cmd/crw-dev cxc check --oracle /var/tmp/cxc-v0.2.40-rec
go run -tags dev ./cmd/crw-dev cxc lint      # no oracle; also part of `crw-dev ci contracts`
```

The recorder needs `node` (v24, for `node:sqlite`; the corpus was recorded with v24.20.0) and
`git`; it is development tooling built only with `-tags dev`, and shells out to Node because
the oracle is the Node build. A few fixtures freeze a Node stack trace or a V8 error message
(an uncaught throw in `cxc status`, `cxc enable` on a hard-flag failure, `subagents dispatch`
with invalid JSON); those bytes belong to the oracle's runtime, and a replay compares their exit
status and the presence of the error rather than V8's wording.

## Oracle findings

Recording surfaced behaviour that looks unintended in v0.2.40. The fixtures keep it as the
oracle does (only the destructive `reset --help` is fixed by the port, decision 9); the port
issues decide each one. The list, one line per defect with its fixture or source pointer, is
[docs/port-cxc/known-defects.md](../../../docs/port-cxc/known-defects.md); a port issue that finds
another adds a line there.
