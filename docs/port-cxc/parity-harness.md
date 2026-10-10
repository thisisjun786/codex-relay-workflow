# Hook parity harness (`crw-dev parity`)

CRW-203. The harness answers one question about the hook surface of a CRW plugin root: for each of the
34 legs (the 32 registrations of CXC v0.2.40, contract K1, and CRW's own completion Stop and GitHub post
guard), is the leg declared, does the declared command run when the host starts it, does it do what the
corpus says it should, and is it as fast as the Node original. Each is a cell of its own in the report,
so a pass in one is never read as a pass in another.

It is development tooling: it lives under `internal/dev/laneparity` (build tag `dev`), is reached as
`crw-dev parity`, and is not part of a release archive. It starts nothing on the host: every case is a
temporary root (`HOME`, `CODEX_HOME`, `XDG_*` and `TMPDIR` inside it), the plugin root is only read, and
the real Codex home, the installed runtime, hook trust and the plugin cache are never touched. A pass here
is not an installation pass: installation parity belongs to CRW-201 and CRW-204. The real-host cells (below) are the
one exception to "starts nothing": they start the real Codex binary, in a temporary home of their own, against a stub
model provider on the loopback interface, and only when asked for (`realhost`, or `all --realhost`).

## Cells

| Cell | What it shows | Fails when |
| --- | --- | --- |
| registration | the plugin root's manifest lists hook files that declare each leg once, under its event, with its matcher, timeout and `(crw)` status message (K1 after the R24 rename); a command counts as the leg's only in the form a CRW root declares, read as the shell runs it: one simple command `<crw> hook <event> --leg <leg>` (or `--leg=<leg>`, or `<crw> hook --plugin-launch` for the completion Stop), optionally followed by `; exit 0`, where `<crw>` is `crw`, `$CRW_BIN` or the path of a file named `crw` (absolute, or under `$HOME` or `$PLUGIN_ROOT`) | a leg is missing, declared twice, under another event name, with a drifted matcher, timeout or status message, async, or a registration starts no leg of the table (hook syntax in a comment, as another command's argument, after `&&` or `\|\|`, in a branch, behind a redirect or pipe, followed by another command or with an extra argument starts none); the manifest names another plugin |
| firing and effect | the command the root declares for the leg, run as the host runs it (`/bin/sh -c`, `PLUGIN_ROOT`, an isolated `CODEX_HOME`), answers each corpus payload (`contract/fixtures/cxc/hook__<leg>__*`) with the expectation, observed tree and calls of `TestDomain/cxc`, status files and claims included | any claimed fixture differs; a leg has no declared command for its event |
| receipts | every firing leaves a receipt naming this run, the plugin root digest, the sha256 of the executable the declared command starts (resolved from the command line: an absolute path, `crw` or `$CRW_BIN`), the leg, its event, and the payload's session, turn, tool call, tool and agent and the skills its answer names; the verifier compares them with what the fixtures say | a receipt is of another run or plugin root, or names a build other than `--crw` (or none: the command starts no executable the harness can identify), names another event, leg, agent or skill, is missing, doubled or unexplained |
| latency | p50 and p95 of the Go command against the CXC v0.2.40 command (`node .../dist/cli.js`) on the same payload (`--oracle` is required: `latency` refuses to run without it, and `all` without it reports the cell not verified), N fresh case roots per side, the two sides alternating; a leg that fails is measured again (`--attempts`, five in all by default) because a shared host's load puts outliers in a p95, and the report records the attempts a leg took and the load average; a leg that still fails while the host's load average is above its CPU count is inconclusive (listed as not verified; `--strict` makes it a failure) | Go p95 is above the TS p95, or above half of the timeout the plugin root declares for the leg (the one the host gives the command, which for CRW's own legs may differ from the table's); the fixture timed does not match its expectation (the command exits nonzero, is killed, times out or answers something else: a hook that fails at once is quicker than one that works), the oracle's command does not exit as recorded, or the declared command starts another executable than `--crw`; none of these is excused by host load |
| switch silence | one fixture per ported leg (an answering one where the leg has it), fired with the switch off (no file) and at `cxc`: every firing exits 0 with nothing on stdout and leaves no invocation record, and every chosen leg fired at least once (a leg that never ran is not silent); the completion Stop is not behind the switch and is left out | a ported leg answers, fails or records with the switch off or at `cxc`, or a leg left no receipt |
| real host | the real Codex binary runs whole turns in isolated homes against a stub model provider (see "Real-host cells"); the report's `realHost` holds one entry per cell | a cell's hooks differ from the declared ones the turn's events match, a hook is given another session or turn than the host's, a ported leg answers with the switch off or at `cxc`, an answer at `crw` never reaches the model, or a hook starts without trust |

The ported legs stay silent until `<CODEX_HOME>/crw/switch.json` says `crw` (CRW-392), so the harness writes
`{"active":"crw","changedAt":...,"by":"laneparity"}` (a temporary file renamed into place) into the Codex home of every
step the firing and latency cells start, and takes it out again before the case's tree is observed, so the observed tree
is the scenario's and the hooks' alone. The Codex home is the one the step's own environment names: a step that sets
`CODEX_HOME` to a home of its own, or unsets it (the hook then reads `$HOME/.codex`), gets the file there, made with the
modes the hooks give what they make (CRW-1082; before it the file was written into the case's `CODEX_HOME` only, and the
fixtures of those steps ran against no switch and were silent). Nothing is written outside the case root (every link on the way to a
directory the harness makes in is followed, and one that leaves the case root, or points nowhere, is refused, for the
switch and for the runtime link, and again before the harness takes them out), and the account's home is never resolved. The report's `switch` (and the fire cell's) names the state every case root held, and
`switchSilence` the cell that fires the same legs with the file absent and at `cxc`.

The shipped plugin starts `"$HOME/.local/share/crw-runtime/current/bin/crw"`. To fire its declarations as they are, the
harness links that path to the build under test in the `HOME` of every step (and removes the link and the directories it
made before the tree is observed); a receipt names the build the link leads to. The installed runtime of the host
running the harness is never read.

A leg whose matched fixtures all expect silence is listed as unverified: a command that does nothing passes
them too. A leg whose fixtures are all still pending is listed the same way. Both stay visible in every
report (`unverified`, `notVerified`) and neither fails the run.

## Running it

```sh
export PATH=/home/jun/.local/go/go/bin:$PATH
go build -tags dev -o "$TMPDIR/crw-dev" ./cmd/crw-dev
# the crw under test, built as TestDomain/cxc builds it (the recorder's frozen clock linked in)
go build -trimpath -ldflags "-X main.recallTestClock=1767225600000 -X github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor.retrustTestClock=1767225600000" -o "$TMPDIR/crw" ./cmd/crw

crw-dev parity all --crw "$TMPDIR/crw" --oracle /path/to/extracted/cxc-v0.2.40 --json report.json
crw-dev parity all --crw "$TMPDIR/crw" --oracle /path/to/extracted/cxc-v0.2.40 --realhost --json report.json   # with the real-host cells
crw-dev parity realhost --crw "$TMPDIR/crw" [--codex /path/to/codex] [--only 'turn/']
crw-dev parity registration --plugin /path/to/plugin-root
crw-dev parity fire --crw "$TMPDIR/crw" --plugin /path/to/plugin-root --only 'hook__stop-'
crw-dev parity latency --crw "$TMPDIR/crw" --plugin /path/to/plugin-root --oracle ... --runs 30 --legs 'session-start'
crw-dev parity plugin-root --crw "$TMPDIR/crw" --out /path/to/new-root/crw
```

The plugin root and the oracle path are arguments; nothing is hard-coded. Without `--plugin`, `all` fires the plugin that
ships (`plugins/crw` of the repository), whose 34 declarations (CRW-392) start the runtime through
`$HOME/.local/share/crw-runtime/current/bin/crw`. `--generated` fires a root generated from K1 instead (`plugin-root`): the
files of `plugins/crw` with a manifest and hook files that declare the 34 legs, each command starting the crw under test
by its path as `crw hook <event> --leg <leg>`.

## Evidence and cleanup

The report (`--json`) records the build (path and sha256), the plugin root (path and digest of its manifest, the files under
`wiring/hooks` and every hook file the manifest lists), the oracle revision of K1, the run id every receipt carries, the owner, process and run directory of the test,
and how it was cleaned up: every case root is removed as its case ends, the run directory (a generated plugin root and the
case roots, under `--scratch` or `$TMPDIR`) when the run ends, and the number of entries left behind must be zero or the run
fails. No shared database, live session, Codex home or installed runtime is used. The report's `key` is a digest of the build,
the plugin root, the corpus files the cells read, every option that changes what is run or how it is judged (including `--strict`),
and every artifact a cell runs, by content: the executable each declared command starts (so a root starting another file at the same
path is another artifact), the node and the oracle tree a latency cell runs (read through links, as the recorder reads it), and the harness's own executable (its verdict rules).
`--reuse FILE` stands a passing report of the same key in for a run, so evidence of the same artifact, revision and criteria is not
produced twice. `latencyLaterAttempts` names the legs whose latency passed only on a later measurement, and the summary line counts
first-attempt and later passes.

## Real-host cells

`crw-dev parity realhost` (and `all --realhost`) runs the real Codex binary (`--codex`, else `codex` on `PATH`) through whole
turns, each in a home of its own under the run directory: `HOME`, `CODEX_HOME`, `TMPDIR` and the working directory are temporary,
the environment holds nothing of the caller's (a `PATH` of system programs, and a proxy that refuses every connection but the
loopback provider, so the host's attempt to fetch its plugin catalogue fails at once), and no credential is copied or needed.
The model is a stub: `StubProvider` serves `POST /v1/responses` on `127.0.0.1` with a script (a tool call, a message, the usage
that makes the host compact), and the host is pointed at it by `model_provider` in the home's `config.toml`.

The plugin is placed as the host's plugin cache holds it (`plugins/cache/<marketplace>/crw/<version>`), enabled in
`config.toml`, and its 34 hooks are trusted by running the built `crw doctor retrust --bootstrap-ok` against that home. No run
passes `--dangerously-bypass-hook-trust` or any other flag that skips trust, approvals or the sandbox (the report's
`bypassHookTrust` is false and `codexArgs` lists the arguments), and a fired hook that needed one would not count. The installed
runtime path the declarations start holds a recorder, which keeps the arguments, the payload on stdin, the answer, the exit
status and the time of every start and hands the start to the build under test unchanged. The host's own record of what ran
(the `hook-observations` invocation records the pabcd-state legs write) is read too.

| Cell | Script | Passes when |
| --- | --- | --- |
| `turn/crw` | a shell command, an image view, an answer; switch at `crw` | the hooks the host started are exactly the declared ones the turn's events and matchers select (SessionStart 9, UserPromptSubmit 5, PreToolUse 3 for the shell tool, PostToolUse 1 for the viewer, Stop 3), each given the host's thread and one turn id, a tool call id for tool events, and exiting 0; at least one answered; every answer that carries context is in a request the model received; the home holds invocation records naming the thread; the turn ran to its end |
| `turn/off`, `turn/cxc` | the same, with no switch file, and with the switch at `cxc` | the host started the same 21 hooks, none of the ported legs answered, no invocation record was written, and no hook text reached the model |
| `untrusted/crw` | the same, with no trust recorded | the turn ran to its end and the host started no hook |
| `compaction/crw` | a tool call that reports more usage than the home's auto-compaction limit | the host sent one compaction request, started the three PostCompact hooks and the SessionStart of the new session (source `compact`), and the recall context that answered reached the model |
| `spawn/crw` | the turn spawns an agent of role `worker`, waits for it and answers | the agent had a turn of its own with the provider, the spawn hook, both SubagentStop hooks (the one matched by `.*` and the one by `^(executor\|worker)$`) and the Stop hooks started once each, and the parent's turn ran to its end |
| `permission/crw` | a command that needs escalation | verified only if the host starts a PermissionRequest hook for it; on Codex 0.154.0 `codex exec` refuses the escalation first (`approval policy is Never` comes back to the model), so the cell reports itself not verified with that measurement |

Hook starts are compared as a multiset of (leg, event, tool): a hook started twice, not at all, or that no declaration is due
for fails the cell. A cell the host cannot be driven into is `notVerified` with the reason; a run without a Codex binary on
`PATH` reports every cell not verified, with that reason, and does not fail. A cell whose turn sent the stub provider no model
request (`turnDriven` false), or whose `crw doctor retrust` failed, is compared with a control run once in a home of the stub
provider alone (no plugin, no trust, no switch): when the control's turn sends no request either (or its `codex features list`,
the check retrust runs, fails too), the host cannot be driven or prepared with the stub and the cell is not verified with both
measurements; when the control succeeds, what the cell adds stopped it and the cell fails, as does a hook the host started and
that failed. Only cells whose turn reached the provider are named in the scope as turns that ran; the others are listed as
attempted. The cells are heavy (a host process each), so they
run only on request, and `go test` runs them behind the `realhost` build tag: `go test -tags dev,realhost ./internal/dev/laneparity`.

## Faults

`fire --inject <fault>` injects a fault and succeeds only when the run turns red for it. The faults are the ways
a cell could pass without the hook having done its work: `wrong-event`, `missing-hook`, `noop`, `kill` (the
declaration), `drop-stdout` (a lost answer) and `reverse-steps` (payloads in the opposite order) (the transport),
and `stale-receipt`, `other-plugin`, `other-build`, `receipt-event`, `receipt-agent`, `receipt-skill`,
`lost-receipt`, `double-receipt` (the record). `internal/dev/laneparity` tests every one against a real build. For the real-host
cells `--inject noop` (the recorder starts the build for no hook) and `--inject drop-stdout` (the host is handed nothing of the
build's answer) must turn `turn/crw` and `compaction/crw` red; a host whose hooks do nothing, or are not heard, cannot pass.

## What it does not show

Every run lists these as `notVerified` (a run that includes the real-host cells takes off the first three, each only when the
real-host cells that cover it ran and passed: the turn needs `turn/crw`, hook trust `turn/crw` and `untrusted/crw`, the
compaction `compaction/crw`; a cell that `--only` left out, failed, or could not be driven is listed with why, and a filter
that matches no cell claims no turn in the scope): the real Codex binary starting the declared hooks from a turn, hook trust, context recovery after a real
compaction; pause, cancel, permission refusal, forced exit and restart of the host and a stall (a host session driven through
the App Server; `codex exec` runs with approval never); which skills a real model selects on a native spawn; real-model
behaviour of the injected directives (the stub's script never reads them); the CRW-392 switch as `crw install switch` turns
it (the harness writes the file itself); a normal installation; and the completion Stop's effect (its guard daemon is not
started).
