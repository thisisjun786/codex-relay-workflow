# CXC differential fuzzing

The CXC v0.2.40 port has one way to prove the Go code answers what the oracle answers: feed
both the same input and compare. This page is the harness that does it, `internal/dev/cxcfuzz`.
It is built only with `-tags dev`, it ships in no release archive, and it never runs in a hook
path. The eight real subjects of milestone M7.5 arrive in their own issues; this one carries the
harness and one self-test target, `echo`.

## Running

```sh
go run -tags dev ./cmd/crw-dev fuzz echo --seconds 30
go run -tags dev ./cmd/crw-dev fuzz echo --cases 500 --seed 7 --workers 4 --out ./fuzz-out
```

`--seconds` and `--cases` bound a campaign; one of them is required. `--skip N` draws the first N cases of
the seed and judges none of them, so a campaign that was stopped carries on at the case after the last
one it judged (see "Stopping and carrying on"). `--workers` defaults to 4 and is
both the number of oracle workers and the number of cases evaluated at a time. `--seed` defaults to the clock, and the seed a campaign used is written to `summary.json` so the run can be
repeated. `--out` defaults under the harness's own output root (`DefaultOutRoot`) at
`<target>/<UTC time>`. Without `node` on `PATH` the command stops with exit 2 and one line, before
any fuzzing. It exits 1 when the campaign found a divergence and 0 when every case agreed.

## What it does

A *target* is a registry entry: a name, a generator, the Go function under test, the oracle worker
that answers the same input, and a comparison. The registry is a function in `registry.go`, so the
package does no work at program start; a later issue adds one entry line.

Inputs and answers travel as JSON text. The Go side reads them with `pyjson.Loads` and the
`Surrogates` option, so a lone surrogate escape stays the three WTF-8 bytes a Go string holds it
in; before comparison both answers are rewritten in one canonical form (object keys sorted, a lone
surrogate as its `\udXXX` escape) and compared as bytes. Two answers that agree in that form are
the same answer.

Each target's oracle side runs as long-lived NDJSON workers: one request per line on stdin, one
reply per line on stdout, `{"id":n,"input":...,"root":"<case root>"}` answered by
`{"id":n,"output":...}` or `{"id":n,"error":{...}}`. A request that outlives five seconds is a
timeout case: the worker is killed, another is started for the next request, and the input is
counted as a timeout rather than a divergence. A worker that dies is replaced the same way. The
worker program is the target's shim, which imports its dist modules under `ORACLE_ROOT` (the
target's `Oracle.Root`, `DefaultOracleRoot` by default) the way the `record-oracle.mjs` recorders
do.

Starting a worker is bounded separately from a request. The start-up deadline
(`DefaultStartupTimeout`, one minute; `Config.StartupTimeout` overrides it) is taken before the
worker process is created, so it covers the whole start: process creation (fork/exec) and the
handshake that proves the worker has booted. A freshly started worker has not booted its
interpreter or run its shim's top-level imports yet, so the pool first sends it a handshake -- one
normal request with a null input and an empty root -- and waits for a reply under what is left of
the start-up deadline. Only a worker that has answered the handshake takes a case, so the
five-second per-case deadline covers that case's own exchange and never a worker's boot. A worker
that does not answer the handshake in time is killed, replaced, and the case that needed it is
counted as a timeout, exactly like a request that times out; the campaign still fails. The
handshake's reply is discarded, so it can never change a verdict -- a divergence is still only ever
a `Differ`, `Miss` or `Extra` from a case's own exchange. Because the handshake is an ordinary
request, every shim must answer a null input inertly (the three committed shims do: `echo` returns
it, `shellwrite` answers `[]`, `memorygate` answers `""`).

An input may carry an `fs` array of `{path, kind, target, mode, content}` entries. Each case gets
two fresh roots, one per side, and builds the same tree in both. A path that leaves its root, and a
symlink whose target leaves it, is refused before anything is written, and that case is skipped.
Before comparison each side's own root is rewritten as `${ROOT}`, so the same relative behaviour in
two different roots compares equal. The roots are removed after the case. Content is written as
content: a generated shell command is never executed.

A difference is shrunk while the same verdict survives: container members go one at a time, then
each member is reduced in place, up to 500 candidate evaluations. The result is written as
`<out>/divergences/<kind>-<sha12>.json` (input, both answers, verdict, seed, dev sha) once per
input hash, and `<out>/summary.json` carries the target, seed, dev sha, elapsed time, case count,
per-kind counts, cases per second, timeout count and the count of refused scenarios.

## Throughput

A campaign is bounded by how fast one case is asked of the oracle and compared, so the harness keeps
each cost small and puts the cases side by side.

**Cases side by side.** The campaign draws its inputs in order from the seed, judges up to `--workers`
of them at a time, each on its own goroutine and its own oracle worker, and applies the results in case
order. The counts, the divergence files and the failure records of a seed are therefore the same at
one worker as at four (`TestCampaignGivesTheSameResultAtOneWorkerAndFour`). A difference is shrunk on
the goroutine that found it. A removal failure or an unknown verdict ends the campaign: the cases
already started finish, their results are dropped, and the pool closes with no worker in a case.

**The pyjson oracle.** `testdata/pyjson/shim.mjs` answers a request with the answer of a fresh
`python3 -m json.tool --json-lines` process fed that request's text; three paths give it, cheapest
first, and each gives the same exit status, stdout and stderr
(`TestPyjsonShimMatchesAFreshJsonToolForEveryRequest` compares the shim with a fresh process for the
committed cases, the boundary documents under six option sets and a generated sample, 1,432 requests,
invalid documents, lone surrogate escapes, nesting around Python's recursion limit and every
`--compact`, `--indent`, `--sort-keys` and `--no-ensure-ascii` combination):

1. a bounded memory of earlier answers, keyed by the option flags and the text (json.tool is a pure
   function of both); the recursion-limit answers are two megabytes each and are kept as reply text,
   compressed when large;
2. a reused process, one per option set: the request text is written, its last line ended, followed by
   a sentinel document no request can contain; Python prints each document's value as it reads it, so
   the sentinel's echo ends the answer. A text goes to a reused process only when every line parses in
   the host JSON parser and the text opens fewer than 256 containers; only an answer with nothing on
   stderr is taken from one. json.tool ends its process on a refusal, and a refusal names a line and
   column that the line ending this path adds could move, so the refused request is asked again fresh;
3. a fresh process per request: every refusal, every text the host parser rejects (`NaN`, `Infinity`),
   every deep nesting and every answer past the buffer.

No `.py` file and no Python source text is committed; the shim builds an argv and pipes text in. A
reused process that is killed (a signal) ends the shim without a reply, which the pool reads as a dead
worker; one that stops answering is a request that outlives the five-second deadline, and the pool
kills the worker and its whole process group, so no stopped `python3` outlives the campaign
(`TestPyjsonShimReportsAKilledReusedProcessAsADeadWorker`,
`TestPyjsonShimReportsAHungReusedProcessAsATimeoutAndLeavesNothingRunning`,
`TestCampaignCountsAReusedProcessThatDiesMidCampaign`). Both are failing cases of the campaign.

**Per-case cost.** `canonical` writes a long ASCII string (4,096 bytes or more) in one pass over its
bytes, because the writer's pass over its characters cost more than 20 ms on a two-megabyte traceback
(`TestCanonicalWritesWhatThePyjsonWriterWrites`). `PrepareRoot` judges a case root once and makes
its homes as plain directories; `RemoveCaseRoot` tries `RemoveAll` before it makes every directory
listable. The case roots are made below `$TMPDIR`: on a tmpfs the state target runs several times
faster than on a disk, because the port's and the oracle's atomic write both sync the file, so a
campaign sets `TMPDIR` to a tmpfs directory of its own.

**What a rate was measured at.** Seed 711, four workers, the CXC v0.2.40 tree extracted from git tag
`v0.2.40` and named by `CRW_CXC_ORACLE_ROOT`, on a host also running other work:

| target | one case at a time, one python3 per request | cases side by side, reused processes |
| --- | --- | --- |
| pyjson | 6.6 cases/s (CRW-711: 11,853 cases in 1,800 s) | 72 cases/s |
| state | 5.1 cases/s (CRW-711: 9,087 cases in 1,800 s) | 8.8 cases/s with `TMPDIR` on a disk, 45 cases/s on a tmpfs |

The state target spends its time in difference shrinking: the known open difference of the lone
surrogate session file (about one case in seven) is shrunk through up to 500 candidates, each a fresh
case root on both sides, so 98 % of the evaluations of a state campaign are shrink candidates. The
wall time of one case at a time, from a thirty-second seed-711 state campaign with the evaluation
timed in five parts:

| part | `TMPDIR` on a disk | `TMPDIR` on a tmpfs |
| --- | --- | --- |
| root preparation, scenario build and input reading | 15 % | 33 % |
| the Go side (read, rewrite, read back) | 47 % | 5 % |
| the oracle worker | 15 % | 25 % |
| the comparison | 2 % | 5 % |
| removing the roots and the rest | 21 % | 32 % |

On a disk the sync of the Go side's atomic write dominates; on a tmpfs the cost that stays is the
home guard, which resolves every path of a root again for each directory made (36 % of the CPU
samples). `PrepareRoot` and `RemoveCaseRoot` are the two places it was spent without need.

## Stopping and carrying on

An interrupt or a terminate signal ends the draw of new cases and nothing else: the cases already drawn
are judged, counted and written as usual, `summary.json` is written with `"stopped": true`, and the
last line on stdout names the `--skip` that carries the same seed on. A campaign of two hours can
therefore run as segments, each a campaign of its own with its own output directory: the next segment
starts with `--seed S --skip K`, where K is the sum of the cases the earlier segments judged and
skipped, and a failure record carries its case's number in the whole run
(`TestCampaignSegmentsGiveTheWholeRun`, `TestCampaignStopLetsTheDrawnCasesFinishAndAResumeFinishesTheRun`).
A host load gate uses this: a campaign is stopped when the host is over its limit and carried on when
the load has fallen, and the campaign time is the sum of its segments' elapsed time.

## Campaign records

A campaign is recorded with its `summary.json`, which holds the target, seed, dev sha, elapsed time,
case count, counts per verdict kind, timeouts, dead workers, cases with no answer and the failure
records written under `failures/`. A campaign counts as failed when any difference, timeout, dead
worker, missing answer, harness error, refused scenario or unrefused unreadable command was found. The
record of a campaign names the target, the seed, the dev sha the binary was built from, the elapsed
time (the sum of its segments), the case count, the difference count per kind, the timeout count and
the dead-worker count; a campaign that ran as segments also names the segments. A
campaign that found a difference shrinks it and pins it with `--adopt` (tag `open`, record the
issue), or the difference becomes a follow-up issue.

## Pinning a difference

```sh
go run -tags dev ./cmd/crw-dev fuzz echo --adopt <divergence file> --tag open --record CRW-000 --name echo-case
```

`--adopt` appends one case to `internal/dev/cxcfuzz/testdata/<target>/cases.json`, whose entries
are `{name, input, oracle, go, tag, record}`. `TestCases` replays every case through the Go side
only, with no Node: `identical` must agree with the oracle, `intentionally-changed` must agree with
the Go answer its `record` names, and `open` must still produce the Go output it was pinned with,
so a change in the Go output fails until the case is pinned again.

## The end condition

Milestone M7.5 ("pre-use verification: differential fuzzing and package audit") is done when every
ported surface has a target whose campaign agrees, or whose differences are pinned as cases. A
campaign runs two hours with four workers at a fixed seed, and each target has a minimum case count:
1,000,000 for shellwrite, spawn, pyjson and doctor, and 100,000 for worktreedel, memorygate, state and
goalplan.

At the rates above the minimums take: pyjson 1,000,000 cases in about 3.8 hours, state
100,000 cases in about 37 minutes on a tmpfs (3.2 hours on a disk), and the
other targets inside the two hours. Only pyjson does not reach its minimum in two hours.

Proposal (not applied; it changes the end condition set in CRW-711 and waits for Jun): for pyjson,
take 500,000 cases as the minimum, which is what two hours at four workers reaches, or keep
1,000,000 and let the pyjson campaign run until it has them. Until Jun decides, the end condition
stays as written above and a pyjson campaign that stops at two hours is recorded with its case count
and the shortfall.
