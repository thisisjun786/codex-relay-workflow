
## Call identity: path and position

A call is identified by `(path, position)`. The root path is the script's top-level sequence (`root`). Each item of a
`parallel` or `pipeline` runs on a child path named by the join's position and the item's key, `root/3:0` for item 0 of the
join at position 3 (nested items extend it: `root/3:0/1:5`). The characters `%`, `/` and `:` in a key are percent-encoded
(`%25`, `%2F`, `%3A`) in the path, so two different sequences of positions and keys never name the same path, call id or
handle. A `workflow()` child runs on the path `<path>/<n>:workflow`.
`position` is the ordinal of the `agent()`, `agent.call()` or `crw.*` call on that path (a join counts as one position),
starting at 1. Completion order is never an identity source, and the call id (`c_` and twelve hex digits) is derived from the
run id, path and position.

**A path has at most one outstanding position.** Opening a call or a join on a path while an earlier call or join on that path
has not settled throws `CrwPathConcurrencyError` (it names both labels, as `e.openLabel` and `e.requestedLabel`)
synchronously, before any journal row or host request for the refused call, and the run ends with status `script_error` and
error kind `path_concurrency` even if the script catches the error. So every position is a function of the script and the
settled results. Concurrency comes only from constructs that open child paths:

```js
// stable: each item is its own path
await parallel(xs.map((x) => async () => { const a = await agent(...); return agent(f(a)) }))
// refused: two calls on one path
await Promise.all(xs.map((x) => agent(...)))
const p = agent.call(...); await agent.call(...)   // refused: p has not settled
```

The calls of a run share an agent pool (`--pool`, default 6): at most that many calls hold a thread at once, and the others wait.
