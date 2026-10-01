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
promotion or rollback rewrites that document.
`/usr/bin/env <destination>/current/bin/crw-completion-hook <settings>` runs the Go hook on a Go runtime and, on a venv, the fence release's
`crw-completion-hook` console script (`codex_session_relay.stopadapter:main`, which reads
`argv[1]` as its settings; its `complaints()` accepts `/usr/bin/env`), and `relayExecutable`
`<destination>/current/bin/codex-session-relay` is the Go link or the venv's console script. The
only rewrite is of a Python-era document, when the pointer moves onto a Go runtime: the relay
host's Python-era document (`adapterInterpreter` `.../current/bin/python3`, a path that vanishes
when the pointer leaves the venv) is archived on the first Go install, and on any later install,
update or rollback onto a Go runtime that finds one put back by hand - under the
`.superseded-<stamp>` name `steps.retire` gives, as a hard link to the same file (a copy where the
filesystem refuses a link), never deleted - and replaced by its
Go variant (the same host facts, only the two adapter keys moved) in one rename, inside the
promotion and before the pointer moves. Both documents run while the pointer still names the
venv, so there is no window. That holds for the Stop commands that run the adapter keys, the
legacy launchers. The native declaration (decision 26) runs `current/bin/crw hook
--plugin-launch` and reads only the document's owner, so it evaluates this document on a Go
runtime and reaches nothing on a venv, which has no `bin/crw`; a host rolling back to a venv holds
the pre-native declaration first, because the payload goes back before the runtime
(docs/plugin-packaging.md "Update and roll back"), and the rollback tests run that declaration.
`crw install hook --owner plugin` replaces a Python-era document on a Go host only by settings that
record the same host facts; other flags answer `config_differs`
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

(Superseded in part by decision 57: the status reading is gone. The recognition of the shipped
native command stays in the hook-file reader the installer uses, `nativeRegistration` in
`internal/relay/hook/registrations.go`; resolving and probing the registered target went with
the reading.)

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
other runtime owns the record"). The fence and the Go owner also read the request alike (the
backlog before todo 42): a `mode` or `now` that is neither null nor a string is refused like a
`socketPath` or `program` that is not a string (`TypeError: guard mode must be a string`), so no
other value reaches a verdict or the observation it records; `protocol` is compared with 1 as
Python compares it and `noRecord` read by its truth value; the 5 s read bound is on the whole
request line, not on each read, so a peer that trickles its line is answered `TimeoutError: timed
out` by both; and the scanner's recursion budget covers the calls that raise a refusal near its
edge (test_fence.py's `test_python_control_server_bounds_the_whole_request_line`). Decision R3S-2
ends the Go owner's reading of a request as `control.py` read it: it reads the line strictly, in
Go's words, and keeps the 5 s bound on the whole line
(`Test30ControlPeerFailuresAreAnsweredWithTheHostRecord`). Tests: `Test33RoutedSelectionRefusals`,
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
  `_DIGEST` naming another). Its stderr is the Python launcher's byte for byte, down to repr()'s
  escape of every character `str.isprintable()` rejects and the stream's escape of a lone
  surrogate, except for the three repairs, which name `crw install register-mcp --owner plugin`,
  the record's writer since todo 38, where the Python launcher names runtime_install.py. The
  Python launcher keeps its text: runtime_install.py is the host's installer until the cutover.
- The record's strings reach the file system and the exec as `os.fsencode` spells them
  (`reading.FSEncode`, the one fs-encoding `register-mcp` and the doctor use too). A lone surrogate
  in U+DC80..U+DCFF, which is how runtime_install.py and `crw install register-mcp` (decision 18)
  record a byte of a path or argument that is not UTF-8, is that byte again in the policy path the
  launcher opens, compares with an inherited variable and exports, and in every argument. Where Python's `execv`
  raises instead (a `bridgeExecutable` or argument holding any other lone surrogate, or a NUL),
  the Python launcher exits 1 with a traceback and starts nothing; the Go launcher refuses that
  record with exit 2 naming it.
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
  adapter Go replaces. So the one document `crw install` writes for both runtime kinds (decision
  18: owner plugin, `/usr/bin/env`, `current/bin/crw-completion-hook`) is evaluated as it stands,
  the Go variant an update writes over a Python-era document included; its adapter keys serve the
  legacy launchers only. Without the flag nothing changes: the legacy launchers' `[adapterInterpreter,
  adapterEntryPoint, settings]` call through todo 38's `/usr/bin/env` settings, and a user
  hook-file entry, keep the todo-33 settings precedence. A host carrying both registrations
  evaluates each Stop once while the settings name the user as owner: the flag's stand-down is
  what makes it once. Under settings the plugin owns, both evaluate it, because the hook-file
  entry's path reads no owner and cannot (the legacy launchers reach `crw hook` without the flag
  under those same settings); an established Stop is then kept to one guard request by the event
  claim and journals a second `duplicate_invocation` row, and an unestablished one asks the guard
  twice. Python does the same (`crw_stop_hook.py` stands down only for an owner other than the
  plugin). Only the installers keep that state away: `crw install hook --owner plugin` and
  runtime_install.py each refuse to register a second owner, so it takes a hand edit. Hook status
  (decision 23) reads the flag in a registered command as naming no settings file, not as a
  settings path.
- `runtime_install.py register-mcp`'s launcher probe stages a stand-in runtime at the probe
  HOME's pointer, `current/bin/codex-thread-bridge`. The stand-in accepts only `--plugin-launch`
  and hands off to this checkout's `crw_bridge_mcp.py`, the reference the Go contract is compared
  with. The probe still judges the package's declared command, arguments and working directory
  under the App Server's environment.
- No compatibility floor between the payload and the runtime is checked. A runtime built before
  this decision reads the flag as something else: its `crw hook` takes `--plugin-launch` for a
  settings path relative to its working directory, finds nothing there and exits 0 with empty
  stdout and stderr and no journal row, and its bridge refuses the flag with exit 2 from argument
  parsing. A pointer at a Python `env-*` runtime has no `bin/crw` for the hook, and its bridge
  refuses the flag the same way. The bridge therefore never runs without the recorded policy,
  but every Stop is released unrecorded. The guard is an order: the pointer names a runtime
  built with this decision before a payload declaring these commands is cached, and a rollback
  returns the payload before the runtime (docs/plugin-packaging.md "Update and roll back").
  The older hook answers the flag exactly as it answers a missing settings file, so its exit
  status and output do not tell it apart. The bridge half of the same binary does, without side
  effects: against an empty CODEX_HOME, `codex-thread-bridge --plugin-launch` exits 2 in all three
  cases, with Go's `flag provided but not defined: -plugin-launch` from a runtime built before this
  decision, `crw bridge launcher: no record at ...` from one built with it, and argparse's
  `unrecognized arguments: --plugin-launch` from a Python runtime. Nothing runs that probe yet.
  The Python half needs no probe, since no Python runtime reads the flag: `crw install rollback`
  onto a Python `env-*` runtime reads every cached version's declarations with the retention
  scan's row-5 readers (`doctor.PluginLaunches`, the shell scripts they run included) and
  refuses, with nothing written, while any runs a program through the pointer with
  `--plugin-launch` or cannot be read (`TestARollbackToAVenvRefusesWhileTheNativePayloadIsCached`
  in internal/runtime/install/rollback_test.go). The order stays the only guard for a Go runtime
  built before this decision, and `crw doctor` and `crw install status` do not report the pair.
- `scripts/plugin_transition.py` `transition`, `disable` and `remove` refused with exit 2 and one
  line on stderr before reading the host. Their steps hand the surfaces only to a Python runtime
  (preflight requires `<dest>/current/bin/python3` and a cached payload equal to this checkout's),
  and this checkout's payload now declares these commands, so on every host they accepted an
  applied transition removed the working Stop registration and bridge table, reported every step
  settled, and left a Stop hook and a bridge that could not run there. `inspect`,
  `check-declaration` and `swap-state` still answered. Todo 39 then retired the tool rather than
  porting it (there is no `crw install transition`) and deleted it with its tests; a host that
  still needs it runs it from a revision before this decision (docs/plugin-transition.md).

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
`internal/pluginwiring/launch.go`; `launch_test.go` (51 record cases with stderr and exit equal
to the Python launcher run from the same cache layout, the repairs rewritten, among them
surrogate-escaped policy paths and arguments and the characters repr() escapes; args
pass-through; the policy reaching the bridge; the re-entry refusal; the records Python's `execv`
raises on); `internal/bridge/settings/repr_test.go`; `wiring_test.go` (the
declared commands through `sh` against a fake and a real pointer; the running bridge's
`/proc` cmdline, environ and exe); `internal/relay/hook/pluginlaunch_test.go` (owner and
configVersion stand-down, double registration evaluated once under user-owned settings and
twice under plugin-owned ones, the settings path, the status reading);
`internal/relay/hook/status_native.go`;
`internal/runtime/install/wiring_test.go` (`TestTheNativeStopCommandJournalsTheStopThroughThePointer`;
`TestTheWiringLaunchersStartTheGoBridgeUnderTheRecordedPolicy`, whose native case starts
`crw-bridge.sh` from the cache layout with only HOME set against a record `crw install
register-mcp` wrote; `TestLegacyStopLaunchersReachTheGoHook`;
`TestARollbackToAVenvNeverRewritesTheSettings`, the native declaration evaluating the Go-era
document an update wrote and reaching nothing on a venv; `TestRegisterMCPRecordsANonUTF8PolicyPathAsPythonDoes`,
both launchers starting under a surrogate-escaped policy path); scripts/ci/tests/test_plugin_wiring.py
`BridgeRecordPolicyTest`; scripts/ci/tests/test_plugin_transition.py `RetiredCommandsRefuse`
(deleted with the tool by todo 39).

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
`scripts/port/dump_contracts.py` derived from that enum (until todo 44), does not list it. Absent-store
initialization happens on write or daemon opens only (decision 30). A partial store (a
mirror or a write gate without `D`) is never read, created or repaired: every form of both
runtimes that opens the store, read or write, answers the fence writer's refusal, reason
`store_owned_by_other`, exit 2, detail `partial store: write-gate.lock without a database` for
a gate alone and, beside a mirror, the refusal `check_start` meets first (the mirror's own when
it cannot be read, else validate's `missing or unsupported writer protocol`). A `takeover.json`
link naming no file reads as no record but is there, so beside no `D` it is partial with a
gate's answer, gate or none; the fence's writer, which had initialized a store over it, refuses
it too. The `service` forms but `status`, and `daemon` given `--socket`, refuse it in the same
words before they write anything into `S` or the scope registry: Go in its start preflight
(`store.StartPreflight`), the fence in `main` ahead of `check_start` (`ownership.refuse_partial`),
since `check_start` passes a store whose mirror reads as no record as unfenced. A gate beside
no `D` and no mirror that another opener holds EX is a first opener still creating the store,
found by probing the gate without waiting. Both wait for it, polling every 10 ms without
blocking, within the 30 s creation bound (`ownership.CREATION_WAIT_SECONDS`,
`store.CreationWait`), then judge the store again from the start: the other runtime's store is
refused in validate's words, a gate let go with no `D` is partial, and a creator still holding
the gate at the bound is refused with reason `store_owned_by_other`, detail `store creation in
progress: write-gate.lock still held after 30s; retry`, exit 2. `S` and the scope registry are
untouched either way, and `service status` and the read-only forms never wait. A `daemon`
without `--socket` meets `check_start` alone before it asks for its socket, in `main` and in
Go's `runDaemon` (`store.CheckStartLikeFence`): beside a mirror, or on the other runtime's, a
draining or a starting store, it is refused in the fence's words, and otherwise answers the
usage error (exit 4). A reader never takes the gate. Both
runtimes judge absence beside the `D` every opener opens, `Path.resolve()`'s, so a `D` link
naming no file is no `D`: alone it is an absent store (`store_absent` to a read-only form, and
created through the link by a writer, a service form or a daemon), beside a gate a partial one.
Go had worded all of these `ownership refused: existing database required: lstat <D>: ...`
(its start preflight so refused a dangling `D` link alone too); the fence answered a read-only
form with the host error `OperationalError: unable to open database file` (exit 3), took a
dangling `D` link for a store (creating one through it for a reader, or the host error beside a
gate), let `service enable`, `disable`, `stop` and `declare` act on a gate alone, and had its
daemon write `daemon.lock`, `daemon.json` and a released scope claim before refusing one; its
writer created a store over a `takeover.json` link naming no file, and its service forms and
daemon acted on one. Both start preflights also refused a gate its creator still held, and
Go's socketless daemon asked for its `--socket` before `check_start`. The first repair of that
refusal let such a gate through to the admitted open, which a service form never reaches:
`service enable`, `disable` and `declare` then wrote `daemon.lock` and `service.json` for a
store the creator went on to stamp for the other runtime (PR #202 review). All are corrected.
A read-only form never binds an unbound store to its socket either: it reads `mode=ro` instead (decision 30).

Argument refusals keep `cli.py` `main`'s order against the store. A read-only form refuses
its own arguments where its handler does, before the lazy `Services.store`: `merge-turn-show`
without exactly one selector and `linkage-up --scope` without `--task` answer
`bad_invocation` on an absent store and create nothing. A write form's store is opened by
`_ownership_preflight` (`services.store`) before its handler, so a refusal of its own
arguments (`register`'s unreadable settings, `settings-record --exception` beside
`--clear-exception`) leaves an absent store initialized, and a store another runtime owns
answers the ownership refusal instead. Go's relay-registry dispatch (`registry.run`) opens a
write form's store before the command's `precheck` and a read-only form's after it. Before
either, `cli.py` `main` runs `ownership.check_start` for every command that is neither read-only
nor answers without the selected store (`_reads_no_selected_store`), and so does the Go relay
dispatch (`store.CheckStartLikeFence`, with the command's `--socket`): after the command's own
argument parse, and before the selection refusal, `--kind-module` and the handler's own
refusals. A store the other runtime owns, or one mid-transition, therefore answers
`store_owned_by_other` in both runtimes, byte for byte, where the command's `--kind-module`
cannot be imported, where `daemon` or `managed-start` names no `--socket`, and where its
`--socket` is not the one the store recorded, and a write form on a mirror without `D` is
refused in `validate`'s words (`missing or unsupported writer protocol`), as the fence's is,
where a read form still answers the writer admission's refusal above. The intent commands keep
their own check (delivery's fenced markers), and the takeover candidate (`service run
--takeover-candidate`) is checked by its handler, which holds the permit
(`Test31_check_start_precedes_the_selection_kind_module_and_handler_refusals` and
`TestReadOnlyForms_refuse_a_partial_store_as_a_writer_does`, against the live fence).

A read the live-state guard refuses (only under test isolation since todo 43, decision 46)
reports the refusal. The owner's control socket answers a guard that failed with the relay's
host record, as `control.py` does, so a Stop journals `guard_host_error` instead of
`guard_said_nothing`; the journal row keeps the adapters' error-record shape, with `detail` null.

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
stores and process records normalizes (`testsupport.OwnerNeutral`; `RuntimeIdentityText`
for record bytes), and only once each side carries its own: the fence build and `python` from
the Python side, null and `go` from the Go side. Any other value, the other runtime's
included, fails the comparison, so a Go record naming the fence build is a difference. (Since
refactor R2 only the Go side is compared, and the two helpers accept Go's own values alone.)
A service supervisor opens, and so creates, its store in recovery, as `cli.py` `_supervise`
does, and publishes the identity it read into both records (`publish_store_identity`): a
`service run` whose bound is already spent creates no store and records the identity read at
start, null for an absent store.

The relay's `faultsweep.INSTALLATION.version` and the bridge's `__version__` are mirrored
as `faults.RelayPackageVersion` and `appserver.BridgeVersion` (0.2.0), each checked
against the real Python package; `ownership.PythonBuild` stays the fence identity and does
not follow later bumps. The Go relay's installation identity is decision 34's: package
`codex-session-relay` at the directory of its resolved executable
(`faults.ExecutableInstallation`), which `crw install` records as the Go install entry's
`location`, for the daemon's sweeper and the `fault-sweep` command alike.

Evidence: the relay command table's read-only attributes (`ReadOnly`, `ReadOnlyWhen` in
`internal/relay/dispatch`; refactor R2 moved them there from `argparse.ReadOnlyForm`),
`internal/relay/store/hold.go`
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
existing unfenced store is initialized only by the Python fence (Step 0). That rule binds
openers and writers. The one torn publication the protocol recovers, "initial stamp committed,
mirror absent", is recovered by an explicit controller action, `crw relay takeover
repair-mirror` (cutover.md Record). Under the full lock order it publishes the mirror
`InitialRecord` derives from `schema_meta` and writes no key. It refuses every other state,
including a directory holding only `write-gate.lock`. `takeover status` reports that state
from the stamp, with `jsonStale` true, rather than failing on the missing mirror, and answers an
absent mirror whose stamp is not that state with the repair's refusal. Both refuse a `--socket`
the stamp does not name. A store without a socket has no takeover once repaired: every takeover
action then refuses it as it refuses any socketless store.
Before refusing an existing `D` that has no `write-gate.lock`, a Go writable open reads
its `schema_meta` from a disposable copy, as the fence's `Store()` reads it before
deciding what the store is: a `D` that cannot be read, or is not a database, fails
with that error (in Go's words since decision R3S-1, `file is not a database (26)`, a host
error, exit 3; `store_unopenable` in an intent's store record) and gains no gate or
sidecar in either runtime; only a readable `D` is refused as unfenced. The
registration hold (`intent-register`) stats `D` before its admission, as
`registration_hold` does, so a missing store or directory answers
`the relay store could not be opened for writing: stat <D>: no such file or directory`
(decision R3S-1), and it raises its admission's refusal (`register_relationship` re-raises the
fence's `OwnershipRefused`): a store the other runtime owns answers reason
`store_owned_by_other` in the fence's words, not `unregistered_relationship`.
The creator places `write-gate.lock` already held EX (a temporary `S/.write-gate-*`
file, `flock`, `link(2)`), as the fence does, and a Go writer that finds the gate held EX
while the store is incomplete (no `D` or no mirror) or active (a creation finishing, a socket
binding) waits to share it within its busy timeout before admission, so racing first
openers of either runtime never refuse each other as a partial store. The start preflight of a
service command or a daemon lets such a creation through in both runtimes (it probes the gate
once, without waiting, and refuses only a gate nobody holds), so a `service start` or `daemon`
racing a first `register` waits at its admitted open rather than failing. A transfer barrier,
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
`internal/relay/store/registration_hold_refusal_test.go`
(`TestOpen_reads_a_gateless_store_before_refusing_it`,
`TestRegistrationHold_answers_an_unstattable_store_with_the_stat_error`), `Test24_SOS_14_WholeOutputAndSQLite`,
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

Evidence: 148 of the 2061 cases in `TestSkillJSONShapeLivePython` (`TestSkillJSONShape` since refactor R3)
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
`TestSkillUnreadableInputsLivePython` (`TestSkillUnreadableInputs` since refactor R3) passes under outer `LC_ALL=C`, `LC_ALL=C.UTF-8`
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

Correction (todo 39): `scripts/plugin_transition.py` and `scripts/crw_transition/steps.py` retired
with the transition, so `scripts/runtime_install.py` and the placement it calls
(`completion.place_launcher`) are the last Python writers under this lock. The ownership-checked
removal of `<CODEX_HOME>/crw-stop-hook.py` moved to `install.RemoveLauncher`, which takes the same
`<launcher>.crw-lock` (`TestRemoveLauncherWaitsOnTheLockThePlacementTakes`). That lock is a leaf
in the order above, like the settings records': nothing else is taken while it is held. Its wait
ends when the caller's context does, as every crw install path's does, and the context is asked
again immediately before the unlink, so an interrupted removal removes nothing
(`TestRemoveLauncherInterruptedRemovesNothing`).

## 34. The host record stays recordVersion 1; Go install entries are additive

Decision: `internal/runtime/record` reads and writes the one host record
(`${XDG_STATE_HOME:-~/.local/state}/codex-relay-workflow/host-record.json`) as `recordVersion`
1, byte for byte as `json.dump(indent=2, sort_keys=True)` plus a newline writes it, and never
rewrites a Python entry or point. There is no record v2 and no "write beside, then rename". A Go
install entry carries `location` (`<runtime>/bin`, the directory a running Go relay reports as
its own), `entryPoint`, `environment`, `integrity` and `binaryDigest` (the binary's SHA-256),
`target` (`<goos>/<goarch>`), `reachedVia`, `digestMatchesDefinition` and `source`, and no
`interpreter`, `interpreterPath` or `installMode`. `source` is what both fault sweepers read as
the installed revision. A Go relay finds its own entry by that `location`: the daemon's sweeper
and `fault-sweep` both name the directory of the resolved executable
(`faults.ExecutableInstallation`), never a path derived from the Go source, which a `-trimpath`
build turns into a module path that matches no entry. `repositoryCommit` and
`workingTreeClean` come from the binary's build information, and `repositoryTree` and `subdirectoryTree` (equal: the Go module is the repository
root) from the tree `make build`/`make dist` stamp with
`-X .../internal/runtime/record.sourceTree=$(git rev-parse HEAD^{tree})`, and only when the
working tree is clean (`git status --porcelain` empty, the test behind Go's own `vcs.modified`):
a build with a modified tracked file or an untracked one is not built from HEAD's tree, so it
stamps nothing. GoReleaser release builds carry the same stamp since todo 44 (`.goreleaser.yaml`
reads `SOURCE_TREE`, which the release workflow sets from `git rev-parse HEAD^{tree}` in a step
that refuses a checkout with changes). A binary built without the stamp - a build from a dirty
tree, and every GoReleaser release build before todo 44 - records null trees, and both sweepers then report "this copy's install entry records
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
`TestUpdateWritesWhatPythonWrites` (seven deltas against goldens that began as Python's bytes
in the former internal/runtime/testdata/goldens.json), `TestMakefileStampsTheTreeOnlyFromACleanTree` (the
Makefile run in a temporary repository: clean stamps HEAD's tree, modified or untracked stamps
nothing), `TestSourceWithoutAStampIsNull`, `Test37_SweeperReadsTheGoInstallEntry` and
`Test37_FaultSweepCLIRecordsTheRunningBinarysInstallEntry` (a `-trimpath` build's `fault-sweep`
records its Go install entry's location and revision; internal/relay/faults) and, under the
parity tag, `TestParity_python_faultsweep_reads_a_go_install_entry`.

## 35. The components definition stays definitionVersion 1, without per-target digests

(Superseded in part by decision 47: todo 44 deleted `scripts/crw_runtime/components.json`, and
`internal/runtime/definition` is the one definition. The absence of per-target digests and
`definition.Digest` stand.)

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
no interpreter. `store.Open` refused the live state root until todo 43 (`ErrLiveState`, since
then only under test isolation, decision 46) and a plain `mode=ro` connection creates `-wal`
and `-shm` beside a checkpointed store. Python's
`read_only_rows` answers the swap gate through `/proc/self/fd`, which SQLite resolves to the
file a link names, so it reads the WAL beside that file; its
`ownership.stop_metadata` applies the same no-sidecar rule and raises on a WAL holding frames
beside no usable index, where it used to read `D` immutable (review finding PR190 4130471334).

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

An argument is a thing such a program may run only if it could be executed: a regular file with
an execute bit, a file with no `#!` among them because a shell's ENOEXEC fallback runs it. A
socket, FIFO or device is not, whatever its mode bits, because execve(2) refuses it. Before todo
40 the scan asked only for the mode bits, so the App Server socket a bridge record passes with
`--socket` (bind(2) creates it with 0777 less the umask, so it has execute bits) made the record
unreadable, and `crw install remove`, which refuses whatever registration it cannot judge,
refused every runtime on such a host.

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
`TestInstallRefusesASecondOwner`, `TestRetentionScanDoesNotTakeWhatCannotBeExecutedForAProgram`,
`TestIsolatedHome` (IS-8's remove, under the `integration` tag).

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
exactly two slashes (`//`, which pathlib keeps and a lexical join folds) is refused as well, since
pathlib and a lexical join would spell the destination as two directories. A relative XDG_STATE_HOME
is refused as a usage error, as the doctor refuses to read it (it reports the host record as
ACCESS_ERROR and exits 0): runtime_install.py reads it against the working directory and the XDG
specification says to ignore it, and either would put the host record somewhere the doctor and the
runtime's record readers do not look. `--execution-policy` given at all is read as a policy, so an
empty value (or an empty last value of a repeated flag) is refused as runtime_install.py refuses
it, never taken as no policy. Exit
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
into a directory that is gone, which the claim's lock would otherwise make again. A rollback onto
a venv never rewrites the Stop settings, and one onto a Go runtime replaces a Python-era document as
a promotion does (decision 18); a Stop on a venv reaches them through the pre-native
declaration, which a host rolling back holds because the payload goes back before the runtime
(decision 26). For a Python venv target the gate's schema cell compares the store with this
build's declared schema, which stands for the Python runtime's because the DDL is identical (decision 14) and no Go
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
options - resolving inside it; a process whose argv runs something relative to a working directory
it cannot read is not ruled out, whatever its uid, because that working directory may itself be
inside the directory whatever the relative path names (and a process of this user whose working
directory fails for a reason other than a refusal is not ruled out at all). The one exception is
another user's process, not root's, whose working directory the kernel hides (EACCES, EPERM:
`/proc/<pid>/cwd` needs ptrace access) and which provably cannot work inside: some directory from
the runtime directory up to `/` (resolved) denies search (x) to every uid it holds, by that
directory's mode, owner and group, read against the credentials `/proc/<pid>/status` shows every
user (its real, effective, saved and filesystem uids and gids and its supplementary groups). A
process holding root among its uids or CAP_DAC_OVERRIDE or CAP_DAC_READ_SEARCH in its permitted
or effective set is never ruled out this way, a directory carrying an ACL (POSIX or NFSv4) or that
cannot be read closes nothing, and a status that cannot be read or lacks any of those lines rules
out nothing. It judges the modes and credentials as they are now through the path the directory
resolves to here, so a process that entered it before either changed, or through a bind mount or
a descriptor it was passed, is not seen; the answer's `processTable` says so. A runtime under a
home closed to other users (0700, or 0750 to users outside its group) is therefore removable past other users'
processes, but not past root's: a host whose root agents run a relative script - every Azure VM
runs WALinuxAgent as `python3 -u bin/WALinuxAgent-<version>.egg -run-exthandlers`, which is how
todo 40's integration test found it on a GitHub-hosted runner - sees `crw install remove` refuse
there, naming each such pid, its uid and why (`unreadableProcesses`), and giving the removal by
hand as `recoveryRequires` (the same as where there is no process table, below); the reclaim of an
abandoned staging refuses the same way with its own recovery (delete the staging, rerun the
install). Deleting a directory a process may run out of is worse than a removal left to the user),
and no registration the host reads names a path inside it. The registrations are read by the
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
registry is the one the relay resolves in the command's environment: `CODEX_SESSION_RELAY_SCOPE_DIR`
alone when it is set, as a relay started there reads and claims its scope in that one and no
other, and otherwise the production one under the passwd entry's home. The retention scan reads
the production registry under an override too, because it looks for every daemon on the host
whatever environment started it; remove finds a process running out of the runtime in the
process table whichever registry recorded it, so an isolated environment's remove (todo 40's
integration test) no longer reads, or is refused by, the machine's live registry. A remove that
succeeds names the registries and state directories whose records it read (`relayRecords`). The
reading rests on two assumptions, stated in every answer that depends on it (`processTable`): the
process table is this host's, in this command's PID namespace, so a process in another PID
namespace (a container sharing the directory) or on another host (a network home) is not seen,
and `crw install remove` is to be run where the runtime's processes run; and another user's
hidden working directory is ruled out only by a directory closed to it as modes and credentials
now stand, so one entered before either changed, or through a bind mount or a passed descriptor,
is not seen. The reclaim of an
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

`managed-start` sends on the command's own context, as every other relay command does. The
first SIGINT cancels that context (cmd/crw `cancelOn`), so an interrupted start ends through
this path: nothing more is sent, the send it interrupted keeps `outcome_unknown`, and the
command closes its adapter and store and answers exit 3 `{"error": "host", "detail": "context
canceled"}`. Python installs no handler there: `KeyboardInterrupt` ends `cmd_managed_start`,
`cli.main`'s `finally` closes the transport, whose drain cancels the send and saves the same
`outcome_unknown` receipt, and the process dies of the interrupt without a stdout document.
The stored outcome and the retry by request id are the same in both runtimes; only the
interrupted process's own ending differs. Before this, the Go start ran on
`context.Background`: the first interrupt was ignored until the host answered, and a second
one killed the process before its adapter or store was closed.

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
`accepted`, and an extra `thread/resume`. `Test28_ManagedStartEndsOnTheFirstInterrupt` in
internal/relay/adapter/managed_test.go interrupts the built CLI's `managed-start` while the
business `turn/start` is unanswered; with `managedStart` back on `context.Background` the
process outlives the interrupt.

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

## 42. An interrupted Go supervisor passes the interrupt to its worker and exits after it

Decision: the Go service supervisor (`service run`, the takeover candidate included once it is
active) waits for its worker only until its own interrupt. The first SIGINT cancels its context
(cmd/crw `cancelOn`). If a worker is running, or has just been spawned, the supervisor sends it
SIGINT, the signal a Go worker stops on and a Go-owner drain sends, and keeps waiting for it. It
then records the worker's exit (`lastExit`, `workerPid` cleared), starts no successor and
returns the interruption: exit 3, `RuntimeError: context canceled`, as an interrupt during the
restart delay already did. Its records and scope are released only once the worker holding the
inherited locks has exited. A supervised worker (`daemon --supervised-lock-fd`) keeps SIGINT
caught from the start of its run until the process exits. An interrupt it receives twice for one
request, from its process group or a drain and again from its supervisor, therefore stops it
once, through its own cleanup, and its exit status is reported. Its hard stops stay SIGTERM,
SIGKILL and its supervisor's death (PR_SET_PDEATHSIG). The supervisor
keeps cmd/crw's rule that a second interrupt has the default disposition: it ends the
supervisor, and so the worker. Python is unchanged. Its supervisor's KeyboardInterrupt ends it
at once, and PR_SET_PDEATHSIG then ends the worker.

Why: the supervisor waited on `cmd.Wait()`, which no interrupt reached. Interrupted while its
worker ran, it swallowed the interrupt until the worker's segment ended, 3600 s by default. Only
a second interrupt, or one aimed at both processes as a drain aims it, stopped it. So whether an
interrupt was honoured depended on scheduling. `Test30RealCandidateControllerEOF/after-active`
publishes active, closes the channel, probes control.sock and interrupts the activated
candidate alone. It passed only when the interrupt landed before the supervisor spawned its
first worker. When the spawn came first, the supervisor was still alive 5 s later, and the test
failed with "candidate outlived controller EOF without active publication" on a 4-core runner
(PR #194, run 36595382729, go-product test-4). Delaying the interrupt by 1.5 s failed it 3 of 3
times, and each passing run had found no `workerPid` recorded. The same test once failed locally
with "connection reset by peer". There, its probe dialed the candidate's readiness listener as
activation closed it: decision 30 releases that listener before the first worker binds its own.
The test now waits for the recorded worker, so it probes the worker's listener and always
interrupts while a worker runs. Passing the interrupt on was not enough by itself. A worker
already interrupted by its process group or a drain has restored SIGINT's default disposition,
so the supervisor's copy killed it before its cleanup ran (`lastExit` -1). That happened in
15 of 20 drain-order runs until the worker absorbed the repeat. Absorbing it only while the run
lasted still lost 1 of 50 such runs under load: the copy arrived after the run had returned and
killed the worker before it reported its exit. Pinned to four CPUs
shared with eight busy loops, the previous build failed the test in 39 of 50 runs. In 32 the
interrupt was swallowed. In the other 7 the probe was caught in the listener handoff: 3 dials
found no socket, 3 answers were `guard_said_nothing` and 1 was a connection reset.

Cost: an interrupted Go supervisor exits only when its worker has stopped, where Python's exits
at once. A worker that never stops is ended by the second interrupt, as before. A second SIGINT
sent to a supervised worker alone no longer ends it; SIGTERM or its supervisor's death does. Two
interrupts that land while a worker is still starting, before its run begins, still end it, as
before, before it has done any work.

Evidence: `internal/relay/service/supervise.go` (`awaitWorker`, `Supervise`),
`internal/relay/cli/daemon.go` (`runDaemon`), `internal/relay/service/takeover.go`
(`takeoverRuntime.Drain`), `cmd/crw/main.go` (`cancelOn`),
`packages/codex-session-relay/src/codex_session_relay/service.py` (`supervise`, its `finally`).
Tests: `internal/relay/service/process_test.go` (`Test42InterruptedSupervisorStopsItsWorker`:
python, go, go-interrupted-together) and `internal/relay/service/takeover_test.go`
(`Test30RealCandidateControllerEOF/after-active`). Restoring the previous `supervise.go` fails
the go subtest and after-active 3 of 3 times each. Restoring the previous `daemon.go` fails
go-interrupted-together 15 of 20 times. Under the load that failed the previous build 39 of 50
times, the final build passed every run: the takeover test 50 of 50,
`Test42InterruptedSupervisorStopsItsWorker` 50 of 50, and go-interrupted-together 200 more.

## 43. A host turn start is a time only when it reads as a finite number, in both runtimes

Decision: every reader of a host turn's `startedAt` reads it by one rule, the fence's
`hostadapter.host_time` and Go's `delivery.HostTime`. Null and a bool are no time. A number, or a
string Python's `float()` reads (surrounding whitespace, a sign, underscores between digits, an
exponent, Unicode decimal digits), is the seconds it spells. NaN and an infinity, however spelled,
an integer too large for a double, and anything `float()` refuses (other text, a list, an object)
are no time. A start that is no time is read exactly as a missing one: an acknowledgement from
that turn is `unverified_turn`, a supervisor readback naming it is `unverified_turn`, a listed turn
never ends the dispatched-turn listing and is never a fold candidate, and a continuation's host
ordering is `not_corroborated`. The fence's readers that changed are `ack._verify_ack_turn`,
`hostadapter.find_in_listing` (with `_older` and `_older_after_match`), `hostloss._fold_candidates`
and `admission.AnchorOrExplicit._corroborate`. `supervisorchannel._host_time`, where the rule was
written first, is now `host_time` itself. Go's `TurnStartedAt` and the `BridgeReads` listing now
read a numeric string as the fence does.

Why: only the supervisor channel applied the rule, and each other reader compared the raw value.
The fence's acknowledgement checked only for a missing start and then asked `certainly_before`,
whose `float()` answered False for what it could not read. So a start of `"bad"`, `[1]` or NaN
verified the acknowledgement, and `True` (1 s after the epoch) or `"-inf"` refused it as a turn
begun before the delivery. The listing and the fold raised TypeError on any string, and the
admission raised it or compared two strings as text. Go read every string and bool as no start,
so the runtimes disagreed on a numeric-string start: Go left the acknowledgement unverified where
the fence verified or refused it, and Go's listing read on where the fence raised. The App Server
sends numbers, so only a host that sends something else reaches any of this.

Cost: a fence acknowledgement whose turn start is unreadable stays `unverified_turn` where it was
verified, and one with a `True` or negative-infinity start stays unverified where it was refused.
A listing or fold that meets a string start reads it instead of raising.

Evidence: `packages/codex-session-relay/src/codex_session_relay/hostadapter.py:29` (`host_time`),
`:174-233` (`find_in_listing`, `_older`, `_older_after_match`), `ack.py:420` (`_verify_ack_turn`),
`hostloss.py:429` (`_fold_candidates`), `admission.py:118` (`_corroborate`),
`supervisorchannel.py:220`; `internal/relay/delivery/adapter.go:49-99` (`TurnStartedAt`,
`HostTime`), `internal/relay/delivery/bridge_reads.go:44`. Tests:
`internal/relay/delivery/host_time_test.go` (`TestHostTimeReadsEveryValueAsTheFence`, 49 values
against the live fence), `currency_ack_test.go` (`TestAckTurnStartIsReadAsAHostTime`, ten starts,
whole records and tables against the fence), `unknownsend_a_test.go`
(`Test21_USL09b_a_listed_turn_start_is_read_as_a_host_time`, mirroring
`AListedTurnStartIsReadAsAHostTime`), `internal/relay/supervisor/batch6_test.go`
(`Test24_SCH_52b_TurnStartIsReadAsAHostTime`); in the fence, `tests/test_ack_reconcile.py`
(`Acknowledgement.test_a_turn_start_that_is_no_time_does_not_verify_the_ack`,
`test_a_numeric_string_start_is_the_time_it_spells`) and `tests/test_delivery.py`
(`ContinuationCorroborationReadsHostTimes`). Before the change the acknowledgement test failed
for all ten starts, the listing test on the fence's TypeError, the readback test for its three
numeric strings, and the fence's acknowledgement and ordering tests for every start that is no
time.

## 44. A frozen copy is reached, read and raised the same way in both runtimes

Decision: the fence's `manifest.verify_frozen_detailed` asks whether a frozen `MANIFEST.json` is
a regular file with `os.path.isfile`, which answers False for any stat failure on every supported
interpreter. It then names the failure as before. Absence, a traversed file, a loop and a stale
handle (ENOENT, ENOTDIR, ELOOP, ESTALE) are a problem only. Any other errno is also an access
failure: `<ref>: the frozen manifest could not be reached: [Errno N] <strerror>: '<document>'`.
A document that is reached and cannot be read, or is not UTF-8, not JSON, nested deeper than
`json.loads` descends, or not shaped as a manifest's records, still raises. Go's
`store.VerifyFrozenDetailed` answers the same, and follows the fence at each of the three steps
below. A digest, in the fence as in Go, is 64 lowercase hex characters and nothing after them:
`manifest.DIGEST_RE` and `identity.DIGEST_RE` end at `\Z`, not `$`.

- Reaching. The document, the blobs and the files directory are the paths pathlib spells from
  the reference (`str(Path(ref) / "MANIFEST.json")`): `.` parts, repeated slashes and a trailing
  slash dropped, exactly two leading slashes kept, and every `..` kept for the kernel, which
  resolves it after the symlink before it. `<document>` above is that spelling. A blob is
  resolved as `Path.resolve()` resolves it (`os.path.realpath`, not strict): a component that
  cannot be examined is kept, and a `..` after it removes it. Go builds the same strings
  (`store.FrozenDocument`, `frozenPath`) and never cleans a `..` away itself, so
  `<ref>/missing/../frozen` is no frozen copy and `<ref>/lnk/../frozen` is the one beside the
  link's target. `store.FreezeManifest` makes, writes and re-reads its copy at the same paths,
  so it freezes where the fence freezes.
- Reading. The document is `Path.read_text()`'s, then `json.loads`'s. `read_text()` reads with
  universal newlines, so a CRLF or a lone CR is one line feed by the time `json.loads` names the
  line, column and character where a document stops being JSON; a `UnicodeDecodeError` counts
  the bytes before that translation. `json.loads` reads NaN and the infinities as floats, an
  integer exactly, and a repeated key keeps its first place and its last value. Its C scanner
  spends one level of the interpreter's recursion budget on each object or list, and where the
  installed `codex-session-relay` console script reaches it on CPython 3.13 that budget is 9998
  (measured at the guard's read through `cli.main`; `reception/depth.go` and `merge_evidence.go`
  keep the same boundary). A document nested deeper is a `RecursionError`, `maximum recursion
  depth exceeded while decoding a JSON array from a unicode string` (or `object`), raised where
  the scanner meets that container, so a syntax error it meets first is raised instead. Within
  four levels of the budget the scanner's error paths spend more than one level: from 9995
  containers deep the fence raises a `RecursionError` in place of each JSONDecodeError the scanner
  raises itself, and at 9998 also in place of `Expecting value` and an over-long integer's
  ValueError, and of NaN or an infinity, which it reads anywhere else. Go keeps the one-level
  rule there, so it raises the JSONDecodeError or ValueError or reads the constant;
  known-defects.md records that band and each reader's answer in it as a Python defect not
  carried over. Go reads with the same translation and the same depth (`store.frozenJSONDepth`),
  and its decoder keeps each object's key positions, so a document with many keys is read in
  time proportional to its length, as `json.loads` reads it. The
  budget is what is left at the call, so code that reaches `json.loads` through C re-entries has
  less (measured: 9995 under `python -m`, 9980 inside a unittest test, 9978 under pytest). The
  fence therefore answers a document within twenty levels of the boundary differently on such a
  path, and only a document well past it raises on every path; the parity tests stage the
  boundary from a script's top level, as the console script calls it, and the whole-reading
  tests a document 100000 levels deep. Each record's `path`, `sha256` and `bytes` are
  the values `json.loads` made of them, and they are compared, printed and hashed as Python does
  (`store.PythonEntry`). Only a number or a bool can equal a byte count: `"19"`, `[19]`, `{}`,
  `"x"` and NaN never do, `19.0` and `19.000000000000001` (the same double) do, and `true` is 1.
  A problem prints a value as `str()` does: `19`, `[19]`, `{}`, `x`, `nan`, `inf`, `True`.
- Raising. Each step raises what the fence raises there, as `store.ManifestException` with the
  Python class and words, or as the `ScopeError` revision_hash raises (a `RefusedError`). A list
  or dict path or digest is `TypeError: unhashable type: 'list'` when the frozen set is built. A
  true digest that is not a str is `TypeError: expected string or bytes-like object, got 'int'`,
  and a false one (null, 0, false) is a digest problem. Then revision_hash closes the function: a
  path that is not a str is `AttributeError: 'int' object has no attribute 'encode'`, a lone
  surrogate in a path a `UnicodeEncodeError`, and a digest that is not a str the `ScopeError`
  that names its repr (`None`). A digest that ends in a newline is a digest problem like any
  other non-digest, and revision_hash then raises that `ScopeError`. The `RecursionError` of a
  document nested too deep is the one exception here that is not an OSError, a ValueError, a
  KeyError, a TypeError or an AttributeError (`ManifestException.RuntimeError`).

Each Go reader answers as the fence's reader of the same function, outside the band above:

- the intake reads `store.VerifyFrozen`, now the detailed call without the access list, as
  `verify_frozen` is. An unreachable frozen copy is refused `manifest_unverified` and recorded.
  One that is not a manifest, or is nested too deep, is the fence's host error (`{"error":
  "host", "detail": "JSONDecodeError: ..."}`, `"RecursionError: ..."`), and no refusal is
  recorded. The ScopeError of a frozen record is a refusal, as it is in the fence. The receipt's
  own digests are read by the same rule: a `sha256` or `revisionHash` ending in a newline is
  `malformed_receipt` in both runtimes;
- the omission reader (`delivery.omissionDeliverable`) and the Stop hook (`hook.DeliverableState`)
  follow `guard.deliverable_state`. An unreachable frozen copy, a manifest that cannot be read, a
  ScopeError, and unreadable live bytes that no frozen copy answers for are unverifiable, which
  the omission reports as `receipt_unreadable`. A frozen copy that is not a manifest is changed,
  and the detail is the exception's words alone. A live artifact that is gone is changed. A
  frozen copy nested too deep raises the `RecursionError` out of `deliverable_state`, whose
  except clauses do not name it, and out of `lookup_receipt`: the Stop hook's evaluation faults
  (`guard_faulted`, released and recorded, the fault `RecursionError: ...`), and the omission
  reader, which catches RuntimeError, is `unmeasured` with `evidence_unreadable: <its words>`
  (`delivery.omissionReceiptFailure`). The hook reads the document whole under its deadline, as
  the fence and the store's readers do, without the 4 MiB bound it keeps for other evidence
  (decision 24's native reads). It reads what follows the document's read with the store's reader
  (`store.VerifyFrozenDocument`), and it reads the stored receipt's own records the same way
  (`store.PythonManifestEntries`, `store.PythonRevisionHash`), so a stored byte count of `"19"` is
  a size that disagrees, not 19;
- `store.FreezeManifest` re-reads the blob it copied where `_read_frozen_blob` reads it. A
  destination reached through a symlinked directory, named relative to the working directory, or
  holding a `..` now freezes where the fence freezes, and the refusals quote paths as Python's
  `repr` does.

Why: `Path.is_file()` raised PermissionError for a blocked parent up to Python 3.13 and answers
False from 3.14. The fence runs on 3.13, and the packages are tested on 3.11 and 3.13. There, a
frozen copy behind a permission left the fence as an exception: the intake printed a host error
and recorded nothing, and the guard answered unverifiable with the exception's text. A 3.14 fence
refused the same copy and named it. Go's detailed verifier returned an error, as 3.13 did. Go's
intake used a loop of its own. It read every read failure as "no MANIFEST.json" and every
undecodable document as "the frozen manifest is not readable JSON", both refused, NaN included,
which the fence reads. It refused a symlinked or relative reference that the fence verified,
because it resolved with `filepath.EvalSymlinks` and not as `Path.resolve()` does. It also
cleaned every `..` away with `filepath.Join`, so it judged, and `FreezeManifest` wrote, a
different directory than the fence whenever the component before a `..` was a symlink or
missing. `FreezeManifest` refused symlinked and relative destinations too (`symlink_component`,
`scope_escape`). The omission reader read the frozen copy through that loop, so an unreachable
copy was a changed deliverable, and it read every failed live hash as unreadable where the fence
reads a vanished artifact as changed. The Stop hook named a blocked frozen copy "no
MANIFEST.json" and merged a JSONDecodeError into the live problems. It also resolved blobs
strictly, so a blocked blob directory surfaced Go's `lstat` text, and it read the frozen
document with the 4 MiB bound it keeps for other evidence, so a larger copy was unverifiable
where the fence and Go's other readers read it. It read a byte count through
`evidence.IntOf`, which takes the string `"19"` as 19, and refused a path or digest of another
type with words of its own. A first repair decoded a frozen byte count as a JSON number, which
also took `"19"`, and returned Go's decoder errors for a list, a NaN or a non-str path.

A repair then found three more differences and one in the fence. Go checked the frozen document
for JSON errors with no depth limit and decoded it by recursion, so it read a copy that is a
good freeze beside a value nested 100000 levels deep, where the fence raises RecursionError: the
intake stored the receipt and the hook answered current. Go counted a CRLF or CR as the fence's
text never holds it, so a corrupt copy's JSONDecodeError named another line, column or
character. Its decoder searched an object's keys for every key it added, so 200000 keys took
26 s where the fence takes 0.05 s, past the hook's five-second budget. And the fence's `DIGEST_RE` was `^[0-9a-f]{64}$`, whose `$`
also matches just before a final newline: the fence took `<hex>\n` as a digest in the receipt's
shape check, in `canonical_payload`, in `freeze` (where the digest names a file) and in the
frozen record check, and then refused the receipt as `manifest_unverified` or
`revision_mismatch`, or answered a frozen copy holding it as changed. Go's pattern already ended
at the end, so it refused `malformed_receipt` and answered unverifiable.

Cost: a 3.13 fence now refuses and records a receipt whose frozen copy it cannot reach, where it
answered a host error. The guard's detail for that copy now names the unreachable manifest, not
the PermissionError. Go's intake answers a host error for a corrupt frozen copy, and for a frozen
record revision_hash cannot hash, where it refused. It accepts a frozen copy that holds NaN
outside its records, as the fence does. Go's intake answers a host error, the hook a fault that
releases the Stop and the omission reader an unmeasured reading for a frozen copy nested past
the scanner's depth, where Go read it; the fence's guard releasing on that fault is its
behaviour, kept as it is. The fence refuses a receipt whose digest ends in a newline as
`malformed_receipt`, where it refused it later for another reason, and its guard answers a frozen
record holding one as unverifiable, where it answered changed. No contract fixture freezes a
copy or carries such a digest.

Evidence: `packages/codex-session-relay/src/codex_session_relay/manifest.py:269-358`
(`_frozen_document_access`, `verify_frozen_detailed`), `:209-266` (`freeze`, `_read_frozen_blob`),
`:28-76` (`DIGEST_RE`, `Entry.from_record`, `canonical_payload`, `revision_hash`),
`identity.py:23-24` (`DIGEST_RE`), `guard.py:123-195` (`deliverable_state`), `guard.py:906-987`
(`evaluate`, `_faulted`), `omitted.py:505-507,624-626` (`evidence_unreadable`);
`internal/relay/store/frozen_detailed.go:22-162` (`VerifyFrozenDetailed`,
`FrozenDocument`, `frozenPath`, `VerifyFrozenDocument`), `:164-268` (`frozenJSONDepth`,
`frozenRecords`, `ManifestException`, `RuntimeError`, `resolveFrozenPath`),
`internal/relay/store/frozen_value.go` (`PythonEntry`,
`PythonManifestEntries`, `PythonRevisionHash`, `PythonStr`, `PythonEqual`, the `json.loads`
decoder), `internal/relay/store/manifest.go:84-176` (`FreezeManifest`, `ReadFrozenBlob`,
`VerifyFrozen`), `receipt_intake.go:186` (`verifyBytes`), `ownership.go:120` (`PythonHostDetail`),
`internal/relay/delivery/omitted.go:660-720` (`omissionDeliverable`, `omissionReceiptFailure`),
`internal/relay/hook/receipt.go:150-306` (`DeliverableState`, `raisedState`, `verifyEntries`,
`verifyFrozen`), `settings.go:158-199` (`unbounded`, `readRegular`),
`internal/relay/hook/guard.go:110-120` (the fault). Tests: `internal/testsupport/frozen.go` (`FrozenManifests`, 29 frozen documents
no freeze writes, staged for every reader below: among them a digest ending in a newline, a
value nested 9997, 9998 and 100000 levels beside a good record, and corrupt documents with CRLF
and CR line ends); `internal/relay/adapter/scope_test.go`
(`Test28_MSC_9_AccessFailureIsNotDisagreement`, whose fence answer flips with the fix and which
fails on Go's former error, and `Test28_MSC_8b_FrozenCopyIsReadAsTheFenceReadsIt`: every crafted
document with and without the caller's entries, and references through a missing directory, a
symlink and a blocked directory, revision, problems, access failures and the exception's class
compared, and `Test28_MSC_1b_DigestEndingInANewlineIsRefused`), `intake_test.go`
(`Test28_MSC_11_IntakeAdmissionUnchanged`: blocked, corrupt, unreadable, crafted and `..` frozen
copies, and a receipt whose `sha256` or `revisionHash` ends in a newline, with the host detail
and the recorded refusals compared), `internal/relay/delivery/omitted_deliverable_test.go` and
`internal/relay/hook/deliverable_test.go` (every case against the fence's `deliverable_state`,
an exception it lets out included, the hook's also with stored records whose bytes, path or
digest is another type and with a frozen copy past its 4 MiB evidence bound),
`internal/relay/delivery/omitted_capture_test.go`
(`Test24_OMI_7b_WholeOutput`, the whole omission reading of a copy nested too deep),
`internal/relay/hook/guard_test.go` (`Test33GuardFrozenCopyAtTheDecoderDepth`, the whole Stop
verdict, bytes and recorded observation, at and past the depth),
`internal/relay/store/frozen_copy_test.go` (a source changed during the copy, a blob pre-seeded
under the digest's name, symlinked, relative and `..` destinations),
`internal/relay/store/frozen_value_test.go` (200000 keys read in well under five seconds); in
the fence, `tests/test_manifest_scope.py`
(`test_an_unreachable_frozen_directory_is_named_as_an_access_failure`,
`test_an_unreachable_frozen_directory_is_refused_as_unverified`,
`test_a_corrupt_frozen_manifest_raises_and_records_no_refusal`, three `FrozenCopy` tests, and
the four digest-newline tests `test_a_digest_ending_in_a_newline_is_not_a_digest`,
`test_a_digest_ending_in_a_newline_names_no_blob`,
`test_a_frozen_record_whose_digest_ends_in_a_newline_is_not_hashed` and
`test_a_receipt_digest_ending_in_a_newline_is_malformed`) and `tests/test_omitted.py`
(`test_a_frozen_copy_nested_past_the_decoder_is_unreadable_evidence`).
Before the repair the new cases failed in every reader: the intake 16 of the 22 crafted
documents and both `..` references, the omission reader 16 and both, the hook 32 of its 37 new
cases, the direct capture 18 documents and the `..` references, and both `..` freeze tests.
Before the second repair the intake failed 7 of its new cases, the direct capture 6, the
omission reader 6 and its whole reading, the hook 6 and its whole verdict past the depth, the
canonical form 1, the 200000-key read took 26 s, and the fence failed its four digest tests.

## 45. Discovery asks the stores under the absolute state root, in both runtimes

Decision: the retained Python fence's `discover_state_dir` walks the stores beside the canonical
directory under `(base / "codex-session-relay").absolute()`, the directory its own selection
names, where it walked `base / "codex-session-relay"` as spelled. Go's `DiscoverStateDir` already
walked the absolute root. Under a relative `XDG_STATE_HOME`, or a relative `HOME` with no
`XDG_STATE_HOME`, both runtimes now adopt the one store that records the requested socket, report
two such stores as ambiguous, and name a store that records no socket by its absolute path. An
absolute root, which every measured host uses, is walked as before.

Why: `store_socket` opens a store through `Path(db_path).as_uri()`, which raises ValueError for a
relative path, and it answers that as a store recording no socket. Under a relative root every
store beside the canonical directory therefore read as unidentified: the fence never adopted the
store a socket already had, reported it under a relative spelling beside the absolute canonical
path, and selected the canonical directory, while Go adopted the recorded store. The two runtimes
chose different stores for one socket, which the single-writer cutover cannot allow.

Cost: none on a host whose state root is absolute. Where a fence has already created a canonical
store beside a store it failed to adopt, the canonical store keeps answering first in both
runtimes, as it does for any existing canonical store.

Evidence: `packages/codex-session-relay/src/codex_session_relay/store.py` (`discover_state_dir`),
`internal/relay/store/state.go` (`DiscoverStateDir`). Tests:
`packages/codex-session-relay/tests/test_store.py`
(`Precedence.test_a_relative_state_home_still_asks_the_stores_themselves`) and
`internal/relay/store/pathlib_parity_test.go` (`TestDiscoverySpellsEveryStoreItNamesAsPythonDoes`,
subtests `XDG_STATE_HOME=rel` and `HOME=hrel`, which compare the fence's selection, detail and
candidates with Go's). Restoring the relative walk fails both.

## 46. The live-state guard refuses only under test isolation (todo 43)

Decision: the Go store refuses a database under `~/.local/state/codex-session-relay` or
`$XDG_STATE_HOME/codex-session-relay` only when `CRW_REFUSE_LIVE_STATE=1` is set
(`store.ErrLiveState`, `store: live state refused under CRW_REFUSE_LIVE_STATE=1 (test
isolation)`). With no variable, as Codex starts the Stop hook, the skills' `codex-session-relay`
and the plugin bridge, it opens the live state like any other store, and `CRW_ALLOW_LIVE_STATE`
is no longer read anywhere. `internal/testsupport` sets `CRW_REFUSE_LIVE_STATE=1` in its `init`
whenever it is linked into a test binary, and `IsolateRelayState` sets it again, so every Go test
and every process a test starts (a built `crw` inherits the environment) keeps refusing. Every
test binary that links the store links testsupport, through a blank import in `livestate_test.go`
where nothing else links it, and a test requires that. A test that exercises the live path clears
the variable for that command (`t.Setenv("CRW_REFUSE_LIVE_STATE", "")`, or
`CRW_REFUSE_LIVE_STATE=` in a child's environment); a test that asserts the refusal sets it. A
child environment a test builds from nothing carries the variable only where the test names it,
as the isolated-home integration run, the Stop-event harness, the plugin-wiring and install hook
runs and the install signal test do; the ones that start a relay, a hook or an installer all point
`HOME` at a temporary directory. Where the guard refuses, the refusal is still reported as
before: a read-only form's host error, the owner's host record to a Stop, `guard_host_error` in
the Stop journal.

Why: the production cutover is committed. The owner's host ran todo 42 through
`takeover commit` (rollback_allowed=0), so the Go runtime owns the live store. The product guard
refused that store unless `CRW_ALLOW_LIVE_STATE=1` was set, and Codex starts the Stop hook, the
relay commands the skills run and the plugin bridge without it, so the guard blocked the hooks and
the skills on the store they serve. It also kept the test suites off the owner's live state, and
that protection stays by inverting who sets the variable: the tests set it, the product does not.

Cost: a test that forgets isolation is still refused, because testsupport sets the variable before
any TestMain runs, in every test binary that links the store. A child process a test starts with
an environment built from nothing refuses only where the test names the variable. A developer who
runs a built `crw` by hand against their own live state is no longer refused.

Evidence: `internal/relay/store/store.go` (`ErrLiveState`, `refuseLiveState`);
`internal/testsupport/testsupport.go` (`RefuseLiveStateEnv`, `init`, `IsolateRelayState`). Tests:
`internal/relay/store/store_test.go` (`TestOpen_opens_the_live_state_by_default`, which opens a
store under both live roots made temporary directories with no variable set, and
`TestOpen_refuses_live_state_under_test_isolation`), `internal/testsupport/livestate_test.go`
(`TestInit_sets_the_live_state_refusal_in_a_test_binary`,
`TestIsolateRelayState_sets_the_live_state_refusal`,
`TestEvery_test_binary_that_links_the_store_links_testsupport`, which fails for a test package
that links the store without testsupport), and the refusal-and-live pairs
`TestGuardEvaluate_reports_the_live_state_refusal`, `TestPacketCheck_reports_the_live_state_refusal`
(`internal/relay/cli/readonly_test.go`) and `Test33ReviewD8`
(`internal/relay/hook/review_selection_test.go`). Restoring the product guard fails
`TestOpen_opens_the_live_state_by_default`.

## 47. The Go definition is the one compatibility definition; components.json is deleted (todo 44)

Decision: `scripts/crw_runtime/components.json` is deleted with the rest of `scripts/crw_runtime`,
and `internal/runtime/definition` is the one compatibility definition (OPS-1.1). It keeps what Go
reads: each component's name, console-script name (the links beside `crw`), version (the bridge's
`--version` and the relay's fault-sweep facts are tested against it), licence path and identity
tool. `upstream` and `exerciseCommand` are dropped from it, because nothing in Go read them except
the check that compared them with the file: the bridge's upstream provenance stays in
`packages/README.md` until `packages/codex-thread-bridge/PROVENANCE.md` takes it (todo 44, C6).
`definition.Version` stays 1: it is what a host record states (decision 34), not the version of a
file. No `definitionVersion` 2 file is written, which supersedes the plan's "move components.json
to internal/runtime/definition/components.json (v2)". `crw-dev ci contracts`' `runtime` check
now judges the Go definition against what it names outside itself: each licence file exists and
is among the files `.goreleaser.yaml` archives, the identity tool is a tool
`contract/schema/bridge-mcp-tools.json` lists, and `definition.Links()` equals the names the
release build links to `crw` (`ln -sfn crw <name>`). Its contract, for "present exactly when its
contract is", is `internal/runtime/definition/definition.go`.

Why: a second copy has to be kept equal to the first, and after todo 44 nothing but that equality
test and the check read the file. Every Python-shaped field in it (module locations, package
trees, source digests, `requiresPython`, `exerciseScript`) described the Python packages todo 44
deletes, and a `v2` file holding only the Go fields would restate the Go slice while adding a
parser for it. The release configuration and the bridge's tool contract are independent sources,
so checking against them catches a drift the equality test could not: a link the installer
expects that the release no longer makes, or a licence the archive no longer ships.

Evidence: `internal/runtime/definition/definition.go` (`Components`, `Links`, `Version`);
`TestDefinitionNamesTheComponentsTheInstallerPlaces`; `internal/dev/ci/contracts.go`
(`runtimeCheck`, `releaseLinks`, `releaseArchiveFiles`) and `TestContractsRuntimeDefinition`
(a changed link, an unarchived licence, a missing licence, tool contract or release configuration
each fail it); `internal/bridge/mcp/version_test.go`, `internal/relay/faults/version_test.go`.

## 48. No Python takeover candidate: the way back to a Python owner is refused (todo 44)

Decision: `crw relay takeover` launches no Python candidate. A transition toward a Python owner
(`takeover rollback --to python`, or an activation that would resume a Python owner) is refused
in the controller's preflight, before any durable edge, as `store_owned_by_other` with the detail
`service.NoPythonCandidate`; the candidate command of a record naming a Python owner is refused
the same way. `service.TakeoverOptions` no longer carries the Python relay's path. The
`--python-relay` option stays on `activate` and `rollback`, with its usage checks (absolute path;
launching actions only), because `contract/schema/records.json` freezes the takeover action set
and its options; its value is never executed. The ownership controller's protocol, including
`Rollback`, is unchanged and stays covered by `internal/relay/store/ownership`'s protocol tests
with fake runtimes.

Why: the only candidate a rollback could launch was the retained fence release's Python console
script. The owner's host committed the cutover at todo 43 (`rollback_allowed=0`, which already
refuses a rollback), no Python runtime is installed there, and todo 44 removes the Python
implementation from the repository, so keeping a path that executes whatever `--python-relay`
names would keep a Python execution path in the product for a transition nobody can complete.
Refusing before the reverse CAS leaves the store where it was.

Cost: a store that is not committed and whose operator still holds a fence release can no longer
be handed back to it by this controller. The readiness bound (`--ready-timeout`), which the
silent Python candidate used to exercise end to end, is exercised in process with a silent
command standing in for the Go candidate (`takeoverRuntime.command`).

Evidence: `internal/relay/service/takeover.go` (`NoPythonCandidate`, `Preflight`, `candidate`,
`Start`); `internal/relay/cli/takeover.go`; `Test30TakeoverBuiltCLI`,
`Test30AbortAndFailedCandidateBuiltCLI`, `Test30ReadyTimeoutBoundsSilentCandidate`,
`Test30StatusSchema` (the frozen action set is still accepted); docs/port/cutover.md "Rollback".

## 49. The bridge's printable table is frozen; its Python generator is deleted (todo 44)

Decision: `internal/bridge/settings/generate`, the `go generate` program that ran `python3 -c` to
read `str.isprintable()` of every code point under CPython 3.14's Unicode database (16.0.0) into
`printable_generated.go`, is deleted with the `//go:generate` line. The generated table is kept as
committed and is not regenerated: `TestPrintableIsCPython314sIsprintable` (`TestPrintableIsTheFrozenTable` since refactor R3) compares it with a
golden that began as that interpreter's answer.

Why: the table's source of truth is the Python bridge's behaviour, which is frozen with the
Python implementation; a generator that needs a CPython 3.14 on PATH is a Python execution path
that could only ever reproduce the same table, and a later CPython would answer another question.

Evidence: `internal/bridge/settings/printable.go`, `printable_generated.go`,
`printable_test.go`.

## 50. `hook-status` still asks a named program whether it runs Python (todo 44)

(Superseded by decision 57: the status reading and its interpreter probe are deleted, so no
product path executes a Python interpreter.)

Decision: the Stop-hook status reading (`answersPython` in `internal/relay/hook/status.go`) keeps
running a program that a registration or the settings name as the adapter's interpreter with a
bounded `-c` version probe (5 s, only the printed version is read). No other product path
executes a Python interpreter: the processes the installer, the doctor and the swap gate start
are the runtime's own executables, and the retention scan classifies Python without running it.

Why: the reading judges whatever Stop registrations a host holds, including a user-owned
`hooks.json` entry or Python-era settings that no Go install wrote, and for such a registration
whether it can start turns on whether the program it names runs a supported Python; a wrapper
around an interpreter answers only when asked. The contract corpus's hook `status` fixtures
(`test_completion_hook__*`) freeze this reading, the Go install's own settings name no Python,
and the probe executes only what the host's configuration already names. It retires with the
user-owner and Python-era shapes of the reading, which is a contract change of its own.

Evidence: `internal/relay/hook/status.go` (`interpreterProbe`, `answersPython`, `programCell`);
contract/fixtures/hook/`test_completion_hook__test_a_recorded_interpreter_that_is_not_one_is_not_startable_either.json`,
`...__test_a_valid_answer_survives_a_wrapper_that_replaces_the_exit_status.json`,
`...__test_every_probe_that_ran_a_program_says_so_not_only_the_one_that_worked.json`; the
exec sites listed by `git grep -n 'exec.Command' -- 'cmd/*.go' 'internal/*.go' ':!*_test.go'`.

## 51. The store keeps only the typed row queries the product runs (refactor R1)

Decision: `internal/relay/store` no longer carries the typed row layer nothing in the product
calls: about 150 exported `*Store` methods and the row types, scanners and column lists only
they used (the fault-ledger, managed-sidecar, edit-region, linkage, record and receipt readers
and writers the domain packages replaced with their own SQL), together with the registry's and
the supervisor channel's test-only wrappers over them (`Registry.RecordSettingsViolation`,
`Registry.SettingsViolation`, `Channel.Eligible`). Every table keeps its schema and its rows;
only unused Go goes. The store tests that seed rows or read back what a live writer stored keep
the queries they need in `internal/relay/store/rowqueries_test.go`, and other packages' tests
seed through `internal/testsupport/storeseed`, which no product package imports.
`TestEverySchemaTable_has_a_go_query_referencing_it` now searches the product's SQL under
`internal/` (not the store package alone), so the store no longer has to hold a query for a table
another package owns. One table has no query in either runtime and is listed as stored-only:
`attempt_settings_violations` (Python's `DeliveryService.record_settings_violation` and
`settings_violation` had only test callers, and so did their Go copies).

Why: the layer was ported table by table before the domain packages existed and was kept alive
only by that test and by tests of itself; a second implementation of a query that the product
runs elsewhere tests nothing the product does.

Evidence: `internal/relay/store/table_coverage_test.go` (`storedOnly`, `productionSQL`);
`internal/relay/store/python_parity_test.go` (a table without a typed store query is dumped but
not compared; the recorded Python runs are unchanged); `internal/testsupport/storeseed`.

## 52. Production code only tests reached moves into the tests or goes (refactor R1)

Decision: functions, methods and constants of the store, delivery, adapter, managed and registry
packages that no product path calls leave the product. Where a test of the same package drives
one to set up or observe live code it moves, unchanged, into that package's `testonly_test.go`
(delivery also `anchorpasses_test.go` and `projection_helpers_test.go`; the adapter's store
forwarders into `store_forwarders_test.go`). Where nothing but its own tests used it, it goes with
them:

- the second registration hold, `store.HoldForWrite` and `registry.RegisterUnderHold`: the relay
  registers under `store.RegistrationHold` only (its refusals stay covered by
  `TestRegistrationHold_answers_an_unstattable_store_with_the_stat_error`);
- `delivery.VerifyResume` and `TaskSettings.Mismatches` with their helpers, a second copy of the
  resume check the bridge adapter runs through `registry.TaskSettings`; the delivery and
  supervisor tests that replayed Python's answers through the copy now run the adapter's
  algorithm and still match the recordings (`TestORD*`, `Test24_SLF_*`);
- `store.AckProof` (the product's is `delivery.AckProof`).

Kept although only tests reach them, because a test in another package needs them and has no
other way in: `delivery.NewFakeClock`, `Service.SnapshotItem` (mergeturn's recorded snapshots),
`Service.PreviewReport` (supervisor), `store.ReceiptIntake.ResolveStaged` (delivery), the store's
authorized-file surface the adapter's lease tests drive (`IsWithin`, `ArtifactBinding.Record`,
`AuthorizedFile.Rebaseline`, `HashAuthorized`), and `Store.SetFaultHook`.

Why: the port kept every Python function whose test it ported; the product reaches these paths
through other code, so a copy that only its tests call tests nothing the product does.

Evidence: `git grep -n 'testonly_test.go'`; `internal/relay/delivery/onrequest_adapter_test.go`
(`verifyResume`); `internal/relay/supervisor/partd_res_rcf_slf_test.go` (`slfCheck`).

## 53. No work-report writer: the supervisor's report library is deleted (refactor R1)

Decision: the supervisor's work-report library - `RecordWorkReport` with its validation and
normalization (`report_record.go`, `report_review.go`, `report_restore.go`, `report_subset.go`),
its read-back (`ReadWorkReport`, `ReadWorkReports`), the correction gate
(`AssertReportResubmission`) and delivery's `ProjectReportRestoration` - is deleted, with the
tests whose subject it was and their recordings. The `work_reports`, `work_report_handoffs` and
`attempt_report_submissions` tables stay in the schema with any rows a store holds, and every
product reader of them stays: delivery's report composition and the attempt's submission freeze,
`crw relay show`, the supervisor obligation and packet reads, the registry's dispositions and the
evidence reports. The supervisor replay harness (`partd_report_alias_test.go`) no longer replays
Python's recorded `report` and `read` operations; each operation runs on its own snapshot, so
the others are unchanged. The invariants `report.record` enforced (I-90, I-96, I-100, I-105,
I-113, I-118, I-129, I-132, I-122 in docs/relay/invariants.md) are marked retired.

Why: nothing in the product records a work report in either runtime (Python's `report.record`
had only test callers, and so did the Go port of it), so the library tested a writer nobody runs.
The owner chose deletion over wiring a command for it (2026-10-01).

Evidence: `git grep -n 'INSERT INTO work_reports'` (no product match); the readers named above;
`internal/relay/store/table_coverage_test.go` (every report table still has a product query).

## 54. The takeover controller, its candidate and `crw relay takeover` are removed (refactor R1)

Decision: the Go runtime no longer carries the ownership transition controller. Removed:
`crw relay takeover` with every action (`status`, `begin`, `drain`, `transfer`, `activate`,
`abort`, `rollback`, `commit`, `repair-mirror`) and its options (`--to`, `--python-relay`,
`--ready-timeout`); the candidate's inherited activation channel and the hidden
`service run --takeover-candidate` flag; `ownership.Controller` with its transfer backup and
inventory; the candidate permit (`ownership.Candidate`, `WithCandidate`); the takeover-only
export `store.RefuseLiveState`; and the `takeoverStatus` record in contract/schema/records.json.
A command line naming `takeover` is now an unknown command (argparse's exit 2), and `service run
--takeover-candidate` an unknown flag. A store whose mirror says `starting` is refused to every
opener with the words a process without the candidate's permit always got ("only designated
candidate may enter starting"). Nothing else an opener, the hook or the doctor reads changes: the
mirror, the six schema_meta ownership rows, write-gate.lock and takeover.lock stay as every store
has them, and `S/takeover-backups/`, which only the transfer wrote, is the operator's to delete.
docs/port/cutover.md marks Steps 1-7, the Rollback and the Commit point historical.

Not removed here, although survey B-03 names them: the `guard-evaluate` command and the doctor's
`ownership.processes` echo of each record's `python_compatibility_build`. Removing the command
changes the root parser's usage and choice list, and removing the echo changes the doctor's
ownership block, in about 140 recorded Python answers (argparse sweeps, formatter and doctor
comparisons) that wave R1 can neither keep byte-identical nor regenerate; they go in wave R3, once
the goldens have an update mode.

Why: the owner's host committed the cutover at todo 43 (owner go, epoch 8, rollback_allowed 0),
no Python runtime or candidate remains (decision 48), and no skill, hook, wiring file or
relay-emitted text names the takeover command. A controller whose only transitions are refused
or already done is a second, unexercised writer of the fence.

Evidence: `git grep -n 'takeover' -- plugins` (only the mirror's file name in
crw-run/references/relay.md); internal/relay/store/ownership/admission.go (`judge`);
internal/relay/store/ownership/protocol_test.go (the admission tests, now on a Go-created store);
internal/relay/service/control_test.go (`Test30ForeignOwnerCLIRefusesWithoutDBChanges`).

## 55. The takeover inbox is retired: nothing queues into S/takeover-inbox or drains it (refactor R1)

Decision: the decision-25 takeover inbox leaves the Go runtime: `internal/relay/inbox` (the
envelope, the durable enqueue and the replay), the relay CLI's drain on a writable command's first
admitted open, in the daemon before its first tick and in the service supervisor's recovery, the
per-family appliers (`delivery.ApplyQueued`, `faults.ApplyQueued`, the legacy `supervisor-read`
replay and its lazy host), the queue branches of `emit`, `ack` and `fault-notification-ack`,
`ownership.IsInboxEntry`, the `Queueable` mark on an ownership refusal, and
contract/golden/takeover-inbox. A receipt or acknowledgment the store's ownership refuses (another
runtime's store, or one draining or starting) is now answered with that refusal - exit 2,
`store_owned_by_other`, in the fence's words - where it used to be queued with
`{"status": "durably_queued", ...}`. A writable command still opens its store once before its
handler and before `--kind-module`, so the refusal order is unchanged.

Why: the inbox carried receipts and acknowledgments across the interval when no runtime was the
active owner during a takeover; with no takeover left (decision 54) and the owner's host on an
active Go owner for good, nothing can be refused queueably, and the live `S/takeover-inbox` was
empty when the drain was removed (checked 2026-10-01). The directory and its `.replay.lock` are the
operator's to delete. docs/port/cutover.md marks the Inbox section historical.

Evidence: `internal/relay/delivery/cli.go`, `internal/relay/faults/cli.go` (no queue branch);
`internal/relay/cli/registry.go` (`admit`, `admitsBeforeHandler`);
`Test31_check_start_precedes_the_selection_kind_module_and_handler_refusals` (refusal order).

## 56. The write fence is a frozen stamp read on the writer's own connection (refactor R1)

Decision: a writable open of an existing store no longer runs the fence's admission. It opens the
database read-write and, on that connection and before any statement of the open writes, reads
`schema_meta`: the six ownership keys must be present and valid and `owner` must be `go`, and
every table and column of the frozen v1 schema must exist; then it takes `write-gate.lock` SH for
the store's lifetime (binding the store to the App Server socket it names first, when the store
records none). Removed: `ownership.Admission` (`Admit`, the per-connection `Check`, the
per-transaction `Revalidate` and `Compose`'s revalidation), the start preflight's `CheckStart`
and `CheckStop` judgements of the mirror against the stamp (`ownership.Validate`), the fence's
wording of mirror disagreements (store/fence_wording.go), and every disposable copy of the
database a writable open used to read (`SnapshotMeta` for the admission, the connection hook and
the binding, `validateSchemaSnapshot`). The lock-free preflights (`StartPreflight`,
`CheckStartLikeFence`, `CheckStop`) still refuse a mirror whose bytes are unreadable or not a JSON
object, then read the stamp in place (no copy, no sidecar; a command's preflight waits for a
writer's lock as its open would, the Stop path does not wait) and refuse a fenced store that names
another owner, or none, in the fence's words ("the relay store belongs to another runtime",
"missing or unsupported writer protocol"); a store whose write-ahead log an in-place read cannot
use is left to the writable open. `ownership.PythonBuild` is `ownership.CompatibilityBuild`, with the
same value.

Kept, for the one live store and for any older Go runtime (the rollback target admits a store only
with all of them): the six `schema_meta` ownership rows, `takeover.json`, `write-gate.lock` and
`takeover.lock` are never rewritten or deleted. A new store is still created with the six rows
(owner go, epoch 1), the gate and the mirror; the socket binding still records `socket_path` and
republishes the mirror with the socket and scope key, every other field as it was, and a torn
binding is completed by the next open naming the same socket. Nothing repairs a missing mirror
(decision D0): a store whose creator died after linking its stamped database and before publishing
the mirror is written on its stamp, and its mirror stays absent (the operator's
`takeover repair-mirror` went with decision 54). The Stop hook (`ownsGuard`, `localStop`) and
the doctor's ownership block read the mirror and the stamp as before.

Changed, only for states the live host cannot reach: the mirror's phase and its agreement with the
stamp are no longer inputs, so a Go store whose mirror says draining or starting, or disagrees
with the stamp, is written as any Go store is; the doctor's access probe names another owner in
the fence's words instead of validate's; and opening another runtime's store read-write before
refusing it may leave SQLite's WAL sidecars beside it (no row changes: the stamp is judged before
the schema script runs).

Why: the admission guarded a two-runtime handoff that no longer exists (decisions 54 and
55). It copied the database and its WAL three to four times per writable open (the admission,
the schema check, the connection hook, and the binding's preflight) and reread the mirror and the
stamp on every transaction; the stamp alone decides who may write, and it is read where the write
happens.

Measured (`go test -run XXX -bench 'BenchmarkWritableOpen|BenchmarkTransaction' -benchtime 30x
./internal/relay/store/`, five CPUs, the same host, before and after this change): an open and
close of an empty store went from 5.3 ms to 3.0 ms, of a store holding 20 MB of rows from 25.0 ms
to 2.7 ms (the copies grew with the database; the stamp read does not), and a one-row transaction
from 6.8 ms and 173 allocations to 6.9 ms and 16 allocations (its time is the synchronous commit;
the stamp and mirror reread were its allocations).

Evidence: internal/relay/store/stamp.go (`verifyWritable`, `holdGate`, `bindSocket`,
`stampInPlace`), internal/relay/store/ownership.go (`checkStamp`);
internal/relay/store/open_bench_test.go;
Test30SocketBindingBindsAnUnboundStoreOnce, Test30TornSocketBindingIsCompletedOnlyByItsSocket,
Test30CreateAbsentNeverExposesUnstampedDatabase (a crashed creation is written on its stamp),
TestDoctor_ownership_block_matches_python_on_a_broken_store (the probe's words for a mirror-only
breakage).

## 57. The Stop-hook status reading is deleted (wave R1)

Decision: `hook.Status`, the Go port of the Python installer's `hook-status` reading
(`completion.status`), is deleted with its absence diagnosis (`status_absence.go`), its
target and interpreter probes (`probeRegistrations`, `interpreterProbe`, `answersPython`), its
journal and budget cells and the helpers only they used. No command reached it: `crw` has no
`hook-status` (the causes of a missing journal row are read from `crw doctor`, the settings and
the journal, [as the runtime page lists them](../runtime-install.md#registration-is-not-firing)), and its only callers were tests, the contract corpus's hook `status` runs and the
isolated-home integration test. What the installer reads of the user hook file stays:
`AdapterIdentities` and `AdapterCommands` over the same registration reader, which recognizes the
Python-era entry point by name and the shipped native command as decision 23 describes, a home
prefix counting only while the home directory can be named. This supersedes decision 50 and the
status half of decision 23.

The contract corpus drops the reading's scenarios: the 119 `status` runs under
contract/fixtures/hook and the 11 hook runs whose only checks read `status` afterwards are
deleted; `test_completion_hook__test_a_registration_that_keeps_no_journal_is_reported_beside_a_peer_that_does`
and `...__test_an_unstartable_peer_does_not_make_a_working_one_look_like_another_path` keep their
run and its journal-row check without `run.status`. internal/contracttest answers a hook `status`
run (other than the settings document, `document: true`) and `run.status` as fixture errors. The
Go tests that judged the reading go with it; those that judged a production reader through it now
call that reader: `ReadSettings` (the settings-symlink cases), `configurationPath` (the override),
`AdapterIdentities`/`AdapterCommands` (which commands are registrations of this adapter). The
integration test reads the installed settings' owner and the files they name directly.

Why: the reading ran host programs (the relay with `--help`, a registered interpreter with a
`-c` probe) for an answer nobody asked for, and it kept Python-era judgements (whether a named
interpreter runs a supported Python) alive in the product after the cutover.

Cost: none that a command offered. An operator asking why a Stop left no journal row reads the
same causes by hand, in the order docs/runtime-install.md gives.

Evidence: `internal/relay/hook/registrations.go` (`readRegistrations`, `nativeRegistration`,
`AdapterIdentities`, `AdapterCommands`); `TestPluginLaunch_the_declared_command_is_a_registration_of_this_adapter`,
`Test33PR181NativeAndPythonRegistration`, `Test33ReviewD12`, `Test33PR181StatusSymlinkPython`,
`Test33ReviewD1Status`; `internal/contracttest/hook.go` (`runHook`); the product's exec sites,
`git grep -n 'exec.Command' -- 'cmd/*.go' 'internal/*.go' ':!*_test.go'`, none of which runs an
interpreter.

## 58. Refusal reasons no runtime can give leave the vocabulary (wave R1)

Decision: six members of the relay's frozen refusal vocabulary
(`contract/schema/relay-exit-codes.json` `refusalReasons`, and the `contract.Refusal*` constants
generated from it) are removed. `route_ledger_pending` was product routing's answer when the
ledger contract it binds to was absent: the Python `ledger_port.py` checked the binding at run
time, and the Go router kept the state as a `Missing` list only its tests filled, because the Go
binary always carries the ledger. That list, the readiness checks on every ledger-backed path and
the CLI's context override go with it. `operator_release_disabled`, `release_evidence_missing`,
`root_moved` and `fault_write_uncertain` are raised nowhere: no Go path, document, skill or
recorded answer names them outside the schema. `inbox_conflict` was the takeover inbox's refusal
of a retried entry whose bytes differ; with the inbox retired (decision 55) nothing queues an
entry, so nothing can give it. The exit codes are unchanged, and every reason a command can still
give is still a member.

Alongside: the router's test hooks (`BeforeWrite`, `BeforeDigestPage`, `DigestPageSize`,
`DecisionRead`) leave its exported fields for an unexported `routerSeams` only the package's
tests set, and the generator gains a sync test (`TestTheGeneratedCodesAreTheSchemas`). That test
found the generated constants already behind the schema by two members: `store_owned_by_other`,
which the regeneration adds, and `inbox_conflict`, which leaves the schema as above.

Why: a consumer reads a refusal's `reason`; a reason nothing can emit only asks every reader to
handle a case that cannot occur, and a state reachable only by a test's injection is a second
behaviour the product does not have.

Cost: the recorded PRD-13 scenarios that ran with the ledger contract absent are skipped by the
integration replay (`retiredLedgerScenarios`), and the CLI replay of PRD-13, whose route commands
all answered `route_ledger_pending`, is deleted with its recording; docs/port/test-map.md keeps
naming it as the record of the port. The other PRD-13 scenarios (an unwatched surface refused
before the ledger, a binding that touches no ledger) replay unchanged.

Evidence: `contract/schema/relay-exit-codes.json`; `internal/contract/exit_codes_generated.go`,
`internal/contract/generate/main_test.go`; `internal/relay/routing/router.go` (`Router`,
`routerSeams`), `integration_test.go` (`retiredLedgerScenarios`, `Test23_PRD_13_LedgerGate`);
`git grep -n` for each removed reason prints only history; docs/relay/product-routing.md "Status".

## 59. `crw doctor retention-scan` is retired; `crw install remove` keeps its registration and relay-record readers (refactor R1)

Decision: `crw doctor retention-scan` and everything only it read are deleted: the Stop-event
claims (row 1), the journal window (row 2), the managed-start lock holders (row 6), the resumable
threads it asked the App Server for (row 7), the pointer-target row (row 11), the Python-reference
report, the live holds and `clear`. `crw doctor retention-scan` is now an unknown argument of
`crw doctor` (exit 2). What `crw install remove` (and the reclaim of an abandoned staging) reads
before it deletes a runtime is kept as it was: `doctor.RegisteredMatching` reads the registration
rows 4, 5, 8, 9 and 10 with the same readers, shell grammar and unreadable texts, and
`doctor.RecordedDaemons` the relay records of row 3; a registration remove finds still carries its
`row` and `surface` as the scan numbered and named them. `doctor.RetentionOptions` is
`doctor.ScanOptions`, without the clock and the App Server socket only the scan read, and the
unused `doctor.RegisteredInside` is deleted.

Why: the scan was the clearance todo 43 waited on before removing any Python path. The owner's
relay host has no Python runtime, venv or `<CODEX_HOME>/crw-stop-hook.py` shim left (verified
2026-10-01), no skill, hook, wiring file or relay-emitted text runs the command, and the owner
approved removing it (todo 45 reads a host with `grep` and `ps`). Its readers of the host's
registrations and relay records are what `crw install remove`'s in-use rule rests on, so they stay.

Cost: nothing in the product reports whether a turn that started before a replacement may still
hold a replaced hook command; docs/runtime-install.md "Removing a runtime" says to wait for those
turns, as the turn-command cache measurement bounds them (one turn).

Evidence: `internal/runtime/doctor/scan.go`, `references.go`, `cli.go`; `scan_test.go` (the
retention scan's tests of rows 3, 4, 5, 9 and 10, ported to `RegisteredMatching` and
`RecordedDaemons`); `internal/runtime/install` remove and reclaim tests; docs/runtime-install.md
"What remove reads", docs/port/cutover.md "Retention scan surface".

## 60. `install.RemoveLauncher`, the legacy launcher remover, is deleted (refactor R1)

Decision: `internal/runtime/install` no longer carries `RemoveLauncher` (with `LauncherName`,
`LauncherMarker` and its outcome vocabulary), the ownership-checked removal of the
`<CODEX_HOME>/crw-stop-hook.py` launcher copy that `scripts/crw_transition` did before todo 39. It
was deliberately never a command and nothing in the product called it; decision 33's correction
(todo 39), which made its `<launcher>.crw-lock` a leaf of the lock order, no longer applies.

Why: the copy it removed is gone from the relay host (verified 2026-10-01), no Python writer
places one any more, and the operator removes one found on another host by hand once no turn can
still run the pre-native bootstrap that falls back to it (docs/port/cutover.md "Retention").

Evidence: `git grep -n RemoveLauncher` over cmd/ and internal/ before the deletion (its own file,
its tests and a test seam only); docs/plugin-transition.md.

## 61. `crw install` moves the pointer only between Go runtimes and rewrites no Stop settings (refactor R1)

Decision: `crw install rollback` takes only a Go runtime (`bin/crw`) as its target; a directory
the record lists that is not one - a Python `env-*` runtime the fence installer made - is refused
with nothing changed ("... is not a Go runtime (bin/crw), so the pointer is not moved to it",
where it answered "is neither a Go runtime (bin/crw) nor a Python virtual environment" for any
other kind). With it go what only a rollback onto a venv read: the venv launchability reading
(`fenceProblems`, the console-script and interpreter checks), the cached native-payload refusal
(`nativePayload`, `doctor.PluginLaunches`, the answer's `pluginLaunches` and its repair) and this
build's declared schema standing in for the Python runtime's at the swap gate. And no promotion
or rollback writes the Stop settings any more: the Python-era replacement decision 18 added for
the relay host's first Go install - the archive `crw-completion-hook.json.superseded-<time>`, the
Go variant, the atomic-exchange undo (`exchange_*.go`) and the retry on a document that changed
under the read - is deleted. A move still reads the settings under the promotion lock and refuses
settings that name through the pointer a path a Go runtime does not serve, as it did; the answer's
`settings` member keeps `configuration`, `state`, `action` (always `none`) and `detail` and loses
`undone`, `write`, `retired` and `rebuiltFromFreshReading`, which only the replacement wrote; the
`detail` of Go-era settings is the generic "every path the settings reach through the pointer is
one this runtime serves". `crw install hook` no longer replaces a Python-era document either: one
found answers `config_differs` like any other document that says something else
(`config_replaced` is no longer an outcome).

Why: the relay host made the Python-era replacement at its first Go install and has had no Python
runtime since (store owner=go, rollback_allowed=0, no venv or shim, 2026-10-01); decision 48
already refuses the store's half of a return to Python, so a pointer moved back onto a venv could
only release every Stop unrecorded.

Evidence: `internal/runtime/install/{rollback.go,settings.go,target.go,install.go,hook.go,mcp.go}`;
`TestARollbackNeverRewritesTheSettings`, `TestARollbackKilledAtItsCommitLeavesStopsRecorded`,
`TestARollbackRefusesARuntimeThatCannotBeLaunched` (a recorded directory without bin/crw),
`TestAFailedPromotionPutsBackEverythingItChanged`, `TestAUserRegistrationThroughThePointerIsASecondOwner`;
docs/runtime-install.md "One Stop settings document" and "Rolling back".

## 62. The doctor and `crw install remove` no longer tell Python apart (refactor R1)

Decision: `internal/runtime/doctor` classifies an executable without asking whether it is Python.
The kinds `python-interpreter`, `python-script` and `python-venv` are gone, with what decided them
(a `python*`/`pypy*` name, the `.PyRuntime` section or `libpython` in an ELF image, `pyvenv.cfg`,
a `.py` name, the `'''exec'` launcher pip and uv write), and the `python` member of every
executable `crw doctor` and `crw install status` report under `settings` is removed. A Python
interpreter is a native binary like any other: a script it runs is judged as a script run by an
interpreter this reading does not read (unreadable), and a directory is `go-binary` or
`unknown`, never `python-venv` (`crw doctor`'s `selected`, `runtime.kind` and
`runtime.recordSelectsKind`, and each runtime's `kind` in `crw install status`). `crw doctor` no
longer reports a pointer on a venv as "a Python install, which this command does not classify";
it reports it as selecting no runtime it can classify, and a component's `recordedInstall` is
`go` for any entry at its location (a Python entry there is read as the install, and its missing
`binaryDigest` leaves the component unreadable). The doctor's registration judgement no longer
names a Python interpreter or the checkout's `completion_hook.py` as such (each is a conflict as
any other program is), and a `python -m codex_thread_bridge` table is not recognised as starting
the bridge. `crw install remove` follows the script operand of a shell only: a Python
interpreter's operand and its `-m`, `-c`, `-W` and `-X` options are no longer read, so a root
Python agent run from a hidden working directory (the Azure WALinuxAgent) no longer makes remove
refuse. A release archive may carry `pyvenv.cfg`, which nothing reads any more.

Why: no Python runtime is left on the relay host, rollback and the settings transition no
longer act on one (decision 61), and the retention scan that reported Python references is
retired (decision 59). A Go runtime directory holds no Python program, so reading Python
interpreters' operands found nothing that could run out of one.

Evidence: `internal/runtime/doctor/{executable.go,shell.go,scan.go,doctor.go,registration.go}`,
`internal/runtime/install/remove.go` (`shell`, `argvOperands`);
`TestClassifyJudgesAScriptByWhatItRuns`, `TestAScriptIsJudgedByTheInterpreterItsHashBangResolvesTo`,
`TestLiveProcessesRuleOutOnlyWhatTheyRead` (a Python program run relative),
`TestRegisteredMatchingJudgesOnlyWhatItsGrammarReads`, `TestDoctorJudgesTheStopCommandsInHooksJSON`.

## 63. A claim the Python installer wrote, and an `env-*` directory, are no longer `crw install`'s (refactor R1)

Decision: a staging claim is this command's only when it says `writtenBy` `crw install`; one the
retired Python installer wrote (`runtime_install.py`) reads as somebody else's file, like any
other writer's ("this claim was not written by crw install, it names 'runtime_install.py'",
where it named both writers). `staging.WriteClaim` no longer rewrites a claim into the Python
installer's shape for an `env-*` directory or a claim that writer made, and settling a claim
always writes `crw install`'s. `crw install remove`, `crw install status`, the tombstones and
the destination check take only `bin-*` runtime directories: an `env-*` directory is refused as
"not a bin-* runtime directory (or the tombstone of one)" and no longer listed among the runtimes
`crw install status` reports.

Why: the claims were written in the Python installer's shape so that `runtime_install.py`, which
reads only its own marker, would still read its venvs as its own. It left with todo 44, the relay
host has no `env-*` directory (2026-10-01), and `crw install rollback` no longer points at one
(decision 61).

Evidence: `internal/runtime/staging/staging.go`, `internal/runtime/install/{identity.go,remove.go}`;
`TestReadClaimKeepsFourAnswersAndOwnership`, `TestRemoveRefusesWhatMayStillBeInUse`,
`TestRemoveRefusesARuntimeARegistrationStillNames`.

## 64. `definition.Digest` is deleted; an outgoing selection names a Go runtime's binary digest only (refactor R1)

It supersedes the `definition.Digest` sentence of decision 35.

Decision: `definition.Digest`, the port of `ops12_digest` (the tree digest of a Python selection's
package directory, with `os.fsdecode` ordering and `UnicodeEncodeError`), and what only it used
are deleted. The one caller, the outgoing baseline a promotion records, gave it the selected
location when that was not a Go runtime's `bin` beside a native `crw`; such an entry's `digest` is
now null (`present` and `selected` are as before). A Go runtime's entry keeps the SHA-256 of its
`crw`.

Why: only a Python selection took that branch, and no promotion leaves one any more (decision
61).

Evidence: `internal/runtime/definition/definition.go`, `internal/runtime/install/install.go`
`outgoingOf`; `TestOutgoingIsWrittenOnlyByAPromotion`, `TestAPromotionRecordsTheRuntimeThePointerLeaves`.

## 65. One reader of the bridge record for the launcher, the installer and the doctor (refactor R1)

Decision: the bridge record's contract is read once, in `internal/pluginwiring` (`ReadBridgeRecord`,
`ReadPolicyReference`, `ReadPolicyPath`, `PolicyDigest`), by the launcher (`Prepare`), the
installer's record checks (`bridgeComplaints`, `policyComplaints`, `policyFileComplaints`,
`executionPolicyReading`) and the doctor's judgement (`pluginBridge`, `policy`). Each keeps the
order it checks in and its own words. Two readings of the installer and the doctor move to the
launcher's: the installer reads `recordVersion` by Python's `==` as the launcher and the doctor
already did (`true` and `1.0` are version 1, where it named them malformed), and a policy file
whose descriptor cannot be examined (fstat failing) reads as not a regular file in all three,
where the launcher named the error and the doctor left it unreadable.

Why: the three copies had drifted only in those two corners, and the launcher is the reader
whose answer decides whether a bridge starts.

Evidence: `internal/pluginwiring/record.go`, `launch.go`; `internal/runtime/install/mcp.go`;
`internal/runtime/doctor/registration.go`; `TestRegisterMCPWritesTheRecordAndRefusesASecondOwner`,
`TestDoctorJudgesTheBridgeRecordAsTheLauncherAcceptsIt` and the launcher's contract tests in
`internal/pluginwiring`.

## 66. The Stop settings' adapter keys and the legacy hook entry forms are retired (refactor R1)

Decision: the Stop settings keys `adapterInterpreter` and `adapterEntryPoint`, and every way of
reaching the Go hook other than `crw hook --plugin-launch` and `crw hook` with no argument, are
retired:

- the hook's reader (`internal/relay/hook` `Complaints`) no longer requires the two keys for a
  plugin owner nor checks them when present: a document that carries them is read as it is, so the
  relay host's current settings stay accepted by the new build before anything rewrites them;
- `crw install hook --owner plugin` no longer writes them, nor refuses while
  `CRW_COMPLETION_HOOK_CONFIG` is set. Settings that differ from the document it would write only
  by the two keys and `installedBy` are rewritten without them, answering a new outcome,
  `config_replaced`, with `retiredFields` naming the keys dropped (a dry run answers
  `config_would_create` with a detail saying so); any other difference is `config_differs` as
  before. On the relay host, `crw install hook --owner plugin`, run with the flags its settings
  were written with once a runtime carrying this change is installed, rewrites the document once;
- `crw hook` reads no settings path argument and no `CRW_COMPLETION_HOOK_CONFIG`; given any
  argument other than `--plugin-launch` it releases the Stop in silence, as every controlled path
  of the hook already exits 0 with nothing on stderr;
- `crw` no longer dispatches on the program name `crw-completion-hook`, the release build
  (`.goreleaser.yaml`) no longer makes that link, and `crw install` no longer places it
  (`definition.Links`); the `codex-session-relay` and `codex-thread-bridge` links stay;
- the doctor no longer judges the two keys (the `adapterEntryPoint` and `adapterInterpreter`
  registration entries, and the `env` interpreter contract of decision 18 as todo 38 corrected it,
  leave its answer), `crw install remove` no longer counts them as registrations, and a move of the
  pointer judges only `relayExecutable` of the settings; the Stop-hook status reading, which had
  cells for them, is already deleted (decision 57).

Why: the keys and the entry forms served the Python launchers (`crw_stop_hook.py` and its
`<CODEX_HOME>/crw-stop-hook.py` copy) and the user-owned registration; the launcher left the
package in todo 43, the user owner retired with the Python installer, and the plugin's declared
Stop hook has run `crw hook --plugin-launch` since decision 26. The order keeps the live host
working: the reader accepts the old document first, the writer stops writing the keys, and the
rewrite is one command the operator runs.

Evidence: `internal/relay/hook/settings.go`, `adapter.go`;
`internal/runtime/install/settings.go`, `hook.go`; `internal/runtime/doctor/registration.go`,
`doctor.go`, `scan.go`; `internal/runtime/definition/definition.go`; `cmd/crw/main.go`;
`.goreleaser.yaml`; `TestHookWritesTheGoSettingsAndRefusesASecondOwner`,
`TestHookRewritesSettingsThatDifferOnlyByTheRetiredKeys`, `TestHookSettingsBytesArePythons`, the
hook's settings and plugin-launch tests, and the isolated-home integration test.

## 67. The doctor and `crw install remove` stop recognising the Python Stop shapes and the user-owned bridge table (refactor R1)

Decision: the user-owned registration shapes that `--owner user` wrote (retired with
runtime_install.py) keep only their generic reading. What stays: `crw install hook` still refuses
while `hooks.json` registers the Stop hook (hook.AdapterIdentities), a promotion or a rollback still
refuses a second owner of either surface, and the doctor still judges every `hooks.json` Stop
command that runs the hook against the selected runtime. What goes:

- the doctor and the registration reading `crw install remove` rests on no longer recognise the
  checkout's Python adapter (`completion_hook.py`) or the Python launchers (`crw_stop_hook.py` and
  its `<CODEX_HOME>/crw-stop-hook.py` copy) as a Stop hook; such a command is read as any other
  command is;
- `crw install remove` no longer reads the launcher copy (row 8 of its readings; rows 4, 5, 9 and
  10 keep their numbers), which the relay host no longer holds (decision 59);
- neither reads `CRW_COMPLETION_HOOK_CONFIG` any longer (decision 66);
- the doctor judges a `hooks.json` Stop hook as `crw hook` (the name its answers give it, where
  they said `crw-completion-hook`): started under the retired `crw-completion-hook` link, or given
  an argument other than `--plugin-launch`, it is a conflict, since the selected runtime then runs
  no hook; a settings path such a command names is no longer compared with the judged settings.
  `definition.HookScript` is deleted;
- a user-owned bridge record's `serverName` no longer makes the doctor judge that `config.toml`
  table: the launcher stands down for a user-owned record, and a table that starts the bridge is
  judged under any name, as before.

The registration reading still reads the settings document the word after `crw hook` (or after an
older runtime's `crw-completion-hook` link) names, because a runtime installed before decision
66 reads it; reading it can only find more.

Why: nothing writes these shapes since the Python installer's removal, the relay host holds none of
them, and the doctor's judgement of a Stop command should be the selected runtime's reading of it.

Evidence: `internal/runtime/doctor/registration.go`, `scan.go`, `references.go`;
`internal/runtime/install/remove.go`; `TestDoctorJudgesTheStopCommandsInHooksJSON`,
`TestDoctorJudgesTheBridgeUnderEveryTableName`, `TestRegisteredMatchingFollowsEveryRegistrationIntoTheRuntime`,
`TestRegisteredMatchingReadsTheSettingsAStopCommandReads`, `TestRemoveRefusesARuntimeARegistrationStillNames`.

## 68. `crw install remove` holds a removal back only for what CRW registered in hooks.json and config.toml (refactor R1)

Decision: in the registration reading `crw install remove` (and the reclaim of an abandoned staging)
rests on, `doctor.RegisteredMatching`, an entry of `<CODEX_HOME>/hooks.json` (row 9, a hook command
of any event) or of `config.toml`'s `mcp_servers` (row 10) is CRW's when a word it names or runs, a
path the reading classifies for it, or where that path resolves is one of CRW's programs (`crw`,
`codex-session-relay`, `codex-thread-bridge`, the retired `crw-completion-hook`) or lies in the
destination, spelled literally, resolved, or from `$HOME`, `${HOME}` or `~`. What the reading could
not read or judge of an entry that is not CRW's (and the Stop settings such an entry would leave
unknown) is dropped rather than listed under `unreadable`, so it no longer refuses a removal. The
rest is unchanged: a path any entry names inside the directory is still found and refuses; a
`hooks.json` or `config.toml` that cannot be read or parsed, or holds a malformed entry, still
refuses; the CRW files, the `crw-*.json` records (row 4) and the CRW plugin's cache (row 5), still
refuse on anything they hold that cannot be judged. `doctor.ScanOptions.Foreign` keeps the dropped
entries, for the tests of the reading's grammar.

Why: todo 43's removal of the Python runtime directories refused on three registrations that were
not CRW's and that the reading could not judge (another tool's `SessionStart` hook running
`python3`, a `gemini_notebook` server started over `ssh`, an `oracle` server run by `node`), and
succeeded only once `--codex-home` named an edited copy of the Codex home. Such an entry cannot
start a runtime it never names: the risk accepted is a foreign script that starts a CRW runtime by
a path it computes itself, which the reading could not have established either.

Evidence: `internal/runtime/doctor/scan.go` (`shared`, `crwWord`, `crwPath`), `references.go`;
`TestRegisteredMatchingHoldsARemovalBackOnlyForCRWsRegistrations`,
`TestRemoveRefusesARuntimeARegistrationStillNames` (fails with the narrowing disabled);
docs/runtime-install.md "What remove reads".

## 69. `crw install hook` drops `--adapter` and `--event`; `--owner` defaults to plugin (refactor R1)

Decision: `crw install hook` no longer takes `--adapter` (whose one value was `completion`) or
`--event` (whose one value was `Stop`); either is now an unknown flag (exit 2). `--owner` of
`crw install hook` and `crw install register-mcp` defaults to `plugin`, the only owner either
accepts, so `--owner plugin` may be left out and `--owner user` is refused as before. The answers
are unchanged: `hook` still names `adapter` `completion`, `event` `Stop` and `owner`. The no-op
`--json` of `crw doctor` stays accepted. `record.Path` and `staging.Claim`, which nothing called,
are deleted.

Why: a switch with one allowed value only adds a way to fail; no skill, document or wiring file
passes `--adapter` or `--event`, and every one that passes `--owner` passes `plugin`.

Evidence: `internal/runtime/install/cli.go`, `hook.go`; `TestHookAndRegisterMCPNeedNoSingleValueSwitch`.

## 70. The Stop hook's final row is bounded by the 5 s deadline, not a shortened budget

Decision: a `timeoutSeconds` below 5 still shortens every wait of the native Stop hook (the
input, the identity scan, the dial and the guard allocation, decision 32), but no longer the
final journal row and ledger outcome that record how the invocation ended. That bookkeeping is
bounded by the unshortened absolute deadline, 5 s after process entry. With the default budget
(5 s, which `crw install hook` writes) the two deadlines are the same and nothing changes.

Why: the bookkeeping reserve inside a shortened budget is at most 100 ms. A guard that used its
whole allocation expires at the reserve's start, and a host that leaves the process unscheduled
past the reserve made the journal refuse the write (`ctx.Err()` before the create-once open) or
`bounded` return before it, so the invocation left no row at all: the contract corpus's
guard-timeout fixtures (`test_completion_hook__test_a_guard_that_never_answers_is_killed_and_the_turn_ends`,
`test_adapter_agreement__test_a_runtime_past_its_budget_times_out_in_both__packaged`, both with a
1 s budget) failed twice on loaded CI runners with `rows` empty. Python journals after a guard
timeout without a deadline; decision 32's rule applies: time the process spent unscheduled says
nothing about the host or the owner, so it must not remove the record. The row's content does not
change, and the hook still prints nothing and exits 0 on this path.

Cost: settings with a budget below 5 s and a journal on a hung filesystem hold the hook until
5 s after entry instead of the shortened budget, still inside the host's 10 s.

Evidence: internal/relay/hook/adapter.go (`bookkeeping`, used by `finish`, `recordFault` and the
decision-22 pre-scan row); `Test33StalledPastTheBudgetKeepsTheTimedOutRow` in
internal/relay/hook/stall_test.go stops the process from the guard request until 1.5 s later and
fails without the change (no row). With every hook process of the two fixtures stopped for a
random 50-450 ms, the pre-change binary failed 17 of 50 runs with exactly the CI signature and the
changed one none of 50.

## 71. CI runs every check on every event; `dev-gate` is a shell check of its prerequisites

Decision: the `selection` job, `crw-dev ci scope` and `crw-dev ci gate` are removed, with their
Python twins (`scripts/ci/scope.py`, `scripts/ci/gate.py`) and tests. Every CI job runs on every
pull request, dev push and dispatch, and `dev-gate`, the one required check (its name is
unchanged), needs every other job, runs `if: always()`, and passes only when every job it needs
reports `success`; it still fails a pull request whose base is `main`. The job results reach its
shell step as a file the step's own script writes from `toJSON(needs)`, never as an environment
variable. The `tests` job, which ran the CI checks' Python twins' own tests, is removed too: its
release cases are Go tests (R1D1), and the twins that stayed (`plugin.py`, `validate.py`) were
compared with their Go checks by `internal/dev/ci` in `make test` until refactor R3 deleted them
(decision R3R-1). The dist leg's
second `crw-dev ci plugin` run is removed; the validate job runs it once.

Why: the Go product legs, the longest, always ran whatever changed, so path selection only ever
skipped the Python tests job, and it put one job (and a `crw-dev` build in the gate) on every
run's critical path. The selection's changed-path list travelled to the gate in `NEEDS_JSON`, one
environment variable, so a pull request that changed about a thousand paths made the gate fail
with "Argument list too long" before it judged anything. No consumer reads these commands: no
skill, hook or wiring file names them; they are developer tools.

Evidence: .github/workflows/ci.yml (`dev-gate`); internal/dev/ci/workflow_test.go
(`TestWorkflow_the_gate_passes_only_when_every_prerequisite_succeeded` runs the step's script
against written results, including a failed, cancelled and skipped job, unreadable results, a PR
into main and a 240 KB result; the structural tests hold the needs list, `if: always()`, pinning,
the legs and one run of each check); docs/CI.md.

## 72. Expected test outputs are Go goldens regenerated with `CRW_GOLDEN=update`; Python's inputs are frozen fixtures (refactor R2)

Decision: the Go tests no longer compare with answers the Python reference implementation gave.
Every expected value a test read from a recording under `testdata/python-oracle` (through
`internal/testsupport/pyoracle`) or from a frozen Python answer file (`python_*.json`,
`goldens.json`, `mcp_python.json`, `worker_reasons.json` and the like) is a golden:
`internal/testsupport/golden` keeps the test's own value, after the normalization its comparison
applied, under a key the test names in `testdata/golden/<test>.json.gz`, compares with it by
default, and rewrites it when `CRW_GOLDEN=update` is set. An intended change of output is one
update run over the affected packages followed by a review of the goldens' diff. Goldens are
host-independent: run-specific strings (temporary directories, ports, pids, instants, the
journal's day directory) are stored as placeholders, so no golden holds the day or the host it was
written on; the fixed directories under `/tmp` (`/tmp/crw-delivery-parity`, `/tmp/crw-cli-parity`,
`/tmp/crw-oracle` in the hook tests) stay only where a golden holds a value derived from an
absolute path: a hash of it, or a position in it. What Python produced for a test to start from,
which Go cannot produce (stores, directory trees, scenario calls, CLI case files, a sweep's
generated cases), is a fixture under `testdata/fixtures`, read with `golden.Fixture`; no mode
rewrites a fixture. `internal/testsupport/pyoracle`, `CRW_PYTHON_ORACLE`, the capture closures and
all 2,897 recordings are deleted.

One fixture holds expected values and cannot be regenerated:
`internal/relay/store/testdata/fixtures/draft7-verdicts.json.gz`, the Draft 7 validator's verdict
on each record with the instance it judged. No Go Draft 7 validator is in use, so the test fails
when a record's instance changes in substance until that instance is judged again.

Why: Python left in todo 44, so its recorded answers could never be refreshed, and the next
refactor step (P2, removing the Python-quirk emulation nobody consumes) changes outputs on
purpose; every expected output had to become regenerable from Go first. Each golden was first
written in a run that also held the test to its recording, so every golden began equal to Python's
answer under the test's own normalization; the phase-A reports name the few keys that hold Go's
answer where Python had none comparable.

Evidence: internal/testsupport/golden/golden.go (Check, CheckJSON, Want, Fixture, Substitute,
Compare; `CRW_GOLDEN`); 2,735 golden files and 507 fixtures under 36 packages' testdata; the R2
sections of docs/port/oracles/g1.md to g5.md (per package goldens, fixtures and deleted
recordings); `go list -deps -test ./... | grep pyoracle` is empty; `CRW_GOLDEN=update make test`
leaves the tree unchanged.

## Decision R2B-1. The capacity and edit-region commands parse their lines as every relay command does (refactor R2)

(Placeholder heading: the next free number is given when the R2 groups merge.)

Decision: the thirteen capacity and edit-region commands (`slot-reserve`, `slot-release`,
`limit-declare`, `usage-observe`, `capacity-show`, `region-propose`, `region-settle`,
`region-restate-revision`, `region-reaffirm`, `region-followup`, `region-followup-accept`,
`region-followup-settle`, `region-show`) lose their own parser (`capacity.Precheck`, which
`crw relay` ran before the relay CLI). Their lines are parsed by their argparse specs in the relay
command table, as every other relay command's line already was, and as `cli.ExecuteAs` already
parsed them for in-process callers. What a parsed line answers is unchanged, and so is their help:
`-h`/`--help` still prints the usage line alone, unwrapped (`dispatch.Command.UsageHelp`), which
the root parser sweep's goldens hold (in-process callers, who got argparse's full help, now get
that line too). What changes is how a line the second parser rejected or cut short is answered:

- `-hx` is help, as for any command, where it was an unrecognized argument.
- An abbreviated option (`--subj` for `--subject`) is accepted, or refused as ambiguous, where it
  was reported as missing its full spelling.
- Unrecognized arguments name the root parser (`crw relay: error: unrecognized arguments: ...`
  under the root usage) and a value that looks like an option (`--tenure -x`) is "expected one
  argument", as argparse reports them; usage lines wrap at the terminal width.

Why: two parsers answered the same thirteen command lines, and which answered depended on the
entry point. The argparse spec is the contract every other relay command keeps (`--help` lists the
flags); the second parser was the one deviation. No skill, document or golden depends on the
second parser's error text; its help is the one answer a golden holds
(`Test24BuiltBinaryRootParserParity`, `region-show --help`), and it is kept.

Evidence: `internal/relay/capacity/cli.go`, `cmd/crw/main.go`;
`Test27_CCL1_capacity_and_region_commands_are_registered_offline_and_not_marker_commands`,
`TestCapacityCommands_the_built_crw_prints_what_python_printed`,
`TestRegionCommands_the_built_crw_prints_what_python_printed`, `Test24BuiltBinaryRootParserParity`.
A sweep of 278 capacity command lines through the built `crw relay` before and after found every
difference in the classes above, none in a help answer and none in an answer to a line both
parsers accepted.

## Decision R2B-2. Every relay command takes one dispatch path; its endings read alike (refactor R2)

(Placeholder heading: the next free number is given when the R2 groups merge.)

Decision: the relay CLI keeps one command table (`internal/relay/dispatch`) and one path through
it: the root parse (the global options are read there only), the command's argparse parse, the
state directory, check_start for a write form, the selection refusal, then the writable store's
admission (or `--kind-module` alone), the handler, and one emit. Each command's registration
carries what that path reads: read-only (cli.py READ_ONLY_COMMANDS and `_read_only_command`),
answers without a state directory (`merge-evidence`, `ack-proof`), answers without the selected
store (`_reads_no_selected_store`, the marker commands), exempt from the selection refusal, admits
its own store. The order of the checks is cli.main's for every family, as it was. The command
families' own entry points and their re-parse of the global options are gone.

The doctor's `offlineCommands` and `hostRequiredCommands` stay the doctor's own lists (cli.py's,
in cli.py's order) rather than attributes of the table: the doctor answers them alike in every
build, whichever command packages it links, while the table holds what the binary registered
(in-process test builds link only some families, and the CLI package cannot link them all: the
managed and routing packages' in-process tests import it). A test of the built command set holds
every listed name to a registered command and every parser choice to a registration.

Where the families answered the same ending differently, every command now answers it as cli.py
did:

- A `--state` (or `CODEX_SESSION_RELAY_STATE`) that cannot be resolved because `~user` names no
  user, or `~` has no home, answers `RuntimeError: Could not determine home directory.` (exit 3).
  The registry, linkage, merge-turn and fault commands answered the wrapped Go text instead
  (`cannot determine home directory for "x": ...`), the delivery and marker commands that text
  behind `OSError: `. Any other failure to resolve it (a relative `--state` under a working
  directory that is gone) reads as the command family's unclassified failure, as the handler's
  own failures do; the delivery and marker commands answered it behind `OSError: `.
- A `--kind-module` that cannot be imported is named in Python's repr by every command. The fault
  commands' read-only forms (`fault-show`, `fault-next`, `fault-attention`,
  `fault-notifications`, and `fault-policy`/`fault-limit` naming no class or kind), which imported
  the modules themselves, spelled a name holding a quote or a backslash between bare single
  quotes.
- The families' own consoles (`registry.ExecuteAs`, `delivery.ExecuteAs`, `faults.ExecuteAs`
  without the relay CLI's check, which only tests drove) are gone, and with them the delivery
  package's copy of the selection refusal and of the recovery lines
  (`internal/relay/delivery/selection.go`); the relay CLI's (`selection.Refusal`) is the one,
  tested in `internal/relay/selection`.

Why: six entry paths served the relay CLI, each re-reading the global options and keeping its own
copy of the selection refusal and of the ending-to-JSON classification; the copies had drifted
apart only where nothing looked. One table and one path keep the refusal order in one place.

Evidence: `internal/relay/dispatch` (`Execute`, `Command`, `emit`), the families' registrations
(`internal/relay/cli/registry.go`, `internal/relay/registry/cli.go`, `external.go`,
`internal/relay/delivery/cli.go`, `intent_cli.go`, `internal/relay/faults/cli.go`);
`TestRun_every_relay_parser_choice_is_registered`,
`TestRun_unknown_user_state_is_a_python_host_error` (cmd/crw); every relay CLI golden compares
unchanged.

## Decision R3S-1. The store's and the service's messages name an OS or SQLite failure in Go's words (refactor R3)

Decision: where the relay store, the service and the Stop hook's settings and routing readers
put an OS or SQLite failure into a message that is only shown, the failure is worded as Go's
error says it (`open <path>: permission denied`, `file is not a database (26)`,
`SQL logic error: no such table: x (1)`) instead of as CPython's `str(OSError)` or `sqlite3`
exception (`PermissionError: [Errno 13] Permission denied: '<path>'`,
`DatabaseError: file is not a database`). This covers the doctor's `access` detail and its
ownership block's `detail`, the read-only store readers' `detail` (dispositions, managed-show,
the doctor's nonce and issue lookups, a service's `projects` reading, an omission's
`store_unreadable:` reason), a writable or read-only open's host error, the ownership mirror's
`takeover record unreadable:` refusal, the registration hold's refusal, an unenforced guard
index's `detail`, the launch declaration's unreadable `detail`, the supervised worker's and the
process handle's details, the hook settings reader's `config_unreachable` detail (which only
`crw doctor` shows; the hook journals nothing on that path) and a routed `guard-evaluate`'s
refusal when the owner's socket cannot be reached or trusted. Every message keeps its field, its
reason and its exit code, and still names the path. The store's `pythonHostError` is `hostError`,
which unwraps to the failure, so `store.PythonSQLiteError` of it still answers what it answered.
A routed `guard-evaluate` reads the owner's answer to the depth it always read (9998 containers,
`hook.routeDepth`, now a plain count instead of the C scanner's recursion check); an answer nested
deeper, or an error record whose kind is a list or an object, is "the owner closed control.sock
without a readable guard-evaluate answer", where it was `RecursionError: ...` or
`TypeError: unhashable type: ...`: a host error, exit 3, as before.

Consumer check: `plugins/crw/skills`, `docs/` and `contract/` were searched for `Errno`,
`PermissionError`, `FileNotFoundError`, `OperationalError`, `DatabaseError` and the changed
messages' prefixes. The only consumer of errno wording is the Stop journal's `guard_unreachable`
detail, which `hook.NativePrescanUnreachable` parses back and the contract corpus pins
(`test_adapter_agreement__test_a_runtime_that_cannot_be_run_is_unreachable_in_both__*`); no Go
caller parses the changed details (`crw doctor` reads the relay doctor's booleans and fields,
not its `detail` prose), and every reason (`store_owned_by_other`, `store_unreadable`,
`execution_policy_unreadable`, `supervised_fd_unreadable`) is unchanged.

Kept, because the text is stored: `store.PythonOSError`, `store.PythonOSErrorText`,
`store.PathRepr` and the hook's `pythonErrnoName` where they write the Stop journal (the
pre-scan and post-identity `guard_unreachable` detail and `errno` field), the frozen manifest's
reach failure in a guard's receipt detail (which the hook journals as `receiptDetail`, decision
44), and the state discovery's access error (`discoveryExists`), which the owner's guard reaches
through its store selection and the in-process Stop journals as the row's `fault`. The intent
records' `store_unopenable` and `store_write_failed` details are delivery's
(`store.PythonSQLiteError`, refactor R3D's area) and are unchanged.

Evidence: internal/relay/store/{diagnostic_probe.go,diagnostic_read.go,diagnostic.go,hold.go,
ownership.go,registration_hold.go,store.go,refusal.go (`hostError`),state.go (`discoveryExists`),
pyerr.go}; internal/relay/service/{policy.go,worker.go,process_linux.go};
internal/relay/hook/{route.go (`routeDepth`, `nesting`),settings.go}; the goldens of
internal/relay/{store,service,cli,delivery,registry,linkage,managed}; tests
`TestRouteGuard_reads_an_answer_within_its_nesting_cap`,
`TestGuardEvaluate_routes_to_the_owners_control_socket_as_the_fence_does`,
`TestDoctor_ownership_block_matches_python_on_a_broken_store`,
`Test27_MST_10_ManagedShowAbsentDoesNotCreateStore`.

## Decision R3S-2. The owner reads a control.sock request strictly, in Go's words (refactor R3)

Decision: `hook.HandleControl` no longer reads a guard request as the fence's `control.py`
read it under CPython 3.13. Gone: `json.loads`' decoding of bytes (a UTF-8 byte order mark
skipped, UTF-16 and UTF-32 detected), the C scanner's recursion budget near the nesting edge
(`pyjson.ErrorWithBudget`), CPython's integer-digit limit, `protocol` compared with 1 as Python
compares it (`1.0` and `true` were served), `noRecord` read by its truth value,
`datetime.fromisoformat` as CPython 3.13 parses it (`hook/isoformat.go`, 382 lines), and the
host details in Python's `<exception class>: <message>` words (`JSONDecodeError: Expecting value:
line 1 column 1 (char 0)`, `KeyError: 'params'`, `TypeError: can't subtract offset-naive and
offset-aware datetimes`, `TimeoutError: timed out`). The owner reads one line of at most 64 MiB,
UTF-8, nested no deeper than 9996 containers (`controlDepth`, a plain count), as an object whose
params hold an object `stopInput` and an RFC 3339 `deadline` still ahead (`time.Parse` with
`time.RFC3339Nano`); it serves `protocol` the integer 1 and `method` `guard-evaluate`, reads
`noRecord` as true only when it is `true`, and answers every request it cannot serve with the
host record `{"error": "host", "detail": <why>}` in Go's words (`guard request is not JSON:
invalid character ...`, `guard params must be an object`, `guard request deadline expired`,
`guard request line not received in time`). A dispatch other than guard-evaluate at protocol 1 is
still `{"protocol":1,"requestRejected":true}`.

Kept, for the hook protocol: what a Go hook sends is served exactly as before. The hook writes the
request with `pyjson.Dumps` (ASCII, one line), `protocol` 1, `noRecord` a boolean and the deadline
`deadline.UTC().Format(time.RFC3339Nano)`, and it forwards the Stop payload it accepted from Codex
as it read it, so the owner reads a request's values as the hook read that payload
(`requestValues`: `NaN`, the infinities and a lone surrogate escape are values, objects keep their
order, an integer is an int64) and refuses past the depth it always refused. The hook journals a
host record as `guard_host_error` with no detail, so the new wording reaches no journal row, and
the Stop payload's decoding, the EventKey and every journal row the hook writes are unchanged
(the hook's own stdin reading is not touched). The one reading that differs for a request a hook
could forward is a `NaN` or an infinity at exactly the 9996th container, which CPython's budget
refused and the plain cap reads (docs/port/known-defects.md).

Consumer check: the only clients of `control.sock` are the Go hook (`hook.RequestGuard`) and the
routed `guard-evaluate` CLI (`hook.RouteGuard`), both of which write the request as above; no
skill, doc command or contract fixture sends a frame or reads a host record's detail
(`contract/fixtures` drive the hook against a guard peer, not the owner's reader). The control.sock
request and response fields, the rejection frame, the 64 MiB and 5 s bounds and the answer to a
peer that never finishes its line are unchanged.

Tests: `TestControlReadsEveryFrameAsControlPyReadsIt`, its 28,321-frame fixture
(`control-frames.json.gz`, 199 KB) and its 144 KB golden of Python's exception texts are deleted;
`TestControlAnswersEveryRequestItCannotServeWithTheHostRecord` and
`TestControlReadsNoRecordAsTheBooleanTheHookSends` (internal/relay/hook/control_request_test.go)
hold the reading: every unservable frame answered with exactly the two-field host record, the
rejection, and the served forms (a UTC or offset deadline, a payload holding constants and a lone
surrogate escape, a payload nested to the cap). `Test30ControlPeerFailuresAreAnsweredAsPythonAnswersThem`
is `Test30ControlPeerFailuresAreAnsweredWithTheHostRecord`: the listener, accept and daemon cases
keep their properties (each failure answered with the host record, the whole-line read bound,
a hang-up skipped, an accept retried, the next Stop served, a clean Close and exit 0), without the
edge corpus of CPython's recursion texts and its golden.

Evidence: internal/relay/hook/control.go (`controlDepth`, `requestValues`, `readRequest`,
`guardParams`), internal/relay/hook/adapter.go (`HandleControl`), internal/relay/hook/route.go
(`nesting`); internal/relay/hook/control_request_test.go; internal/relay/service/control_test.go;
docs/port/cutover.md (the control server paragraph).

## Decision R3S-3. What the store side keeps of the Python emulation, because it is stored, hashed or journalled (refactor R3)

Decision: wave R3 leaves these readings and wordings of the relay store, service, managed,
adapter, daemon, linkage, selection and hook packages as they are, each because a byte it
produces is stored, hashed or journalled (the brief's items 1 and 2), or because it is the hook
protocol's:

- The Stop hook's reading of its stdin (`hook.Decode`, `hookValues`: `json.loads`' refusals and
  their `JSONDecodeError` text, `NaN`, the infinities and lone surrogate escapes accepted). The
  values feed the EventKey and every journal row the hook writes (`sessionId`, `turnId`,
  `eventIdentity`), and the refusal text is the stored `stdin_not_json` detail the contract corpus
  pins (`test_adapter_agreement__test_every_payload_failure_is_the_same_failure_in_both__not_json__*`).
  Codex sends neither a constant nor a lone surrogate, so refusing them would change no real
  journal row, but it would change the stored row and key of such a payload, and it deletes no
  code: the readings are options of the shared `internal/pyjson`. The owner keeps accepting what a
  hook forwards for the same reason (decision R3S-2).
- The hook's errno names (`hook/errno.go`) and `store.PythonOSErrorText`/`store.PathRepr` in the
  journal's `guard_unreachable` detail, which `hook.NativePrescanUnreachable` parses back; the
  guard's verdict texts (receipt details, `fault`s, selection refusals), which the hook journals;
  and the frozen manifest's Python value model (`store/frozen_value.go`, `frozen_detailed.go`:
  `PythonManifestEntries`, `PythonRevisionHash`, the `ManifestException` classes), whose entries
  feed the manifest revision hash and whose exception texts reach a guard's journalled answer and
  the omission reader (decision 44).
- pathlib's spelling of paths (`store.PathlibSpelling`, `PathlibChild`, `PathlibParent`,
  `ownership.PathlibSpelling`, `pythonNormpath`, `ScopeRoot`): it names the state directory, the
  store's `Path` and the scope root, which feed the state-dir key, the scope key and the managed
  request fingerprint (`managed/identity.go`), are stored in the ownership mirror and the scope
  records, and decide which declared paths a scope accepts.
- The bridge adapter's Python-shaped host-reply errors (`adapter/shape.go`, `pythonError`,
  `pythonConnectionError`, `cursor.go`): their class and message are written into delivery
  receipts (`transport.go`, the ledger's `error`; `delivery.errorLabel` in the delivery receipt
  and its observations), and `pythonError` decides whether a failed listing is recorded.
- `repr()` quoting (`pyvalue.StrRepr`, `pyvalue.Repr`) in the store's artifact, scope and
  continuation refusals, which the guard journals and the receipt intake stores (the `refusals`
  table's `detail`), in the receipt intake's `unassigned_turn` refusals (`adapter/cli.go`) and in
  the selection refusals the guard answers with; `json.dumps` spacing in the worker policy record,
  the scope record and the daemon's journal rows, which are stored.
- Readers of stored documents keep their leniency and their refusal texts: the launch
  declaration (`launch-policy.json`, `service/policy.go`, read to the depth CPython read it), the
  service and worker records, receipts and continuation claims, and the ownership mirror.

Consumer check: for each item the writer was followed to where its bytes land (SQLite `journal`
and receipt rows, the bridge ledger, the Stop journal, the mirror and scope records, a hash);
none is display-only.

Evidence: internal/relay/hook/{value.go,errno.go,adapter.go,journal_row.go}; internal/relay/store/
{pyerr.go,frozen_value.go,frozen_detailed.go,state.go,pyloads.go,scope.go};
internal/relay/store/ownership/record.go; internal/relay/managed/{identity.go,start.go};
internal/relay/adapter/{shape.go,transport.go,cursor.go}; internal/relay/service/{policy.go,
record.go,worker.go}; internal/relay/daemon/observe.go.

## Decision R3S-4. Values in the store side's display-only refusals are quoted with Go's %q (refactor R3)

Decision: the managed reservation's refusals (`managed-start`'s reservation, `managed-release`:
`relationship_conflict`, `duplicate_assignment`, `malformed_receipt`, `unregistered_relationship`),
the managed request's key check (`missing [...], unknown [...]`), the launch policy's `conflict`
detail and the registration hold's unreadable store path quote the values they name with Go's
`%q` (`"req-1"`, `["a" "b"]`) instead of Python's `repr()` (`'req-1'`, `['a', 'b']`). A
managed request holding a lone surrogate escape is still refused before any field is judged, now
as `managed request holds a lone surrogate escape`: the reconstruction of `json.dumps(raw,
ensure_ascii=False)` that named the position `str.encode` would name (`dumpsUnescaped`,
`dumpsString`, 75 lines) is gone. The request's fingerprint, taken over the values an accepted
request holds, is unchanged. Reasons, exit codes and fields are unchanged.

Consumer check: these refusals are returned to the command that asked and printed; none is
written to SQLite, a record or a journal (`Reservation.Reserve` and `Release` return their
refusal to `managed-start` and `managed-release`, which print it; the request check is a usage
error; the launch policy resolution is not persisted; the registration hold's refusal is
delivery's printed `unregistered_relationship`), and no skill, doc or fixture matches their
prose.

Kept: the `repr()` quoting decision R3S-3 lists, where the text is stored or journalled.

Evidence: internal/relay/managed/{reservation.go,request.go (`loneSurrogate`)}; internal/relay/
service/policy.go (`ResolveLaunchPolicyAt`); internal/relay/store/registration_hold.go;
`TestAnUnknownRequestFieldIsNamed` (internal/relay/managed/request_refusal_test.go, which was
`TestAnUnknownRequestFieldIsNamedAsPythonReprsIt`); the goldens of
internal/relay/{managed,service,sync}.

## R3R-1. The CI checks read JSON, paths, URLs and flags the Go way; their Python twins are deleted (refactor R3)

Decision: `crw-dev ci plugin`, `validate`, `operations` and `contracts` no longer reproduce
CPython. What goes: the json.loads/json.dumps emulation of `internal/dev/ci` (`pyjson.go`,
`pyjsondecode.go`, `pyvalue.go`: NaN and Infinity literals, Python dict order, int/float/bool
equality, repr of values, CPython's JSONDecodeError and UnicodeDecodeError texts), the
`urllib.parse.urlsplit` emulation (`urlsplit.go`: NFKC netloc and bracketed-host ValueErrors), the
`os.path.realpath`, `str.isspace`, `str.splitlines` and Unicode `\d`/`\b` emulation, the
CalledProcessError and `[Errno N]` texts, and the argparse-shaped option parser (unique prefixes).
The checks decode with encoding/json (numbers as json.Number, so an integer timeout and a
fraction stay apart; a nesting cap of encoding/json's own), resolve paths with
`filepath.EvalSymlinks`, judge an https URL with `net/url`, split lines on `\n` (and `\r\n`), use
ASCII `\d`, `\s` and `\b`, and parse flags with the standard `flag` package (`-h` lists every
flag; a usage error exits 2). Messages name values as `%q` or compact JSON instead of repr. The
Python twins `scripts/ci/plugin.py`, `scripts/ci/validate.py` and
`scripts/check_operations_contract.py` are deleted with the parity tests that ran them; the tests
now assert the Go check's own fragments and exits, and goldens hold the payload digests of fixed
payloads and the `--payload --json` report. `scripts/dev/ALLOWED_PYTHON.txt` keeps the three port
checkers.

Consumer check: `git grep` of `plugins/crw/skills`, `docs/`, `contract/`, `.github/` and the
Makefile for `crw-dev ci`, `plugin.py`, `validate.py` and `check_operations_contract`: CI's
validate job runs `crw-dev ci validate|plugin|contracts` and reads only the exit status; nothing
ran the twins outside `internal/dev/ci`'s tests; docs/plugin-packaging.md reads `stopHooks` and
`hooksDigest` out of `crw-dev ci plugin --payload <dir> --json` with
`sed -nE 's/^  "<field>": (.*[^,]),?$/\1/p'`, one top-level member per line two spaces in.

What stays and why: the payload digest's framing (`<len>:<name> <mode> <sha256>` lines in name
order, sha256) and the version elision, because the manifest records the digest's first twelve
digits as its version suffix (`plugins/crw/.codex-plugin/plugin.json`); the recorded version of
this checkout still verifies. The `--json` report keeps its field names, its sorted keys and its
two-space indentation, which the doc's `field()` reads; it is byte-identical to the Python
twin's for this checkout. `crw-dev ci validate`'s Python syntax check stays while the port
checkers (`scripts/port/*.py`) remain, so CI's validate job keeps its Python setup.
`scripts/port/check_test_map.py`, `check_inventory.py` and `check_cutover_doc.py` stay: each port
step's verification runs the first two with `--final`, and they keep docs/port honest until todo
48.

Evidence: internal/dev/ci/{common.go,json.go,plugin.go,validate.go,operations.go};
internal/dev/ci/plugin_test.go (`Test47_PLG_11_DigestIsFramedAndCoversMode`,
`Test47_PLG_18_JSONReport`), validate_test.go, ci_test.go (`TestACheckListsItsFlags`);
docs/port/inventory.md (Files deleted with evidence); scripts/dev/ALLOWED_PYTHON.txt.

## R3R-2. The development judges read records to a fixed depth and times as RFC 3339 (refactor R3)

Decision: `crw-dev stop-events` and `crw-dev trial-ledger` stop modelling CPython where the
model was only Python's own wording. What goes: `internal/dev/pyload`'s model of the
interpreter's stack edge (a `RecursionError` with CPython's message, raised past 57,900
containers and turned into a reader fault or "this run raised before it could report"), its
CPython JSONDecodeError and UnicodeDecodeError texts and its 4300-digit integer refusal; and
`internal/dev/trialledger/isoformat.go`, a port of CPython 3.14's `datetime.fromisoformat`
(separator rules, week dates, hour 24, `timedelta` repr in its refusals). A record nested past
`pyload.MaxNesting` is a record the judge could not read, like any other undecodable one, and a
decode refusal is in encoding/json's words; the trial ledger reads a time with `time.Parse` as
RFC 3339 and refuses one that names no offset as before. The trial ledger's read failures name
the system's error (`lstat <path>: permission denied`) instead of `PermissionError: [Errno 13]`.

Consumer check: `git grep` of `plugins/crw/skills`, `docs/`, `contract/` and the product for
`stop-events`, `trial-ledger`, `readerFault` and the judges' refusal texts: no skill, hook, doc
command or product code runs the judges or reads their output; they are developer tools whose
JSON a person reads.

What stays and why: the depth itself (57,900, past encoding/json's 10,000) and the reading of
NaN, the infinities, lone surrogate escapes and repeated keys, because the judges read stored
records (journal rows, host ledgers, trial ledgers) that the Python writers wrote and a reader
of stored data keeps accepting what an earlier writer wrote. The verdicts, exits and field names
of both judges are unchanged. Their remaining Python emulation is decided in R3R-7.

Evidence: internal/dev/pyload/pyload.go (`MaxNesting`, `Loads`) and pyload_test.go;
internal/dev/trialledger/moment.go (`parseMoment`), fs.go (`failure`), ledger_test.go
(`TestParseMomentReadsRFC3339`, `TestLedgerReadsRecordsAsDeepAsAWriterWrote`);
internal/dev/stopevents/stopevents_test.go (`TestSEV12_OnDiskFormIsTheWriters`);
docs/port/known-defects.md.

## R3R-3. `crw skill` parses flags with Go's flag package and words its refusals the Go way (refactor R3)

Decision: the `crw skill` commands (`hook-probe`, `parent-title`, `start-policy`) stop
emulating the Python scripts they were ported from where only Python's own bytes were
reproduced. What goes: the argparse emulation (`internal/skill/argparse.go` and the argparse help
texts in `help_text.go`, with the stale program names `hook_probe.py`, `parent_title.py` and
`start_policy.py`, unique-prefix abbreviation and repr() of a refused argument); `pySorted`
(`pysort.go`, CPython's TimSort replayed comparison by comparison so the first unorderable pair
raised the same `TypeError` and a NaN landed where CPython left it); the Python exception
vocabulary (`AttributeError: 'list' object has no attribute 'get'`, `TypeError: ... is not
iterable`, `unhashable type`, `UnicodeDecodeError`, `json.decoder.JSONDecodeError`); CPython's
`[Errno N]` texts with repr() of a pathlib-spelled path; repr() of values in replay and host
messages; and the U+2028/U+2029 unescaping of `parent-title`'s JSON. Each command now reads its
flags with the flag package (`crw skill <family> <command> -h` lists every flag; a usage error
exits 2); values sort by Go comparison of numbers, strings and arrays and a pair that cannot be
ordered is refused; a refusal names the JSON kind it found (`expected a JSON object, found an
array`), the system's error (`open <path>: no such file or directory`) or encoding/json's; and a
message names a value as JSON. The fixtures, observations and requests decode with
`pyjson.Loads` under explicit options rather than `hook.Decode`, so the relay hook's input
decoding can change without moving the skills.

Consumer check: `git grep` of `plugins/crw/skills`, `docs/` and `contract/` for `crw skill`,
`hook-probe`, `parent-title` and `start-policy`: the skills run `hook-probe observe --sanitize`,
`hook-probe replay`, `parent-title decide`/`replay` and `start-policy vocabulary`/`check`/
`selftest` with fixed argv, read `parent-title decide`'s JSON fields and the exit codes, and copy
`start-policy vocabulary`'s lines; `crw-dev ci contracts` runs the replays and the self-test in
process and reads their exit and summary lines. None reads a refusal's prose.

What stays and why: every exit code (0, 1, 2 and 3 for an unreadable file), every JSON field and
summary line, the decision table and its return sites, the record values (NaN, the infinities
and a lone surrogate escape still decode, because a committed fixture holds `"\ud800"`), and
Python's truthiness and equality where the decision reads a field (`pyvalue.Truthy`,
`pyvalue.ItemEqual`), because those decide verdicts, not wording. Three inputs answer otherwise:
a trailing `--` after a command that takes no argument now ends its flags (exit 0) where argparse
refused it (exit 2); and `hook-probe observe` reads the schemas a binary embeds as JSON, so one
holding NaN or Infinity is not a schema (exit 3, "No embedded hook schemas found", where Python
read it) and one holding an integer longer than 4300 digits is (Python refused it, exit 3).

Evidence: internal/skill/command.go (`family`, `commandLine`), values.go (`decodeJSON`,
`sortValues`, `notObject`); internal/skill/command_test.go (`TestSkillCommandLine`,
`TestSkillDoubleDashPassesOptionLikePositionals`); the goldens of `TestSkillJSONShape` and the
other renamed command tests; docs/port/known-defects.md.

## R3R-4. The runtime's messages name a failure in Go's words and a value as JSON (refactor R3)

Decision: `crw install`, `crw doctor` and the runtime packages under `internal/runtime` stop
spelling a failure or a value as CPython did. What goes: CPython's OSError text
(`store.PythonOSError`: `PermissionError: [Errno 13] Permission denied: '/x'`) and SQLite error
text (`store.PythonSQLiteError`) in every detail, refusal and unreadable entry; Python type names
(`found list`, `found NoneType`, `TOML ... it is str`) and repr() of values (`'other'`, `None`,
`True`) in messages; `scope.PyStr` and `staging.repr`, Python's str() and repr() of decoded
values; subprocess's `TimeoutExpired: Command '[...]' timed out after N seconds`; and the
pathlib spellings the runtime compared paths by (`store.PathlibSpelling`). A failure is named by
the Go error's own text (`lstat /x: permission denied`), a value by its JSON text
(`reading.Show`), a kind by `reading.JSONKind` (`an array`, `null`) or `doctor.TOMLKind`, and a
path is compared in a lexical form that drops repeated separators, "." and a trailing separator
and keeps ".." (`reading.Spelling`). The record reader (`reading.Decode`) decodes with
`pyjson.Loads` alone, its refusal in encoding/json's words, rather than first scanning with
CPython's JSONDecodeError texts. `crw install` keeps refusing a HOME that holds ".." or starts
with exactly two slashes, now said without pathlib.

Consumer check: `git grep` of `plugins/crw/skills`, `docs/`, `contract/` and the product for the
replaced words (`Errno`, `PermissionError`, `FileNotFoundError`, `TimeoutExpired`, `found str`,
`NoneType`): no skill, doc command or product code reads a detail's prose; docs/runtime-install.md
documents the four reading states, which are unchanged. `crw doctor declared-schema --json`, which
another runtime's swap gate reads, keeps its fields; its `detail` is prose.

What stays and why: every field, state, verdict and exit code; a reading refusal's `exception`
field keeps its vocabulary (`FileNotFoundError`, `PermissionError`, `NotADirectoryError`,
`OSError`, `UnicodeDecodeError`, `JSONDecodeError`, `TypeError`, `ValueError`), because a reader of
the doctor's JSON may branch on it; the host record's bytes (`record.Encode`, json.dumps with
indent 2 and sorted keys), a measured point's `appServer` (json.dumps of the structured content,
compared across runs), the bridge record's execution policy path (absolute without folding "..",
surrogate-escaped) and the Stop settings, because they are stored and compared; and Python's
truthiness and equality where a decision reads a field.

Evidence: internal/runtime/reading/{reading.go,value.go} (`classOf`, `Decode`, `JSONKind`,
`Show`, `Text`, `Spelling`); internal/runtime/doctor/registration.go (`TOMLKind`);
internal/runtime/install/cli.go (`fixedHome`); internal/runtime/scope/scope_test.go
(`TestARelayTheDeadlineEndedIsUnreadable`); the goldens of `TestShapeRefusals`,
`TestServiceStateReadings` and `TestCellsAndVerdicts`.

## R3R-5. The bridge's MCP server is the SDK's outside the frozen tool listing; its messages name values the Go way (refactor R3)

Decision: `crw bridge` (`codex-thread-bridge`) stops reproducing FastMCP and pydantic on the
wire and CPython in its messages. What goes: `pythonWire` (FastMCP's initialize capabilities
bytes and member order, its empty prompts/resources/templates lists, its answers to
`logging/setLevel`, `completion/complete`, `prompts/get` and `resources/read`), `pythonTransport`
(the MCP 1.30.0 low-level server's validation of every message: "Invalid request parameters" for a
method outside ClientRequest, the `notifications/message` log for non-object params, dropped
unknown notifications, the bare "Method not found"), pydantic's lax argument reading
(`preParseArguments`: a JSON-shaped string decoded for a non-str field; `laxNumber`: a bool as 0
or 1, underscores in numbers) and its refusal text (`N validation errors for <tool>Arguments`,
`Input should be a valid ...`), CPython's OSError text in receipts, tool errors and policy refusals
(`bridge/pyerr`), and repr() of values in findings, refusals and the policy's messages. The SDK
answers every method outside tools/list and tools/call, a call's arguments are judged against the
frozen input schema by the bridge's own checker, which names every problem in field order (`invalid
arguments for <tool>: model is required; limit must be an integer`) so a refusal reads the same
every time, and a value is named as Go quotes a string (`"x y"`, a lone surrogate's bytes
visible) or as JSON. A receipt's error is the innermost error's type name and the Go error's text
(`OpError: dial unix ...: connect: no such file or directory`).

Consumer check: `git grep` of `plugins/crw/skills`, `docs/`, `contract/` and the relay for the
MCP surface: the skills read tool results and their structured fields, the `isError` flag and the
refusal codes (`execution_not_allowed` and the rest); contract/fixtures/mcp-tools checks errors,
codes and fields; `internal/contracttest` holds tools/list to contract/schema/bridge-mcp-tools.json
member for member (names, order, annotations, schemas, no extra member) and expects an unknown tool
to be an error result. The relay reads a receipt's fields, never its error's prose. crw-run's
bridge.md says a missing `turn_id` is a validation error, which it still is.

What stays and why: the tool names, input and output schemas, annotations and instructions; the
tools/list reply as the frozen contract has it (registration order, no idempotentHint, "tools" its
only member: `frozenToolsList`); an unknown tool answered as an error result (`unknownTool`), which
the contract corpus expects; every structured field and refusal code; a number field reading a
string that holds the number ("5"), which a model writing a call sends and the bridge always
accepted; the indented JSON text content beside the structured content; the execution policy's
acceptance (its codec handling, duplicate-key refusal, depth and constants), because a host's
policy file must keep loading; the ledger's request fingerprint (`ledger/canonical.go`) and the
bridge record format; `bridge/settings.Printable`, frozen since decision 49, which `settings.Repr`
and through it `pyvalue.StrRepr` use across the relay; and `bridge/pyerr`, which
`internal/relay/registry` still calls.

Evidence: internal/bridge/mcp/middleware.go (`frozenToolsList`, `unknownTool`,
`checkedArguments`, `numberText`); internal/bridge/mcp/{wire_test.go,validate_test.go}
(`Test_tools_list_is_the_frozen_listing`, `Test_a_refused_argument_is_an_error_result_the_same_way_every_time`,
`Test_a_number_written_as_a_string_is_read_as_the_number`); internal/bridge/{bridge.go,show.go},
execution/{decode.go,load.go}; internal/contracttest (`Test_a_live_tools_list_from_the_built_binary_equals_the_frozen_contract`,
`Test_every_mcp_reply_equals_the_python_servers_whole_json`).

## R3R-6. The bridge launcher names what it refuses the Go way (refactor R3)

Decision: `codex-thread-bridge --plugin-launch` (`internal/pluginwiring`, the record contract of
decision 26) keeps every check crw_bridge_mcp.py made, in its order, and stops wording them as
CPython did. What goes: repr() of the values a refusal names (`'user'`, `None`, `'1'`); CPython's
OSError text (`[Errno 2] No such file or directory: '<path>'`) and its UnicodeDecodeError and
JSONDecodeError texts for an unreadable record; `sys.stderr`'s backslash escaping of a lone
surrogate; and `pathlib.PurePosixPath`'s "//" root in the Codex home's spelling. A string is
named as Go quotes it, any other value as JSON, a failure in the Go error's words (`open <path>:
no such file or directory`), the record is decoded through `reading.Decode` (the runtime's reader
of stored records) instead of the relay hook's decoder, and the home is spelled by
`reading.Spelling`. The five launcher cases that only pinned repr()'s escapes (an owner holding a
no-break space, a zero-width space, private-use characters or a lone surrogate, a server name
holding a Mongolian vowel separator) are deleted.

Consumer check: `git grep` of `plugins/crw/skills`, `docs/` and `contract/` for the launcher's
refusals: the wiring (`wiring/crw-bridge.sh`) reads only its exit; docs/plugin-packaging.md and
docs/runtime-install.md name the refusals' meaning, not their text; the doctor and the installer
read a record through `ReadBridgeRecord` and word their own findings.

What stays and why: every check and its order, exit 2 for a refusal, the record versions 1 and 2
and their fields, a stored recordVersion read as the Python writer compared it (True is 1, 1.0
is 1), a surrogate-escaped path or argument turned back into its byte (the Python installer
recorded a byte that is not UTF-8 that way), the policy digest, and the stripping of the policy
variable, which the bridge applies the same way.

Evidence: internal/pluginwiring/{launch.go,record.go}; internal/pluginwiring/launch_test.go
(`TestBridgeLaunch_answers_each_record_as_the_golden`,
`TestBridgeLaunch_refuses_a_record_no_exec_could_start`).

## R3R-7. The development judges fault, expand and name failures the Go way (refactor R3)

Decision: `crw-dev stop-events`, `crw-dev trial-ledger` and `crw-dev skills link` stop modelling
CPython's exceptions and pathlib. What goes from stop-events: the dict-key `TypeError` a value
that is an array or object raised (a legacy row naming its session by an array, an outcome or
acceptance that is not text) and the reader fault it made; such a row is now counted or judged
like any other (a legacy row is still UNREADABLE, an unknown acceptance a row the writer never
writes). The `RuntimeError: Could not determine home directory.` and getcwd's
`FileNotFoundError` reader faults are the Go error's text (`cannot expand "~": ...`, `cannot make
"rel" absolute: getwd: no such file or directory`). A root or host ledger that cannot be
examined is detailed by the Go error (`stat <path>: permission denied`) instead of CPython's
OSError and repr(), and a NUL in a path is the system's refusal instead of Python's `embedded
null byte`. `pathlib.Path()`'s spelling before `expanduser` goes: a ~ is expanded only where it
leads the path given, so "./~" and ".//~/x" name a directory called ~, and a path given on the
command line is cleaned with `filepath.Clean` (a leading "//" is "/"). The `$`-before-a-newline
match of Python's regular expressions goes: a file name ending in a newline is a foreign entry.
From trial-ledger: the Python exception class of "this run raised before it could report"
(`exception` is now "panic", `detail` the panic's value), which only a Go panic reaches. From
skills link: `store.PathlibSpelling`; the destination drops repeated separators and "."
components and keeps "..", without POSIX's "//" root.

Consumer check: as R3R-2, `git grep` of `plugins/crw/skills`, `docs/`, `contract/` and the
product for `stop-events`, `trial-ledger`, `readerFault`, `exception` and the faults' texts: the
judges' JSON is read by a person (docs/runtime-install.md keeps it as a receipt, and names only
the verdict and the exits); `crw-dev skills link`'s lines are read by a person.

What stays and why: every verdict, exit and field, and the reading order that lets a reading
that cannot finish report what it reached. The surrogate-escape spelling of a path (`shown`,
`pyvalue.FSEncode`/`FSDecode`), because the adapter's records spell a byte that is not UTF-8 that
way and a ledger a claim names is matched by that spelling; `normpath`'s "//" for a recorded path,
which is checked against the form its writers gave it; `record.ExpandUser`'s HOME rules, the
runtime's. Trial-ledger's grade document keeps `str.splitlines`, the non-strict
`os.path.realpath` containment and the `Path()` spelling of the trial root and the ledger,
because docs/live-trial.md promises a grade that compares byte for byte with a Python-era grade
of the same trial, and they decide which lines and paths that document names. Both judges keep
reading their command lines with internal/relay/argparse, which R3C owns.

Evidence: internal/dev/stopevents/{judge.go,shape.go,stopevents.go} and stopevents_test.go
(`TestSEV08_LedgerIntegrity`: "a root that cannot be examined is named in the error's own
words", "a root is read where its expanded absolute path is", "a ~ is expanded only where it
leads the path"; `TestSEV09_RowIntegrity`: "a legacy row naming its session by an array is
counted, never TRUE"); internal/dev/trialledger/ledger.go; internal/dev/skills/link.go.

## R3R-8. The last Python words in the runtime's and the bridge's messages (refactor R3)

Decision: the messages R3R-4 and R3R-5 left in CPython's words take Go's. What goes: the App
Server client's `JSONDecodeError: ` in the transport failure a frame that is not JSON causes
(now `App Server transport failed: a frame is not JSON: <encoding/json's error>`); `ValueError:
embedded null byte` in the pointer's and the residue scan's details (now "it holds a NUL byte");
Python's list repr in the bridge's approval-policy refusal and the execution policy's
supported-roles refusal (now `"never", "on-request", "untrusted"` and `"child", "parent",
"supervisor"`). Seven tests whose names said their answers were Python's, where those answers
are now the goldens' (`Test_round3_argument_refusals_read_as_their_goldens`,
`Test_round3_destination_refusals_read_as_their_goldens`, the busy-thread, capabilities and
worktree-receipt round-3 tests, `TestReceive_non_json_frame_fails_pending_request_as_a_transport_error`,
`TestDecodeReadsUniversalNewlines`), are renamed; their goldens are unchanged but for the name.

Consumer check: `git grep` of `plugins/crw/skills`, `docs/`, `contract/` and the product for
`transport failed`, `embedded null byte`, `approval_policy must be one of` and `supported are`:
no skill, doc command or product code matches the texts; the relay's own `embedded null byte`
and `JSONDecodeError` details are the relay areas'.

What stays and why: the transport failure's prefix and its error type, every refusal code
(`execution_policy_unreadable`, the settings codes), the readings' `exception` vocabulary
(R3R-4). Test names that say "Python" where they name where a case or a fixture came from (the
re-expressed `test_settings.py` and `test_execution.py` cases, the Python-generated ledger
fingerprint and receipt) or the stored bytes Python wrote (host record, Stop settings, bridge
record, staging claim) keep it, because that is still what they hold.

Evidence: internal/bridge/appserver/receive.go and receive_frame_test.go;
internal/runtime/pointer/pointer.go; internal/runtime/residue/residue.go;
internal/bridge/settings/settings.go, internal/bridge/mutations.go;
internal/bridge/execution/roles.go (`TestAPolicyRefusalQuotesANameAsJSON`).

## Decision R3D-1. A refusal the relay only returns names a value as Go quotes it; a stored or hashed one keeps repr() (refactor R3)

Decision: in the domain packages (`internal/relay/{capacity, delivery, evidence, faults,
mergeturn, reception, registry, routing, supervisor, sync}`) a refusal, usage or host error that
the relay only returns to its caller names a string with Go's quoting (`strconv.Quote`, `%q`:
`"x"`, an invisible character escaped as `\u00a0`) and any other value with
`pyvalue.Quote` (its compact JSON), where it used Python's `repr()` (`'x'`, `True`, `None`,
`[1, 2]`). A float in such a message is Go's spelling (`-1`, `NaN`, `+Inf`), not `float.__repr__`.
Message prose only: every `error`, `reason`, `code`, field and exit stays.

Consumers checked: `plugins/crw/skills`, `docs/` and `contract/` quote no relay detail with
Python's quotes (the skills read `reason`, `error` and codes); the product's own parses of detail
text (`adapter/cli.go` reads a `KeyError: ` prefix, `mergeturn/target.go` an `HTTP 404`,
`delivery/reconcile.go` `not scanned`, `delivery/hostcheck.go` `unreadable`) read messages this
entry does not change. The contract fixture
`test_management_cli__test_an_argument_echoed_back_is_the_repr_of_its_str__relationship` pinned
`relationship-status`'s `no relationship 'x\xa0y'`; it now pins Go's quoting
(`no relationship "x\u00a0y"`), so an invisible character in an echoed argument is still shown
escaped.

What stays, and why: a message that is also written to disk or SQLite, or that feeds a hash,
keeps its bytes, so a stored row reads the same whichever runtime wrote it. Which messages
those are was settled two ways and their union kept: by running every relay, contract and skill
test with `repr()` marked by its call site and the store's bound arguments, the hook's records,
the marker files, the reception ledger, the frozen manifests and every SHA-256 input checked for
the mark; and by the call graph (VTA) from the functions whose callers store an error's text
(the hook and `control.sock`, which journal an evaluation's error; `delivery.Enqueue`, whose
refusal the daemon stores in `delivery_intent.last_error`; `delivery.AuthorizedSettings`, whose
refusal the send journals; `Reconciler.ReconcileAttempt`; the supervisor's `attempt`). Kept
therefore: the contests recorded in `coordination_conflicts` and `linkage_conflicts` (capacity's
and edit regions' `refusal`, merge-turn's `coordination` and `CoordinationRefusal`, registry's
`linkRefusal`), the sync job problems stored as `sync_outbox.last_error`, the settings and role
refusals a withheld send journals, the merge-turn grant's wake refusal (stored in the grant's
evidence), a routing observation's detail (part of the fault's evidence digest) and the
supervisor's held and withheld records. In delivery that keeps the recipient resolution's and
`Enqueue`'s refusals (stored in `delivery_intent.last_error`), the settings and role refusals, the
rendered messages and their helpers, the lifecycle's integer reading and the marker path checks
(their host error text reaches the supervisor's `supervisor_attempt_faulted` journal as the
attempt's fault label), the omission readings and the `KeyError: ` texts `adapter/cli.go` parses; the
acknowledgement, verdict, criteria and intent-registration refusals and the claim's request-id
clash quote as Go does.

Evidence: `internal/pyvalue/quote.go`; the golden diffs of the R3D commits (message prose only).

## Decision R3D-2. merge-turn, routing and fault answers print a map's keys in sorted order; Python's key order is not rebuilt (refactor R3)

Decision: `mergeturn.PythonOrder` and its table of eleven answer shapes, which re-sorted a map's
keys into the order `mergeturn.py` inserted them, are deleted. A `merge-turn-*` answer built from
a map prints its keys sorted (`mergeturn.plain`); every key, value and type is unchanged. The
same holds for the routing commands (`product-*`, `route-*`, `completion-check`):
`routing.CommandRecord`, which carried each command's and each nested record's key order from
`products.py`/`routing.py` (about 150 lines of order tables), now prints every map with its keys
sorted. So do the fault commands (`fault-*`): `faultAnswer` chose among five key-order
reconstructions (`f1Ordered`, `f2Ordered`, `cOrdered`, `dOrdered` and `ordered`, about 250 lines
of order tables keyed by command family and by which keys a map happened to hold); it now renders
every answer and refusal with one sorted `answerObject`.

Consumers checked: the skills and `crw` read merge-turn and routing answers as JSON fields
(`plugins/crw/skills/crw-run/references/merge-readiness.md`, docs/relay/product-routing.md); no
consumer reads them as bytes or by position. The answers are never stored or hashed: the stored
grant evidence, the turn's ledger rows and the routing records (`product_registry`,
`incident_routes`) are written by their own encoders, which this entry leaves alone.

Evidence: internal/relay/mergeturn/order.go, internal/relay/routing/command_records.go,
internal/relay/faults/cli.go (`answerObject`); the contracttest goldens `TestMergeTurnCommands_*`
and the routing and fault goldens (key order only). The fault ledger's stored journal JSON
(`noticeJournal`'s fixed key order) is unchanged.

## Decision R3D-3. `--kind-module`'s module model is the relay CLI's; the domain packages keep none (refactor R3)

Decision: the fault package no longer models Python's import for `--kind-module`
(`faults.RegisteredModule`, the prefix walk for "No module named", the `ValueError` and
`TypeError` texts): with the dispatch table (decision R2B-2) the check moved to
`internal/relay/dispatch` (`importKindModules`, `OnKindModule`), which the relay CLI owns, and the
fault package only installs its product declarations there
(`dispatch.OnKindModule("codex_session_relay.projects", InstallProductDeclarations)`). This
wave's first commit had simplified the fault package's copy; the merge of the dispatch table
replaced that copy, so the simplification is left to the relay CLI's own wave (R3C): accept the
three documented names (`codex_session_relay.projects`, which docs/relay/product-routing.md
names, and `json` and `os.path`, which docs/port/known-defects.md names), refuse anything else
with the same exits (3 for an empty or relative name, 4 for any other) in Go's words, naming the
value given, without the prefix walk.

Consumers checked: docs/relay/product-routing.md, docs/relay/faults.md and the holder protocol
name only `codex_session_relay.projects`; no skill passes `--kind-module`.

Evidence: internal/relay/faults/cli.go (`dispatch.OnKindModule`); internal/relay/dispatch/dispatch.go.

## Decision R3D-4. packet-check's text checks say what they found, not CPython's exception (refactor R3)

Decision: `packet-check` and the reception reads it makes word their refusals without CPython's
exception text: a packet holding a lone surrogate escape is refused (`malformed_receipt`, as
before) naming the escape and its byte offset, where it quoted `'utf-8' codec can't encode
character ... in position N` at a position in `json.dumps`' re-spelled text (the re-spelling is
deleted); a document nested past the bound is refused naming the bound (the bound, 9,998 levels,
and 9,997 for recorded settings, stays) instead of `RecursionError: maximum recursion depth
exceeded while decoding a JSON array from a unicode string`; a file that cannot be read names Go's
error; the `FileNotFoundError:`, `RecursionError:`, `JSONDecodeError:`, `TypeError:`,
`OperationalError:` and `ValueError:` prefixes are gone from the reception notes and the sync
host errors. The packet, record and observation files packet-check reads are decoded by
encoding/json's reading rather than re-checked as `json.loads` would (`registry.DecodeJSON`), so a
file that is not JSON is refused (exit 4, as before) in encoding/json's words; a document both
readings accept decodes to the same values, so its content digest is unchanged. The reception
ledger, a file the relay itself writes and reads back, keeps its lenient reader.

Consumers checked: plugins/crw/skills/crw-run/references/{relay.md,task-packet.md} read
packet-check's `verdict`, `disposition` and `act`, not the detail; nothing parses a reception note.
The reception ledger's file format (written by `SaveLedger`) is unchanged.

Evidence: internal/relay/reception/{unicode.go,depth.go,depth_test.go,store.go};
internal/relay/sync/cli.go, packet_read_test.go; the sync goldens (message prose only).

## Decision R3D-5. The registry's host errors are Go errors; `--settings` is read as encoding/json reads it (refactor R3)

Decision: `registry.HostError`, which carried a Python exception class name so the host envelope
read `<Class>: <message>`, is deleted. The registry's host errors are plain Go errors and still
answer `{"error": "host", "detail": ...}` with exit 3: a settings file that cannot be read names
Go's error (`open <path>: no such file or directory`), settings that are not UTF-8 say so, and
`register --parent-settings`/`--child-settings` and `settings-record --settings` are decoded by
encoding/json's reading (`pyjson.Loads` without `Python`, which only re-checked the document as
`json.loads` would to word its refusal), so a refusal reads `the settings are not JSON:
<encoding/json's error>` instead of CPython's `JSONDecodeError` text. Both readings decode a
document they accept to the same values (constants were already refused), so
`authorized_settings` and the settings' canonical bytes are unchanged. The test of `store.PythonJSONError`'s wording that lived in the registry's tests is deleted
(it tested another package's emulation); the host-error test checks the envelope.

Consumers checked: the skills pass settings documents written by `json.dumps` (no constants);
docs/port/known-defects.md's settings entry is updated.

Evidence: internal/relay/registry/cli.go (settingsJSON); Test25_CLI_host_errors_exit_three_with_their_detail.

## Decision R3D-6. Fault and routing commands word unreadable input as Go reads it (refactor R3)

Decision: `fault-observe --observation`, `fault-complete --observed` and `fault-sweep --readings`
refuse text that is not JSON with `fault_observation_malformed: the ... is not readable JSON:
<encoding/json's error>`, and the routing commands' `--incident`, `--classification` and
`--registry` documents with `route_input_malformed: ... is not readable JSON: <encoding/json's
error>`, where both quoted CPython's `JSONDecodeError` text (`store.PythonJSONError`) for a
document their encoding/json reading had already refused; an `@file` that cannot be read names
Go's error instead of `[Errno N] ...`. The reasons and exits are unchanged. The fault ledger's
`f1Repr` (repr of a str, `None`, fmt for the rest) is deleted: its refusals name a value with
`pyvalue.Quote` (R3D-1), and a choice list in a routing refusal is Go's `%q` of the list instead of
a Python tuple's repr.

Consumers checked: the holder protocol (docs/relay/product-routing.md, docs/relay/faults.md) reads
the refusal's `reason`; no skill parses these details.

What stays: the fault sweep's reading of the host record (`the host record at ... could not be read:
OSError`) and the managed readings' `TypeError:`/`ValueError:` reasons, because a sweep's reading
can become a recorded observation; the readback problems a publication's block carries are
returned only and now quote as Go does.

Evidence: internal/relay/faults/{cli.go,commands_f1.go}, internal/relay/routing/{cli.go,products.go};
the faults and routing goldens (message prose only).

## Decision R3C-1. The relay CLI reads its command lines with its own parser, not an argparse emulation (refactor R3)

(Placeholder heading: the next free number is given when the R3 groups merge.)

Decision: `internal/relay/argparse` stops reproducing CPython 3.13's argparse and reads relay
command lines with a parser of its own, driven by `specs.json`, which now declares each parser's
options only: flag, whether it takes a value (`append` keeps every one, `true`/`false` take
none), type (`int`, `float`), choices, required, its help line, the groups whose options exclude
one another, and which parsers name a command with their first word (the root's and service's).
The file went from 460 KB to 48 KB: the rendered usage parts, textwrap chunks, section tables,
dests and headers argparse's formatter needed are gone. What goes:

- Abbreviations: an option is its full flag (`--subj` for `--subject` is an unrecognized
  argument, where argparse took a unique prefix and refused an ambiguous one). No skill, doc,
  contract fixture or product subprocess call spells a relay flag short of its declaration;
  only the argparse byte tests did.
- `-hx` as help, and `--help=x`/`-h=x` read as help: help is `-h` or `--help` as a token of its own
  (still wherever it stands among words the parser does not know).
- argparse's prose and layout: usage and help are one line per option, never wrapped and never
  depending on `COLUMNS` or the terminal; messages quote a value Go's way (`%q`), not with
  Python's repr; an unknown command or option is reported by the parser that read it (the root
  for a word before the command, the command's own after it, `crw relay service` for a service
  word), where argparse reported every unrecognized word under the root parser; unrecognized
  words are reported before a missing required option.
- Python's number syntax: an int option is `strconv.ParseInt(text, 10, 64)` (no Unicode digits,
  no underscores, no surrounding whitespace, nothing outside int64, see decision R3C-2) and a
  float option is `strconv.ParseFloat`.

What stays, because consumers read it: every command, flag and choice; exit 0 with the usage
and every option on stdout for `-h`/`--help`; exit 2 with the usage line and the reason on
stderr (nothing on stdout) for a line the parser cannot read, in the vocabulary the contract
fixtures read (`invalid choice`, `the following arguments are required: <flags>`,
`unrecognized arguments: <words>`, `expected one argument`, `not allowed with argument`); a
value that looks like a negative number or holds a space is a value (`--session -1`,
`--evidence "- merged by hand"`); global options before the command. The parsed result keeps its
shape for the handlers (values by flag, an int as `*big.Int`, a float as `float64`), and the
capacity and edit-region commands' help is still their usage line alone
(`dispatch.Command.UsageHelp`). `argparse.ParseInt`/`ParseFloat` (Python's int()/float() of a
text) stay for the packages that read stored or forge text with them; the parser does not use
them. The development tools `crw-dev stop-events` and `crw-dev trial-ledger` read their lines
with the same parser and change the same way.

Consumer check: `git grep` over `plugins/crw/skills`, `plugins/crw/wiring`, `docs/` and
`contract/` for every token that is a strict prefix of a declared flag finds none; the product's
own relay argv (service's daemon launch, `crw doctor`/`install`/`exercise` through
`scope.Relay`, the recovery and readback commands the relay prints) spells every flag in full;
no Go caller and no skill reads a relay usage or help text; the two cli-shape fixtures that read
stderr look for `invalid choice` and `required`, which the parser still says.

Removed with it, as tests that pinned only argparse's bytes: the formatter test over every spec
at five widths, `Test24ArgparsePython`, the built-binary argparse, runtime and root-parser sweeps
with their 2,000-case fixture, the contract corpus's per-command argparse sweep and its sweep of
the eleven routing commands' help and error lines at three widths (447 cases). The
contract they shared is held instead by `TestParseReadsWhatTheSpecDeclares`,
`TestTheRootParserStopsAtTheCommand`, `TestEveryParserListsItsOptions` (internal/relay/argparse)
and `TestRun_every_relay_command_line_has_the_usage_contract` (cmd/crw: every registered
command's help lists its options, an unknown option and a missing required option exit 2 with
usage on stderr and nothing on stdout).

Evidence: `internal/relay/argparse/argparse.go`, `specs.json`, `parse_test.go`;
`internal/relay/dispatch/dispatch.go` (`Execute`, `parsedLine`); `cmd/crw/main_test.go`;
`cmd/crw-dev/main_test.go`.

## Decision R3C-2. An int option is a signed 64-bit integer (refactor R3)

(Placeholder heading: the next free number is given when the R3 groups merge.)

Decision: the parser refuses an int option's value outside int64 as a usage error (exit 2,
`argument --x: invalid int value: "..."`), and `dispatch.Args.Number` answers an int option as an
`int64` whether the line gave it or its default stands. Before, the parser accepted any Python
integer as a `*big.Int`, so a value beyond int64 reached the handler and answered a host error
about SQLite's INTEGER (`OverflowError: Python int too large to convert to SQLite INTEGER`,
exit 3) or, for a command that stored nothing, whatever the handler made of it; it is now refused
before any handler runs. `Args.Number` answered a given int as that `*big.Int` while its default
was an `int64`, which is how the host adapter's `deliver --limit`/`verify-acks --limit` came to
panic; R3S5 fixed the adapter (`hostLimit`, `TestHostCommandsReadAGivenLimit`, whose out-of-range
rows now expect this refusal), and `Number` now hands every caller one type. The parsed value
stays a `*big.Int` in `Result.Numbers` for the handlers that read it there.

Consumer check: no skill, doc, fixture or product call passes an integer beyond int64 to a relay
option; the numeric downstream test holds the extremes the parser accepts and one it refuses.

Evidence: `internal/relay/argparse/argparse.go` (`convert`), `internal/relay/dispatch/args.go`
(`Number`); `TestParseReadsWhatTheSpecDeclares`; `Test24NumericDownstreamBytes`
(internal/relay/cli); `TestHostCommandsReadAGivenLimit` (internal/relay/adapter).

## Decision R3C-3. `--kind-module` is checked by name, worded in Go; the import model goes (refactor R3)

(Placeholder heading: the next free number is given when the R3 groups merge.)

Decision: the relay's global `--kind-module` keeps accepting the three names it accepted,
`codex_session_relay.projects` (the value docs/relay/product-routing.md tells a credential holder
to pass), `json` and `os.path` (docs/port/known-defects.md), and keeps its exits: an empty or
relative name is a host error (exit 3), any other unknown name a usage error (exit 4), before
the command runs and after check_start and the selection refusal, as before. What goes is the
stand-in for Python's `importlib.import_module`: the walk that named the first missing package
(`No module named '<prefix>'`), Python's repr, and the `ValueError: Empty module name` and
`TypeError: the 'package' argument is required ...` texts. The refusals now read
`--kind-module "<name>" is not a module this relay knows; it knows codex_session_relay.projects,
json, os.path` and `--kind-module "<name>" is not an absolute module name`. This follows the
proposal of decision R3D-3, which moved the model out of the fault package.

Consumer check: docs/relay/product-routing.md, docs/relay/faults.md and the invariants name only
`codex_session_relay.projects` or a placeholder; no skill, contract fixture or product call passes
`--kind-module`.

What stays: the module's install hook (`dispatch.OnKindModule`, which the fault package uses to
install the product declarations) and the order of the checks.

Evidence: `internal/relay/dispatch/dispatch.go` (`kindModules`, `importKindModules`);
`TestKindModule_refuses_a_module_the_relay_does_not_declare` (internal/relay/cli),
`Test22_FLT_33_StaticKindModules` (internal/relay/faults).

## Decision R3C-4. The relay CLI's own failures are worded in Go, without Python's exception classes (refactor R3)

(Placeholder heading: the next free number is given when the R3 groups merge.)

Decision: where the relay CLI's dispatch and the cli package's commands put a failure into an
answer's `detail` (or a doctor field that is only shown), it is worded as Go says it, without a
Python exception class in front and without Python's repr:

- A `--state` (or `CODEX_SESSION_RELAY_STATE`) that cannot be resolved answers, for every command
  family alike, `the state directory cannot be resolved: <why>` (exit 3, `error: host`), where
  `<why>` is Go's (`cannot determine home directory for "x": user: unknown user x`). It was
  `RuntimeError: Could not determine home directory.` for a missing home and the family's
  unclassified failure otherwise. The store's `ErrNoHome` keeps its words, which the dispatch
  leaves out of the answer.
- `dispatch.Host(detail)` is the host envelope (exit 3) a command words itself; the service and
  daemon commands' unclassified failures, a launch policy that cannot be applied, a daemon run
  that fails, merge-evidence's forge failures (a missing `gh` is Go's `exec: "gh": executable
  file not found in $PATH`) and a routed `guard-evaluate` whose owner failed mid-answer answer
  through it, where they answered `ValueError: `, `RuntimeError: `, `OSError: `/`<errno class>: [Errno
  N] ...` or `FileNotFoundError: [Errno 2] No such file or directory: 'gh'`.
- The doctor's `actorReachability.socketConnect` is `ok`, `not configured`, or Go's dial error
  (`dial unix <path>: connect: connection refused`) where it was
  `ConnectionRefusedError: [Errno 111] Connection refused`; `crw doctor` reads only whether it is
  `ok`. Its `accessReceipt.detail` quotes the measured and read identities as JSON
  (`"store-1"`, `null`) where it used Python's repr.
- A JSON document a caller hands a command (doctor's `--require-worker-policy`, the supervisor
  commands' `--observation` files, merge-evidence's `--restate` record) is read strictly, as
  encoding/json reads it: UTF-8 only, no `NaN`/`Infinity`, at most 10000 levels of nesting, with
  Go's error text. CPython's `JSONDecodeError`/`UnicodeDecodeError` texts, Path.read_text's
  universal newlines and the 9998-level `RecursionError` host error (exit 3, now the usage error
  exit 4 every other unreadable record gets) go. Stored JSON (the mirror, process records,
  stored settings, staged packets) is still read as leniently as any writer wrote it.
- merge-evidence's `--timeout` is a duration: zero or less times out at once, and a value past
  what a Go duration holds is the longest one, where CPython's poll conversion raised
  `OverflowError` (exit 3) past 24.8 days.
- Names and values inside messages are quoted with `%q` (`no event "x"`,
  `the observation at "/p"`, `event "x" raises no obligation`), and a stored settings value
  that is not an object is named by its JSON type (`an array`).

Every `error`, `reason` and exit code stays, except the two edge inputs named above (a record
nested past the cap, a timeout past 24.8 days). Consumer check: `plugins/crw/skills`, `docs/` and
`contract/` hold none of the changed texts; `crw doctor`, `install` and `exercise` read the relay
doctor's `socketConnect` only as `ok` and its other fields by name, never a `detail`.

What stays: `dispatch.HostError` (`<Class>: <Detail>`), which the delivery, registry, sync and
managed commands still raise (refactor R3D's and the store side's), and the error texts the cli
package passes through from other packages unchanged (the store's and the evidence collector's).

Evidence: `internal/relay/dispatch/{answer.go (Host),dispatch.go (run)}`;
`internal/relay/cli/{daemon.go,doctor.go,guard.go,merge_evidence.go,pyvalue.go (decodeInput),
sandbox.go,show.go,supervisor.go}`; `TestRun_unknown_user_state_is_a_host_error` (cmd/crw); the
goldens of internal/relay/cli, internal/relay/hook and internal/contracttest.

## Decision R3C-5. The doctor no longer echoes the process records' `python_compatibility_build`; `guard-evaluate` stays for now (refactor R3)

(Placeholder heading: the next free number is given when the R3 groups merge.)

Decision: `doctor`'s `ownership` block loses `processes` (`{"supervisor": ..., "worker": ...}`),
the raw echo of `daemon.json`'s and `worker-policy.json`'s `python_compatibility_build`, which
named the Python fence build a process ran. A Go process writes that key as null (decision 31);
with no Python runtime left (decision 48) the echo can only say null or repeat a stale record.
The records keep the key (they are stored, and the store keeps reading what Python wrote), and
the block's six stamp keys, `phase`, `runtime_build` and `detail` are unchanged. Decision 54
deferred this here because about 140 recorded Python answers held the block; they are goldens now.

Consumer check: no skill, doc, contract fixture, `crw doctor`/`install`/`exercise` reading or Go
caller reads `ownership.processes`; `git grep processes` over `plugins/`, `docs/relay/` and
`contract/` finds no reader.

Not removed, although survey B-03 names it: the `guard-evaluate` command. No skill, wiring file
or product path runs it (the Stop hook evaluates in process or through `control.sock`'s
`guard-evaluate` method, which stays), and docs/relay/operations.md only describes its
selection exemption. But it is the binary surface the hook package's guard tests drive
(`Test33Guard*`, `Test33PR181GuardDiscoveryPython`, `Test33ReviewD10`), and its routing is the only
caller of `hook.RouteGuard`, `hook.SelectedStore` and `store.CheckStop`: removing it means moving
those tests onto the hook's own entry and deleting those functions, in the store side's packages.
It is left for that change. The hidden `service run --takeover-candidate` flag that the takeover
candidate used left the parser in refactor R1 (decision 54); nothing of it remains in
`specs.json`.

Evidence: `internal/relay/cli/doctor.go` (`runDoctor`); `TestDoctor_ownership_block_matches_python_on_a_broken_store`
(its "non-string process builds" case goes with the echo); the doctor goldens of
internal/relay/cli, internal/relay/managed and internal/relay/supervisor.

## Decision R3C-6. The delivery commands' entry layer answers in Go's words (refactor R3)

(Placeholder heading: the next free number is given when the R3 groups merge.)

Decision: the delivery package's command entry files (`cli.go`, `cli_emit.go`, `intent_cli.go`),
which decision R3D-3's wave left to the relay CLI's, stop raising `dispatch.HostError` with a
Python class: `ack-proof`'s and a routed `ack`'s malformed event or turn, `emit`'s generation,
sentinel and event-identity refusals, `verdict`'s `--criteria` that cannot be read, is not JSON or
is not a list (`FileNotFoundError: `, `JSONDecodeError: `, `TypeError: '<type>' object is not
iterable`), `intent-declare`'s `--settings` that is not JSON (CPython's `JSONDecodeError` text)
and an intent's store under an unknown `~user` (`RuntimeError: Could not determine home
directory.`) answer through `dispatch.Host` with the same exit 3 and Go's words. The family's
unclassified failure loses its `RuntimeError: ` prefix. The stale `HostUnavailable: the relay host
adapter (bridge_adapter.py) is not ported to Go yet (todo 28)`, which only a build without the
host adapter reaches, says that the build registers no host adapter. `--adjudicate` and
`--restoration` echo their value with `%q` instead of Python's repr.

Kept, because it is stored: the intent records' `store_unreadable`/`store_unopenable` details,
the marker facts and everything the delivery domain writes; and the `KeyError: ` texts the host
adapter's `reconcile` reads back (`adapter/cli.go`), which none of these paths produce.

Consumer check: no skill or doc reads these details; the contract fixture
`test_management_cli__test_an_argument_echoed_back_is_the_repr_of_its_str__adjudicate` expects
the `%q` spelling now (its exits unchanged), and cli's `TestAnEchoedArgumentIsQuotedInTheRefusal`
(was `TestAnEchoedArgumentIsPythonsReprOfIt`) holds both echoes.

Evidence: `internal/relay/delivery/{cli.go,cli_emit.go,intent_cli.go}`;
`internal/relay/dispatch/answer.go` (`Host`, `Detail`); the goldens of internal/relay/delivery,
internal/relay/cli and internal/contracttest.

## Decision R3F-2. What R3 left of the Python value helpers is named for the format it keeps (refactor R3, final sweep)

Decision: the helpers that outlived the Python relay say what they keep.

- `pyvalue.Quote`, the Go-native quoting decision R3D-1 added, is `quote.Value` in
  `internal/quote`: it is how a relay message shows a value (a string as `%q`, any other value as
  compact JSON), not Python's. Its bytes are unchanged.
- `argparse.ParseInt`/`ParseFloat` (Python's `int()`/`float()` of a text) are
  `pyvalue.ParseInt`/`ParseFloat`: the relay's parser stopped using them in R3C, and their readers
  (delivery's `HostTime`, the evidence forge's numbers, the fault commands' text arguments) read a
  number as Python did. `Test24NumericPythonBytes` and its golden moved with them, unchanged.
- The store's helpers that word a failure as the stored rows word it are named for that role:
  `PythonOSError`, `PythonOSErrorText` and `PythonSQLiteError` are `StoredOSError`,
  `StoredOSErrorText` and `StoredSQLiteError`; `ManifestException.PythonText` is `StoredText`;
  the frozen copy's value model (`PythonEntry`, `PythonEntries`, `PythonManifestEntries`,
  `PythonRevisionHash`) is `FrozenEntry`, `FrozenEntries`, `FrozenManifestEntries` and
  `FrozenRevisionHash`. `PythonHostDetail`, which words a failure in Go's since R3S-1, is
  `HostDetail`. `PythonSQLiteMessage` is unexported. The pass-through wrappers
  `store.PythonJSONError`, `PythonJSONErrorWithLimit`, `DecodeUTF8` and `ValidUTF8` are gone:
  their callers call `pyjson.Error`, `pyjson.ErrorWithLimit`, `pyjson.DecodeUTF8` and
  `utf8.Valid`.
- The package comments of `internal/pyjson` and `internal/pyvalue` name the stored, hashed and
  machine-read formats each keeps (decision R3S-3, R3R-4 and R3D-1 list them by package).
- Kept under their names: `store.PathlibSpelling`, `PathlibChild`, `PathlibParent` and
  `ownership.PathlibSpelling`, named for the rule they implement (`str(Path(...))`), which spells
  the paths the state-dir key, the scope key and the managed request fingerprint are taken over
  (decision R3S-3); `store.PathRepr`; and the packages' own names, since renaming `pyjson` and
  `pyvalue` would touch about a hundred files for no change of meaning.

Consumer check: renames inside the Go module only; no doc outside docs/port/decisions.md (which
records them as they were) names a renamed symbol.

Evidence: internal/quote/{quote.go,quote_test.go}; internal/pyvalue/{pyvalue.go,number.go,
number_test.go}; internal/pyjson/scan.go; internal/relay/store/{pyerr.go,ownership.go,
frozen_value.go,frozen_detailed.go}.
