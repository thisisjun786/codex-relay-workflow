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

`--seconds` and `--cases` bound a campaign; one of them is required. `--workers` defaults to 4 and
`--seed` to the clock, and the seed a campaign used is written to `summary.json` so the run can be
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

Starting a worker is bounded separately from a request. A freshly started worker has not booted its
interpreter or run its shim's top-level imports yet, so the pool first sends it a handshake -- one
normal request with a null input and an empty root -- and waits for a reply under the start-up
deadline (`DefaultStartupTimeout`, one minute; `Config.StartupTimeout` overrides it). Only a worker
that has answered the handshake takes a case, so the five-second per-case deadline covers that
case's own exchange and never a worker's boot. A worker that does not answer the handshake in time
is killed, replaced, and the case that needed it is counted as a timeout, exactly like a request
that times out; the campaign still fails. The handshake's reply is discarded, so it can never
change a verdict -- a divergence is still only ever a `Differ`, `Miss` or `Extra` from a case's own
exchange. Because the handshake is an ordinary request, every shim must answer a null input
inertly (the three committed shims do: `echo` returns it, `shellwrite` answers `[]`, `memorygate`
answers `""`).

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
ported surface has a target whose campaign agrees, or whose differences are pinned as cases.
