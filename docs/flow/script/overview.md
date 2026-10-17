# CRW workflow scripts

Here the parent (relay role `parent`) and the child (relay role `child`) are relay roles, read through [the level table](../../../plugins/crw/skills/crw-canon/references/hierarchy.md#the-levels).

A CRW workflow script is a JavaScript file that `crw flow run` executes. It orchestrates agent calls with ordinary
control flow (loops, conditionals, `await`) and the engine records every call it makes. This page is the reference for
the script format, the loader, the globals, call identity, the call result and the limits. It describes what
`internal/flow/script` (loader and runtime) and `internal/flow/engine` (calls, journal, paths) do.

```text
crw flow run --backend fake [--args JSON | --args-file FILE] [--pool N] [--token-budget N] [--journal] [--summary-document D] <script.js>
crw flow run --script JS [--plan-only=false --backend fake] [--args JSON] [--journal]
crw flow roles [--json]
crw flow recover [--run RUN] CALL (--adopt THREAD|TURN | --absent)
crw flow resume [--state DIR] [--socket PATH] [--repo DIR] [--args JSON | --args-file FILE] RUN SCRIPT
crw flow lint [--args JSON | --args-file FILE] [--no-dry-run] [--json] <script.js>...
crw flow audit (--run RUN | --acceptance ID) [--milestone M] [--state DIR] [--socket PATH] [--json] [--no-record]
```

The commands that reach up from a run - `report-up`, `reports`, `report-ack`, `report-delivered` - and the summary document
`--summary-document` names are in [reporting.md](../reporting.md).

`--backend fake` runs the script against the in-process fake backend: every turn answers immediately with a value that
satisfies the call's schema, including its numeric bounds, `multipleOf` and `pattern` (or a short sentence for a call without one). It is the only backend the
command selects; the App Server backend is the package `internal/flow/backend/agent` (see [The App Server backend](app-server.md#the-app-server-backend)),
which the engine drives through the same `Backend` interface and which no command selects yet. The command prints the run's result as JSON and exits 0 when the script
completed, 1 when the script failed (`status: script_error`) or did not load, and 2 for a command line it cannot use.
The journal sequence numbers of a call (`meta.startSeq`, `meta.endSeq`) depend on the order the host finished concurrent
calls, so under `crw flow run` neither the `CallResult` the script holds nor the printed result carries them (a value,
a log line or an error the script builds from a `CallResult` cannot hold them either) and two runs of one script print the
same bytes; `--journal` adds the journal rows, with those numbers. `testdata/flow/hello.js` is a small script that runs this way.

`--script JS` runs an ad-hoc script given as text instead of a file. Its default is `--plan-only`: the script runs on
the fake backend only, with no backend named, and the result says `"planOnly": true`. A script that is to run on a
backend names it and turns plan-only off (`--plan-only=false --backend fake`); `--plan-only` on a script file is a usage
error. An ad-hoc script has no workflows directory: it imports `crw:lib/<name>` only and nests no workflow.

`crw flow run` (like every `crw flow` command that resolves calls) reads its role table once, when it starts, from
the helper role store `CRW_HOME/subagents.json`: the pairs of `architect`, `executor`, `executor-t1`, `reviewer`,
`explorer` and `critic`, with the code's alias table (see
[role-execution-policy.md](../../role-execution-policy.md#the-agent-roles-of-a-flow-run)). The execution policy's
`agent-roles` and `agent-aliases` sections are not read; a file that still declares them is a warning, and one whose
sections do not parse still loads, with the sections ignored and the loader's reason as another warning. The run record
keeps the store's path and the canonical digest of the six pairs, and the warnings of the table (a flow-only key at
its compiled default, an undecided role, same-thread review when observed, and the retired role key
of the `crw.reviewerFinal` op when the store or the policy file still holds it: the key is ignored, takes no pair and is not in the
digest). A test with no store runs on the fake backend's placeholder table. `crw flow roles` prints the table, the source, the digest and the warnings.
