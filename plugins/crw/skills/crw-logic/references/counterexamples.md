# Counterexample patterns

These are probe ideas, not universal invariants. Establish preconditions from the canonical contract before using them.

| Target | Candidate probe | Valid contrast or prerequisite |
|---|---|---|
| Nested limits or totals | Can a contained quantity exceed its containing budget? | Same units, population, accounting basis, and containing interval; independent limits/scopes may differ |
| Missing measurements | Does no observation become zero, success, or certainty? | Measured zero differs from absent observation |
| Additive accounting | Are retries, requests, physical attempts, or cache subsets counted twice? | Define the counted level; do not add overlapping representations |
| Order-insensitive logic | Does permuting equivalent inputs change the result? | Exempt order-dependent contracts and compare equivalent preconditions |
| Retry/duplicate delivery | Can one logical action cause a second effect? | Use contractual identity; equal text alone need not mean duplicate work |
| Cancellation/crash | Can completion be reported while durable state is partial? | Probe disposable state or an explicitly authorized environment |
| Versioned behavior | Do new-write rules make valid old records unreadable? | Separate historical parsing, new generation, and accepted migrations |
| Evaluation | Can implementation and scorer agree without outside truth? | Use `mandela`; reviewer agreement alone is not a reference answer |
| Conflicting documents | Do two currently binding rules require incompatible behavior? | Resolve scope and accepted superseding decisions before declaring a contradiction |

Example: a short-period estimate of 140 and a containing-period estimate of 100 is suspicious only after confirming they express the same constrained budget and scope. Different model pools with those values are not a counterexample. Unknown common scope is an evidence gap, not a reason to clamp the numbers.

For retry logic, test a repeated delivery of one logical request and a new legitimate request with similar content. A correction suppressing both may replace duplication with data loss.

If a Linear document says to retain all historical receipts while a later accepted decision limits only newly generated output, the statements need not conflict. Check version and write/read boundaries before proposing a universal limit.

Keep probes tied to the caller's target. Do not create a generic test framework or run every row for a small audit.
