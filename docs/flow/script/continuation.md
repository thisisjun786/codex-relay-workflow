
## Continuing a call

`agent.call(prompt, {resume: handle})` continues the thread of a call. `resume` takes a handle string or a `CallResult`; a
bare object, a copy of a `CallResult`, `null` and anything else is a `TypeError` (`crw.classify` is the same: there is no
implicit "last call"). The continuation inherits role, pair, node and `stepDecision` from the handle's journal record, adds one
turn to the same thread and gets a handle of its own. An explicit value that differs from the inherited one is refused:

- A handle from another run, another role family or another node, and a handle that names no call with a thread in this run,
  is `refused` with reason `foreign_handle`. The run of a handle is judged by the journal row of the call it names, not by the
  run id written in the handle's text.
- The same role family on another role, a `model` that is not the thread's model or an alias of it, an `effort` that is not
  the thread's effort, or another `stepDecision` is `refused` with reason `pair_refused`.

- A thread whose pair read back wrong when it started (its call was `refused` with `pair_refused`) stays refused: continuing
  it is `refused` with `pair_refused` too. A continuation carries the thread's read-back pair, not the role's.

A refused continuation sends nothing to the host.
