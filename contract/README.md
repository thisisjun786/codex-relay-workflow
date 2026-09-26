# CRW Python/Go contract scenarios

A fixture is written and green against the real Python implementation **before** its Go domain is implemented. The Go runner consumes the same JSON, not a translated or generated expectation. Neither corpus runner writes fixtures. The frozen external contracts live in `schema/`; this task does not alter them.

## Identity and ownership

Use exactly `contract/fixtures/<domain>/<source-test-file-stem>__<test-function-name>[__<param-id>].json`. Domains: `cli-shape`, `exit-codes`, `sqlite-ddl`, `mcp-tools`, `appserver`, `git`, `hook`, `records`, `ledger-fingerprint`, `event-key`. Keep parameter IDs stable and filesystem-safe. Relay owns cli-shape, exit-codes, sqlite-ddl, hook, records and event-key; bridge owns mcp-tools, appserver and ledger-fingerprint. A scenario ID is its filename stem; failures must report it. Each scenario is a separate JSON file, even when several scenarios share a source test.

## Grammar

A scenario is `{"given": {...}, "run": {...}, "expect": {"exit": 0, "checks": [...]}}`. `given` builds only temporary, case-local resources. A `run` is an ordered invocation (CLI argv with optional stdin, hook entrypoint or API, MCP tool name and JSON arguments, appserver request/notification, ledger method/params, SQLite query). Multiple invocations are an array of steps, with references to earlier JSON results, for create/bind/read, replay and idempotence. A step may declare an environment override, process cwd, transcript, file tree, seeded SQL, fake host scripted responses, git repository and an ephemeral unix socket. All paths are rooted in the test temp directory except explicit repository artifacts. External programs are driven as real processes, and real SQLite, git, MCP stdio and App Server sockets are used for their respective domains. No fixture contains executable Python, an arbitrary callable, or a test name to invoke instead of the behavior.

For the checkout hook, `{"run":{"kind":"entry","stdin":{}},"expect":{"exit":0}}`
spawns the entrypoint; `{"run":{"kind":"hook","stdin":{}},"expect":{"exit":0}}`
invokes its real API; `{"run":{"kind":"status"},"expect":{"exit":0}}`
reads its real status. These are distinct from the packaged `stop` run kind.
The hook spike's implemented form uses `given.relay` (a real executable stub with `stdout` and `exit`), `given.settings` (false means absent), `given.settings_overrides`, and `given.files` (relative UTF-8 text). File content supports `${HOME}`, `${PYTHON}`, `${ENTRY}` expansion. `run.kind` is `entry` (subprocess), `hook` (the real completion.run) or `status` (the real completion.status); `run.stdin` is a JSON value or a raw string, `run.argv` contains entrypoint arguments, `run.status` reads the status after an invocation. `expect.exit` is mandatory. `expect.stdout_json` compares parsed JSON. `expect.files` maps relative paths to expected existence. `expect.checks` is a list of `{"kind":"eq", "path":["rows",0,"adapterOutcome"], "value":"guard_answered"}`. Paths address `exit`, `stdout`, `stderr`, `rows` (journal JSON), `status`, `call` (the fake relay's actual argv and stdin), and `files`. Checks run against observed data; expected values must not be derived from the observed values.

## Assertion survey of all 14 class-A files

The 14 source files were surveyed before choosing assertion operators. Their assertion vocabulary is: exact equality and inequality (including identity with None/booleans), truthiness/falsiness, membership and nonmembership, set equality/subset, ordered and unordered collections, cardinality, less-than/greater-than bounds, regular-expression match, parsed JSON fields and JSON schema, substrings, raised exception type/message, process exit/stdout/stderr, timeout and signal outcome, filesystem presence/absence, file bytes/text/mode and directory entries, symlink target, git status/revision/worktree listing, SQLite rows/DDL/PRAGMAs, stable hash and ledger fingerprint, MCP tool list/input schema/content/error, App Server request count and ordered call trace, absence of side effects, same/different value across steps, and agreement between packaged and checkout adapters. `test_coordination_cli.py`: equality, truth, membership, absence, ledger filtering and command re-execution. `test_dispositions.py`: equality, truth, membership, None, counts, SQLite and no writes. `test_management_cli.py`: equality, truth, membership, exit codes, negative filesystem effects and recovery command. `test_reporting_cli.py`: exit, stdout/stderr, JSON, membership and absent store. `test_stop_adapter.py`: stdout, record content/mode, path regex and event deduplication. `test_worker_policy.py`: worker identity, digest, retained file bytes/mtime and process liveness. `test_mcp.py`: MCP errors, schema required-fields, structured content, call counts and absent host effects. `test_worktree.py`: git and filesystem state, exceptions, replay, exact revision and cancellation. `test_adapter_agreement.py`: output/row/ledger equality, deliberate mutations noticed, paths and file mode. `test_completion_hook.py`: JSON, journal/count/claim contents, configuration, stdout/exit, statuses and path identity. `test_parent_title.py`: CLI exit, JSON and replay coverage. `test_relay_schema_shipped.py`: normalized SQLite CREATE text and snapshot cardinality. `test_release.py`: fake remote git state, workflow gates, logs, publication refusal/recovery. `test_stop_events.py`: per-event journal/ledger counts, duplicate handling, verifier JSON/exit and corrupted-file refusal.

Checks encode these as `eq`, `ne`, `contains`, `excludes`, `truthy`, `falsy`, `length`, `regex`, `lt`, `gt`, `json_eq`, `after` (value immediately after a named argv flag), and `same`, with domain-specific `stdout_json`, `rows`, `hash`, `files` and process `exit`. These checks are implemented by the domain runners as observations or evaluator operators; conversions must assert the original property rather than substitute weaker truthiness checks. An assertion about prose is not a portable contract; preserve only machine-consumed structural tokens and routing fields.

## Spike and exceptions

The 20 `test_completion_hook__*.json` cases added `call.argv`, captured raw stdin, `rows`, `status`, text-template file setup, JSON-decoded equality (`json_eq`), stdout JSON and entrypoint exit/stderr. They exercise the actual checkout hook and an executable fake relay, not a mocked completion.run. The matching original 20 test functions now call their named fixtures; they do not write fixtures.

Live-interleaving cases remain Python tests until an equivalent Go package test exists: `test_worktree::test_cancellation_after_git_creation_retains_checkout_without_starting_task` requires intercepting the exact git subprocess between creation and checkout; `test_worktree::test_checkout_change_during_thread_start_withholds_prompt` and `test_worktree::test_interrupted_dispatch_is_retained_and_never_repeated` require live paused RPC interleavings; `test_worker_policy::test_record_changing_during_read_is_not_a_ready_snapshot` requires replacing a method between two reads; `test_completion_hook::test_a_link_retargeted_under_the_listing_is_not_published_as_an_alias` requires an exact mid-read symlink retarget. They must not be represented by a fixture that simply calls the original test. Other live races and monkeypatch cases in those files need the same per-case determination during lane conversion.

`python3 scripts/port/check_corpus_count.py` is a coverage check, not a fixture count. It parses every fixture as JSON and fails naming any that does not parse. It lists every class-A test function from the class-A files in `docs/port/test-map.md` (by AST, including class methods; one parametrised function is one function). It reads `contract/notes/*.md` and requires each function to appear exactly once with status `converted`, `kept`, or `blocked`: converted needs at least one existing fixture whose name starts with `<stem>__<function>`, kept needs a reason, and blocked needs the missing kind named. It prints per-file converted/kept/blocked counts and exits 0 only when every function is accounted for. The number of fixtures does not need to match the number of tests.

## Implemented runner families

`contract.runner` owns dispatch and assertions; the root `conftest.py` only re-exports discovery.
Every step runs in a case-local temporary directory. Examples below are abbreviated; real
fixtures use the identity filename rule above.

- `cli`: `{"run":{"kind":"cli","argv":["dispositions-show"]},"expect":{"exit":2}}` spawns the relay module with `--state <tmp>/state`. `steps` is an ordered list with optional `id`, `stdin`, `env`, `cwd`, and `timeout`; `{"$step":"register","path":["stdout_json","id"]}` resolves a prior result. `given.files` creates text files and `${HOME}` expands to the case root. `given.sql_seed` opens the real relay Store and executes test-owned SQL only for tests that seed SQL themselves (`given.sql` is for a complete standalone database). `expect.queries` maps labels to SQL queries and exposes result tuples under `sql`.
- `mcp`: `{"run":{"kind":"mcp","steps":[{"tool":"get_capabilities","arguments":{}}]},"expect":{"exit":0}}` uses the real MCP stdio process and the existing fake App Server on a real unix socket. `tools` is a name-to-input-schema map; `results` expose `error`, `content`, `structured`; `calls` records mutating host calls.
- `appserver`: `{"run":{"kind":"appserver","steps":[{"method":"project/read","params":{"projectId":"p"}}]},"expect":{"exit":0}}` connects the real Python client to the fake host; `given.host` configures its scripted response knobs, `calls` is ordered.
- `git`: `{"given":{"files":{"tracked":"base\n"}},"run":{"kind":"git","steps":[{"kind":"worktree","path":"isolated"}]},"expect":{"exit":0}}` uses Git and the bridge's real worktree API. `given.staged` and `given.dirty` establish index and working-tree changes; `branch` and `commit` steps are available. `status`, `revision`, `branches`, `worktrees`, `inspection`, `checkout`, and `source` are observable.
- `release`: `{"run":{"kind":"release","job":"validate","step":"release-inputs"},"expect":{"exit":0}}` runs the extracted workflow script in a fresh real Git clone with the shipped fake_gh.sh/fake_git.sh. `given.ci`, `given.tag`, `given.release`, `given.push` script fake state; `calls` records gh invocations.
- `stop`: `{"given":{"relay":{"stdout":"{}"}},"run":{"kind":"stop","stdin":{}},"expect":{"exit":0}}` launches packaged stopadapter.py with settings and stdin. `given.settings_overrides` changes settings, `concurrency` starts processes before feeding them. `rows` are journal JSON, `outcomes` retain per-process exit/stdout/stderr, `observed` contains file `exists`, `bytes` (hex), `mode`, `target`, and `entries` for paths in `expect.observe`.
- `agreement`: `{"run":{"kind":"agreement","stdin":{}},"expect":{"exit":0}}` drives checkout and packaged adapters with independent settings and real relay stub processes and compares returned output and journal records, excluding only the six documented volatile fields. `stdin: null` passes None to each real adapter.
- `entry`, `hook`, `status`, `install`: the checkout hook family described under Grammar. `{"run":{"kind":"status","document":true},"expect":{"exit":0}}` exposes the checkout configuration builder's JSON (the document proof checks that the old socket key is absent). `{"run":{"kind":"install"},"expect":{"exit":0}}` starts the real runtime installer; no proof fixture uses it yet, so installation conversion remains blocked.

## Round 2 runner extensions

The second conversion round grew the families above. Every extension keeps the real Python
surface under test and the fake App Server only as the external peer. Nothing below lets a
fixture supply executable Python.

### Relay CLI (`cli`)

- `expect.observe` exposes `size`, hex `bytes`, `mode`, and sorted `entries`, besides existence: `{"observe":["state/relay.sqlite3","state"],"checks":[{"kind":"eq","path":["observed","state/relay.sqlite3","size"],"value":0},{"kind":"eq","path":["observed","state","entries"],"value":["relay.sqlite3"]}]}`. The two unrelated-database tests use this, not merely the absence of WAL and shm files.
- `given.host` starts an ephemeral Unix websocket App Server and passes its socket as the relay CLI's `--socket`. `threads` seeds host threads; the host answers lifecycle filters and settings readback while the real relay owns every delivery decision: `{"given":{"host":{"threads":{"01child-task":{"id":"01child-task","source":"cli","archived":true,"status":{"type":"idle"},"turns":[]}}}},"run":{"kind":"cli","steps":[{"id":"withhold","argv":["deliver","--event","bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"]},{"id":"read","argv":["dispositions-show","--relationship","rel-1"]}]}}`. `host_calls` and `host_threads` expose the fake host's wire observations; they never stand in for the relay's CLI response or SQLite state.
- Every relay CLI subprocess receives `HOME`, `XDG_STATE_HOME`, `XDG_DATA_HOME`, `XDG_CONFIG_HOME`, `CODEX_HOME`, and `CODEX_SESSION_RELAY_SCOPE_DIR` under its scenario root. The last one matters: service uses the passwd home for production scope regardless of HOME, and the isolated scope must be authorized with `daemon --allow-isolated-scope`: `{"given":{"host":{}},"run":{"kind":"cli","argv":["daemon","--max-ticks","0","--allow-isolated-scope"]},"expect":{"exit":0,"observe":["scopes"],"checks":[{"kind":"truthy","path":["observed","scopes","entries"],"value":null}]}}`.

### Bridge MCP (`mcp`)

- Env and files: `{"given":{"files":{"policy.json":"{\"allowed\":[]}"}},"run":{"kind":"mcp","env":{"CODEX_THREAD_BRIDGE_EXECUTION_POLICY":"${HOME}/policy.json"},"steps":[{"tool":"get_capabilities"}]}}` creates a policy and hands its absolute path to the server.
- Bare spawn: `{"run":{"kind":"mcp","process":true,"env":{"CODEX_THREAD_BRIDGE_EXECUTION_POLICY":"${HOME}/broken.json"}}}` captures `exit`, `stderr`, `state_exists`, and `calls`.
- Launcher: `{"given":{"policy":{"roles":{"child":{"model":"m","reasoningEffort":"high"}}}},"run":{"kind":"mcp","launcher":true,"steps":[{"tool":"get_capabilities"}]}}` builds the plugin-cache layout, record, and policy digest, then starts `./wiring/crw_bridge_mcp.py` with a separate HOME and without policy or CODEX_HOME in the launcher environment. The launcher proof uses the record's computed digest, not a literal one.
- Host cursor: `{"given":{"host":{"cursor_format":"json_turns","threads":{"thread-1":{"cwd":"${HOME}"}}}},"run":{"kind":"mcp","steps":[{"tool":"list_threads","arguments":{"limit":1}}]}}` configures JSON cursor/offset methods and expands seeded host paths.
- Host mutation: `{"run":{"kind":"mcp","steps":[{"tool":"create_thread","id":"created","arguments":{}},{"host_set":{"path":["complete_turns"],"value":false}},{"tool":"get_active_turn","arguments":{"thread_id":{"$step":"created","path":["structured","threadId"]}}}]}}` changes the fake peer between calls in one session; `host_set.path` may traverse the fake's `threads` mapping.
- Socket alias and restart: `{"run":{"kind":"mcp","socket_alias":true,"sessions":[[{"tool":"create_thread","arguments":{}}],[{"tool":"create_thread","arguments":{}}]]}}` starts a second sequential server on the canonical socket path with the same state after the first used a symlink alias. Results and calls span both sessions.

Proof fixtures are the nine `test_mcp__*.json` scenarios under `contract/fixtures/mcp-tools`.

### Bridge worktree (`git` with `bridge`)

`"bridge": true` builds a fresh Git repository under `/dev/shm`, connects the real `Bridge` and `AppServer` client to the suite's socket-backed fake host, and keeps a real SQLite ledger. The source commit is exposed as `base`; `receipts`, `ledger`, `threads`, `thread_views`, `calls`, `launch_calls`, `status`, `worktrees`, `source`, `checkout`, and `paths` are observations. `given.files`, `staged`, `dirty`, `arguments`, and `host` set the starting state. `expect.paths` lists case-local paths and returns existence booleans. Fixtures without `bridge` keep their earlier behavior.

- `launch`: `{"run":{"kind":"git","bridge":true,"steps":[{"kind":"launch"}]},"expect":{"paths":["isolated"],"checks":[{"kind":"eq","path":["receipts",0,"status"],"value":"accepted"}]}}`. A second `launch` replays the same request; `step.arguments` overrides `given.arguments`. Exceptions expose `exception` and `message` in the receipt. `{"kind":"launch","fail_before_write":"thread/start"}` fails the real client's call before the frame is sent, and `{"kind":"launch","env":{"GIT_DIR":"wrong-git","GIT_WORK_TREE":"wrong-tree","GIT_INDEX_FILE":"wrong-index"}}` applies case-rooted environment variables during the call.
- `restart`: `{"kind":"restart"}` between two launches closes and reopens the ledger and builds a new `Bridge` over the same socket. `ledger[request_id]` exposes the retained receipt.
- `snapshot`: `{"kind":"snapshot"}` records `git worktree list --porcelain` at that point.
- `concurrent`: `{"kind":"concurrent","request_ids":["first","first","second"]}` starts all calls in one task group, keeping receipts in input order.
- `move` and `remove`: `{"kind":"move","from":"isolated","to":"moved-checkout"}` or `{"kind":"remove","path":"isolated"}` change case-local paths between launches.
- `host`: `{"kind":"host","field":"complete_turns","value":false}` sets a field on the fake host mid-scenario.
- `seed_legacy`: `{"kind":"seed_legacy","arguments":{"request_id":"legacy-worktree"},"receipt":{"requestId":"legacy-worktree","status":"outcome_unknown"}}` computes the real ledger fingerprint from the case-local source, destination and starting revision, then inserts the receipt into SQLite.
- Hostile givens: `{"given":{"hostile_git":true},"expect":{"paths":["unexpected-effect"]}}` installs a real post-checkout hook, required clean/smudge filter, fsmonitor, conditional include and `.gitattributes`; the marker must stay absent. `given.git_config` maps arbitrary local Git config keys and values, substituting `${SOURCE}` and `${ROOT}`.
- Declined: `RelabellingPolicy` is a Python object whose executable `authorize()` returns a different pair from the requested one. A JSON allowlist can't express that, so `test_a_worktree_launch_follows_the_authorization_not_the_arguments` stays a Python test until a Go package test models the custom policy.

### Stop adapter, agreement and hook (`stop`, `agreement`, `entry`, `hook`, `status`)

- `given.relay: {"stdout":"...","signal":true,"delay":1,"die_first":true}` records each invocation in `calls` with `argv` and raw `stdin`; `die_first` makes the first guard process signal the adapter parent after logging the call. The replay proof checks `calls` length one.
- `run.steps: [{"transcript_stop":0},{"transcript_stop":0}]` invokes the packaged program twice under one isolated home; `concurrency: 2` starts both before feeding stdin. `entry: "console"` launches the packaged `main` (`stopadapter.main()`, not the script wrapper), `entry: "checkout"` runs the checkout script, `command` supplies a declared command argv for another registration (an argv array, never shell text), and `settings` selects a precreated per-step settings path.
- `given.transcript_r1: true` with a `transcript_stop: 0` step writes the first real captured transcript prefix and supplies its local path and cwd in stdin. Literal `${HOME}` in ordinary stdin is expanded too.
- `given.settings_overrides: {"installedBy":"CRW-212","journalRoot":"${HOME}/other"}` changes the registration; `given.files` supplies additional settings.
- `verify`: `{"run":{"kind":"stop","steps":[{"kind":"verify","argv":["--journal-root","${HOME}/journal"]}]}}` executes the real `scripts/stop_events.py`; its exit and parsed JSON land under the final `outcomes` element.
- `mutate`: `{"run":{"kind":"stop","steps":[{"kind":"mutate","role":"claim","operation":"pop","field":"claimedBy"}]}}` addresses case-local files by role; operations are `set`, `pop`, `unlink`, and `symlink`. The role glob can't reach outside the case root.
- `row_files` exposes each journal date/name, bytes and mode, selected only by the writer's filename pattern. The record proof checks one row, the 32-hex name and mode 384.
- Completion hook givens: `given.symlinks: {"codex-session-relay":"${HOME}/gone"}` creates a broken pointer (the proof observes the real status cell); `given.modes: {"launcher":493}` chmods a file; `given.relay.delay` and `elapsed` measure the checkout run. Isolated `HOME`, `XDG_*` and `CODEX_HOME` are provided, and `run.env`, `run.cwd` and templated `run.argv` pick the launch context.

Proof fixtures: `test_stop_adapter__test_the_same_stop_replayed_is_accepted_once_and_asked_about_once`, `test_stop_adapter__test_the_record_is_written_where_the_settings_said_and_only_for_its_owner`, `test_stop_adapter__test_the_entry_point_exits_zero_and_says_nothing_on_stderr`, `test_completion_hook__test_a_broken_link_is_unreadable_and_not_absent`, and `test_adapter_agreement__test_a_document_written_without_a_socket_carries_no_such_key`.

Not converted, and not claimed converted (source tests stay intact): the agreement settings-reader observation, nonregular paths and deadline-bounded FIFO reading (observing silence would weaken the source assertions); agreement fault/scan injections (they need a controlled module seam); the completion elapsed proof; completion probe_timeout (no safe bounded process-group observation); completion expected_template (checks would need `${PYTHON}` expansion in the shared evaluator). Declined as corpus kinds: completion call_count (read counts are internal and need an instrumented Go package test, not a weaker row count) and completion no_surface (module inventories and pure helpers belong to Go package or inventory tests).

Adapter-agreement mutation proofs (`test_adapter_agreement` cases that mutate module attributes to show the comparison notices a deliberate change) stay Python tests, because attribute mutation can't be expressed as portable data; they move to Go package tests in todo 9.

For checks use `{"kind":"eq","path":["exit"],"value":0}`. The evaluator also
supports `ne`, `json_eq`, `contains`, `excludes`, `truthy`, `falsy`, `length`, `regex`,
`lt`, `gt`, `after`, `same` (with `other` path), `set_eq`, `subset`, `bytes` (hex),
`ordered_calls`, `absent_effects`, `exception_code`, `timeout`, and `signal`.
Filesystem `observed` paths expose mode, symlink target and sorted directory entries;
`sql` exposes rows and DDL through `sqlite_master`. A failed check always includes the
scenario ID.

## Check examples

Each entry below is a complete `expect.checks` element; `path` addresses the observed
result (and `other` addresses another result for `same`). File observations require
`expect.observe` to list the relative paths, e.g. `"observe":["journal"]`.

| Kind | Example |
| --- | --- |
| `eq` | `{"kind":"eq","path":["exit"],"value":0}` |
| `ne` | `{"kind":"ne","path":["exit"],"value":2}` |
| `json_eq` | `{"kind":"json_eq","path":["stdout"],"value":{"ok":true}}` |
| `contains` | `{"kind":"contains","path":["stdout"],"value":"ready"}` |
| `excludes` | `{"kind":"excludes","path":["stdout"],"value":"error"}` |
| `truthy` | `{"kind":"truthy","path":["rows"],"value":null}` |
| `falsy` | `{"kind":"falsy","path":["rows"],"value":null}` |
| `length` | `{"kind":"length","path":["rows"],"value":1}` |
| `regex` | `{"kind":"regex","path":["stdout"],"value":"^ready"}` |
| `lt` | `{"kind":"lt","path":["exit"],"value":3}` |
| `gt` | `{"kind":"gt","path":["exit"],"value":-1}` |
| `after` | `{"kind":"after","path":["call","argv"],"flag":"--state","value":"ready"}` |
| `same` | `{"kind":"same","path":["inspection","initialRevision"],"other":["base"]}` |
| `set_eq` | `{"kind":"set_eq","path":["branches"],"value":["main"]}` |
| `subset` | `{"kind":"subset","path":["branches"],"value":["main"]}` |
| `bytes` | `{"kind":"bytes","path":["observed","record","bytes"],"value":"6869"}` (hex for `hi`) |
| `ordered_calls` | `{"kind":"ordered_calls","path":["calls"],"value":[["project/read",{"projectId":"p"}]]}` |
| `absent_effects` | `{"kind":"absent_effects","path":["calls"],"value":null}` |
| `exception_code` | `{"kind":"exception_code","path":["results",0,"error","code"],"value":-32601}` |
| `timeout` | `{"kind":"timeout","path":["outcomes",0,"timeout"],"value":false}` |
| `signal` | `{"kind":"signal","path":["outcomes",0,"signal"],"value":null}` |

Filesystem mode, symlink target and directory entries use `eq` against `observed`, for
example `{"kind":"eq","path":["observed","record","mode"],"value":384}`,
`{"kind":"eq","path":["observed","link","target"],"value":"record"}` and
`{"kind":"eq","path":["observed","journal","entries"],"value":["day"]}`.
SQL rows use `eq` against a query label, e.g.
`{"kind":"eq","path":["sql","seeded_scope"],"value":[["r","p"]]}`. The bridge fake host is only a fake of the external App Server, not the code
under test.
