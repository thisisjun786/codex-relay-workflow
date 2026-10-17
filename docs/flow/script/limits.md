
## Limits

A step is one uninterrupted stretch of script execution: from the run's start, or from the engine delivering a result, until
the script awaits again or ends. A step that runs longer than the step limit (10 s) without awaiting is interrupted, and so
is a step during which the Go heap grows by more than the heap limit (512 MiB, sampled; the figure covers the whole process,
so it is a ceiling against runaway allocation and not an exact account). The run ends with status `script_error` and error
kind `step_limit` or `heap_limit`. The call stack depth is bounded as well. Rendering a thrown value and serialising the
returned one run the script's own `toString`, `toJSON` and getters, so they run under the same limits; one that throws ends
the run with error kind `js_error`.

The run also ends as soon as the main script's promise is rejected (not after the work still queued has run: a
continuation of another path that has not started does not start) and as soon as the context of `crw flow run` is
cancelled (error kind `cancelled`). The value or error of a `workflow()` child is rendered in the child's VM under the
child's own step and heap limits, also when the child had settled at once. A child that awaits something nothing will
settle does not keep the run waiting: it ends `unsettled`.

A run that ends with an error (a limit, `path_concurrency`, an engine error) stops at once, in a `workflow()` child as in the
main script: no completion resumes the script, nothing it still awaits keeps the run waiting, and every call in flight is
cancelled and writes its terminal row (`superseded`) before the run's own last row. A script that catches the error and
goes on may finish the statements it is running, but the first `agent()`, `agent.call()`, `crw.*`, join or `workflow()`
it opens after that stops it for good: no position, journal row or host request follows the error.
