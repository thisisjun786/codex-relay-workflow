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
is a documented limitation carried over, not a new one.

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
holds no long-lived executable.

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
`crw-bridge-mcp.json` keeps `recordVersion = 2`; the host record keeps `recordVersion` and
`definitionVersion = 1`. The Python-shaped keys (`interpreterPath`, `interpreter`,
`adapterInterpreter`, `requiresPython`, `module`, `packageLocation`, `serverModule`,
`exerciseScript`) are read and preserved when present and written as absent by the Go
installer; their replacement shape is CRW-157's components definition v2, decided there and
not here.

Evidence: scripts/crw_runtime/completion.py:53 (`CONFIG_NAME`), :558-563 (`configVersion`
check); scripts/crw_runtime/bridgerecord.py:79 (record home); scripts/runtime_install.py:
725-771 (`interpreterPath` read paths); scripts/crw_runtime/components.json:3
(`definitionVersion: 1`), :9-26 (Python-shaped fields); draft L1, L4, L5, GAP-9, D6.

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
the prose itself. Four places, recorded as accepted differences:
- Tool-argument validation: the first line `Error executing tool <t>: <N> validation
  error(s) for <t>Arguments`, the error count and the failing field locations in signature
  order are Python's; each field's reason line is the bridge's own, without pydantic's
  `[type=...]` detail or the version-specific `errors.pydantic.dev/<ver>` URL.
- `initialize.serverInfo.version` is the bridge's version (0.1.0, as `--version` prints);
  FastMCP sends the mcp library version (1.30.0 at the pin).
- The JSON inside a tool reply's text content is compared as parsed values; its key order
  is Go's, not the dict insertion order of the Python receipt.
- An execution-policy file that is not valid JSON is refused with the same exit code,
  empty stdout and `execution_policy_unreadable` prefix; the decoder message after it is
  Go's, not CPython's `json.JSONDecodeError` text.
Every other reply on the wire is Python's bytes (decision 5, todo 16 wire tests).

Evidence: packages/codex-thread-bridge/src/codex_thread_bridge/server.py (FastMCP tool
registration); todo-16 checker comparison against mcp 1.30.0 / pydantic 2.13.5 recorded in
.omo/evidence/task-16-crw-go-port.txt; the question to Jun on 2026-09-25 went unanswered
for 30 minutes and the recommended option was taken, reversible by changing
internal/bridge/mcp/validate.go and server.go.

## How this file is checked

The acceptance check for this document is structural: every decision heading is followed
by an `Evidence:` line, and the file contains no open marker. A reviewer resolves each
citation against the worktree revision named at the top. There is no failure scenario to
run for a document; a citation that stops resolving is caught when the cited line moves,
by the reviewer, not by a script.
