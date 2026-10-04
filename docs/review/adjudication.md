# Parent judgments and review comparisons

The parent keeps independent review, Devin and Codex findings in one private `adjudication.jsonl`, then records its own verdict for each finding. `internal/review/adjudication` imports saved inputs, appends validated records and computes a JSON report. This is a file convention with a Go API: no new command, model execution, network access or merge gate. It reuses the existing artifact validator without changing the artifact or execution ledger.

## Append observations, then parent judgments

Each line is one `Record`: `schema` is `crw-review-adjudication/1`, `id` is unique, and exactly one of `run` or `judgment` is present. UTF-8 JSON uses a final newline. The operator owns the destination directory; the ledger is private evidence, never repository content.

| Payload | Fields |
| --- | --- |
| `run` | `id`, `pr` (`repository`, `number`, reviewed `head`), `source` (`crw_review`, `devin`, `codex`), requested `model`, `artifactSha256`, `status`, `reason`, `reviewers`, `calls`, `findings` |
| Finding | `id`, relative `file`, positive `line`, `title`, reported `grade` P0–P3, `security` |
| Call | original schema-v1 `record`, independent `elapsedKnown` and `tokensKnown` flags |
| `judgment` | `runId`, `findingId`, `parent`, explicit `problem`, `verdict` (`correct`, `false_positive`, `minor`), parent `grade` and `security`, `codeChanged`, `changeCommit`, `note` |

`ReadArtifact(bytes, pr, sha256)` verifies the supplied hash and head with `review.ParseArtifact`, keeps the model, artifact status/reason, reviewer count and every call, and assigns indexed finding IDs. Its run ID includes repository, PR number, head and artifact hash. The same artifact cannot be imported twice for the same PR even under a different run ID. Dropped findings are outside this ledger's scoring population.

`ReadExport(bytes, pr)` accepts saved, normalized `Run` JSON for Devin or Codex and verifies the expected PR identity. The parent normalizes its already saved review data into the fields above, using stable comment/finding IDs and `model: "unknown"` when no model is known. No GitHub fetch or interpretation of badge prose occurs here. The small reader rejects unknown fields, invalid identities, grades, states and negative accounting. Reviewer text stays data; neither reader executes or follows it.

Saved JSON must preserve the canonical Go JSON field shape: required fields cannot be omitted or null, except `reviewers`, `calls` and `codeChanged`, whose null value explicitly means unknown. Zero-valued optional fields are omitted as `encoding/json` writes them. Readers compare the decoded JSON with that typed shape so null cannot become measured zero or a parent's false security flag. Replay rejects overflowing ledger-wide elapsed, token or reviewer sums before reporting any subset.

Run states are `complete`, `partial`, `invalid`, `unavailable`. Every non-complete run needs a reason; invalid/unavailable runs have no findings. A partial run can retain invalid/unavailable calls and usable findings. Missing runs are visible in the report's coverage, rather than invented as observations.

The parent assigns `problem` to correlate different claims about the same issue on the same PR/head. This correlation never assigns correctness. Canonical grade/security are supplied by the parent; two effective correct judgments on one problem must agree. `codeChanged: null` means unknown, `false` means unchanged, and `true` requires a different valid `changeCommit`. The parent verifies that commit followed the finding and explains the evidence in `note`; the package checks its shape, not Git ancestry. `parent` is attribution, not authentication.

A repository-scoped Go caller wraps imports and judgments into records (handle each returned error):

```go
run, err := adjudication.ReadArtifact(artifactBytes, pr, artifactHash)
if err != nil { return err }
if err := adjudication.Append(ledgerPath, adjudication.Record{
    Schema: adjudication.Schema, ID: "import-"+run.ID, Run: &run,
}); err != nil { return err }
judgment := adjudication.Judgment{
    RunID: run.ID, FindingID: run.Findings[0].ID, Parent: "parent",
    Problem: "bounds-check", Verdict: adjudication.Correct,
    Grade: review.P1, Security: false, CodeChanged: nil, Note: "parent code evidence",
}
return adjudication.Append(ledgerPath, adjudication.Record{
    Schema: adjudication.Schema, ID: "judgment-1", Judgment: &judgment,
})
```

Only create that judgment when the imported run has a finding. For saved external reviews, use `ReadExport` and the same record wrapper. The caller imports this repository's `internal/review/adjudication` and `internal/review` packages; there is no installed CLI consumer.

`Append(path, records...)` appends a validated batch under a nonblocking exclusive lock. A new judgment with a fresh record ID replaces the effective judgment for its finding in file order and leaves history intact. To correct a shared problem's canonical severity, append all affected judgments together: final-state validation happens before any write. Duplicate event/run IDs, unknown finding references, contradictory final severity and malformed records refuse the append. A contended writer returns an error immediately.

Reads require a stable saved snapshot. The package follows no ledger symlink and never trims or repairs history. A torn tail or corrupt line stops `Read` and `Append`; the operator preserves the original and reconciles the damaged file separately. Append batches serialize cooperating writers, but do not promise transactional filesystem writes across a crash. Limits are 1 MiB per JSON object and 64 MiB per ledger.

## Compute every row on the same explicit PR cohort

Call `Read(reader)` and `Report(records, prs, groups)`, then serialize the returned `Summary` with `encoding/json`. Both selections must be nonempty and duplicate-free. `prs` includes repository, number and reviewed head; `groups` contains source/model pairs. Rows and PRs are sorted, the input slices are not mutated, and no timestamp is generated. Reports do not silently intersect available runs: every row retains the complete selected PR cohort and lists its `missingPRs`.

| Metric | Definition |
| --- | --- |
| Precision | Parent-correct findings / (correct + false positive + minor). Pending findings are shown separately. |
| Unique correct | Distinct parent-correct problems caught by exactly one selected source/model group. Duplicate findings within that group count once. |
| P0, P1, security catch | That group's distinct parent-correct problems of the category / the union of parent-correct problems of that category across selected groups and PRs. A false-positive/minor claim matching another reviewer's correct problem gets no credit. |
| Change corroboration | Correct findings with changed, unchanged or unknown later code change; a code change never promotes a verdict. |
| Runs/reviewers | Stored run count and reviewer totals, with `knownReviewerRuns` distinguishing unknown external counts. |
| Calls/time/tokens | All stored calls, `knownCallRuns`, independent `knownElapsedCalls`/`knownTokenCalls`, known elapsed milliseconds and known-token totals; `recordedTokens` separately sums every original call's counters. |
| Failures | `invalidRuns`, `unavailableRuns`, `partialRuns`; invalid/unavailable count any matching call or run status and can overlap. Failure calls contribute their recorded economics. |
| Model evidence | Group by requested run model; sorted `servedModels` preserves call labels (empty means unknown), and `modelMismatchRuns` exposes empty/different served labels. |

Every ratio carries numerator, denominator and a value that is `null` when the denominator is zero. No reviewer is another reviewer's ground truth: only effective parent judgments determine scores, and later code changes provide separate corroboration. Catch rate is limited to observed, parent-correct problems; it does not measure defects nobody found. Selecting another cohort or group set changes its denominator and unique counts, so compare models in one report with identical explicit selections.

Schema-v1 artifacts always carry scalar token counters and cannot distinguish absent usage from measured zero. Imports preserve those counters in `recordedTokens`, conservatively set `tokensKnown: false`, and mark measured call duration known. Saved exports may attest known tokens, including measured zero. A report with unknown accounting is not a complete usage measurement or a quality verdict.

The scripted tests separately cover saved imports into combined report records and append → replay → report, coordinated corrections followed by another append, fixed metric numbers, false-positive correlation, model comparison with failed/missing runs, unknown versus measured-zero usage, null/missing fields, aggregate overflow and corrupt/contended inputs. They use temporary files and synthetic data only:

```sh
go test -count=1 ./internal/review/adjudication
```
