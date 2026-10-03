# ACR a3e438e: provenance

The independent code review adapts parts of agentic-code-reviewer ("ACR"). This file records which upstream revision that is, which Go files were adapted from which upstream files, and how each was compared. The notices the port owes are in [NOTICE](../../NOTICE); the license text is in [LICENSE](LICENSE).

## Upstream

| | |
| --- | --- |
| Repository | <https://github.com/richhaase/agentic-code-reviewer> |
| Commit | `a3e438e2bd1f0824c1eab88db738aa3c82c69e99` |
| License | Apache License 2.0. [LICENSE](LICENSE) is byte-identical to the upstream `LICENSE` at that commit (sha256 `c71d239df91726fc519c6eb72d318ec65820627232b2f796219e87dcf35d0ab4`) |

## Adapted files

Each section below belongs to one package and lists, for every adapted Go file, the upstream path, what was kept, what was changed, and how the two were compared. A package's section is added by the change that adapts its files.

### internal/review

Three files carry the header; the others (severity.go, confidence.go, location.go, validate.go, doc.go, schema_v1.json and the tests) were written for CRW and take nothing from ACR.

| Go file | Upstream | Kept | Changed |
| --- | --- | --- | --- |
| filter.go | internal/agent/nonfinding.go, internal/filter/filter.go | The eight non-finding phrases and the case-insensitive substring test, as `IsNonFindingText`. A filter of regular expressions compiled once and refused when one is invalid, as `NoiseFilter`. | ACR matched user-configured exclude patterns against the text of a finding; `NoiseFilter` matches the finding's file path against built-in lock-file, vendored, minified and generated-file patterns plus optional extras. `Rules.Apply`, the title-first non-finding test, location validation and the threshold are new. |
| finding.go | internal/domain/finding.go | A finding names the reviewers that found it (`Sources`, here `Reviewers`) and their count (`ReviewerCount`, here `Support`); filtered findings carry a reason (`Disposition` kinds FilteredFP and FilteredExclude, here `DropReason`). | ACR's finding is free text. Here it has a location, a grade P0 to P3 with a security flag, a verification verdict and the reviewer's own severity text; nine drop reasons replace the disposition kinds. No code is copied. |
| artifact.go | internal/domain/review.go, internal/domain/result.go | Review status (`ReviewStatus` completed, failed, interrupted) and reviewer counts (`ReviewStats` TotalReviewers, FailedReviewers, TimedOutReviewers, AuthFailedReviewers). | Status becomes complete, partial or unavailable with a required reason; the counts become `run`, `failed`, `timedOut` and `authFailed`; the rest (head, patch id, diff statistics, model, findings, drops, times) is the new schema v1. |

How the files were compared. `<ACR>` is a checkout of the upstream repository at the commit above and `<repo>` this checkout; both commands read only.

```sh
git -C <ACR> rev-parse HEAD                      # a3e438e2bd1f0824c1eab88db738aa3c82c69e99
phrases() { awk '/nonFindingPhrases = /,/^}/' "$1" | grep -o '"[^"]*"'; }
diff <(phrases <ACR>/internal/agent/nonfinding.go) <(phrases <repo>/internal/review/filter.go) && echo same-phrases
grep -n 'strings.Contains(lower' <ACR>/internal/agent/nonfinding.go <repo>/internal/review/filter.go
grep -n 'regexp.Compile' <ACR>/internal/filter/filter.go <repo>/internal/review/filter.go
grep -n 'Sources \|ReviewerCount ' <ACR>/internal/domain/finding.go
grep -n 'TotalReviewers\|FailedReviewers\|TimedOutReviewers\|AuthFailedReviewers\|ReviewStatus = ' <ACR>/internal/domain/result.go <ACR>/internal/domain/review.go
git -C <ACR> show a3e438e2bd1f0824c1eab88db738aa3c82c69e99:LICENSE | cmp - <repo>/docs/port-acr/LICENSE
```

Measured on 2026-10-03: the eight phrases are identical in both files, both files test with `strings.Contains(lower, phrase)` and compile their patterns with `regexp.Compile`, the identifiers named above exist at those lines, and `docs/port-acr/LICENSE` is byte-identical to the upstream `LICENSE`. The rest was compared by reading: the comparison shows which names and values were taken, not that behavior is equal, because the Go code around them is new.
