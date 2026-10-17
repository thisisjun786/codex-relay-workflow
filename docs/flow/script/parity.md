
## Parity corpus

`testdata/flow/parity/<name>/` holds Claude workflow scripts as `claude.js` (byte for byte) and their CRW ports as
`crw.js`, with a `golden.json` of the port's call graph, return value and, per call, the resolved role key, the sandbox,
`requestedPair` and `configuredPair`. A port differs from the original only by keys added to the options object of
`agent()` calls (`isolation: 'worktree'`, `agentType: 'explorer'`, `stepDecision`) and one `crw.tierStep` line before a
`model: 'sonnet'` call; the corpus README gives the reason for every such line and for every line that names
`ocx-gpt-6-1-sol`. A script that reads `args` gets them from `args.json` beside it, and the `real-*` directories hold
originals with a `source.json` naming the file they were copied from.
