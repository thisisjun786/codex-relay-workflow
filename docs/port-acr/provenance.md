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

### internal/review/agy

Four files carry the header; the others (doc.go, envelope.go, classify.go, agylog.go, env.go, workdir.go, lock.go and the tests) were written for CRW and take nothing from ACR. ACR has no agy envelope or classification: its agy adapter sends `--print=-` and treats output as review text, and its parser extracts JSON out of prose, which is not what a call whose whole output must be one envelope needs.

| Go file | Upstream | Kept | Changed |
| --- | --- | --- | --- |
| agy.go | internal/agent/antigravity.go | The availability check only: `exec.LookPath` of the agy binary (ACR's `IsAvailable`). | ACR passes `--print=- --print-timeout <d>` and no model. Here the call is `--model <config> --output-format json [--json-schema <file>] --disable-slash-commands --log-file <file>` with the prompt on stdin and no print flags (agy 1.2.16 reads `-` as the whole prompt). `Run`, `Config`, `Result` and the classes are new; ACR's print-timeout formatting and grace are not taken. |
| exec.go | internal/agent/executor.go, internal/agent/cmd_reader.go, internal/agent/result.go | `cappedBuffer` (a capped writer that never stops the writer), the start in its own process group with `cmd.Cancel` ending that group, `-1` as the exit code of a process that did not exit by itself, and the exit code and stderr kept beside the output (`ExecutionResult`). | ACR streams stdout through a reader the caller closes and caps only stderr; here stdout and stderr are each read to the end under a cap and returned as one value, the prompt comes from bytes, the time limit is a context deadline that sets a flag, `WaitDelay` bounds Wait, and the group is signalled again after agy has exited, while its sentinel leader is still unreaped, and awaited. ACR's temp-file cleanup and `executeOptions` are not taken. |
| process_unix.go | internal/agent/process_unix.go | The kill: SIGKILL to the negative group id, with ESRCH read as done (ACR's `terminateProcessGroup`, here `processGroup.kill`), and `Setpgid` to give the call a group of its own. | ACR makes the process it starts the group leader and signals that pid. Here the leader is a sentinel shell (`processGroup`, new) that stays unreaped until the last signal has been sent, agy joins its group with `Pgid`, and `close` kills the group, reaps the sentinel and waits up to `groupGrace` for the group to be empty. ACR's `!windows` build tag and process_windows.go are not taken: the package uses flock and is Unix only, like the runtime. |
| timeout.go | internal/review/timeout.go | The 100 KiB threshold and a limit that grows in proportion to the size beyond it. | ACR multiplies a configured reviewer timeout, up to ten times, in integer arithmetic and prints a message; here the limit has a floor and a configurable ceiling, grows in float arithmetic and prints nothing. |

How the files were compared. `<ACR>` is a checkout of the upstream repository at the commit above and `<repo>` this checkout; both commands read only.

```sh
git -C <ACR> rev-parse HEAD                      # a3e438e2bd1f0824c1eab88db738aa3c82c69e99
grep -n 'syscall.Kill(-\|ESRCH\|Setpgid' <ACR>/internal/agent/process_unix.go <repo>/internal/review/agy/process_unix.go
grep -n 'func (c \*cappedBuffer) Write\|cmd.Cancel = ' <ACR>/internal/agent/executor.go <repo>/internal/review/agy/exec.go
grep -n 'r.exitCode = -1' <ACR>/internal/agent/cmd_reader.go; grep -n 'ExitCode()' <repo>/internal/review/agy/exec.go
grep -n 'reviewerTimeoutScaleThreshold = ' <ACR>/internal/review/timeout.go; grep -n 'sizeScaleThreshold = ' <repo>/internal/review/agy/timeout.go
grep -n 'LookPath' <ACR>/internal/agent/antigravity.go <repo>/internal/review/agy/agy.go
```

Measured on 2026-10-04: both process-group files send SIGKILL to the negative group id and read ESRCH as done, both thresholds are `100 * 1024`, both executors cap with a `cappedBuffer` and end the group from `cmd.Cancel`, and both adapters call `exec.LookPath`. The rest was compared by reading: the comparison shows which names and values were taken, not that behavior is equal, because the Go code around them is new.
