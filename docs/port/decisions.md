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

Evidence: scripts/crw_runtime/completion.py:529-547,624-635,2728-2737,1870-1902;
packages/codex-session-relay/src/codex_session_relay/stopadapter.py:241-254,318-323;
docs/port/cutover.md "Hook budget" (startup/input 100 ms, guard 3500 ms);
`Test33ReviewD1` through `Test33ReviewD12`, `Test33ReviewD9Late`,
`internal/relay/selection`, and .omo/evidence/rev33/result.json;
stopadapter.py:1136-1162,1198-1200; marker.py:289-315; store.py:1833;
`Test33AnsweredDeadlinePython`, `Test33PeerCredentials`, `Test33UntrustedPeerReleases`,
`Test33PythonStateDirectoryMode`, `Test33LargeMarkerFactPython`, and
`Test33LargeFactHookPython` (PR #181 threads 4120181139, 4120181488, 4120181269).

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

Evidence: `packages/codex-session-relay/src/codex_session_relay/service.py:1304-1421`
(stop and worker re-read), `:1480-1516` (grace and process-state outcomes),
`:299-322` (parent-death signal); `internal/relay/service/reap_test.go`
(`Test29D1DeterministicReapStates`), `check29_test.go`
(`Test29D1PlainRoundTripTwenty`, `Test29D3LaunchSnapshotTwenty`),
`.omo/evidence/check29-fixes/` (`python-sigterm-proof.log`, `packages.log`,
`packages-serial.log`), and `.omo/evidence/check29-flake/`
(`reproduce_leader_exit.py`, `leader-exit.log`, `run-3.log`).
