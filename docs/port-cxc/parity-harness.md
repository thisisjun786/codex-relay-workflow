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
is not an installation pass: installation parity belongs to CRW-201 and CRW-204.

## Cells

| Cell | What it shows | Fails when |
| --- | --- | --- |
| registration | the plugin root's manifest lists hook files that declare each leg once, under its event, with its matcher, timeout and `(crw)` status message (K1 after the R24 rename) | a leg is missing, declared twice, under another event name, with a drifted matcher, timeout or status message, async, or a registration starts no leg of the table; the manifest names another plugin |
| firing and effect | the command the root declares for the leg, run as the host runs it (`/bin/sh -c`, `PLUGIN_ROOT`, an isolated `CODEX_HOME`), answers each corpus payload (`contract/fixtures/cxc/hook__<leg>__*`) with the expectation, observed tree and calls of `TestDomain/cxc`, status files and claims included | any claimed fixture differs; a leg has no declared command for its event |
| receipts | every firing leaves a receipt naming this run, the plugin root digest, the sha256 of the crw build, the leg, its event, and the payload's session, turn, tool call, tool and agent and the skills its answer names; the verifier compares them with what the fixtures say | a receipt is of another run, plugin root or build, names another event, leg, agent or skill, is missing, doubled or unexplained |
| latency | p50 and p95 of the Go command against the CXC v0.2.40 command (`node .../dist/cli.js`) on the same payload, N fresh case roots per side, the two sides alternating; a leg that fails is measured again (`--attempts`, five in all by default) because a shared host's load puts outliers in a p95, and the report records the attempts a leg took and the load average; a leg that still fails while the host's load average is above its CPU count is inconclusive (listed as not verified; `--strict` makes it a failure) | Go p95 is above the TS p95, or above half of the declared timeout |

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
crw-dev parity registration --plugin /path/to/plugin-root
crw-dev parity fire --crw "$TMPDIR/crw" --plugin /path/to/plugin-root --only 'hook__stop-'
crw-dev parity latency --crw "$TMPDIR/crw" --plugin /path/to/plugin-root --oracle ... --runs 30 --legs 'session-start'
crw-dev parity plugin-root --crw "$TMPDIR/crw" --out /path/to/new-root/crw
```

The plugin root and the oracle path are arguments; nothing is hard-coded. Without `--plugin`, `all` generates a
root from K1 (`plugin-root`): the files of `plugins/crw` with a manifest and hook files that declare the 34 legs,
each command starting the crw under test as `crw hook <event> --leg <leg>`. That root exists so the harness does not
wait for the activation PR (CRW-392); pointing `--plugin` at the real root checks the real declarations.

## Evidence and cleanup

The report (`--json`) records the build (path and sha256), the plugin root (path and digest of its manifest and hook
files), the oracle revision of K1, the run id every receipt carries, the owner, process and run directory of the test,
and how it was cleaned up: every case root is removed as its case ends, the run directory (a generated plugin root and the
case roots, under `--scratch` or `$TMPDIR`) when the run ends, and the number of entries left behind must be zero or the run
fails. No shared database, live session, Codex home or installed runtime is used. The report's `key` is a digest of the build,
the plugin root, the corpus files the cells read and the options; `--reuse FILE` stands a passing report of the same key in for
a run, so evidence of the same artifact, revision and criteria is not produced twice.

## Faults

`fire --inject <fault>` injects a fault and succeeds only when the run turns red for it. The faults are the ways
a cell could pass without the hook having done its work: `wrong-event`, `missing-hook`, `noop`, `kill` (the
declaration), `drop-stdout` (a lost answer) and `reverse-steps` (payloads in the opposite order) (the transport),
and `stale-receipt`, `other-plugin`, `other-build`, `receipt-event`, `receipt-agent`, `receipt-skill`,
`lost-receipt`, `double-receipt` (the record). `internal/dev/laneparity` tests every one against a real build.

## What it does not show

Real-host cells with a stub model provider are out of this issue's scope (the 10-10 coordinator decision) and
are reported as `notVerified` on every run: the real Codex binary firing the hook from a turn (trust, thread,
turn and socket receipts), pause, cancel, permission refusal, forced exit and restart of the host, context
recovery after a real compaction, the native spawn surface and a spawned agent's skill, real-model behaviour,
the CRW-392 switch file, and a normal installation.
