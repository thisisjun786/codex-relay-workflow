# Recall operator notes

## Parent operator work

Diagnosing the host's memory extraction pipeline is operator work, not recall: it reads and can change the live
memory database, and no task needs it to answer a past-work question. The parent (repository session, relay role `supervisor`,
[the level table](../../crw-canon/references/hierarchy.md#the-levels)) runs it; any other level
reports the symptom to the parent and does not run these commands.

```
crw recall memory status [--json] [--home PATH]
crw recall memory requeue [--apply] [--include-context-window] [--kind K] [--limit N]
                   [--retries N] [--json] [--home PATH]
```

`crw recall memory status` reports the HOST extraction pipeline, which is a different system
from the recall index of `crw-recall`: per-kind job counts, jobs that exhausted their retries
bucketed by cause, and the newest success. It names the store it read, because the host
supports more than one memories schema, and reports `unsupported` rather than guessing.

Every snapshot carries `observationSource: "jobs-db"`, identifying the collector even
when the store is missing or unreadable. `effectiveExtractionRoute` and
`startupGuardDecision` are always `"unknown"`: job history, including a recent success
or a quota-classified error, cannot establish the current extraction route or startup
guard decision. Text output states this limit too; healthy SessionStart notices stay
silent. Provider-bridge status is not extraction-route evidence, and setting the
threshold to zero is not a universal bypass: a present `rate_limit_reached_type` still
blocks startup, while an absent flag permits a numeric 100% window at threshold zero.

`crw recall memory requeue` returns dead-lettered jobs to that pipeline's retry queue. It is a
**dry run unless you pass `--apply`**, because it writes to the live memory database.
By default it selects only transient causes; `--include-context-window` is opt-in and
usually a bad idea, since an input that did not fit the context window will not fit on a
retry either — it just spends quota failing again.
