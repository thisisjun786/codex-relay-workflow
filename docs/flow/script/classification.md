
## crw.classify

`crw.classify(result)` takes an executor `CallResult` or the handle of one of this run's results and maps it to one state: `ready` (completed, `ready_for_review`), `research`
(`needs_research`), `scope` (`blocked_scope`), `self_failed` (`failed` or `budget_exhausted`), `null_model`, `host_held`,
`null_unknown`, `refused` or `superseded`. Each classification reads only its own result, so failures mixed on one path or
across parallel paths are attributed to the exact call. A result of another role family than the executor, and a completed
result whose output is not a complete `RECEIPT` (every field of `RECEIPT` in `crw:lib/schemas`, with the types it lists),
is a `TypeError`; so are a bare object, a copy of a `CallResult`, `null`
and a string that is no handle of a result of this run. Like every `crw.*` call, `crw.classify` takes a position of its path
(journaled as an `Op: crw.classify` row), so calling it while another call or join of the path has not settled throws
`CrwPathConcurrencyError` and ends the run `script_error`.
