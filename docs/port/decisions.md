# Platform, build and data-compatibility decisions for the Go port (CRW-140)

This file is the written contract behind the decisions the port relies on. Each decision is
one heading with the choice, why it holds, and an `Evidence:` line pointing at the file:line
or URL that backs it. Nothing here is open. Where a number is cited, it was read from the
worktree at `6472e1f9dfbe570c809f341177228657d7b1abfa` on 2026-09-25, or from the host that
same day.

Decisions are grouped: targets and toolchain, dependencies, locking, binary and install
layout, repository layout, data compatibility, protocol pins. A decision that later proves
wrong is reversed in a new PR that edits this file; it is never silently overridden by code.

## 1. Supported targets

Decision: linux/amd64, linux/arm64 and darwin/arm64 are built. Linux is the validated host.
darwin/arm64 archives are published but labelled unvalidated until a macOS host runs the
acceptance checks. Windows stays unsupported, exactly as the Python build.

Why: the current Python code imports `fcntl` unconditionally in four relay modules and uses
`socket.AF_UNIX` for the control probe, so it never ran on Windows. The relay host today is
Linux; macOS is where Jun continues sessions in Codex Desktop. Choosing a target the Python
build never served would be a scope widening, and dropping one it did serve would be a
silent narrowing. Both are forbidden by the plan.

Evidence: packages/codex-session-relay/src/codex_session_relay/daemon.py:13,
receiver.py:29, scope.py:23, service.py:18 (`import fcntl`); cli.py:3456 (`AF_UNIX`);
.github/workflows/ci.yml:19,44,60,76,88 (`ubuntu-24.04` only); draft A8, IS-10, IS-11;
plan "Must NOT have" (darwin labelled unvalidated).

## 2. Go version and toolchain directive

Decision: `go 1.27.1` in the root `go.mod`, with a `toolchain go1.27.1` directive. CI uses
`actions/setup-go` with `go-version-file: go.mod` so the pinned toolchain is the one that
builds. No workstation has Go today; the toolchain is installed by the todo-7 setup step and
by CI, never assumed.

Why: a pinned toolchain plus `CGO_ENABLED=0` is what makes the digests in the release
reproducible across the three targets. Floating to "latest" would let a release differ by
who built it.

Evidence: draft A4 and D7 (Go 1.27.1 resolved); draft F11 (no Go toolchain on the
workstation, confirmed again on 2026-09-25 by `go version` returning "command not found");
https://go.dev/doc/toolchain (toolchain directive semantics).

## 3. No CGO

Decision: every product binary builds with `CGO_ENABLED=0`. No dependency that needs a C
compiler is admitted. `mattn/go-sqlite3` is rejected on this ground alone.

Why: a static binary is the whole point of the port for U1 (no interpreter, no shared
libraries). Cross-building three targets from one Linux runner is only trivial without CGO.

Evidence: draft L34 (mattn needs CGO, rejected); plan "Must NOT have" ("No CGO; no
mattn/go-sqlite3"); IS-10.

## 4. SQLite driver

Decision: `modernc.org/sqlite` v1.59.0 (SQLite 3.53.4, pure Go). Its `modernc.org/libc`
dependency is pinned to the exact version that v1.59.0's own `go.mod` requires; the pin is
never bumped independently. OFD locking stays OFF (the driver default), so Go and Python
observe the same POSIX advisory locks inside SQLite during the transition window.
`db.SetMaxOpenConns(1)` is set on the relay store: Python opens one connection per Store with
`isolation_level=None`, and one Go connection reproduces its transaction behaviour.

Why: the on-disk format is the same as Python's `sqlite3` module, so the live
`relay.sqlite3` and the bridge `operations-<hash>.sqlite3` are readable without conversion
(IS-3). Turning OFD locking on would make Go's SQLite-level locks invisible to the Python
fence release's locks on the same file, which defeats the single-writer guarantee (IS-4).

Evidence: draft A2, L34; https://pkg.go.dev/modernc.org/sqlite@v1.59.0 (version, OFDLocking
opt-in); packages/codex-session-relay/src/codex_session_relay/store.py:1837 (`timeout=30,
isolation_level=None`), :1839-1841 (PRAGMAs WAL / FULL / foreign_keys);
packages/codex-thread-bridge/src/codex_thread_bridge/ledger.py:27-35 (ledger connection and
DDL).

## 5. MCP SDK

Decision: `github.com/modelcontextprotocol/go-sdk` v1.8.0, stdio transport, typed tool
handlers. The 12 tool names and their input schemas must serialise byte-for-byte to what
the Python `mcp` package emits on `tools/list`; the frozen copy lives in
`contract/schema/bridge-mcp-tools.json` (todo 2) and the Go server is tested against it.

Why: first-party SDK, permissive licence, and it speaks the same protocol revision the
Python `mcp>=1.26` package does. The alternative (`mark3labs/mcp-go`) adds a second schema
dialect to reconcile for no gain.

Evidence: draft A3, L33; packages/codex-thread-bridge/pyproject.toml:12 (`mcp>=1.26,<2`);
packages/codex-thread-bridge/src/codex_thread_bridge/server.py:59-380 (the 12 tools);
https://github.com/modelcontextprotocol/go-sdk/releases/tag/v1.8.0.

## 6. WebSocket client

Decision: `github.com/coder/websocket` v1.8.15. The App Server is reached by dialling the
unix socket through a custom `http.Transport.DialContext` and handshaking with URI
`ws://localhost/`, compression disabled, a 16 MiB read limit, and a 2 s close budget.

Why: these four settings mirror `rpc.py` exactly. Codex 0.153.4 closes unix handshakes that
offer permessage-deflate, so compression must stay off. The frame limit is what turns an
oversize response into `ResponseTooLarge` instead of unbounded memory.

Evidence: packages/codex-thread-bridge/src/codex_thread_bridge/rpc.py:13 (`unix_connect`),
:23 (`MAX_FRAME_BYTES = 16 * 1024 * 1024`), :286-294 (uri, open/close timeouts, max_size,
`compression=None` with the 0.153.4 comment), :53 (`REFUSED_REQUESTS_KEPT = 64`), :89
(establish/transmit/ack phase bounds default to one timeout); packages/codex-thread-bridge/
pyproject.toml:12 (`websockets>=15,<18`); draft L35; https://github.com/coder/websocket/
releases/tag/v1.8.15.

## 7. Advisory locks: `unix.Flock` only

Decision: every lock file the Python build takes with `fcntl.flock` is taken in Go with
`golang.org/x/sys/unix.Flock` (flock(2)). `unix.FcntlFlock`, `lockf`, and `F_SETLK` are
banned for these files. The affected files are: `<state>/daemon.lock` (daemon single
instance, adopted by the supervised worker through the inherited descriptor),
`<scope_root>/<socket_key>.lock` (scope ownership), `<ledger>.lock` (receiver ledger
sidecar), `<store_dir>/managed-start-<sha256>.lock` (managed start), the staging
`.crw-staging-lock`, and the two files the fence release adds, `takeover.lock` and
`write-gate.lock`. Release is by closing the descriptor when the lock is shared with a
child (daemon.lock), and by `LOCK_UN` otherwise, matching the Python sites one to one. No
lock file is ever unlinked or atomically replaced.

Why: flock(2) and fcntl(2) record locks do not see each other on Linux. A Go daemon using
record locks would start beside a Python daemon holding `daemon.lock` with flock, which is
the exact race the cutover must exclude. flock ownership belongs to the open file
description, which is what lets a supervisor and its worker share one lock; Go must keep
that by passing the descriptor, not by re-locking.

Evidence: packages/codex-session-relay/src/codex_session_relay/daemon.py:95-142 (ownership
by open file description, close-not-unlock for shared holders, `LOCK_EX | LOCK_NB` at :123);
service.py:192-241 (scope lock), :917-921, :950-961, :2135-2142 (daemon.lock probes);
receiver.py:660-672 (ledger sidecar); managed.py:318-333 (managed-start lock with
`O_NOFOLLOW` and ownership checks); scripts/crw_runtime/staging.py:45-48 (claim and lock
names), :185-205, :230-240; draft F7, L15, L38, L41; plan "Must NOT have" (never unlink
or replace a lock file; no FcntlFlock).

## 8. File leases: `F_SETLEASE` on Linux only, graceful absence elsewhere

Decision: the artifact read lease is implemented with
`unix.FcntlInt(fd, unix.F_SETLEASE, unix.F_RDLCK)` behind a `//go:build linux` file. On
darwin the same API returns "F_SETLEASE unavailable on this platform" and the caller
degrades exactly as the Python `hasattr` guard does today. Lease break is observed through
`SIGIO` on a dedicated signal channel; the lease is released with `F_UNLCK` before close.

Why: Python already treats the lease as an opt-in tier that may be absent. Porting it as
Linux-only with the same detail string keeps the JSON reports identical on both platforms
and adds nothing to the macOS surface that Python never offered.

Evidence: packages/codex-session-relay/src/codex_session_relay/scope.py:194 (`AVAILABLE =
hasattr(fcntl, "F_SETLEASE") ...`), :203-206 (absence detail string), :216 (`F_SETLEASE,
F_RDLCK`), :235 (`F_GETLEASE`), :242 (`F_UNLCK`); draft L15, L38.

## 9. Release tooling

Decision: GoReleaser v2.18.2, run from the existing owner-authorised
`.github/workflows/release.yml` (workflow_dispatch with the `dry_run` gate) as an added job.
It builds the three targets with `CGO_ENABLED=0`, `-trimpath`, and `-ldflags=-s -w
-X main.version=<tag>`, and publishes `crw_<os>_<arch>.tar.gz` plus `SHA256SUMS` to the same
GitHub Release. No code signing or notarisation.

Why: one release channel with one authorisation gate. The dry-run path already exists, so
the Go job inherits it instead of inventing a second policy.

Evidence: .github/workflows/release.yml:4 (`workflow_dispatch`), :18 (`dry_run` input),
:137, :148 (jobs gated on `inputs.dry_run == false`); draft Q2 (adopted default), D7;
https://github.com/goreleaser/goreleaser/releases/tag/v2.18.2.

## 10. Binary layout: one `crw` plus three symlinks

Decision: one static multi-call binary `crw`. It dispatches first on
`filepath.Base(os.Args[0])` (so `codex-session-relay`, `codex-thread-bridge` and
`crw-completion-hook` behave as those console scripts did) and then on the subcommand (`crw
relay ...`, `crw bridge`, `crw hook`, `crw install ...`). The installer creates the three
symlinks beside the binary. The `hook` code path must not trigger package-level
initialisation of SQLite or the MCP SDK; heavy state is constructed inside `main` per
subcommand.
Consequence: embedded commands name the invoking form: one quoted `codex-session-relay` path,
or the quoted `crw` executable followed by the separate word `relay`.

Why: the host record, `crw-completion-hook.json` and `crw-bridge-mcp.json` all store
absolute paths named after the console scripts (`relayExecutable`, `bridgeExecutable`,
`adapterEntryPoint`), and `components.json` names `consoleScript`. Keeping those names valid
means no record migration at cutover. One binary means one digest and one atomic swap.

Evidence: packages/codex-session-relay/pyproject.toml and packages/codex-thread-bridge/
pyproject.toml `[project.scripts]` (the three console script names, draft F3);
scripts/crw_runtime/components.json:9 and :39 (`consoleScript`); scripts/crw_runtime/
completion.py:53 (`crw-completion-hook.json`) and draft L4 (its `relayExecutable`,
`adapterEntryPoint` keys); scripts/crw_runtime/bridgerecord.py:79 and draft L5
(`bridgeExecutable`); draft A1, L49.

## 11. Install directory and pointer

Decision: the runtime stays under `~/.local/share/crw-runtime/`. Each Go install lands in
`bin-<version>-<digest12>/bin/{crw, codex-session-relay, codex-thread-bridge,
crw-completion-hook}` where `<digest12>` is the first 12 hex characters of the archive's
SHA256. The existing `current` symlink is repointed by creating a temporary link and
`rename(2)`-ing it over `current`, so `current/bin/<name>` is a valid path at every instant.
Python `env-1-<hash>` directories are left in place until todo 43's retention scan says no
live or resumable task references them. `XDG_DATA_HOME` is not honoured on this path; this
is a documented limitation carried over, not a new one. `crw install` has no `--dest` (todo 38
review): every subcommand acts on `<home>/.local/share/crw-runtime`, the directory whose
`current/bin` the plugin wiring runs, with the home as `pathlib` expands it, and a host record
whose `pointer.path` (read through `Path()`) names another link - a host installed at another
destination by `runtime_install.py --dest` - is refused by every command that would act on it,
with the repair, and reported by `crw install status`.

Why: the plugin's hook command and the MCP launcher both resolve through `current`, and the
host record's `pointer` field records that path. Changing the root would force a record
rewrite and a plugin re-wire in the same step as the writer takeover, which the cutover
protocol forbids.

Evidence: scripts/crw_runtime/pointer.py:27 (`POINTER_NAME = "current"`);
scripts/runtime_install.py:2655 (`env-<definitionVersion>-<12 hex>` naming, the pattern
`bin-<ver>-<digest>` mirrors); docs/runtime-install.md:686-687 (`<destination>/current` is
a directory symlink; commands name `current/bin/<console script>`); draft L3 (observed
`~/.local/share/crw-runtime/` with 14 `env-1-*` dirs and `current`; re-observed 2026-09-25
by `ls ~/.local/share/crw-runtime`), L50 (rename swap, XDG_DATA_HOME not handled today:
`grep -rn XDG_DATA_HOME scripts/` is empty).

## 12. Plugin wiring commands

Decision: the Stop hook command becomes the shell string
`"$HOME/.local/share/crw-runtime/current/bin/crw" hook; exit 0` (not `exec`, so the turn is
never blocked when the binary is absent mid-rollback). The MCP entry becomes `command:
"sh", args: ["./wiring/crw-bridge.sh"], cwd: "."`, where the three-line launcher in the
plugin cache execs `$HOME/.local/share/crw-runtime/current/bin/crw bridge`. The plugin cache
holds no long-lived executable. Decision 26 (todo 34) refines both: each passes
`--plugin-launch`, and the launcher execs `current/bin/codex-thread-bridge` so the bridge keeps
its argv[0].

Why: a plugin hook runs through a shell with `CODEX_HOME` and `PLUGIN_ROOT` in the
environment; the MCP server inherits only `HOME` and its working directory. `$HOME` is the
one variable both paths share, which is why the pointer is anchored under it. The plugin
cache is replaced wholesale on install, so the binary must live outside it.

Evidence: docs/plugin-packaging.md:111 (hook env carries `CODEX_HOME`, `PLUGIN_ROOT`),
:117 (MCP server inherits `HOME` and cwd only), :114 (relative MCP command resolves only
with `cwd`); plugins/crw/wiring/hooks/stop-recording-completion.json:8-9 (today's
`python3 -c` bootstrap and `timeout: 10`); plugins/crw/wiring/mcp.json (today's
`python3 ./wiring/crw_bridge_mcp.py`, `cwd: "."`); draft F5, F6, L50, Q5 (adopted default).

## 13. Repository layout

Decision: a single `go.mod` at the repository root with module path
`github.com/thisisjun786/codex-relay-workflow`. Tree: `cmd/crw/main.go` (dispatch only),
`internal/bridge/{appserver,mcp}`, `internal/relay/{store,delivery,registry,daemon,hook,cli}`,
`internal/contract` (shared JSON output shapes and exit codes), `internal/runtime`
(installer, pointer, host record, staging), `internal/contracttest` (fixture runner).
Contract data lives at `contract/fixtures/<domain>/` and `contract/schema/` at the root.
Python packages stay where they are until CRW-141 deletes them. `staticcheck` is pinned in
`tools.go`; `gofmt -l` must be empty.

Why: the relay imports the bridge in-process today, so two modules would need replace
directives for nothing. No `go.mod`, `cmd/` or `internal/` exists in the checkout, so there
is no collision; `contract/` is being created by todo 2 at the root named here. The corpus
must sit outside `packages/` because CRW-141 removes that tree and CRW-161 still runs the
corpus.

Evidence: packages/codex-session-relay/src/codex_session_relay/bridge_adapter.py:875-900,
managed.py:66, rolepolicy.py:176 (in-process bridge imports, draft F4); draft A10, L51, L52;
`ls /home/jun/code-worktrees/codex-relay-workflow/crw-go-port-w0` on 2026-09-25 shows no
go.mod, cmd or internal entries (contract/ is present, untracked, from todo 2); plan "Must
have" (module path, internal tree).

## 14. Schema: `SCHEMA_VERSION` stays 1, ownership keys added

Decision: the Go store executes the identical DDL string the Python `Store.__init__` runs
(frozen in `contract/schema/relay-sqlite.sql`), sets the same three PRAGMAs, and stamps
`schema_meta.version = "1"`. It adds only the ownership keys the fence release introduces:
`writer_protocol`, `owner`, `owner_epoch`, `takeover_id`, `rollback_allowed`,
`python_compatibility_build`. Opening validates required tables and the pinned
`python_compatibility_build`, never `version == 1` alone. No migration, no new
`SCHEMA_VERSION`, no data-format change before `rollback_allowed = 0`.

Why: the DDL is applied with `executescript` on every open and only ever appends
`IF NOT EXISTS` tables, so "same DDL, same version" is the only compatible contract. A
rollback onto the same live DB must find nothing the Python fence release cannot read.

Evidence: packages/codex-session-relay/src/codex_session_relay/store.py:23
(`SCHEMA_VERSION = 1`), :1839-1841 (PRAGMAs), :1842 (`executescript(DDL)`), :1846-1850
(GUARD_INDEXES applied non-fatally), :1852-1861 (`version`, `store_id`, `store_created_at`
in `schema_meta`), :1556-1580 (GUARD_INDEXES); draft A5, D4, L14, L41.

## 15. Byte rules: event key and ledger fingerprint

Decision: two hashes must be reproduced byte-for-byte.

- Stop event key: `sha256(json.dumps([EVENT_KEY_TAG, session, turn, active, item],
  separators=(",", ":")).encode("ascii"))` with `EVENT_KEY_TAG = "crw-stop-event/1"`. Go
  builds the same compact JSON array: no spaces, keys in the given order, `null` for None,
  `true`/`false` for booleans, and non-ASCII escaped as `\uXXXX` the way Python's default
  `ensure_ascii=True` does.
- Bridge operation fingerprint: `sha256(json.dumps([method, params], sort_keys=True,
  separators=(",", ":")).encode())`. Go must sort object keys recursively (Python's
  `sort_keys` is recursive), keep array order, use the same compact separators, and apply
  the same `ensure_ascii` escaping. `request_id` must be 1 to 128 characters.

Both are pinned by fixtures in `contract/fixtures/event-key/` and
`contract/fixtures/ledger-fingerprint/` that are green on Python before the Go code exists.

Why: the event key dedups Stop hook claims across the Python and Go adapters during the
transition; the fingerprint decides whether a retried bridge operation is the same request.
One differing byte either replays a claimed event or refuses a legitimate retry.

Evidence: packages/codex-session-relay/src/codex_session_relay/stopadapter.py:157
(`EVENT_KEY_TAG`), :857-858 (hash); packages/codex-thread-bridge/src/codex_thread_bridge/
ledger.py:38-43 (fingerprint and the 1 to 128 rule); https://docs.python.org/3/library/
json.html#json.dumps (`sort_keys` and `ensure_ascii` semantics); draft L18, L46.

## 16. Exit codes and JSON output

Decision: the relay CLI keeps exit codes 0 (ok), 2 (refused, with the `RefusalReason` value
inside the JSON), 3 (host), 4 (usage) and 5 (bound spent). Every subcommand prints one JSON
document on stdout. The Go CLI never uses `flag.ExitOnError`, `log.Fatal` or an unrecovered
panic on a product path; the hook recovers panics and exits 0.

Evidence: packages/codex-session-relay/src/codex_session_relay/cli.py:45 (`EXIT_OK,
EXIT_REFUSED, EXIT_HOST, EXIT_USAGE = 0, 2, 3, 4`), :5770-5814 (`main` prints the handler
payload as JSON and returns `EXIT_OK`), :5817-5830 (error payloads printed as JSON);
service.py:54 (`EXIT_BOUND_SPENT = 5`); stopadapter.py:84 (`DEFAULT_TIMEOUT_SECONDS = 5`),
:1183-1188 (`main` docstring: parses nothing, says nothing on stderr, always exits 0); plugins/crw/wiring/crw_stop_hook.py:62-63 (`MARGIN_SECONDS = 2`,
`MAX_SECONDS = 9`); draft L12, L19, L47.

## 17. App Server protocol pin

Decision: the Go client is derived from `rpc.py` behaviour and the `openai/codex`
`codex-rs/app-server-protocol` Rust structs, pinned against codex-cli 0.154.0. The wire
envelope carries NO `jsonrpc` field: requests are `{"id", "method", "params"}`,
notifications `{"method", "params"}`, responses `{"id", "result"}` or `{"id", "error"}`.
The `initialize` request sends `clientInfo.name = "codex_thread_bridge"` and
`capabilities.experimentalApi = true`, followed by an `initialized` notification. The seven
server-to-client approval and input methods are answered with error `-32601`, and the last
64 refusals are kept for `get_operation`. `crw doctor --json` records the codex-cli version
it was pinned against so drift is visible.

Why: there is no public spec; the Rust crate says outright that it neither sends nor expects
`jsonrpc`. Sending it would be a behaviour change, not a port.

Evidence: packages/codex-thread-bridge/src/codex_thread_bridge/rpc.py:31-32 (refused
methods list sourced from `codex app-server generate-json-schema --experimental` on
codex-cli 0.154.0), :38-48 (`APPROVAL_METHODS`, the seven refused methods), :53
(`REFUSED_REQUESTS_KEPT = 64`), :214 (refused with -32601), :298-306
(`initialize` params and `initialized`), :398 (`{"id", "method", "params"}` envelope);
`codex --version` on the host 2026-09-25 prints `codex-cli 0.154.0`;
https://github.com/openai/codex/tree/main/codex-rs/app-server-protocol (rpc.rs comment on
the absent jsonrpc field, draft L36); draft D8, GAP-10.

## 18. Settings records

Decision: `crw-completion-hook.json` keeps `configVersion = 1` and its key set;
`crw-bridge-mcp.json` keeps `recordVersion` 1 (no policy) and 2 (a policy named by path and
digest); the host record keeps `recordVersion` and `definitionVersion = 1`. Both settings records
are written byte for byte as Python writes them (`json.dumps(indent=2, sort_keys=True)` and a
newline). A Go install writes the plugin-owned Stop settings with `relayExecutable` =
`<destination>/current/bin/codex-session-relay`, `adapterEntryPoint` =
`<destination>/current/bin/crw-completion-hook` and `adapterInterpreter` = `/usr/bin/env`, and the
bridge record with `bridgeExecutable` = `<destination>/current/bin/codex-thread-bridge`. The
Python-shaped keys of the host record and the components definition (`interpreterPath`,
`interpreter`, `requiresPython`, `module`, `packageLocation`, `serverModule`, `exerciseScript`) are
read and preserved when present and never written by the Go installer.

Correction (todo 38): an earlier text had `adapterInterpreter` written as absent. The Go hook's
own reader (internal/relay/hook/settings.go `Complaints`) requires `adapterInterpreter` and
`adapterEntryPoint` whenever `owner` is plugin, so an absent key would have every plugin-owned
document refused. And the value cannot be the binary: the legacy launchers a cached turn still
runs (plugins/crw/wiring/crw_stop_hook.py, its `<CODEX_HOME>/crw-stop-hook.py` copy) execute
`[adapterInterpreter, adapterEntryPoint, <settings>]`, so the binary as interpreter would receive
its own path as the settings argument and evaluate nothing on every Stop. `/usr/bin/env` executes
the entry point with the settings path as its one argument, which is what the Go hook reads.

One document for both runtime kinds (todo 38 review): `crw install` writes one plugin-owned Stop
settings document, valid through the pointer for a Go runtime and for a venv alike, and no
promotion or rollback rewrites it. `/usr/bin/env <destination>/current/bin/crw-completion-hook
<settings>` runs the Go hook on a Go runtime and, on a venv, the fence release's
`crw-completion-hook` console script (`codex_session_relay.stopadapter:main`, which reads
`argv[1]` as its settings; its `complaints()` accepts `/usr/bin/env`), and `relayExecutable`
`<destination>/current/bin/codex-session-relay` is the Go link or the venv's console script. The
only rewrite is forward, once: the relay host's Python-era document (`adapterInterpreter`
`.../current/bin/python3`, a path that vanishes when the pointer leaves the venv) is archived on
the first Go install - under the `.superseded-<stamp>` name `steps.retire` gives, as a hard link
to the same file (a copy where the filesystem refuses a link), never deleted - and replaced by its
Go variant (the same host facts, only the two adapter keys moved) in one rename, inside the
promotion and before the pointer moves. Both documents run while the pointer still names the
venv, so there is no window. `crw install hook --owner plugin` replaces a Python-era document on
a Go host only by settings that record the same host facts; other flags answer `config_differs`
with the fields and the repair, because a silent rewrite of the mode changes whether turns can be
held.

A rollback to a venv never puts the archive back. Before it moves the pointer it requires the
venv to serve the document: `bin/crw-completion-hook`, `bin/codex-session-relay` and
`bin/codex-thread-bridge` resolve to regular files this user may execute, and so does each one's
`#!` interpreter and the recorded `interpreterPath`; the hook script names
`codex_session_relay.stopadapter`; the relay package the record lists there is the fence release
(its `ownership.py` declares `BUILD = "codex-session-relay/0.2.0"`, `ownership.PythonBuild`); and
every path the live document names through the pointer exists in the venv. Otherwise it refuses
with nothing changed. An operator who wants the Python-era document back restores it by hand, and
only while the pointer names the venv (after `crw install rollback <venv>`): take the newest
`<CODEX_HOME>/crw-completion-hook.json.superseded-*` whose `adapterInterpreter` ends in
`/current/bin/python3` and `mv` it over `crw-completion-hook.json` - one rename, so a Stop never
finds the path empty. The next `crw install` archives it again and writes its Go variant.

A replacement never destroys a document. A write that fails leaves the path as found and drops
only the archive name. Bytes at the path that are not this run's - another writer's save, which
is what a read-back mismatch after a successful atomic write means - are left where they are with
the archive kept, and the answer names both. A transition undone after a failed commit or pointer
move puts the archive back only while the path holds exactly the bytes this run wrote, and then by
an atomic exchange of the two names (renameat2 `RENAME_EXCHANGE` on Linux, renamex_np
`RENAME_SWAP` on darwin), which restores the document's own file; the displaced file is removed
only when it holds this run's bytes, and is exchanged back otherwise. Where no atomic exchange
exists (another platform, or a filesystem without one) nothing is renamed over the active path:
both files stay and the answer gives the `mv` that puts the found settings back by hand. A document whose `adapterInterpreter` is `env` and whose
`adapterEntryPoint` holds `=` is never written: GNU env reads such an argument as an assignment
and executes the settings path instead.

A write lands only on the document it was decided from (todo 38 review, PR #192). Every write of
the Stop settings and of the bridge record looks at the file before it reads anything - device,
inode, size, modification time and bytes, through one descriptor - and decides (and, for the
Python-era replacement, builds the Go variant) from that document; under the lock the Python
writers take it looks again, and anything but the same document writes nothing. A transition then
decides again from a fresh reading, up to three times, so a cooperating writer's newer document is
the one carried forward and archived, never overwritten by a variant of the bytes read before it;
`crw install hook` and `register-mcp` answer `config_changed_underneath` /
`record_changed_underneath` with the repair to rerun, as the Python writers do. The archive a
replacement takes has to hold those same bytes, or its name is dropped and nothing is written.

The bridge record names its execution policy as runtime_install.py records it,
`Path(value).expanduser().absolute()`: `..` is kept rather than folded by text, so the record,
its digest and the bridge all name the file the kernel opens for that spelling, a symbolic link
before the `..` included. A name holding a byte that is not UTF-8 is recorded as `os.fsdecode`
spells it (the byte as its surrogate escape, which the record carries as `"\udcXX"`) with the
digest over the file itself, and every reader - the Python launcher, the Go wiring, `register-mcp`'s
own check and the doctor - opens `os.fsencode` of it, the byte again.

Evidence: scripts/crw_runtime/completion.py:55 (`CONFIG_NAME`), :550 (`complaints`), :2751
(`configuration`); scripts/crw_runtime/bridgerecord.py:77 (`record_path`), :163 (`document`);
plugins/crw/wiring/crw_stop_hook.py:97 (`adapter_call`), :141 (the `subprocess.run` of
`call + [settings]`); scripts/crw_transition/steps.py:59 (`retire`); scripts/runtime_install.py:4828
(the policy path); packages/codex-session-relay/pyproject.toml:28 (the console script),
packages/codex-session-relay/src/codex_session_relay/stopadapter.py:259 (`complaints`), :1350
(`main`), ownership.py:17 (`BUILD`); docs/port/cutover.md:446 (never an unfenced Python
fallback); internal/relay/hook/adapter.go (args[0] is the settings path); internal/runtime/install
(`TestHookSettingsBytesArePythons`, `TestBridgeRecordBytesArePythons`,
`TestLegacyStopLaunchersReachTheGoHook`, `TestPythonEraSettingsMoveWithThePointer`,
`TestARollbackToAVenvNeverRewritesTheSettings`, `TestARollbackKilledAtItsCommitLeavesStopsRecorded`,
`TestARollbackToAVenvNeedsNoArchive`, `TestARollbackKeepsTheHostFactsTheSettingsRecord`,
`TestHookKeepsThePythonEraHostFacts`, `TestAReplacementNeverDestroysAnotherWritersDocument`,
`TestAnEntryPointEnvWouldMisreadIsRefused`, `TestTheExecutionPolicyPathIsSpelledAsPythonRecordsIt`,
`TestATransitionNeverWritesAGoVariantBuiltFromStaleBytes`,
`TestHookNeverWritesOverADocumentThatChangedAfterItWasRead`,
`TestRegisterMCPNeverWritesOverARecordThatChangedAfterItWasRead`,
`TestRegisterMCPRecordsANonUTF8PolicyPathAsPythonDoes`,
`TestASettingsUndoWithoutAnAtomicExchangeRenamesNothing`);
draft L1, L4, L5, GAP-9, D6.

## 19. Bridge provenance

Decision: the Go bridge package carries the upstream MIT notice from
`packages/codex-thread-bridge/LICENSE` in `internal/bridge/LICENSE` and the upstream
revision `bb684f35b4919a82b09d2290dc26623716803d62` in its package doc and README provenance
section.

Evidence: packages/codex-thread-bridge/LICENSE:1-3 (MIT, codex-thread-bridge contributors);
scripts/crw_runtime/components.json:20-26 (upstream remote, revision, licence); draft A9,
F12; AGENTS.md provenance rule.

## 20. Library-generated MCP text is not reproduced

Decision: where the Python bridge's caller-visible text is produced by a Python library
rather than by the bridge, the Go bridge keeps the machine-readable parts exact and writes
the prose itself. Three places, recorded as accepted differences (a fourth, the policy
file's decoder message, was retired by todo 32):
- Tool-argument validation: the first line `Error executing tool <t>: <N> validation
  error(s) for <t>Arguments`, the error count and the failing field locations in signature
  order are Python's; each field's reason line is the bridge's own, without pydantic's
  `[type=...]` detail or the version-specific `errors.pydantic.dev/<ver>` URL.
- `initialize.serverInfo.version` is the bridge's version, as `--version` prints it
  (`codex_thread_bridge.__version__`); FastMCP sends the mcp library version (1.30.0 at the pin).
- The JSON inside a tool reply's text content is compared as parsed values; its key order
  is Go's, not the dict insertion order of the Python receipt.
Every other reply on the wire is Python's bytes (decision 5, todo 16 wire tests). The
execution-policy file is no longer among the differences: the one parser behind the bridge
and the relay (`internal/bridge/execution` over `internal/pyjson`) decodes its bytes as
`json.loads` does (UTF-8, UTF-16 or UTF-32 by `detect_encoding`, `surrogatepass`) and scans
them as CPython's scanner does, so which files are accepted, and the codec, JSONDecodeError
or integer-limit text after `is not valid JSON:`, are Python's.

Evidence: packages/codex-thread-bridge/src/codex_thread_bridge/server.py (FastMCP tool
registration); todo-16 checker comparison against mcp 1.30.0 / pydantic 2.13.5 recorded in
.omo/evidence/task-16-crw-go-port.txt; the question to Jun on 2026-09-25 went unanswered
for 30 minutes and the recommended option was taken, reversible by changing
internal/bridge/mcp/validate.go and server.go.

## 21. Unrepresentable report manifest references

Decision: Python `json.loads` can hold a native lone surrogate in a receipt's `manifestRef`.
Go strings cannot represent that Unicode code point, and Go's JSON decoder does not retain
it as a native surrogate. The Go comparison therefore uses the escaped `\\ud800` form at
the report-rendering boundary. For that escaped form, Go renders
`manifestRef: present but not renderable; read it in the record`, matching the Python
recipient-visible message. This is a documented input-representation divergence, not a
claim that Go can round-trip a native lone surrogate.

Evidence: packages/codex-session-relay/src/codex_session_relay/report.py
`_manifest_ref_lines` (the UnicodeEncodeError fallback);
internal/relay/delivery/report_render.go (the escaped-reference renderer);
`Test24_RC_10_LiveUnencodableManifest` in
internal/relay/supervisor/partb_set1_test.go (live Python whole-message comparison).

## 22. Unreachable native Stop calls retain the unsynced failure journal

Decision: preserve the Python adapter's single `guard_unreachable` invocation row when
one native control-socket dial fails. The row records the dial's errno (`ENOENT`,
`ECONNREFUSED`, `EACCES`, or the dial deadline), `held = false`, and no guard decision.
Stdout remains empty and the hook exits 0. Journal policy still applies. The write is
best effort, create-once, at most one small file, without file or directory fsync, and
is bounded by the earlier of the absolute hook deadline and 125 ms from process entry.
No transcript scan, event claim, DB open, writer lock, daemon start or retry is permitted
on this path. Identity and acceptance remain null because no identity scan took place.

Why: todo 33's corpus requires the Python failure row, while the Hook budget design
was being implemented as no diagnostic writes at all. The binding clarification keeps
the observable failure and the latency bound, rejecting the silent-row-loss option.
The cutover prohibition on synchronous diagnostic fsync still holds. Twenty built-binary
invocations, each with its row read back, measured 17.916 ms p95 on the development Linux
host; this is local evidence, not a hosted-runner latency guarantee.

A young row remains live evidence for retention, just as a Python invocation row does;
it is not exempted from the retention window. This native row records no spawned Python
command and cannot create an orphan event claim. Settings and cached commands retain
their separate retention obligations. The retention scanner is not yet implemented in
this checkout, so its eventual interpretation is not claimed as runtime-verified here.
Jun chose the reader-contract extension on 2026-09-28, rather than moving the scan
and claim ahead of the failed dial. The relaxed invariant is narrowly this: payload
fields may be present while acceptance, acceptedAs, identityScanMs, eventKey and
eventIdentity are null, only for a `guard_unreachable` / `not_started` row with
`ENOENT`, `ECONNREFUSED`, `EACCES`, `EAGAIN` or `ENOTDIR`, empty stderr, no exit code or signal, no verdict,
and `held = false`. Every required field and its existing timestamp/count/path checks
remain enforced. The diagnostic must carry the matching OS errno text and the canonical
Python repr of an absolute normalized path ending in `control.sock`. A dial-deadline row
is not part of this reader exception. Missing fields, extra fields and neighbouring
shapes remain rejected; in particular a non-null identityScanMs, timed-out outcome or
missing errno does not qualify. The D3 correction adds `EAGAIN` (a full Unix listen
backlog) and `ENOTDIR` (a routing component is a file): both are observed failures to
start a guard, just like the original three. Errnos use Python's symbolic names,
including alias choices, rather than numeric substitutes. Every other row constraint
is unchanged; unsupported errno/detail pairs still do not qualify.

The readable pre-scan row remains counted in `unjudgedInvocations` under
`no_event:guard_unreachable`, and in the session/turn and journal counts. It creates no
event or acceptance. This exact known non-evaluation does not by itself make a window
UNREADABLE: an otherwise sound window containing it reads TRUE, matching a Python
unreachable invocation whose scan established an event. Other unjudged invocations,
legacy rows, corruption and duplicate acceptances retain their previous verdict rules.
Reading never rewrites or removes the row. Tests preserve the before-change journal
count and exercise the existing retention age predicate at 0, 9, 10 and 11 seconds for
a five-second timeout, with the boundary still strictly younger than ten seconds.
No scan result or acceptance is fabricated to satisfy the reader.

Evidence: packages/codex-session-relay/src/codex_session_relay/stopadapter.py
`run()` (1080-1180), `journal()` (1030-1070), and `invoke_guard()` (466-478);
docs/port/cutover.md "Hook budget" and "Retention scan surface", row 2;
internal/relay/hook/adapter.go unreachable dial branch; `Test33HookNoSocketJournalOnly`,
`Test33LatencyAcceptance`, and the four unchanged Domain/hook unreachable fixtures;
.omo/evidence/task-33-crw-go-port.txt (orchestrator clarification, 2026-09-28), and
.omo/evidence/task-33-reader-conflict.json (original blocker);
`completion._native_prescan_unreachable`, `hook.NativePrescanUnreachable`, and
`test_journal_reader.py` (native/Python live comparison, before-change neighbours,
read-only journal counting and retention boundary); .omo/evidence/task-33-reader.json.

## 23. Hook status recognizes the shipped native registration

Decision: keep Python registration parsing unchanged and additionally recognize a direct
`crw hook` command, including the shipped quoted `$HOME` runtime path and optional
`; exit 0` suffix. Expand quoted or unquoted `$HOME/`, `${HOME}/`, and unquoted
`~/` prefixes for this direct native command, never quoted tildes or escaped dollars;
resolve a bare
`crw` with the status process's PATH and report a failed lookup explicitly. Do not
execute a shell or infer a wrapper's target. Native startability checks the native executable,
not whether it answers a Python `-c` probe. Neighbouring names, `crw relay`, and an
`echo` or other wrapper containing the words are not native hook registrations.

Why: Python `completion.names_this_adapter` deliberately recognizes only a complete
argument named `completion_hook.py`. Exact parity for that shape remains required,
but copying its exclusion of the replacement executable would make todo 34's shipped
registration invisible. This is a necessary native-surface extension, not a change to
what existing Python registrations mean.

Evidence: scripts/crw_runtime/completion.py `registered_argv`, `names_this_adapter`,
`adapter_entries`; docs/port/decisions.md decision 12 (native wiring);
internal/relay/hook/status_native.go and `Test33PR181NativeAndPythonRegistration`;
PR #181 thread 4119152521 and .omo/evidence/task-33-pr181.json (2026-09-28);
`Test33NativeRegistrationPATH`, `Test33PythonBareLauncherUnchanged`, and PR #181
thread 4119373492 (bare-command lookup).

## 24. Native hook review boundaries and checkout settings authority

Decision: native settings resolution and complaints follow the checkout adapter
(`crw_runtime.completion`): nonempty argv, nonempty CRW_COMPLETION_HOOK_CONFIG,
then Codex home; expanduser followed by lexical abspath, never realpath. Plugin
budgets must not exceed 7 seconds. The retained packaged stopadapter intentionally
ignores the environment override and still uses the older under-9 limit; it is not
the authority for these native settings paths. Guard-evaluate reads no hook settings.

Keep the 100 ms startup/input deadline required by cutover.md's Hook budget table.
This deliberately releases a 300 ms-late input that Python's unbounded stdin read
accepts. Remove the separate 4 MiB stdin cap: input size is bounded by that deadline,
while the socket frame has a separate 64 MiB transport limit. The existing 1 MiB
settings bound is unchanged. This is bounded native execution, not unbounded stdin
parity. The five-second end-to-end deadline remains in force.

Use the relay's state discovery for routing, including absolute expanded XDG paths
and legacy socket hashes. This supersedes the old no-DB-before-dial requirement
for unpinned discovery: sibling provenance may require read-only DB inspection.
Pinned routing still needs no discovery, and neither path starts a daemon or writes
relay evidence. Selection refusals are shared with the CLI, never bypassed because
the local owner is Go or the request arrived on control.sock.

Keep decision 22's five-errno pre-scan reader exception narrow. Other dial failures
use Python's ordinary post-identity, post-claim guard-unreachable shape, with the
actual socket failure detail. They may scan/claim; they do not invent identity or
claim an evaluation happened. EINVAL and ELOOP remain outside the pre-scan exception.

Native timeout detail names the actual remaining guard allocation and cancellation,
not the configured outer budget or a nonexistent process group. Neither Python's
row reader nor stop_events parses this prose; they consume outcome/processEnding
and the presence of detail. Comparison tests retain every machine-consumed field
but exclude only timeout detail when comparing native and subprocess transports.
No new prose-pinning test is used for this diagnostic-only difference.

PR #181 round-4 note: an already validated block is written synchronously even if
its context expired after evaluation. A pipe write cannot meaningfully be undone
by cancellation; letting the main goroutine return before the write completes can
lose a reserved hold's answer at process exit. Bookkeeping stays bounded, and guard
calls that never return an answer still fail open. A blocked output pipe may therefore
outlive the self-deadline, just as Python's final stdout write can; the host's outer
timeout remains the last bound.

Authenticate native control peers before sending a guard request or trusting its
verdict: Linux SO_PEERCRED (Darwin LOCAL_PEERCRED), socket-file uid, and parent-directory
uid must equal the caller's uid; the parent must not be group/world writable. Reject
symlinks at the socket pathname. New Python Store directories are 0700, but an existing
writable directory is not chmodded by Python, so its safety cannot be assumed. This
is uid isolation, not proof against another process running as the same user or an
ownership-epoch protocol; daemon serving/fencing remains its owning task.

An authentication failure uses the existing ordinary post-identity
`guard_unreachable` / `not_started` / `said_nothing` row, with `errno=EACCES` and an
explicit native trust-failure detail. No request or verdict is accepted. The reader's
pre-scan exception is unchanged; the normal row can already describe this failure.
This is a native transport policy, not a Python socket oracle (Python spawns its CLI).

Published marker facts have no Python byte-size ceiling. Native context-aware reads
retain deadline and regular-file checks but no longer impose a separate 4 MiB limit.

PR #185 notes (todos 30+36 integration). Every control client sends the selection
inputs `socketPath` and `program`; the Python ones (the legacy `guard-evaluate` CLI and
the Stop adapter's pinned route) did not, so the owner resolved an unpinned Stop's
store without the socket-specific refusals (thread 4127894191). The owner decides
where it evaluates: only under its own marker root and its own `S/relay.sqlite3`; a
request naming another `markerRoot` or `dbPath` is a host error answered before any
read or write, worded alike by both owners (thread 4127894432; cutover.md "When the
other runtime owns the record"). Tests: `Test33RoutedSelectionRefusals`,
`Test33OwnerEvaluatesOnlyItsOwnLocations`, and test_fence.py's
`test_a_routed_stop_is_refused_by_the_owner_as_the_owners_fallback_refuses_it` and
`test_the_owner_writes_only_under_its_own_marker_root_and_reads_only_its_own_store`.
The legacy CLI's preflight before routing makes the store selection only
(`guard.selected_store`) and reads no receipt: a routed Stop's receipt read and evaluation
happen once, at the owner, and a local one resolves its selection once (thread 4128457287;
held store, intent `dbPath`: 4.2 s to 2.2 s). The Go hook and CLI already evaluate once.
Test: test_fence.py's `test_a_stop_spends_one_receipt_read_and_one_evaluation`.

Evidence: scripts/crw_runtime/completion.py:529-547,624-635,2728-2737,1870-1902;
packages/codex-session-relay/src/codex_session_relay/stopadapter.py:241-254,318-323;
docs/port/cutover.md "Hook budget" (startup/input 100 ms, guard 3500 ms);
`Test33ReviewD1` through `Test33ReviewD12`, `Test33ReviewD9Late`,
`internal/relay/selection`, and .omo/evidence/rev33/result.json;
stopadapter.py:1136-1162,1198-1200; marker.py:289-315; store.py:1833;
`Test33AnsweredDeadlinePython`, `Test33PeerCredentials`, `Test33UntrustedPeerReleases`,
`Test33PythonStateDirectoryMode`, `Test33LargeMarkerFactPython`, and
`Test33LargeFactHookPython` (PR #181 threads 4120181139, 4120181488, 4120181269).

## 25. Durable takeover inbox wire format

Decision: Python fence release and Go share the canonical UTF-8 JSON envelope,
operation-ID encoding, digest, immutable link publication, queue/refusal/host
responses, and transactionally coupled `schema_meta` replay markers specified in
[cutover Inbox / Wire format](cutover.md#wire-format-decision-25). Only `emit`,
`ack`, and `fault-notification-ack` queue when admission refuses
another runtime's ownership or draining. Other mutations still refuse. An inbox
acceptance means durably queued, not yet applied. A crash after application commit
and before unlink cannot repeat the handler. Python applies retained entries when
it owns the store, including rollback; Go must consume the same golden bytes.
The check36 correction excludes new `supervisor-read` requests: that command
verifies a readback against the host rather than ingesting an authored receipt.
Previously queued entries retain their wire format and receive a terminal usage
marker when replay lacks required arguments; they cannot block unrelated writers.

Todos 30/36 integration amendments (2026-09-29, binding for Go todo 31; text in
[cutover Inbox](cutover.md#inbox)): a publisher never removes a linked final entry,
even when its own directory fsync fails; only the owner removes one, after committing
its marker. Identifier overlength and non-UTF-8 arguments are exit-4 usage rejections
before any I/O. Only an `exit` 0 marker with the same digest deduplicates; a refused
request is judged again on an identical retry and its marker replaced. A retired ID
reused with different bytes records `inbox-conflict:<id>:<16 hex>` and is retired,
never raising or blocking. Ownership refusals raised inside the handler and
host-unconfirmed turn reads keep the entry without a marker and do not stop the drain.
Replayers serialize on `S/takeover-inbox/.replay.lock`, waiting at most 30 s (cutover
Lock order) before a retryable host error, and unlink only the inode they read. Names outside the entry grammar are ignored; a grammar-valid link or non-regular
entry is refused by name; invalid bytes fail closed until an operator moves the entry
out. Queued requests are judged when applied, so a queued ACK can go stale.

Go side (todo 31, 2026-09-29): `internal/relay/inbox` builds the same envelope from argparse
dests (defaults omitted, integers at arbitrary precision, argv that is not UTF-8 refused with
Python's UnicodeEncodeError text before any I/O), publishes it with the fence's protocol, and
replays it with the rules above. Go's `emit`, `ack` and `fault-notification-ack` queue on a
queueable ownership refusal (`ownership.Refused.Queueable`: another owner, draining, starting
without the permit), including a Go writer that finds the write gate held by a transfer
barrier, which is refused as the fence's lock-free preflight refuses it. As in `cli.main`,
that preflight (`check_start`, Go `inbox.RefusedAtStart`) runs before the selection refusal and
`--kind-module`, so neither stops a request from being queued under another owner. The Go
owner drains where the Python owner does (cutover Wire format): once the relay CLI's dispatch
admitted a writable command's store and before its handler reads its arguments (the handler
is handed that store), at daemon start and in supervisor recovery, the takeover candidate
before readiness; each entry runs its existing handler on the drainer's store inside one
`store.Compose` with its marker, a legacy `supervisor-read` its readback with a lazily opened
host.
`S/control.sock` carries no inbox method: the placeholder `inbox-submit` was removed, because
the fence never forwards ingress to the socket and a socket method would be a second ingress
the wire format does not define.
The Go `takeover commit` is not a drainer: it writes `rollback_allowed=0` only while
`S/takeover-inbox` holds no entry, by the replay's own name classification, and otherwise
refuses and names a Go drain (cutover Commit point, decision 30).

Why: neither runtime had an inbox format. The orchestrator supplied this contract
on 2026-09-28 rather than allowing two implementations to invent incompatible
persistent ingress records. Canonical argument bytes make retries identical;
link-without-replacement detects conflicts; the domain result and dedup marker
commit together, so rollback never restores a stale snapshot or loses a receipt.
Decision number 25 leaves numbers 23 and 24 to todo 33.

Evidence: orchestrator clarification for todo 36, 2026-09-28;
`packages/codex-session-relay/src/codex_session_relay/inbox.py`;
`packages/codex-session-relay/tests/test_fence.py`
(`test_a_failed_directory_sync_never_removes_a_retry_another_sender_acknowledged`,
`test_reused_retired_id_with_different_bytes_is_a_terminal_conflict`,
`test_only_a_successful_application_deduplicates_an_identical_retry`,
`test_a_stale_replayer_never_retires_a_newer_entry_at_the_same_name`,
`test_ownership_or_host_failure_during_replay_retains_the_entry`,
`test_deterministic_ingress_rejects_are_usage_errors_without_io`,
`test_a_held_replay_lock_bounds_the_writer_wait_as_a_host_error`);
`contract/golden/takeover-inbox/`; `docs/port/cutover.md` Inbox / Wire format;
`internal/relay/inbox` tests (`TestEnvelope_is_byte_identical_to_the_python_goldens`,
`TestEnvelope_matches_the_python_fence_on_edge_arguments`, `TestEnqueue_*`, `TestQueue_*`,
`TestDrain_*`); `internal/relay/cli/inbox_test.go` (`Test31_*`: Python queues and Go applies as
direct commands do, Go queues with the fence's bytes and answers and Python applies, a
queueable refusal queues before the selection and `--kind-module` refusals, every writable
command drains before its handler reads its arguments, 100 receipts queued while draining
applied once); `internal/relay/service/takeover_test.go`
(`Test31GoCandidateDrainsPythonQueuedEntriesBuiltCLI`, `Test30AbortAndFailedCandidateBuiltCLI`).

## 28. Takeover candidate designation and activation channel

Decision: the controller launches exactly one direct child with an inherited,
connected stream socket on fd 3 and `CRW_TAKEOVER_CHANNEL_FD=3`. No other client
can discover that channel. Its first JSON line is `{"kind":"start","record":...}`.
Within 20 seconds the candidate must validate `phase=starting`, its own runtime
as owner, non-null transition and controller, and the controller's boot ID, PID
and start ticks against its actual parent. A rejected candidate exits nonzero
without writing. Python uses the same boot/start identity as its service holder.

The admission permit contains the transition ID, epoch and controller identity.
First writable admission and every transaction revalidation compare it with the
durable record and DB stamp. Only that designated process may recover during
`starting`; ordinary client mutations remain refused or durably queued under
Decision 25. There is no environment-only starting bypass.

After recovery the candidate sends `kind=ready`, its holder identity including
build, store ID, epoch and transition ID. It waits for `kind=active` or EOF and
then rereads both the durable record and DB stamp. Bounds (both runtimes alike):
the 20 seconds cover only receiving and validating `start`; recovery between
`start` and `ready` is bounded by the controller's readiness wait alone
(`--ready-timeout`, default 600 s); sending `ready`, waiting for `active` or EOF
and acknowledging `activated` get a fresh 20 seconds, the same fresh bound the
Go controller gives its `Activate` exchange after readiness. The Python candidate
once reused the expired start deadline there, so a recovery longer than 20 s
refused activation (PR #185 thread 4127894020; tests
`test_recovery_longer_than_the_channel_bound_still_activates` and
`Test30PythonCandidateActivatesAfterRecoveryLongerThanTheChannelBound`).
It binds `S/control.sock` after recovery and before sending `ready` (decision 30);
a bind failure exits without `ready`. The control socket it then serves, like
every owner's, evaluates Stops only under the owner's own marker root and reads
only its own store (decision 24's PR #185 notes). It serves scheduled work and
workers only when the phase
is active and holder, epoch and takeover ID match. Otherwise it closes admission
and exits without serving. Matching active publication survives controller EOF;
an active reply is acknowledged with `{"kind":"activated"}`, as in the Go
controller. The retained Python entry point is `service run --takeover-candidate`.
It consumes the channel before any writable open and passes the permit explicitly
to its Store, supervisor and relay-pinned bridge transport ledger. Workers start only after activation and use ordinary
active admission; they inherit neither the channel nor a discoverable permit.
Candidate-less `check_start` calls keep their existing behavior.

Why: the reverse transfer uses the same concrete protocol as todo 30 instead of
inventing a second designation or permitting every Python client to enter starting.
No controller socket or pending candidate authorizes serving by itself.

Evidence: [cutover record](cutover.md#record) lines 85-104;
[start candidate](cutover.md#step-6-start-go-on-the-original-store) lines 248-261;
[rollback](cutover.md#rollback) lines 308-315;
todo 30 worktree `internal/relay/service/takeover.go` (`Start`,
`launchedCandidate.Activate/Close`, `ReceiveCandidate`, `CandidateChannel.Ready`)
and `internal/relay/store/ownership/admission.go` (`Candidate`, `WithCandidate`);
`packages/codex-session-relay/src/codex_session_relay/takeover.py`,
`ownership.py`, and `tests/test_takeover_candidate.py`. The Go controller
launches the retained Python build through this contract (decision 30,
`Test30TakeoverBuiltCLI`).

## How this file is checked

The acceptance check for this document is structural: every decision heading is followed
by an `Evidence:` line, and the file contains no open marker. A reviewer resolves each
citation against the worktree revision named at the top. There is no failure scenario to
run for a document; a citation that stops resolving is caught when the cited line moves,
by the reviewer, not by a script.

todo 28 adds test seams to delivery/managed/bridge ledger, additive, default unchanged

## 21. Routing uses the transaction-aware fault ledger library (todo 23A)

Decision: expose `faults.KindPolicy` (PreIssue, Validate, Confirm), class-threshold registration, and `Ledger` methods for canonical identities, complete observation receipts with atomic adoption, current link reads, adoption/move/update, publication queue/read/claim/operation/complete/fail/reconcile/cancel, notifications, policies, remediation, and resolution. They reuse the existing helpers and the caller's context/Store; routing wraps composed writes in `Store.Compose`, never opens a second Store or invokes the CLI. Existing CLI claim/operation/fail/cancel dispatch shares these APIs. `WithInputs` supplies deterministic clocks and entropy to tests.

Evidence: `internal/relay/faults/api.go`, `kinds.go`; `internal/relay/routing/integration_test.go` replays Python scenarios and compares every persisted table and whole receipts. The default built-in process remains unchanged until routing installs its project_create declaration.
- [todo23] internal/relay/reception/depth.go fixes the console-script boundary at 9998 nested containers (stored settings first fail at 9998 because their object adds one level) because parity targets the installed `codex-session-relay` entry point on shipped CPython 3.13 with recursionlimit 1000, which relay code never changes; live console `packet-check` coverage detects runtime drift.

## 26. The plugin launchers' record contract moves into Go behind `--plugin-launch` (todo 34)

Decision: the plugin's declared commands name an explicit plugin-launch mode, and the record
contract the Python launchers carried is enforced by the runtime in that mode.

- MCP: `wiring/crw-bridge.sh` is a three-line POSIX sh launcher,
  `exec "$HOME/.local/share/crw-runtime/current/bin/codex-thread-bridge" --plugin-launch "$@"`.
  It execs the installer's `codex-thread-bridge` link to `crw` (decision 38) rather than
  `crw bridge`, because POSIX `exec` cannot set argv[0] and bridges are found by an argv word
  ending in `codex-thread-bridge` (the `/proc` reading in docs/plugin-packaging.md "Updating
  safely"); `crw bridge --plugin-launch` is the same mode under the other name. In that mode the
  runtime reads `<CODEX_HOME>/crw-bridge-mcp.json`, resolving CODEX_HOME the way
  `crw_bridge_mcp.py codex_home()` did: the variable, then six directories above the launcher
  (the declared cwd plus `wiring/crw-bridge.sh`), then `~/.codex`. It applies that launcher's
  checks in the same order, each refusing with exit 2: record missing, unreadable, not an
  object, version not 1 or 2, owner not `plugin` (the stand-down), serverName mismatch,
  bridgeExecutable not absolute, args not a list of strings (false, 0, "" and {} are not
  defaulted), a version-1 record carrying `executionPolicy`, and for version 2 every
  `policy_environment` check (the policy file missing, not regular, unreadable or no longer
  hashing to the recorded digest; an inherited `CODEX_THREAD_BRIDGE_EXECUTION_POLICY` or
  `_DIGEST` naming another). Its stderr is the Python launcher's byte for byte except for the
  three repairs, which name `crw install register-mcp --owner plugin`, the record's writer since
  todo 38, where the Python launcher names runtime_install.py. The Python launcher keeps its
  text: runtime_install.py is the host's installer until the cutover.
- Having judged the record, the runtime execs its own executable as the bridge, argv[0] kept,
  with the record's args followed by the launcher's own `"$@"`, and for a version-2 record with
  the policy path and digest added to the environment, as `crw_bridge_mcp.py`'s execve did. The
  running bridge is the process Codex started: its argv ends in `codex-thread-bridge` without the
  flag, and `/proc/<pid>/environ` carries the policy the operator's reading compares with the
  record. The bridge loads and re-checks the policy as before. Arguments that would begin with
  `--plugin-launch` are refused with exit 2 rather than re-entering the launcher.
- `bridgeExecutable` is required to be present and absolute, exactly as Python required it, and
  is not executed: the runtime behind the pointer is the bridge. `crw install register-mcp`
  writes the pointer's `codex-thread-bridge` there.
- Stop: the command is `"$HOME/.local/share/crw-runtime/current/bin/crw" hook --plugin-launch;
  exit 0`, without `exec`, so exit 0 holds when the pointer names nothing. In that mode
  `crw hook` reads only `<CODEX_HOME>/crw-completion-hook.json`, CODEX_HOME taken as
  `crw_stop_hook.py settings_path` takes it; neither `CRW_COMPLETION_HOOK_CONFIG` nor an
  argument path is read, because a declaration carries no settings argument and an inherited
  override would send the Stop to settings the installer never wrote. It stands down, with exit
  0, empty stdout, no journal row, no event claim and no guard request, unless those settings
  name `owner: plugin`. The rest of `crw_stop_hook.py adapter_call` is not code here: a
  configVersion other than 1, absent included, is refused by the settings validator, in silence,
  before that point, and the adapterInterpreter and adapterEntryPoint checks named the Python
  adapter Go replaces. Without the flag nothing changes: the legacy launchers' `[adapterInterpreter,
  adapterEntryPoint, settings]` call through todo 38's `/usr/bin/env` settings, and a user
  hook-file entry, keep the todo-33 settings precedence. A host carrying both registrations
  evaluates each Stop once. Hook status (decision 23) reads the flag in a registered command as
  naming no settings file, not as a settings path.
- `runtime_install.py register-mcp`'s launcher probe stages a stand-in runtime at the probe
  HOME's pointer, `current/bin/codex-thread-bridge`. The stand-in accepts only `--plugin-launch`
  and hands off to this checkout's `crw_bridge_mcp.py`, the reference the Go contract is compared
  with. The probe still judges the package's declared command, arguments and working directory
  under the App Server's environment.

Why: the plan's launcher (`exec ... crw bridge "$@"`) read no record, so on a plugin host the
bridge checked no role pair, lost the recorded `--socket`, and started even where the user owned
the server. A native `crw hook` did not check the owner either. Measured before this change, a
host with both Stop registrations asked the guard twice for a Stop whose identity could not be
established, and wrote two journal rows for one whose identity could (guard once, plus a
`duplicate_invocation` row). The Python launchers' behaviour is the parity target, so it moves
with the runtime rather than being dropped. Rebuilt on todo 38 (the first version, PR #182,
predates the Go installer), it changed in two places: the bridge is exec'd rather than served
in-process after `os.Setenv`, which left `/proc` showing `crw bridge --plugin-launch` and no
policy, and the repairs name the Go installer.

Evidence: plugins/crw/wiring/crw_bridge_mcp.py (`codex_home`, `policy_environment`, `main`);
plugins/crw/wiring/crw_stop_hook.py (`settings_path`, `adapter_call`);
`internal/pluginwiring/launch.go`; `launch_test.go` (36 record cases with stderr and exit equal
to the Python launcher run from the same cache layout, the repairs rewritten; args
pass-through; the policy reaching the bridge; the re-entry refusal); `wiring_test.go` (the
declared commands through `sh` against a fake and a real pointer; the running bridge's
`/proc` cmdline, environ and exe); `internal/relay/hook/pluginlaunch_test.go` (owner and
configVersion stand-down, double registration evaluated once, the settings path, the status
reading); `internal/relay/hook/status_native.go`;
`internal/runtime/install/wiring_test.go` (`TestTheNativeStopCommandJournalsTheStopThroughThePointer`;
`TestTheWiringLaunchersStartTheGoBridgeUnderTheRecordedPolicy`, whose native case starts
`crw-bridge.sh` from the cache layout with only HOME set against a record `crw install
register-mcp` wrote; `TestLegacyStopLaunchersReachTheGoHook`); scripts/ci/tests/test_plugin_wiring.py
`BridgeRecordPolicyTest`.

## 27. Stop parity follows process state, not round-trip distributions

Decision: a successful ordinary stop may report its worker as `gone` or `exited`,
exactly as Python does. An already-reaped worker gives `gone`; an exited worker
held unreaped gives `exited`. For either controlled state, compare the complete
answer, exit code, and persisted files with Python. Twenty plain round trips
record each runtime's observed distribution, but do not assert equal frequencies
or pairwise outcomes across independently scheduled executions. The repeated
start/restart snapshot loop follows the same evidence-only rule: log Python
non-ok answers and omit that iteration's Go comparisons, including when a Python
start refusal leaves no restart observation. Compare Go readiness snapshot
shapes against all Python observations for that action in the run, not an
outcome-sized list indexed by action position. A Go non-ok answer when Python
was ok, or a Go shape outside those observations, still fails. Plain-stop Go
workers must remain in `{gone, exited}`. Controlled refusal and unconfirmed-exit
tests retain their existing contracts.

Why: Python's supervisor does not install a SIGTERM handler. Its worker receives
PR_SET_PDEATHSIG=SIGTERM, and stop observes supervisor exit at 100 ms grace
boundaries before re-reading the worker. Whether the orphan has been reaped by
then decides `gone` versus `exited`; a fabricated signal handler or normalized
worker field would not be a faithful port. Go keeps the same observation cadence.
The orphan-lock criterion is that the inherited flock survives while the worker
lives; freezing the worker makes that interval deterministic in both runtimes.

Python can also report `replaced_by_new_launch` without any replacement launch.
`ProcessHandle.alive()` in
`packages/codex-session-relay/src/codex_session_relay/service.py:358-366`
uses the leader's `/proc` state and treats `Z` as exited. In a multithreaded
worker the leader can reach `Z` while another thread still holds the inherited
`daemon.lock` descriptor. `_terminate()` then reports `exited` (`:1480-1494`),
and stop's final nonblocking lock acquisition interprets the occupied lock as a
replacement (`:1393-1402`). Restart propagates that refusal (`:1534-1536`).
A pipe-controlled two-thread reproduction observed a zombie leader, Python
`alive() == False`, an unreadable exit pidfd, and a held flock at the same time;
after the remaining thread exited the pidfd became readable and the lock free.
A plain Python stop also reproduced the refusal with runtime-specific state and
scope directories. Isolating directories prevents cross-iteration interference,
but cannot remove this leader-exit race. Do not turn Python's scheduling into a
Go test failure, add sleeps, or alter either runtime's stop contract to hide it.
Decision 40 supersedes this paragraph: the race was the fence misreading process
state, and the fence now reads exit from the pidfd as Go does. The rest of this
decision stands.

Evidence: `packages/codex-session-relay/src/codex_session_relay/service.py:1304-1421`
(stop and worker re-read), `:1480-1516` (grace and process-state outcomes),
`:299-322` (parent-death signal); `internal/relay/service/reap_test.go`
(`Test29D1DeterministicReapStates`), `check29_test.go`
(`Test29D1PlainRoundTripTwenty`, `Test29D3LaunchSnapshotTwenty`),
`.omo/evidence/check29-fixes/` (`python-sigterm-proof.log`, `packages.log`,
`packages-serial.log`), and `.omo/evidence/check29-flake/`
(`reproduce_leader_exit.py`, `leader-exit.log`, `run-3.log`).

## 31. Read-only command opens and the answering runtime's readings

Decision: a read-only form (`cli.py` `_read_only_command`) is marked on the Go CLI's
context, and `store.Open` then serves it as `Services.store` does: the lock-free
ownership preflight first, the admitted opener only where Go may write, and otherwise a
`mode=ro`, `query_only`, deferred-`BEGIN` store with no write gate, DDL or
initialization row. Housekeeping a writer persists is projected, never written, for such a
store - one the answering runtime's ownership preflight or write admission refuses (the other
runtime's, or one mid-transition) - and only for such a store: `fault-notifications` with
Python's effective-state query, and `fault-next` on a private SQLite backup
(`readonly_queue_state`). Both branch on the store having been opened read-only
(`services.store.read_only`, Go's `Store.ReadOnly`), never on the command's read-only
classification, so the owner runtime's `fault-next` on its own store is admitted as a writer
and expires lapsed leases there (it writes), and a lapsed claim is offered again;
`expire_leases` has no other caller. The fence
first branched `cmd_fault_next` on `services.read_only`, which left every lapsed lease
claimed for good on the owner's store; that is corrected in the fence and in Go alike.
`fault-next` checks `--limit` before the store is opened in both runtimes. An absent store
(no `D`, `takeover.json` or `write-gate.lock`) is refused by name and never created by a read-only form:
`{"error":"refused","reason":"store_absent","detail":"no relay store exists at <D>; a
read-only command never creates one"}`, exit 2, byte for byte the fence's `Services.store`
answer. `store_absent` is a literal there (as in `declarations.py`), not an
`errors.RefusalReason` member, so `contract/schema/relay-exit-codes.json`, which
`scripts/port/dump_contracts.py` derives from that enum, does not list it. Absent-store
initialization happens on write or daemon opens only (decision 30). A partial store (a
mirror or a write gate without `D`) is refused with the writer admission's own refusal,
`store_owned_by_other`, never read, created or repaired; the Python half of that refusal is
pending (refactor-backlog.md, audit 25). A read-only form never binds an unbound store to
its socket either: it reads `mode=ro` instead (decision 30).

Argument refusals keep `cli.py` `main`'s order against the store. A read-only form refuses
its own arguments where its handler does, before the lazy `Services.store`: `merge-turn-show`
without exactly one selector and `linkage-up --scope` without `--task` answer
`bad_invocation` on an absent store and create nothing. A write form's store is opened by
`_ownership_preflight` (`services.store`) before its handler, so a refusal of its own
arguments (`register`'s unreadable settings, `settings-record --exception` beside
`--clear-exception`) leaves an absent store initialized, and a store another runtime owns
answers the ownership refusal instead. Go's relay-registry dispatch (`registry.run`) opens a
write form's store before the command's `precheck` and a read-only form's after it; the
`--kind-module` refusal still precedes a write form's store in Go (refactor-backlog.md).

A read the live-state guard refuses (until todo 43) reports the refusal. The owner's
control socket answers a guard that failed with the relay's host record, as `control.py`
does, so a Stop journals `guard_host_error` instead of `guard_said_nothing`; the journal
row keeps the adapters' error-record shape, with `detail` null.

`doctor --json`'s `ownership` block is `ownership.report`: all six keys and the phase
null together, with `detail`, when either the mirror or the database cannot be read; the
six keys with `detail` `takeover record missing` for a stamp whose mirror is absent (the
torn state "initial stamp committed, mirror absent", cutover.md Record);
the phase and each process record's `python_compatibility_build` echoed raw. A store Go
may not write is diagnosed like `store.probe`'s `check_start` branch, without a live open
or a probe file beside it, and its `access.detail` words the refusal as `ownership.mirror`
and `ownership.py validate` do (Go's preflight still decides that it is refused). `ownership.runtime_build` names the answering runtime: the build
Go publishes as its holder identity (`cli.Build`, else `cli.Version`), never the Python
fence build. It is a documented parity difference beside the Go-only `runtime` block, and
parity harnesses normalize it. The production binary links no test support.

The process records carry `python_compatibility_build` where the fence writes it: first in
`daemon.json` and the scope registration `scopes/<key>.json` (`service.py` `new_record`) and
last in `worker-policy.json`'s `worker` (`publish_worker_policy`). It names the Python fence
build a process runs, and a Go process is not one, so Go writes the same key in the same
place with the value null. The retained fence reads Go-written records as its own: `service
status` and `doctor` answer a running Go service (`ownership.processes` null), its `service
stop` of that service is refused by the fence's admission of a Go-owned store
(`store_owned_by_other`; Go stops its daemon), and after a completed takeover to Python its
`status`, `stop`, `doctor` and `service start` read the stopped Go records. That value and
`schema_meta.owner` are the runtime identity, the only values a whole-state comparison of
stores and process records normalizes (`testsupport.RuntimeIdentity`; `RuntimeIdentityText`
for record bytes), and only once each side carries its own: the fence build and `python` from
the Python side, null and `go` from the Go side. Any other value, the other runtime's
included, fails the comparison, so a Go record naming the fence build is a difference.
A service supervisor opens, and so creates, its store in recovery, as `cli.py` `_supervise`
does, and publishes the identity it read into both records (`publish_store_identity`): a
`service run` whose bound is already spent creates no store and records the identity read at
start, null for an absent store.

The relay's `faultsweep.INSTALLATION.version` and the bridge's `__version__` are mirrored
as `faults.RelayPackageVersion` and `appserver.BridgeVersion` (0.2.0), each checked
against the real Python package; `ownership.PythonBuild` stays the fence identity and does
not follow later bumps. Open question, not decided here: the Go daemon's installation
location is its executable's directory under package `codex-session-relay`, so the
host-record revision lookup reports `unknown` (refactor-backlog.md, audit 50).

Evidence: `internal/relay/argparse/readonly.go` (`ReadOnlyForm`, read by the relay CLI and
`registry.ExecuteAs`), `internal/relay/cli/readonly.go`, `internal/relay/store/hold.go`
(`openForRead`, `OpenReadOnlyStore`, `Projection`),
`internal/relay/faults/commands_c.go` (`cNext`), `internal/relay/store/diagnostic_probe.go`
(`ownershipPreflight`), `internal/relay/store/diagnostic_read.go`, `internal/relay/cli/doctor.go`;
`internal/relay/store/fence_wording.go`, `internal/relay/hook/adapter.go` (`HandleControl`);
`TestReadOnlyForms_match_python_in_every_ownership_state`,
`TestReadOnlyForms_never_create_an_absent_store` (byte-compared with the live fence),
`TestDoctor_reports_a_stamp_without_its_mirror_as_the_fence_does`,
`TestReadOnlyForms_refuse_a_partial_store_as_a_writer_does`,
`TestArgumentRefusals_fall_where_the_python_fence_puts_them` (internal/contracttest),
`Test25_CLI23_a_write_forms_own_refusal_comes_after_its_store` (both against the live fence),
`TestGuardEvaluate_reports_the_live_state_refusal`,
`TestPacketCheck_reports_the_live_state_refusal`, `Test33ReviewD8`,
`TestDoctor_matches_python_on_a_store_the_other_runtime_owns`,
`TestDoctor_ownership_block_matches_python_on_a_broken_store`,
`TestDoctor_runtime_build_is_the_answering_go_build`,
`internal/relay/service/record_identity_test.go`
(`Test29GoRecordsCarryANullFenceBuildThePythonFenceReads`, `Test29SpentServiceRunCreatesNoStore`,
against the live fence), `internal/testsupport/identity_test.go`,
`TestVersion_is_the_python_bridge_package_version`,
`TestRelayPackageVersion_is_the_python_package_version`,
`Test22_FC_29_ClaimLeaseBudgetWholeOutput` (the owner's `fault-next` against live Python,
whole tables), `TestCCLIOracle` (`fault-next --limit 0` on an absent store),
`tests/test_fence_readonly.py::test_the_owners_fault_next_releases_a_lapsed_claim`.

## 30. Ownership transfer and the Go takeover controller

This implements decision 28's candidate wire contract (fd 3, start/ready/active) on the Go side.

Decision: the Go controller calls modernc v1.59.0's `NewBackup`, `Step`, and
`Finish` through `sql.Conn.Raw` while daemon, scope, and write-gate exclusion
are held. The backup and inventory are synced before the ownership CAS. It is
not a main-file copy and does not require a WAL checkpoint. `VACUUM INTO` was
rejected because the driver exposes the actual SQLite backup API. Reverse
transfer always uses the live database and never restores this snapshot.

Admission/status preflight uses a disposable main-plus-WAL copy solely to avoid
creating source SQLite sidecars before admission. A racing inconsistent copy
refuses rather than repairing the source; it is not the transfer backup.
The durable owner/epoch and exact transition ID recover the DB-commit/JSON-publish
gap. Lock contention bounded-fails immediately; explicit command resumption,
not a timestamp or PID age, decides the next attempt.

Both runtimes use the same inherited activation channel: a Unix socket on fd 3,
selected only by `CRW_TAKEOVER_CHANNEL_FD=3`. JSON lines carry `start` with the
record, `ready` with identity/storeId/epoch/transitionId, `active` with the durable
record, and the candidate's `activated` acknowledgement. The controller identity
must equal the direct parent; a starting permit binds the transition ID, epoch,
and controller identity. Ordinary starting writers are refused. Readiness follows
recovery and control-socket binding, before scheduled work. EOF quiesces the
candidate unless its exact holder identity and epoch are already active in both
the durable stamp and mirror. Holder identity includes the exact runtime build.

Both candidates are the service supervisor, `service run --takeover-candidate`
(the frozen flag), launched by the controller as its direct child in a new
session: the Go candidate is `crw relay ... service run --takeover-candidate`,
not a bounded daemon segment, and runs under the service's launch declaration.
It recovers, binds `S/control.sock`, sends `ready`, and after activation releases
that listener and starts workers under ordinary admission. A candidate run that
fails, including a missing, non-socket, non-stream, silent or malformed channel,
exits with the retained Python candidate's code (cli.py `main`,
`takeover.receive_candidate`) and prints nothing on stdout; Go writes its error
document to stderr, which a launched candidate shares with `daemon.log`. The retained Python
candidate is `<python-relay> --state S --socket K service run --takeover-candidate`,
where `<python-relay>` is the absolute path of the retained fence release's
console script given explicitly on `takeover rollback --to python` or
`takeover activate` as `--python-relay`; nothing searches PATH or an install
tree. Its readiness identity must carry `build == pythonCompatibilityBuild`,
which is rollback step 4. Every launch precondition (the locator, the enabled
service intent, a launch declaration the candidate will not refuse, and Go's
ability to open the store) is checked before `begin` publishes draining and
again before the ownership CAS and before the launch, so a missing precondition
never strands a store in starting. The 20-second channel bound covers the
candidate's validation of `start` and, separately, the activation exchange; the
controller bounds the wait for readiness, which spans recovery, by
`--ready-timeout` (default 600 seconds). Reverse CAS never fabricates readiness.

`takeover abort` returns a draining transition whose ownership CAS has not
committed to the unchanged owner's active phase, restoring the installed
transition from `takeover_id`; it writes no `schema_meta` key. After the CAS it
refuses; the recovery is activation or the reverse transfer, which `begin` and
`rollback` accept from the transferred owner's starting phase with a new epoch.
Aborting such a reverse transfer before its CAS restores that starting phase,
never active: its draining mirror names no holder, because no candidate became
ready.
An existing lock owned by this user is trusted when it grants no group or other
access (in any directory, as Python and earlier Go builds trust it) or sits in an
owner-only directory whatever its umask-derived mode; locks are never chmodded. The Go candidate
drains the takeover inbox before readiness (todo 31, decision 25), so the CAS toward Go does not
wait for an empty inbox; an entry the candidate cannot apply keeps it from readiness.
`takeover commit` applies nothing itself (the controller has no handler table and no host) and
writes `rollback_allowed=0` only over an empty inbox: it refuses while an entry is queued and
names the Go drain that applies it (a writable relay command or a service restart), so every
entry queued while the way back was open has been applied by Go before that way closes
(PR #185 thread 4128457226). An entry is a name in the decision-25 grammar, classified once for
the replay and the commit (`ownership.IsInboxEntry`); any other name, which no replay removes,
blocks nothing.

The native status envelope is frozen in `contract/schema/records.json`.
Go initializes a truly absent store (no database, no `takeover.json`, no
`write-gate.lock`) as `owner=go`, epoch 1, writer protocol 1, rollback allowed,
with the pinned Python compatibility build, under the exclusive gate: a host with
no Python interpreter still needs a store (IS-1). The database is built and
stamped under a temporary name and linked into place, so it never exists
without its ownership keys. Anything partial is refused, never repaired, and an
existing unfenced store is initialized only by the Python fence (Step 0).
Before refusing an existing `D` that has no `write-gate.lock`, a Go writable open reads
its `schema_meta` from a disposable copy, as the fence's `Store()` reads it before
deciding what the store is: a `D` that cannot be read, or is not a database, fails
with that error in Python's words (`DatabaseError: file is not a database`, a host
error, exit 3; `store_unopenable` in an intent's store record) and gains no gate or
sidecar in either runtime; only a readable `D` is refused as unfenced. The
registration hold (`intent-register`) stats `D` before its admission, as
`registration_hold` does, so a missing store or directory answers
`the relay store could not be opened for writing: [Errno 2] No such file or directory: '<D>'`
in both runtimes, and it raises its admission's refusal (`register_relationship` re-raises the
fence's `OwnershipRefused`): a store the other runtime owns answers reason
`store_owned_by_other` in the fence's words, not `unregistered_relationship`.
The creator places `write-gate.lock` already held EX (a temporary `S/.write-gate-*`
file, `flock`, `link(2)`), as the fence does, and a Go writer that finds the gate held EX
while the store is incomplete (no `D` or no mirror) or active (a creation finishing, a socket
binding) waits to share it within its busy timeout before admission, so racing first
openers of either runtime never refuse each other as a partial store. A transfer barrier,
taken only after the record left active, is still refused at once.
Go binds an unbound store it owns (active, no transition) to the App Server socket of its
first socketed writable open, as the fence does (cutover.md Record): under `write-gate EX`,
waited for at most `ownership.LockWait` (30 s; expiry is the fence's `LockWaitExpired` host
error, exit 3), the rest of the record is revalidated, `schema_meta.socket_path` committed
and the mirror republished with `appServerSocket` and `scopeKey`; read-only forms and the
candidate never bind, and a torn binding is completed only by an opener passing its socket,
which `ownership.CheckStart` lets through. Go's admission and start-preflight refusals for
another owner, a draining store and a starting store without the permit carry the fence's
details (`the relay store belongs to another runtime`, `the relay store is draining`, `only
the designated candidate may enter starting`), so a refused write form answers Python's bytes.
Existing parity harnesses
explicitly stamp stopped synthetic fixtures; no test flag bypasses admission.
`S/control.sock` serves `guard-evaluate` only; todo 31 removed the `inbox-submit`
placeholder (decision 25: ingress is the queued command's own file publication).

Evidence: `internal/relay/store/ownership/{backup.go,controller.go,record.go}`;
`Test30BackupIncludesWALAndInventory`, `Test30HappyRollbackSameLiveStore`,
`Test30CrashMatrix`, `Test30TwoProcessWriterBlocksTransfer`,
`internal/relay/service/takeover_test.go` (`Test30StatusSchema`,
`Test30TakeoverBuiltCLI` (Go epoch 2, retained Python epoch 3, Go epoch 4 on one
store), `Test30RealCandidateControllerEOF`,
`Test30ActivationEOFRequiresMatchingPublishedHolder`,
`Test30AbortAndFailedCandidateBuiltCLI`, `Test30CommitRefusesOverQueuedInboxEntries`,
`Test30ReadyTimeoutBoundsSilentCandidate`,
`Test30DrainComparesRecordedIdentity`, `Test30IsolatedScopeRootMatchesPython`,
`Test30GroupWritableStateDirectoryLocks`, `Test30CandidateChannelRefusalMatchesPython`),
`Test30StartingRequiresExactCandidatePermit`, `Test30AbortReturnsToActiveOwner`,
`Test30AbortOfReverseBeginReturnsToStarting`,
`Test30GroupWritableLocksKeepInodeAndMode`, `Test30CommitTearKeepsGoWritersAdmitted`,
`internal/relay/store/ownership_test.go` (`Test30CreateAbsentNeverExposesUnstampedDatabase`),
`internal/relay/store/create_race_test.go` (`Test30ConcurrentFirstOpenersNeverSeeAPartialStore`,
Go and Python first openers paused after placing the gate and racing unpaused),
`internal/relay/store/socket_binding_test.go` (`Test30SocketBindingBindsAnUnboundStoreOnce`,
`Test30TornSocketBindingIsCompletedOnlyByItsSocket`,
`Test30SocketBindingNeverTouchesAForeignOrMovingStore`,
`Test30SocketBindingWaitsForWritersWithinTheBound`),
`internal/relay/cli/fence_parity_test.go` (`TestSocketBinding_*` against the live fence),
`TestWriteForms_refuse_a_foreign_store_as_python_does` (byte for byte),
`internal/relay/store/registration_hold_python_test.go`
(`TestOpen_reads_a_gateless_store_before_refusing_it_as_python_does`,
`TestRegistrationHold_answers_an_unstattable_store_as_python_does`), `Test24_SOS_14_WholeOutputAndSQLite`,
`TestCLI_intent_register_refuses_a_store_the_other_runtime_owns_like_python`,
`TestINT10_an_unreadable_missing_or_held_store_refuses_registration` (against the live fence);
`.omo/evidence/task-30-crw-go-port.txt`.
## 29a. Uncaught Python exceptions: final line is contract, frames are not

Decision: when a Python skill script dies with an uncaught exception, the parity
contract is the exit code, byte-identical stdout, and the final stderr exception
line (`ExceptionType: detail`). The leading "Traceback (most recent call last):"
block of frames (file paths, line numbers, source lines, caret markers) is an
interpreter implementation detail and is not reproduced; Go prints no fake frames.
The live-Python test helper applies one narrow comparator: it strips only that
leading traceback block from the Python stderr and compares the rest byte for
byte. Go stderr is never stripped, so extra Go frames, a different exit code,
different stdout or a different final line still fail.

Why: the port follows properties, not interpreter internals. Frames name
checkout paths and Python line numbers that change with any edit and with the
installation location, so they cannot be a stable contract.

Evidence: 148 of the 2061 cases in `TestSkillJSONShapeLivePython`
(`internal/skill/shape_matrix_test.go`) end in an uncaught Python exception and
differ from Go only in traceback frames;
`.omo/evidence/task-35-devin2-exception-matrix.json` lists them with matching
exit, stdout and final error. The comparator is `skillProcessParity` in
`internal/skill/process_parity_test.go`, pinned by
`TestSkillProcessParityComparator`.

## 29b. Live-Python oracles run with the supported host's stdio decoding

Decision: every live-Python skill oracle runs with `PYTHONIOENCODING=utf-8:strict`
and `LC_ALL=C.UTF-8` (inherited locale and Python I/O variables removed). The Go
`crw skill` commands read bytes and never consult the locale; invalid UTF-8 on stdin
is refused exactly as Python refuses it on a host with a UTF-8 locale such as
`en_US.UTF-8`.

Why: Python chooses the stdin error handler from the locale. Under `C`, `POSIX` and
the coercion targets `C.UTF-8`/`UTF-8` (the default on the hosted CI runner) it
decodes stdin with `surrogateescape` and continues; under `en_US.UTF-8` it raises
`UnicodeDecodeError`. The same test therefore passed locally and failed in CI
(run 36454431117, `TestSkillUnreadableInputsLivePython`, three `<bad-utf8.json`
stdin cases). That switch is interpreter behaviour, not a skill property; the port
keeps one deterministic behaviour and the oracle is pinned to it. The pin changes
no Python output for valid input.

Evidence: `oracleEnv` in `internal/skill/process_parity_test.go`;
`TestSkillUnreadableInputsLivePython` passes under outer `LC_ALL=C`, `LC_ALL=C.UTF-8`
and `LANG=en_US.UTF-8`, and fails under outer `LC_ALL=C` without the pin.

## 32. Native hook allocations bound waiting, not scheduling

Decision: the native Stop hook enforces its entry-anchored allocations only where it
waits for something outside itself. No number changes: the absolute 5 s deadline
(shortened by `timeoutSeconds`), the 100 ms startup/input allocation from process
entry, the 750 ms identity scan, the 3,500 ms guard allocation and the bookkeeping
reserve stay as they are. What changes is where they apply:

- Settings: the local, 1 MiB-bounded regular-file read is bounded by the absolute
  deadline instead of being cut at entry + 100 ms.
- Input: a descriptor on stdin is polled. The 100 ms allocation bounds how long the
  hook waits for the host's bytes to become readable; bytes that are already readable
  when the hook looks, including a complete payload written while the hook was
  unscheduled and a prefilled regular file, are taken. A payload that has not arrived
  by the deadline is still released as `stdin_unreadable` (decision 24). A reader that
  is not a descriptor, or a descriptor that cannot be polled, keeps the plain
  deadline. The work deadline bounds the whole read.
- Dial: a Unix-socket connect completes or fails at once (a full Linux backlog is
  `EAGAIN`), so it is bounded by the work deadline, not by a 50 ms timer.
- The decision-22 pre-scan row: the small create-once, unsynced write is bounded by
  the absolute deadline instead of entry + 125 ms.

Why: a loaded host can leave a runnable process unscheduled for hundreds of
milliseconds. The entry-anchored timers then expired before the hook did any work,
although its settings and the host's payload were ready. The settings read was cut
and the hook exited 0 with no journal row (in hold mode it also released a turn the
guard would hold); a ready payload was released as `stdin_unreadable`; a healthy socket
became a post-claim `ETIMEDOUT`; and the decision-22 row was skipped. Python has only
its configured budget, and under the same budget it journals and holds in every one of
these cases. Time the process spent unscheduled says nothing about the host or the
peer, so while the configured budget has room it must not change the outcome. The
fast-path latency target remains a property measured on an idle host
(`Test33LatencyAcceptance`), not a reason to drop the record. This supersedes the
125 ms clause of decision 22 and the startup cut on configuration in decision 24;
decision 24's release of late input is kept.

Cost: settings or a journal on a hung filesystem now hold the hook until the absolute
deadline (at most 5 s, inside the host's 10 s) instead of 100 or 125 ms. It still exits
0, and with unreadable settings there is no journal to write, as in Python. Input that
is already readable is not limited by the 100 ms allocation; the work deadline bounds
reading it.

Evidence: internal/relay/hook/adapter.go (settings read, dial, pre-scan journal) and
input.go (`readInput`, `pollDescriptor`); `Test33StalledEntryKeepsThePrescanRow`,
`Test33StalledDialStillConnects`, `Test33StalledEntryStillHolds` and
`Test33LateInputOnADescriptorIsReleased`, where each of the four changes, reverted
alone, fails at least one test. The flaky CI failures of
`Test33NativeJournalReaderPythonLive` (no journal row), `Test33ReviewD3` and
`Test33ReviewD9Large` (Python's claim files missing from Go's snapshot),
`Test33ControlDefaultStorePython/default_receipted` ("fake host did not finish": the
hook exited without connecting) and `TestDomain/hook/...recorded_elsewhere` (CI run
36445722770, observed `nothing_recorded`) were reproduced under CPU throttling. An
instrumented binary showed the settings cut, the input release and the skipped pre-scan
row firing 316-381 ms after entry.

## 33. The `.crw-lock` sidecar keeps Python's O_EXCL protocol until todo 44

Decision: `internal/runtime/record.Lock`, the read-modify-write lock beside the host record, a
staging claim and the settings records, takes `<target>.crw-lock` exactly as
`hostrecord.Locked` does: `O_CREAT|O_EXCL`, the pid written into it, a file older than 300 s
unlinked as stale, and the file unlinked on release. This is the one declared exception to the
rule that no lock file is ever unlinked or replaced, and it ends when todo 44 deletes
`scripts/runtime_install.py` and `scripts/crw_transition`, the last Python writers of those
files. `.promotion-lock` and `.crw-staging-lock` are flock(2) files under decision 7: created
once, never unlinked, released by `LOCK_UN` or by the holder's death.

Lock order (todo 38 review): a runtime directory's `<env>.crw-lock` is taken before the host-wide
`.promotion-lock`, never after it. That is runtime_install.py's order - `cmd_install` holds the
environment's `Locked` (`taking`) across its take/reclaim step and runs `_finish_promotion`, which
takes `Exclusive`, inside it - and take()'s (reclaim and the already-installed reading take the
promotion lock while the directory's is held). `crw install rollback` follows it: it finds its
target on a first reading, takes the target's `<env>.crw-lock`, then the promotion lock, and finds
the target again there, refusing with nothing written when the record moved it. A lock taken in
the other order by one command and this order by another waits out the other's timeout and
refuses. The settings records' and a claim's `.crw-lock` are leaves: nothing else is taken while
one is held.

Why: the Python installer and `plugin_transition` still write the host record, the claims and
`crw-bridge-mcp.json` under this lock. A flock on the same path would not exclude an O_EXCL
holder, and an O_EXCL file would not exclude a flock holder, so switching one side alone would
let a Go and a Python read-modify-write interleave on the same record.

Evidence: scripts/crw_runtime/hostrecord.py:271-315 (`Locked`), :335-378 (`Exclusive`);
scripts/runtime_install.py:2723 (`taking`), :2775 and :2795 (`_finish_promotion` inside it);
`TestARollbackHoldsItsTargetsDirectoryLock`;
scripts/crw_runtime/staging.py:165-209 (`Held`), :212-240 (`owner_liveness`) and :155-162 (`write_claim` under `Locked`);
scripts/crw_transition/steps.py (nine `Locked` sites); internal/runtime/record/locks.go;
`TestCrwLockIsExclusiveAndExpiresOnlyWhenStale`,
`TestPromotionLockContentionAndTheFileIsNeverUnlinked` and, under the parity tag,
`TestParity_locks_exclude_across_runtimes` (a Python holder makes Go's promotion Busy and a Go
holder makes Python's `Exclusive` raise `Busy`).

## 34. The host record stays recordVersion 1; Go install entries are additive

Decision: `internal/runtime/record` reads and writes the one host record
(`${XDG_STATE_HOME:-~/.local/state}/codex-relay-workflow/host-record.json`) as `recordVersion`
1, byte for byte as `json.dump(indent=2, sort_keys=True)` plus a newline writes it, and never
rewrites a Python entry or point. There is no record v2 and no "write beside, then rename". A Go
install entry carries `location` (`<runtime>/bin`, the directory a running Go relay reports as
its own), `entryPoint`, `environment`, `integrity` and `binaryDigest` (the binary's SHA-256),
`target` (`<goos>/<goarch>`), `reachedVia`, `digestMatchesDefinition` and `source`, and no
`interpreter`, `interpreterPath` or `installMode`. `source` is what both fault sweepers read as
the installed revision: `repositoryCommit` and `workingTreeClean` come from the binary's build
information, and `repositoryTree` and `subdirectoryTree` (equal: the Go module is the repository
root) from the tree `make build`/`make dist` stamp with
`-X .../internal/runtime/record.sourceTree=$(git rev-parse HEAD^{tree})`, and only when the
working tree is clean (`git status --porcelain` empty, the test behind Go's own `vcs.modified`):
a build with a modified tracked file or an untracked one is not built from HEAD's tree, so it
stamps nothing. A binary built without the stamp - a build from a dirty tree, and today every
GoReleaser release build, until todo 44 adds the stamp to the release workflow - records null
trees, and both sweepers then report "this copy's install entry records
an incomplete revision (repositoryTree, subdirectoryTree missing or malformed), which identifies
nothing" rather than a revision nobody measured. Component-level facts are carried through when
present and never written. The `outgoing` key has a reader again: `crw install` writes the
selection a promotion replaces as `outgoing`, in the same write that commits the new selection,
and `crw install rollback` returns to it (decision 38).

Why: the Python installer, the developer harness and both sweepers (faultsweep.py:497 and
internal/relay/faults/sweep.go:470) read the same file during coexistence and answer "not
record version 1" for anything else, which would lose every fault's installed revision.

Evidence: scripts/crw_runtime/hostrecord.py:77-160, :451-540; internal/runtime/record;
internal/runtime/record/testdata/host-record-v1.json (the relay host's record, redacted and
reduced to three installs and two points per component); `TestV1RecordRoundTripsByteForByte`,
`TestUpdateWritesWhatPythonWrites` (seven deltas against Python's bytes in
internal/runtime/testdata/goldens.json), `TestMakefileStampsTheTreeOnlyFromACleanTree` (the
Makefile run in a temporary repository: clean stamps HEAD's tree, modified or untracked stamps
nothing), `TestSourceWithoutAStampIsNull`, `Test37_SweeperReadsTheGoInstallEntry`
(internal/relay/faults) and, under the parity tag, `TestParity_python_faultsweep_reads_a_go_install_entry`.

## 35. The components definition stays definitionVersion 1, without per-target digests

Decision: scripts/crw_runtime/components.json keeps `definitionVersion` 1 and every field it has
while the Python installer and scripts/trial_startup.py read it (until todos 44 and 48).
`internal/runtime/definition` carries the fields a Go install uses - component, `consoleScript`
(the compatibility link names), version, `licencePath`, `identityTool`, `exerciseCommand` and
upstream provenance - and `TestDefinitionAgreesWithComponentsJSON` keeps it equal to the file.
No per-target binary digest is committed and no `verify-definition` re-derives one from `dist/`:
release digests live in the release's SHA256SUMS and in the host record (decision 34).
`definition.Digest` is `ops12_digest` itself: it takes the files in the order Python's `sorted()`
gives their `os.fsdecode` spelling, where a byte that is not UTF-8 is a lone surrogate, and fails
on the first path that is not UTF-8, where `.encode()` raises `UnicodeEncodeError`, so it answers
a digest exactly when Python does, and the same one.

Why: a binary digest is self-referential (the build stamps `git describe --dirty`), so a
committed one can never describe the commit it is in, and re-deriving it would make every pull
request cross-build three targets and regenerate a file.

Evidence: scripts/crw_runtime/components.json; scripts/crw_runtime/definition.py:148-206
(`verify`), :62-83 (`ops12_digest`); Makefile `VERSION`/`LDFLAGS`; .goreleaser.yaml `ldflags`,
`mod_timestamp`; .omo/ulw-execute/scope-analysis-31-46.md "# 37" (plan items to drop);
`TestDigestIsTheOPS12Walk`, `TestDigestRefusesANameThatIsNotUTF8`.

## 36. The swap gate reads a store's catalog without store.Open

Decision: the runtime swap gate settles store presence with an lstat of the path the relay's
own selection rule resolves (`store.ResolveStateDir`) and reads the store's schema objects with a
read-only SQLite connection that creates nothing beside the database (`store.OpenInPlace`), under
`store.InPlaceRead`, the rule `store.OpenStopRead` reads the Stop path's store with. The path is
first resolved as SQLite's unix VFS resolves it, symbolic links included, because SQLite keeps
`-wal` and `-shm` beside the file a link names: the sidecars examined are that file's, the
resolved path is what is opened, and the connection is refused unless `PRAGMA database_list`
names that same file. Then: `mode=ro` when both `-wal` and
`-shm` exist (a connection left them and its committed frames are read); `immutable=1` when there
is no `-wal` or it holds no frame (empty, or only its 32-byte header), since every commit is then
in the main file; and no read at all when a `-wal` holding frames has no usable `-shm` beside it
(an unclean shutdown) or cannot be examined, because an immutable read ignores frames that may
commit what the main file lacks and `mode=ro` would create the index. The swap gate reports that
store's schema unreadable, so the gate is UNESTABLISHED rather than AGREES, and the Stop owner
read fails, so the native hook asks the owner rather than trusting the main file. It never
calls `store.Open`, runs no schema script and takes no lock. The daemon and in-flight cells ask the SELECTED relay
executable (`service status`, `doctor`) as a subprocess, and the candidate's schema is what the
candidate binary prints for `crw doctor declared-schema --json`. Each subprocess is bounded (60 s
for the relay, 120 s for the candidate, then `scope.WaitDelay` for output that a process it left
behind still holds); a relay the deadline ended is Python's `TimeoutExpired`, with no exit status
and nothing it printed kept, and an answer whose output stayed open past its exit is not read.

Why: runtime_install.py asked `<interpreter> -c <program>` for all three, and a Go runtime has
no interpreter. `store.Open` refuses the live state root before todo 42 (`ErrLiveState`) and a
plain `mode=ro` connection creates `-wal` and `-shm` beside a checkpointed store. Python's
`read_only_rows` answers the swap gate through `/proc/self/fd`, which SQLite resolves to the
file a link names, so it reads the WAL beside that file; its
`ownership.stop_metadata` shares the immutable read of a WAL without its index (deferred review
finding PR190 4130471334).

Evidence: scripts/runtime_install.py:470-600 (`_STORE_TABLES_PROGRAM`,
`_CANDIDATE_TABLES_PROGRAM`, `store_presence`); internal/relay/store/hold.go (`InPlaceRead`,
`OpenStopRead`); internal/runtime/swapgate/swapgate.go (`StoreSchema`, `readCatalog`,
`DeclaredSchema`); `TestStoreReadingsCreateNothingAndAgreeWithTheDeclaredSchema` (the state
directory's file set is unchanged, and a table committed only to a live WAL is read),
`TestAStoreWhoseWALHasNoIndexIsUnreadable` (also through a symlinked relay.sqlite3),
`TestInPlaceReadRefusesAWALWithoutItsIndex`, `TestInPlaceReadExaminesTheSidecarsOfTheFileALinkNames`,
`TestARelayTheDeadlineEndedIsTimeoutExpired`, `TestARelayThatLeavesItsOutputOpenIsBounded`,
`TestACandidateThatLeavesItsOutputOpenIsBounded`,
`Test30StopOwnerReadCreatesNothing/wal-without-index` and, under the parity tag,
`TestParity_declared_schema_is_the_python_candidates`.

## 37. Two third-party readers: github.com/BurntSushi/toml and mvdan.cc/sh/v3

Decision: Go reads `config.toml` with `github.com/BurntSushi/toml`, at the version go.sum already
pins through staticcheck, now a direct requirement, for reading only. Todo 37's retention scan
reads `mcp_servers.*.command` and `args` with it; todo 38's `crw install register-mcp --owner
plugin` and the promotion's second-owner refusal read the same tables with it, validating each
table's shape (a table, a string `command`, a list of strings `args`) before comparing anything and
refusing on a file that does not parse. Nothing in Go writes `config.toml`: the user-owned
`register-mcp` append writer is retired (docs/port/inventory.md, Functions retired with evidence).

The retention scan parses hook commands, shell wrappers and the programs they hand a shell
(`sh -c` strings) with `mvdan.cc/sh/v3/syntax` v3.14.1, the parser behind shfmt, in its Bash
variant, and judges the syntax tree itself (internal/runtime/doctor/shell.go) against an
allowlisted grammar: simple commands joined by `;`, `&`, `&&`, `||`, pipes and newlines, words
literal after the scan's own expansions (`~`, `$HOME`, `$CODEX_HOME`, `${PLUGIN_ROOT}`),
redirections to such words, and a closed set of commands (`exit`, `true`, `:`, `exec`, `env`
with `-i`, `sh`/`bash`/`dash` with `-c` or a script operand, a Python program, any other program
with its arguments judged as things it may run). Every other node - a function, an assignment,
a compound command, a here-document, a substitution, a glob or brace pattern, an expansion the
scan does not make - is reported unreadable with the construct, never interpreted. Both
libraries are used for reading only. Nothing is formatted, evaluated, expanded or run through
them; the `interp` and `expand` packages are not imported.

Why: codexconfig.py refuses to approximate TOML (a hand-written reader produced ten defects), and
the retention scan must read every registered command rather than guess at text; concluding a
server is absent when it is registered is how a second bridge arrives. The same holds
for shell: todo 43 removes Python behind the scan's answer, so a mis-parse that hides a command
position is a safety defect, and a hand-written lexer and grammar for it is a large surface of
exactly that. The Bash variant, because Codex runs a hook command through a shell the scan cannot
name (dash is /bin/sh on the relay host, bash its login shell) and a wrapper names its own
interpreter: Bash's grammar is a superset of the POSIX grammar those shells share, so every
program either would run parses, and one neither accepts is a parse error, reported unreadable.

Why an allowlist rather than a fuller reading: the todo 37 sweep found one construct after
another that a walker modelling shell semantics read as a row scanned with nothing found while the
shell ran Python - a function shadowing a PATH program, a PATH assignment in any of its forms, a
glob, a runner handing `sh -c` on, a relative PATH directory, a wrapper passing `"$PY"` to an
unmodelled runner.
Emulating shell semantics statically has no bound: every fix admits the next construct. The
allowlist is conservative by construction: what the scan does not read completely is unreadable,
which keeps `clear` false until a person rewrites or removes the command, while the commands the
host really registers (the native Stop command, the pre-native `python3 -c` bootstrap, launcher
invocations and `sh ./wiring/crw-bridge.sh`) are inside it and judged.

Evidence: scripts/crw_runtime/codexconfig.py:1-25, :81 (`registration_view`), :128 (`scan`); go.mod;
internal/runtime/doctor/retention.go (`configToml`, `hookCommands`, `server`); internal/runtime/doctor/shell.go;
internal/runtime/install/mcp.go (`readServers`); `TestRetentionScanReadsTheWiringSurfaces`,
`TestTheGrammarRefusesEveryOtherConstruct`, `TestRetentionScanJudgesOnlyWhatItsGrammarReads`,
`TestRetentionScanJudgesAnMCPServerAsCodexStartsIt`, `TestRegisterMCPWritesTheRecordAndRefusesASecondOwner`,
`TestInstallRefusesASecondOwner`.

## 38. `crw install`: the release digest authorizes the unpack, the exercise earns the point

Decision: `crw install install` and `crw install update` are one code path. The archive comes
from `--from <crw_<version>_<os>_<arch>.tar.gz>` (SHA256SUMS beside it, or `--sums`) or from
`--release <tag>` (both assets fetched), must be named for this host's target, and must hash to the
one digest SHA256SUMS lists for it; the verified bytes are held in memory and unpacked from, so
nothing changes between the check and the unpack, and a mismatch refuses before anything under
the destination exists. A `--release` tag must be `v<version>` (or `<version>`) with the version in
the archive name's grammar, checked before anything is created or fetched, so it never holds a
path separator or a dot segment; the two assets are written under fixed names in the run's
scratch directory, so the tag chooses what is requested and never where anything is written. The
runtime is `<destination>/bin-<version>-<archive sha256[:12]>`, made
with an exclusive mkdir, locked and claimed STAGING under its `.crw-lock` exactly as
runtime_install.py claims `env-*`; the archive may carry only `crw`, the three compatibility
links (as links to `crw` or as its bytes) and regular files at safe relative paths that are not
names the installer or the doctor reads as control data in a runtime directory: `bin/` (the
installer places `crw` and the links there itself, and the doctor reads them and `bin/python*`), a
path component beginning `.crw-` (the staging lock, the claim, the claim's `.crw-lock` sidecar and
its atomic-write temporaries) or ending `.crw-lock` (any file's lock sidecar, decision 33), and
`pyvenv.cfg` at any level (the doctor reads a directory holding one, and every path under it, as a
Python venv, which a rollback's settings transition acts on), compared case-insensitively. No
entry may declare more than `MaxArchiveBytes` (256 MiB, the bound on the compressed archive too),
nor the entries together, and each body must be exactly the size its header declares (read
through with `io.CopyN`, then EOF): nothing is ever truncated to a bound. Every entry is judged
before anything is written, so a refused archive leaves nothing of itself, and the installer
places the three links itself. The Go install entries are recorded before the
candidate is exercised, as runtime_install.py records its entries before it measures
(`cmd_install` writes them, then calls `measure_candidate`); the candidate is exercised through
its own executables (`bin/codex-session-relay doctor` with a real socket connect; a read-only MCP
session with `bin/codex-thread-bridge` that lists its tools and calls `get_capabilities`, its
reader closed at the session's deadline whatever the bridge's descendants hold open), and only
then is one point per component (install, installDigest, codexCli, host, appServer) recorded. An
install entry does not make a directory a runtime: the entries of a candidate whose claim never
settled (a run killed during its exercise leaves them) are acted on by nothing. A rollback
requires a COMPLETE claim; a resume requires the record to select the directory, which only a
promotion's commit writes; a failed run drops its candidate's entries (`ReleaseCandidate`) before
it removes the directory; the next run of the same archive reclaims the directory and replaces
them (one entry per location); and `crw install remove` drops them. Promotion is one critical section under `.promotion-lock`: the record is read inside it,
the swap gate (decision 36) asks the selected relay, the pointer must be absent or a link this
record placed, one owner per surface is required (the bridge in `config.toml` or the plugin record,
never both, and a `codex-thread-bridge` table must name the pointer; the Stop adapter in
`hooks.json` or plugin settings, never both; and no `hooks.json` registration of the adapter and no
`config.toml` table may run through the pointer anything the runtime about to be named does not
provide - the venv's `bin/python3` on a Go runtime - because the user-owned registrations are
retired and such an entry is a second owner that would run nothing after the swap), the Stop
settings are carried to the new runtime kind (decision 18), then the selection, the pointer's placement and `outgoing` are committed in one
write (`outgoing` is the runtime the pointer leaves - the selection unless an interrupted move
left them apart - as a moving rollback records it), the pointer is renamed over and proved - it must resolve without an error (no loop, nothing
dangling) to the runtime directory itself by identity, and `<pointer>/bin/crw` must be a regular
file the user may execute; comparing resolved spellings is not proof, because a pointer that loops
resolves to the same text as a candidate spelled through it - and on any failure the pointer, the selection,
`outgoing` and the settings are put back (the settings are replaced by copying the old document
aside and renaming the new one over it, so the path is never empty and a write that fails or does
not read back leaves the old document where it was). The COMPLETE claim is written last; the
runtime the promotion replaced, selected and named by the pointer and so in service, has its claim
settled COMPLETE too (under the lock, after the pointer is read back) when it still says STAGING
with nobody holding it (an exit 3), so no later install of its archive reads it as abandoned
staging. A claim runtime_install.py wrote (a venv's) is settled in runtime_install.py's own shape
and marker, so runtime_install.py still reads the directory as its own. Finishing a promotion a
killed run committed (a resume) moves the pointer as a promotion does, so it asks the same
questions under the promotion lock before it moves it: the swap gate, the pointer's ownership and
one owner per surface, on the registrations as they stand now, refusing as a promotion refuses. A destination that is
spelled through the owned pointer, resolves inside the pointer's target, or lies inside a runtime
directory is refused before anything is created. An existing directory of the candidate's name
that is abandoned staging (a STAGING claim nobody holds, which the record does not select and the
pointer does not name) is reclaimed only under the rules `crw install remove` applies, read again
- the claim and its lock too - under the promotion lock, which `take()` takes after the directory's
own lock (decision 33): the record selects it, a pointer (the recorded one or the default) names it
or cannot be read, a live process runs out of it or cannot be ruled out, a relay daemon record
cannot be read, or a registration the host reads names a path inside it or cannot be read, each
keeps it and refuses with nothing removed or built. Where there is no process table (darwin) the
reclaim keeps it too, fail-closed, and says so with the recovery for a staging that was never
promoted: once no crw install run and nothing it started is still running, delete it by hand and
rerun. One the record's `outgoing` names was put in service by a promotion, so it is never
reclaimed either: its claim is settled COMPLETE and it is kept, which `crw install rollback`
returns to. What the reclaim removes, and a failed run's own candidate, goes through a tombstone,
as remove's does.
runtime_install.py reclaims the same staging with `rmtree` and none of these readings
(scripts/runtime_install.py:2831-2844); `crw install remove`, an operator's explicit request, still
removes an outgoing runtime nothing else uses (the Python venv after the cutover is one). A runtime
whose install finished is "already installed" only when, on the record read again under the
promotion lock, every component selects it and the owned pointer names it; a split selection (one
component on it, another elsewhere) is refused, naming each component's selection, with `crw
install rollback <dir>` as the repair - it selects every component under the promotion rules and,
the pointer already naming the runtime, swaps nothing. Nor is a runtime a host cannot launch as it
stands (`doctor.LaunchProblems`: `bin/crw` a regular file this user may execute, each compatibility
link resolving to it): its directory is named for the archive's digest, so it cannot be built again
beside itself, and the refusal gives the commands that restore it in place (`repair`: `crw`
extracted from the archive the directory is named for, `chmod 755`, `ln -sfn crw` for each link),
after which the same install answers already installed. A rollback to the outgoing runtime and a
remove is not offered, because a broken `codex-session-relay` link keeps the swap gate from asking
the selected relay. Every command that records the pointer's
placement (install, update and rollback) refuses a blank or whitespace `--issue` before it takes
a lock, since `record.PlacementRecorded` rejects a placement recorded by nobody. The destination
is fixed (decision 11): there is no `--dest`, and a record whose pointer is another link is refused
before any command acts. Paths come from HOME, CODEX_HOME, XDG_STATE_HOME and the path flags;
one holding a byte that is not UTF-8 is a usage error naming its source, and a document about to
be written that still holds one (a marker root from the environment) is refused by its writer.
runtime_install.py would record such a path surrogate-escaped; refusing it is a deliberate
narrowing, because a record, settings document or pointer carrying U+FFFD names a file that does
not exist. The execution policy path is the one path recorded surrogate-escaped, as
runtime_install.py records it (decision 18). A HOME that is relative, holds `..` or starts with
`//` is refused as well, since pathlib and a lexical join would spell the destination as two
directories. A relative XDG_STATE_HOME is refused as a usage error, as the doctor refuses it:
runtime_install.py reads it against the working directory and the XDG specification says to
ignore it, and either would put the host record somewhere the doctor and the runtime's record
readers do not look. `--execution-policy` given at all is read as a policy, so an empty value (or
an empty last value of a repeated flag) is refused as runtime_install.py refuses it, never taken
as no policy. Exit
statuses are 0 (promoted and settled, or already installed), 1 (refused: nothing moved, and this
run's directory was released unless the record or the pointer may name it), 2 (usage) and 3
(promoted and in service, only the claim unsettled: never a free destination). `crw install
rollback` returns the pointer to `outgoing`, or to a runtime directory the record lists exactly (an
install entry's `environment`, never a directory that merely contains one; an empty directory
argument is a usage error), holding the target's `<env>.crw-lock` and then the promotion lock
(decision 33), under the same
rules; both waits, the settings transition and the commit honour the command's context, and a
pointer a rollback placed is proved as a promotion's is (a Go target through its `bin/crw`, a venv
by resolving to it by identity). `outgoing` means the selection the last promotion replaced: crw install writes it only in the
write that commits a promotion or a rollback and puts it back when that promotion is undone, so an
install that fails before its promotion leaves it as it was. runtime_install.py writes the same key
with another meaning, the selection current when its install starts (written right after its
exclusive mkdir) and never put back, and has no reader of it; after a successful install of either
the two agree. While both installers run on one host, a runtime_install.py install that fails leaves
`outgoing` naming the runtime already selected, and a bare `crw install rollback` then refuses,
writing nothing, because there is nothing to roll back to; `crw install rollback <dir>` naming a
directory the record lists still returns to it. The target has to be launchable as it stands, judged
without running it: a Go runtime as the doctor judges a selected one (`bin/crw` a regular file this
user may execute, each link resolving to it), a venv as decision 18 requires. Its claim has to be
COMPLETE, or STAGING with nobody holding it where the record's selection or `outgoing` proves a
promotion committed it (an exit 3, or a run killed after its pointer moved); the rollback then
settles it last, as resuming does, and answers 3 when it cannot. The runtime it leaves, in service
with its claim still STAGING, is settled COMPLETE as well, so a later install of that archive keeps
it rather than reclaiming it. Where the pointer already names the target (a run killed between its
commit and its pointer move) no runtime is replaced, so the swap gate is not asked and only the
selection moves; every other check still applies. The `outgoing` a rollback records is the runtime
the host leaves - the one the pointer names, which is the selection unless an interrupted move left
them apart - and a rollback that moves no pointer keeps `outgoing` as it was (or, where it named
the target itself, records the one runtime the record selected instead). A bare rollback over an
interrupted move (the pointer names a recorded runtime the record does not select) refuses and names
both, since returning to `outgoing` could undo a committed choice. A claim written into a
directory runtime_install.py claimed (an `env-*` one, or one whose claim it wrote) keeps its shape,
`writtenBy` runtime_install.py, so runtime_install.py still reads it; and a claim is never written
into a directory that is gone, which the claim's lock would otherwise make again. No rollback
rewrites the Stop settings (decision 18). For a Python venv target the gate's schema cell compares the store with this build's declared
schema, which stands for the Python runtime's because the DDL is identical (decision 14) and no Go
release changes it before the commit point (docs/port/cutover.md). `crw install remove <dir>`
deletes one `env-*` or `bin-*` directory directly under the destination. It is taken by its name
under the destination and judged by file identity (a path counts when, as written or once its links
are followed, it or an ancestor is the directory's own file, or is an entry of its name in the
destination's file), so naming it through an alias of the destination - a symlink, a bind mount,
a case-folded spelling - or a record written through another spelling cannot pass a check the
canonical spelling fails. It deletes only when the record does
not select it, the pointer does not (and is established not to) name it, it carries a readable
claim of runtime_install.py's or crw install's whose lock nobody holds, no live process runs
out of it (its `/proc/<pid>/exe`, its working directory, or what its argv runs - argv[0] when the
executable could not be read (a readable one settles what runs) and it holds no whitespace (a
process title such as sshd's is no path), the command `env` runs, and an interpreter's script
operand after its options (the interpreter known by argv[0] or by its executable), a relative one
resolved against `/proc/<pid>/cwd`, with a Python's `-c` and `-m` and a shell's `-c` ending the
options - resolving inside it; a process whose argv runs something relative to a working
directory it cannot read is not ruled out), and no registration the host reads names a path inside it. The registrations are read by the
retention scan's own readers (`doctor.RegisteredInside`, rows 4, 5, 8, 9 and 10: every
`crw-*.json` record, so the Stop settings' `relayExecutable`, `adapterEntryPoint` and
`adapterInterpreter` and the bridge record's `bridgeExecutable`; the cached plugin declarations;
the `crw-stop-hook.py` launcher copy; hooks.json; config.toml's `mcp_servers` commands and
arguments; and the settings document a Stop command names), and a path counts when it lies inside
the directory as written or once every link is followed, including each interpreter and script it
is followed through. These are started afresh by each new session, so no process table shows them
between sessions, and runtime_install.py never faced the question because it never removes a
settled runtime; a registration that cannot be read or judged refuses too. Then, in
runtime_install.py's order (`_install_failed` has `release_candidate` drop a candidate's entries
before it removes the directory), the directory is first renamed, in one atomic step, to its
tombstone `<destination>/.crw-removing-<name>`; its install entries are dropped in one write under
the host record's lock, where the selection is read again, and in the same write `outgoing` is
removed when it names anything inside the directory, so a bare `crw install rollback` then refuses
because the record carries no outgoing selection instead of being sent to a directory that is
gone and whose entries are not in the record; only after that write lands is the tombstone
deleted. A drop that cannot be written renames the directory back and refuses; a deletion that
does not finish exits 3 naming the tombstone. A kill anywhere in that sequence leaves either the
whole directory or a tombstone - never a directory under the runtime's name with its claim gone,
which every command would then read as somebody else's. `crw install status` lists each tombstone
(`interruptedRemovals`), and `crw install remove` of the tombstone, or of the name when only its
tombstone is left, finishes it, under the directory's lock and the promotion lock: the tombstone
is finished only when it is one this command began - it carries a readable claim of crw install's
or runtime_install.py's whose staging lock nobody holds, or it is empty - and no process runs out
of it and no registration names it; somebody's directory of that name, holding files and no
claim, is refused by name and left alone, by remove, by the reclaim and by a failed run's
release (which then deletes its own candidate in place), and status reports it as not ours
(`ours: false`). A tombstone is deleted with its claim last (everything else, then the staging
lock, then the claim, then the empty directory), so a deletion that stops part-way leaves it
claimed or empty, never a claimless half. A tombstone is recovery state: status lists every
unfinished one, and it is finished with `crw install remove`, not deleted by hand, because
finishing also drops what the record still lists under the name. What the record still lists under the name (its
entries, an outgoing naming it) is dropped first, unless a runtime was installed under that name
again. Remove, the reclaim and the already-installed reading take the directory's `<env>.crw-lock`
first and the promotion lock after it, the order of decision 33, and the host record's `.crw-lock`
inside both. Every command's lock waits before its first write honour its context
(`record.LockContext`, `record.PromoteContext`, `record.UpdateContext`): the directory, promotion
and ownership locks, the settings and bridge-record locks of `hook`, `register-mcp` and a
promotion's or rollback's settings transition, a build's first record writes and the commit of a
selection (which undoes the settings transition when it is interrupted); so a command interrupted
(SIGINT, SIGTERM, SIGHUP) while it waits stops waiting, writes nothing, and answers so
(`register-mcp` and `hook` with the outcome `interrupted`). A wait inside a sequence already
under way - a restore after a failed promotion or rollback, the entry drop after a directory was
set aside, the claim settled after a promotion, the snapshot that follows it - is bounded and runs
to completion whatever the context says, because stopping there would leave the host
half-written. The process table is read so that
nothing unread passes for absent. A pid whose entries are gone (ENOENT, ESRCH: exited, a zombie, a
kernel thread) is skipped. The kernel shows a process's exe only with ptrace access, which it
refuses for another user's process and for this user's own when it holds capabilities the reader
does not or is not dumpable (a desktop's `systemd --user` holds CAP_WAKE_ALARM), so a refused
(EACCES, EPERM) exe is judged by the cmdline every user may read: its interpreter and script,
spelled and resolved, inside the directory refuse; a cmdline that is hidden as well (procfs
mounted hidepid) or that starts one of this runtime's executables by a bare name leaves the
process not ruled out, which refuses too, naming the pid and why (`unreadableProcesses`). A
process of this user whose exe fails for any other reason, or whose cmdline cannot be read, is
not ruled out either. Relay daemons are also read from their records, as the retention scan reads
them (`doctor.RecordedDaemons`: every daemon.json and scope registry claim; a pid counts while its
start time and boot id match this process table), and a record that cannot be read refuses. The
reading rests on one assumption, stated in every answer that depends on it (`processTable`): the
process table is this host's, in this command's PID namespace, so a process in another PID
namespace (a container sharing the directory) or on another host (a network home) is not seen,
and `crw install remove` is to be run where the runtime's processes run. The reclaim of an
abandoned staging applies the same reading. Live
processes are read from procfs, so on a platform without one (darwin) whether a relay or bridge still runs out of the directory
cannot be established and `crw install remove` always refuses there (fail-closed), saying that
this platform has no process table it can read and giving the recovery by hand as
`recoveryRequires`: stop the relay daemon started from it (`<dir>/bin/codex-session-relay service
stop`) and end every Codex session whose bridge it started, delete the directory, then run `crw
install status` to see that the record and the pointer still name the intended runtime (the
directory's install entries stay in the record, where a rollback naming it is refused because the
directory is gone). A darwin process reader is deferred (docs/port/refactor-backlog.md). `crw doctor` now makes the App Server observation decision 41
left to this todo: with `Options.AppServer` unset it runs the same read-only session with the
selected runtime's `bin/codex-thread-bridge` (`exercise.Session`) and compares its
`get_capabilities` answer as a point's `appServer` dimension, so a real diagnosis of a Go host can
reach `installed: verified`; a bridge that answers nothing leaves the dimension unread, which stops
classification (decision 41). The report's `appServer` member names the bridge asked and whether
it answered.

The execution policy after the cutover is delivered by todo 34 (decision 26): `crw-bridge.sh`,
started with only HOME set from the plugin cache under the Codex home, execs the runtime, which
reads the version-2 record `crw install register-mcp` writes, enforces its policy and names
`crw install register-mcp --owner plugin --execution-policy <file>` as the repair
(`TestTheWiringLaunchersStartTheGoBridgeUnderTheRecordedPolicy`, whose legacy case keeps
`crw_bridge_mcp.py` covered). The Python launcher keeps naming `runtime_install.py`, which is the
host's installer until the cutover.

Why: the plan's per-target digests in the components definition were dropped (decision 35), so
the release's SHA256SUMS is the only digest authority a host can check; everything else carries
runtime_install.py's install properties by behaviour rather than by code.

Evidence: scripts/runtime_install.py:2610-3296 (`cmd_install`; :2898 the stage write of `outgoing`,
:2966-2974 the install entries written before `measure_candidate`), :3297-3636 (`_settle_claim`),
:4027-4414 (restore and release; :4326-4333 `release_candidate` before `rmtree`), :4472-4627
(`measure_candidate`), :5163-5617 (`register-mcp`), :2300-2589 (`hook`);
scripts/crw_runtime/staging.py:350-352 (a finished, unselected environment is kept);
.goreleaser.yaml (archive names, links, SHA256SUMS); internal/runtime/install and its tests
(`TestRemoveRefusesARuntimeARegistrationStillNames`,
`TestRemoveDropsTheInstallEntriesBeforeTheDirectory`, `TestAnArchiveCannotPlantControlData`,
`TestAnOversizedOrShortEntryIsRefusedBeforeAnythingIsWritten`, `TestAnOversizedOrShortEntryIsNotInstalled`,
`TestAReleaseTagIsCheckedBeforeAnythingIsFetched`, `TestAReinstallNeverReclaimsARuntimeThatWasInService`,
`TestReclaimKeepsAStagingThatMayStillBeInUse`, `TestLiveProcessesRuleOutOnlyWhatTheyRead`,
`TestAPromotionProvesThePointerItPlaced`, `TestLandedAtIsWhatAHostReaches`,
`TestADestinationInsideTheRuntimeIsRefused`, `TestSettlingAPythonClaimKeepsItPythons`,
`TestRemoveKnowsARuntimeByIdentityNotSpelling` (bwrap), `TestReclaimWithoutAProcessTableSaysHowToRecover`,
`TestRemoveReadsTheRelayDaemonRecords`, `TestAnInterruptedWaitRemovesNothing`,
`TestAnInterruptedRemovalIsFinished`, `TestAnInstallCommandStopsWhenAsked` (cmd/crw),
`TestRemoveTakesTheDirectoryLockBeforeThePromotionLock`, `TestAPromotionRecordsTheRuntimeThePointerLeaves`,
`TestATombstoneNeedsItsClaim`, `TestAResumedPromotionAsksForSecondOwners`, `TestAnInterruptedRollbackMovesNothing`,
`TestInterruptedRegistrationsWriteNothing`, `TestARollbackProvesThePointerItPlaced`,
`TestADamagedRuntimeIsNotInstalled`, `TestRemoveSeesAScriptStartedByARelativePath`,
`TestRemoveAndReclaimRefuseAProcessTheyCannotRuleOut`, `TestRemovingTheOutgoingRuntimeClearsOutgoing`, `TestOutgoingIsWrittenOnlyByAPromotion`,
`TestAnUnsettledCandidatesInstallEntriesAreNeverActedOn`); internal/runtime/doctor/references.go;
internal/runtime/exercise (`TestTheSessionClosesItsReaderAtTheDeadlineWithoutACopyingGoroutine`);
the rollback rules (`TestARollbackReturnsToAPromotedRuntimeWhoseClaimNeverSettled`,
`TestARollbackThatMovesNoRuntimeAsksNoGate`, `TestARollbackRefusesARuntimeThatCannotBeLaunched`,
`TestANamedRollbackNamesARuntimeDirectory`, `TestAUserRegistrationThroughThePointerIsASecondOwner`,
`TestARollbackNeedsAnIssue`, `TestASplitSelectionIsNotReportedInstalled`, `TestTheDestinationIsFixed`,
`TestPathsTheInstallerCannotSpellAreRefused`, `TestAnEmptyRollbackDirectoryIsAUsageError`,
`TestAnEmptyExecutionPolicyIsRefused`, `TestTheSecondOwnerRuleKnowsThePointerByIdentity`,
`TestOutgoingIsTheRuntimeThePointerLeaves`, `TestARollbackHoldsItsTargetsDirectoryLock`,
`TestAClaimRuntimeInstallPyWroteStaysOneItCanRead`, `TestAClaimIsNotWrittenIntoADirectoryThatIsGone`);
.omo/ulw-execute/scope-analysis-31-46.md "# 38".

## 39. A caller's cancellation claims every answer the send has not yet used

Decision: once the caller of `Adapter.Send` has cancelled, the send uses no further answer
from the host or from the pre-start guard, and sends nothing more. The worker asks the
caller's context directly after each host call and after the guard returns, before it
reads what they answered or the error they returned, and once more before `turn/start`.
When that check finds the cancellation, the receipt is `outcome_unknown` with no error text,
which is what Python's `CancelledError` handler writes, and the worker gives back the
caller's own context error instead of the transport-shutdown message. The receipt statuses
and fields are the ones Python writes; what changes is that which one a send gets no longer
depends on scheduling.

Why: Python's `_guarded_send` takes a cancellation at the `await` it is suspended in. Its
answer or error is never read and no later request is made, whether the answer arrived
before or after the cancel. The Go worker only learned of the cancellation when the
goroutine that `context.AfterFunc` starts cancelled its context. An answer that landed
first was used, and the outcome then depended on which goroutine ran first:
`in_progress_or_unknown` left on a finished send (CI run 36523111203, and 44 of 2,000 local
runs of the `turn/start` stage), `accepted` after the caller had gone, `failed` from an
`active` read, `not_attempted` from a guard refusal, and a `thread/resume` sent after the
caller had cancelled during `thread/read`. Python writes `outcome_unknown` in each of those
cases. The `dispatchGate` mutex that guarded the old pre-dispatch check is removed: it
ordered that check against the propagating goroutine, which cannot change what the caller's
own context reports.

Evidence: internal/relay/adapter/transport.go:164 (`context.AfterFunc` only stops the work),
:196 (`awaited`), :226, :252, :277, :293, :297 (the checks), :317 and :346 (the receipt and the
returned error); packages/codex-session-relay/src/codex_session_relay/bridge_adapter.py:1314-1318;
`Test28CallerCancellationStageParity` in internal/relay/adapter/cancellation_python_test.go.
It runs every stage (`before`, `thread/read`, an `active` read, `thread/resume`, the
guard, `turn/start`) twice. In the first run the caller's cancellation propagates at once.
In the second, `heldCaller` holds it back until the worker has finished. Both runs are
compared with the live Python oracle, where each answer also arrives after the cancel. With
the transport change reverted, the held run fails every time: `failed`, `not_attempted`,
`accepted`, and an extra `thread/resume`.

## 40. A stopped process has exited when its pidfd says so, in both runtimes

Decision: `service stop` counts a supervisor or worker as exited only when its pidfd is
readable, which the kernel reports once the leader has exited and the thread group is empty,
after every thread has closed its descriptors. Go's `ProcessHandle.Wait` already asked this.
The retained Python fence's `ProcessHandle.alive()` read the leader's `/proc/<pid>/stat` state
and took `Z` for exited; it now polls the pidfd the handle already holds, and reads `/proc` only
for a handle without one, which no caller asks. Outside the window below nothing changes: a
stop still reports `exited` or `gone` at its 100 ms grace boundaries with the same answers,
exit codes and records, and the worker-policy reading still refuses a `Z` leader through
`process_state` beside `alive()`.

Why: a multithreaded process killed by a signal can show its leader as `Z` while another
thread is still exiting and still holds the descriptor table the threads share. The
supervisor's `daemon.lock` descriptor is in that table, and the worker inherits the same open
file description. The fence's stop reported such a process `exited`. Its final nonblocking
acquisition then found the lock the dying thread still held. With both processes counted as
gone, it answered `replaced_by_new_launch` ("the daemon lock was taken during this stop"),
exit 2, and left `daemon.json` naming both processes; `restart` propagates that refusal. No
launch existed. Decision 27 put this down to Python's scheduling, but the fence was misreading
process state. The misreading failed `Test29GoRecordsCarryANullFenceBuildThePythonFenceReads`
on a 4-core runner (PR #189, run 36523111203, go-product test-4) at the fence's stop of the
service it had itself started. No Go process holds or takes the lock in that sequence: the Go
service was stopped and waited on through its pidfds before the Python start acquired the lock.
Fence start/stop cycles with an instrumented stop, on four CPUs shared with eight busy loops,
answered `replaced_by_new_launch` in 4 of 90 cycles. Each time `_terminate` had returned
`exited` for both processes with the leader in `Z` and one sibling thread still `R`. No other
process held the lock; in one cycle the sibling still listed `daemon.lock` among its
descriptors at the probe, and in the three that timed it the lock came free 3-5 ms later. With
the fix, 0 of 100 cycles failed under the same load. Starving the service processes'
non-leader threads (SCHED_IDLE beside the same busy loops, neither runtime changed) failed the
Go test in 7 of 10 runs before the fix and 0 of 50 after it. This supersedes decision 27's
paragraph on Python reporting `replaced_by_new_launch` without a replacement launch.

Cost: in the window that used to fail, a fence stop now waits one more grace boundary for the
slowest thread, as Go's stop does. While the fence waits for the supervisor, the worker, killed
by its parent-death signal, is now more often reaped before stop reads it. `gone` is therefore
more frequent than `exited`, which decision 27 already allows.

Evidence: `packages/codex-session-relay/src/codex_session_relay/service.py:359-381`
(`ProcessHandle.alive`), `:1424-1432` (the final probe),
`internal/relay/service/process_linux.go:53-70` (`ProcessHandle.Wait`),
`internal/relay/service/ownership.go:154` (`waitTermination`); `tests/test_service.py`
(`Ownership.test_stop_waits_for_every_thread_of_a_worker_whose_leader_has_exited`) and
`internal/relay/service/reap_test.go` (`Test29D1StopWaitsForEveryThreadOfTheWorker`). In both,
the worker's leader has exited while a thread that blocks SIGTERM holds the daemon lock. Each
runtime's stop answers ok with worker `exited` and leaves the lock free. Restoring the
leader-state reading (the old `alive` body, or `waitTermination` asking `ProcessState`) makes
each answer `replaced_by_new_launch` with that thread still running.

## 41. `crw doctor` judges each registration as the program that reads it accepts it

Decision: `crw doctor --json` says `own`, `agrees` or `installed: verified` for a Go runtime only
when the host would actually run the selected runtime for that component, and a reading it did
not or could not take is an unread signal that stops classification (`unreadable`, `installed:
not_verified`), never a dimension dropped from the comparison. Each registration is judged as a
whole document, by its consumer's own acceptance rule, before any field is compared with the
selected `bin/crw`:

- the Stop settings (`crw-completion-hook.json`) through `hook.ReadSettings`, the check every Go
  Stop runs first: a document it refuses (`configVersion`, `markerRoot`, `mode`, the owner, a
  relative `relayExecutable` or adapter path, a plugin budget over 7 s) is a relay conflict
  naming the complaints, since the hook then runs no relay. It contains every gate of the
  packaged launcher's `adapter_call`. A plugin owner's adapter must be the system env, recognised
  by the path it is run under (`/usr/bin/env` or `/bin/env`, resolving to a native executable
  regular file), and an entry point holding `=` is a conflict because env reads it as an
  assignment;
- every Stop command in `<CODEX_HOME>/hooks.json` that runs a Stop adapter, for every owner,
  read by the retention scan's own Stop-command reader (`readStopCommand`: its allowlisted
  grammar, following `exec`, `env` and `sh -c`), so one reader answers both which settings a
  Stop reads and which hook it runs: the selected `crw-completion-hook` (or `crw hook`)
  executed by path and reading these settings agrees; the checkout's `completion_hook.py`,
  another runtime's hook and a hook handed to an interpreter conflict; a bare hook, a word or
  construct outside the grammar, a script that may run the adapter itself, other settings
  (named, or `$CRW_COMPLETION_HOOK_CONFIG` for a hook naming none) or a hooks.json it cannot
  read are unreadable;
- a plugin-owned `crw-bridge-mcp.json` through `crw_bridge_mcp.py`'s record contract
  (`recordVersion` 1 or 2 by Python's `==`, `serverName`, an absolute `bridgeExecutable`, `args`
  a list of strings, no policy in version 1, a version-2 policy that exists, is regular and
  hashes to its digest): a record the launcher refuses before exec is a bridge conflict;
- `config.toml`'s `mcp_servers` read whole with `codexconfig.registration_view`'s shape rules (a
  malformed table makes the configuration unreadable), then every table that starts the bridge:
  `codex-thread-bridge`, the table a user-owned record names, and any whose command or arguments
  run the bridge. A bare command is looked up only on the table's own `env.PATH`; without one it
  is unreadable.

No value a consumer requires to be absolute is looked up on the doctor's own PATH. The Codex CLI
version, the host name and the App Server identity are point dimensions
(`runtime_install.classify_component`): an unread one stops classification, and `codex
--version` whose output a descendant holds open past a 5 s wait delay (as `scope.WaitDelay`) is
unread. The App Server identity is asked once, through `Options.AppServer` (`func(ctx, bridge
string) *string`, given the selected runtime's `bin/codex-thread-bridge`). Before todo 38 the
doctor made no such observation, so a Go component was `unreadable` with it named and `notChecked`
listed it; todo 38 wires it (decision 38): an unset `Options.AppServer` is a read-only MCP session
with that bridge (`exercise.Session`) whose `get_capabilities` answer is the dimension, a bridge
that answers nothing leaves it unread and the component `unreadable` with the bridge named, and
`notChecked` is empty. The skill links are not a signal for a Go install, unlike in
`classify_component`: an installed product takes its skills from the plugin payload the Codex
marketplace installs, and skill links are a developer-checkout concern (`crw-dev skills link`,
todo 39) that no release archive or installer makes. The host record's
`pointer.path` is read through `Path()` (a trailing `/`, `//` and `/./` name the same link), and
the state home, Codex home and default destination expand the home as `pathlib` does (HOME, else
the passwd entry, `~user` from that user's entry); one that cannot be established, or would be
relative, is reported (`hostRecordState: ACCESS_ERROR`), never read as a clean host.

Why: a field judged on its own reported `agrees` for documents the hook, the launchers or Codex
refuse, and for commands they never look up on PATH, and a dimension left out of `wanted` either
let a point that recorded none cover (App Server) or reported `unmeasured` from a reading nobody
took (Codex CLI, host name). Python's diagnose reads the Stop hook not at all and only the
`codex-thread-bridge` table; the Go doctor judges more, so it judges what the host executes. A
`codex --version` whose output an inherited descendant kept open held the whole diagnosis until
that descendant exited.

Evidence: internal/runtime/doctor/registration.go (`stopSettings`, `interpreter`, `stopHooks`,
`stopCommand`, `hookCall`, `pluginBridge`, `policy`, `codexConfig`, `mcpCommand`);
internal/runtime/doctor/retention.go (`readStopCommand`, `stopAdapterIn`, `settingsOf`); internal/runtime/doctor/doctor.go
(`observe`, `classifyGo`, `CodexVersion`, `Diagnose`); internal/runtime/exercise (`Session`); internal/runtime/record/home.go; internal/relay/hook/
settings.go (`Complaints`, `ReadSettings`); plugins/crw/wiring/crw_stop_hook.py (`adapter_call`);
plugins/crw/wiring/crw_bridge_mcp.py (`main`, `policy_environment`); scripts/crw_runtime/
codexconfig.py (`registration_view`); scripts/runtime_install.py (`classify_component`,
`_starts_this_bridge`); scripts/crw_runtime/hostrecord.py (`state_home`). Tests:
`TestDoctorNeverLooksARegisteredCommandUpOnItsOwnPATH`,
`TestDoctorJudgesTheStopSettingsAsTheHookAcceptsThem`, `TestDoctorJudgesTheLauncherInvocation`,
`TestDoctorJudgesTheStopCommandsInHooksJSON`, `TestDoctorJudgesTheBridgeRecordAsTheLauncherAcceptsIt`,
`TestDoctorReadsTheCodexConfigurationWhole`, `TestDoctorJudgesTheBridgeUnderEveryTableName`,
`TestDoctorStopsOnTheReadingsItDoesNotMake`, `TestDoctorComparesTheAppServerDimension`,
`TestDoctorKeepsAnUnreadDimensionInTheComparison`,
`TestDoctorReadsTheRecordedPointerThroughPath`, `TestDoctorNeverReadsAnUnestablishedHomeAsACleanHost`,
`TestStateHomeExpandsTheHomeAsPathlibDoes`, `TestCodexVersionIsUnreadWhenADescendantHoldsItsOutput`; under the parity tag
`TestParity_the_bridge_record_is_refused_where_the_launcher_refuses_it` (22 records, the real
launcher) and `TestParity_state_home_is_hostrecords`.
