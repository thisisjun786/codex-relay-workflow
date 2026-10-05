# crw review

`crw review` runs the independent code review on one change and writes its artifact. It builds the context bundle (`internal/review/bundle`), runs the pipeline (`internal/review/pipeline`) with the agy runner (`internal/review/agy`), validates the result against schema v1 (`internal/review`) and writes `<head>.json` with its sha256; with `--post-summary` it also keeps one summary comment on the pull request. The review is a reference opinion, never a merge gate. The command is the top-level mode `review` of `crw` (the mode table in `cmd/crw/main.go`); the package contract is [doc.go](../../internal/review/command/doc.go).

```sh
crw review --base <sha> --head <sha> --issue CRW-N --out <dir>
crw review --base <sha> --head <sha> --issue CRW-N --out <dir> --post-summary --pr <n>
crw review --base <sha> --head <sha> --issue CRW-N --out <dir> --post-only --pr <n>
```

`--base` and `--head` are commits or refs of the repository (`--repo`, default the current directory, which may be a subdirectory of the repository: the paths of the findings are relative to the repository root, so a finding on a root file is checked against that file and never against a file of the same name below the directory the command runs in); the artifact names the resolved commits. The diff is the merge base to the head, read from committed Git objects only, with no system or global Git configuration and no external diff or textconv. `--issue` is recorded in the ledger and printed, and never reaches a prompt.

## Run rules

The rules keep agy, an unattended official CLI under one account, at a low and steady rate.

- **Once per patch-id.** A change is identified by its `git patch-id --stable`. Once a review of it has finished (any finished record in the ledger, whatever the artifact's status), the command does not review it again, even for another head or another `--out`; it prints `already_reviewed` and the artifact path recorded by the earlier review. To allow a patch again, an operator removes its `finished` line from `ledger.jsonl`, and its `unavailable` line if it has one (a retry that left no result closes the patch with that line alone).
- **One more attempt when no review could run.** A run whose every review call failed as `unavailable` for a reason of the account or the configuration (`quota`, `authentication`, `unknown_model` or `not_started`) does not use up the patch-id: the ledger gets an `unavailable` line instead of a `finished` one, and `retryNotBefore` names the next UTC day. The same patch may be tried once more on a later UTC day than the one that line was written on (the day the attempt ended), never in the same one; an invocation on the same day (or an earlier one) prints `retry_deferred` with the earlier artifact and starts nothing. The retry may use another head of the same patch, and one on the same head replaces the unavailable artifact only while it still has the bytes the ledger recorded. The attempt is spent when the retry starts: a retry that ends without a result (interrupted, an error, a killed process) leaves the patch closed on the unavailable artifact, and a second unavailable run is written as `finished`. Every other outcome (a content filter, an invalid answer such as a denied action, the time limit, a crash, a wait for agy's lock) is a result and closes the patch-id on its first run. An older ledger needs no change: its `finished` lines, `unavailable` artifacts included, keep closing their patches. A repair of a post is not a retry: on or after the retry day the same command is the retry, so a post is repaired there with `--post-only`, which starts nothing (see Summary comment).
- **A daily cap.** At most `--daily-cap` reviews start per UTC day (default 20). A review that would be the next one is refused before any agy call, with `daily_cap_reached` and the reason recorded as a `refused` line. Every attempt that starts counts, whatever its result, the retry included; a refusal, an already reviewed answer and a `--post-only` run do not. On the retry day a spent cap refuses the retry without using it up.
- **One call at a time.** One review holds the run lock (`run.lock` in the state directory) from its start to its end, and agy's own host-wide lock admits one agy call at a time on the host. An invocation waits for the run lock up to `--lock-wait` (default 30 minutes; a negative value tries once) and then exits `busy`.

## Output

stdout is one JSON object. `outcome` is `reviewed`, `already_reviewed`, `daily_cap_reached`, `retry_deferred` or `recorded`; `issue`, `base`, `head` and `patchId` identify the change.

| Outcome | Other fields |
| --- | --- |
| `reviewed` | `artifact` (absolute path), `sha256`, `status` and `reason` of the artifact (`complete`, `partial` or `unavailable`), `reviewers`, `findings`, `dropped`, `calls` |
| `already_reviewed` | `artifact` and `sha256` as the earlier review recorded them, `status`, `reviewedHead`, `artifactPresent` (whether the file is there now: false while another invocation is still writing it, after a failed write, or when it was moved) |
| `daily_cap_reached` | `reason`, `dailyCap`, `runsToday` |
| `retry_deferred` | `reason`, `retryNotBefore` (the UTC day), and the earlier attempt's `artifact`, `sha256`, `status`, `reviewedHead`, `artifactPresent` |
| `recorded` | `--post-only` only: the newest recorded result of the patch, closed or still open for its one more attempt, as `artifact`, `sha256`, `status`, `reviewedHead`, `artifactPresent` |

`retryNotBefore` is also set on a `reviewed` answer whose run could not review and may be retried. `restored` lists the files a `retry_deferred`, `already_reviewed` or `recorded` answer wrote back from the kept result (see Files). With `--post-summary` or `--post-only` the object has `summaryComment` (`action`: `created`, `updated` or `unchanged`, the comment's `url`, and a `reason` when the comment shows a newer result of the patch than the one this run holds).

| Status | Meaning |
| --- | --- |
| 0 | a review ran (an `unavailable` or `partial` artifact is a result), the patch was already reviewed, or `--post-only` posted its recorded result |
| 1 | an error: no changes between base and head, a Git or ledger failure, a file already at `<out>/<head>.json` that is no review of this patch, an artifact that could not be written or restored, a result that could not be kept (after the review was recorded and published), `--post-only` for a patch with no recorded result or a head that does not resolve, the summary comment that could not be posted (the object is still printed) |
| 2 | a usage error (the usage line and the reason go to stderr) |
| 3 | the run rules stopped it: `daily_cap_reached` or `retry_deferred` (on stdout), or `busy` (on stderr only, nothing recorded; the run lock, or with `--post-summary` the post lock) |
| 130 | interrupted by SIGINT, SIGTERM or SIGHUP (agy's process group is killed first, so no agy call outlives the locks) |

## Summary comment

`--post-summary --pr <n>` posts after the review, for an answer that names an artifact: `reviewed`, `already_reviewed`, `retry_deferred` and `recorded`. The last three read the recorded file and need it to be there with the recorded sha256, so a failed post is repaired by running the command again, which makes no agy call, while the patch is closed or its retry is deferred. When the retry day has come the same command is the retry: it starts a review, uses the retry and counts toward the cap, and a spent cap stops it before it posts. The repair there is `--post-only --pr <n>` (it implies `--post-summary`): it posts the newest recorded result of the patch, closed or open, on any day and with any cap, and it runs no review, writes no ledger record and leaves the retry and the cap as they were. It exits 1 for a patch with no recorded result. `daily_cap_reached` posts nothing. The pull request is named by `--pr`; the repository is the one `gh` resolves from the checkout of `--repo` (or `GH_REPO`), and `gh api` (`--gh`) carries the body on stdin.

The comment is a general comment of the pull request, never a review thread (the relay counts a thread that arrives after the child's receipt as a late finding). Its first line is the marker `<!-- crw-independent-review v1 {head, patchId, status, sha256} -->`. The comment whose body starts with the marker is updated, else one is created, and nothing is written when the text is the same, so a pull request keeps one comment whatever head was reviewed last. It says that the review is a reference opinion and not a merge gate, and gives the model, the status and reason, the status of each reviewer (`ok`, `partial`, `invalid` or `unavailable`, with the reasons: an invalid reviewer is shown as such and is never read as no findings) and up to ten findings. Everything a reviewer or a setting wrote is shown in a code span on one line, cut to a length. A post holds the post lock (`post.lock` in the state directory) from the reading of the ledger to the write, so posts of one state directory take turns, and inside the lock it summarizes the newest result of the patch that the ledger holds: a post prepared from an older result (a review of another day finished while it waited) summarizes the newer one (from the copy kept with its record while its files are not published yet) and says so in `summaryComment.reason`, so a late post never puts an older result over a newer one. Marked comments made elsewhere are not removed: the first is updated and a warning names the count. Posts from different state directories or hosts are not serialized.

## Files

`<out>/<head>.json` is the artifact, validated before it is written. `<out>/<head>.json.sha256` holds `<hex>  <head>.json` and a newline, so `sha256sum -c <head>.json.sha256` run in `<out>` checks the artifact. Both are written through a temporary file in `<out>` and renamed, so a reader sees a whole file or none; a symlink at the final path is followed, so `<out>` belongs to the operator. The file name is the head's while the ledger key is the patch's, so the same head reviewed against another base would land on the same name: the command refuses (exit 1, before any agy call) when `<out>/<head>.json` already exists and the ledger holds no review of this patch; use another `--out`. The ledger records the review as finished before the two files are written, so a failure while writing them (exit 1, naming the file) still counts as the review: the patch is not reviewed again. The result is also kept in the state directory, as `results/<sha256>.json`, before that record is appended, so a later call for the patch writes back what is missing without an agy call or a ledger record: the artifact when nothing is at its recorded path (or only the bytes of the earlier unavailable attempt that a retry replaced), and the checksum file when it is missing or wrong and the artifact beside it has the recorded bytes, at the recorded paths and not at the current `--out`; no other file is replaced, and a checksum is never written beside a file that is not the recorded artifact. This happens in the `already_reviewed`, `retry_deferred` and `recorded` answers and is listed in `restored`. It needs the run lock, tried once: while another review runs, the repair waits for the next call. A directory of an earlier `--out` that is gone is not recreated, and a write that fails exits 1 naming the file. A result that cannot be kept (the state directory takes no copy) still closes the patch and publishes its files, then exits 1 saying so, because a second agy call is what the copy exists to avoid; a ledger from before results were kept restores nothing, and a kept copy that does not have the recorded sha256 is not used (exit 1, naming it).

## Ledger and configuration

The state directory (`--state-dir`, default `crw/review` under `$XDG_STATE_HOME` or `~/.local/state`, next to agy's lock) holds `run.lock`, `post.lock`, the kept results in `results/` and `ledger.jsonl`, one JSON object per line: `time`, `event` (`started`, `finished`, `unavailable`, `failed`, `refused`), `patchId`, `base`, `head`, `issue` and, as they apply, `reason`, `artifact`, `sha256` and `status`; an `unavailable` line lists the reasons in `reason`. It is appended under the run lock. A line that is not a record stops the command and names the file and line; a trailing fragment without a newline, left by a torn append, is ignored and cut off by the next append.

Every setting comes from one place, `Config` in [config.go](../../internal/review/command/config.go): a flag, else the environment, else the default.

| Flag | Environment | Default |
| --- | --- | --- |
| `--state-dir` | `CRW_REVIEW_STATE_DIR` | `crw/review` under the state home |
| `--daily-cap` | `CRW_REVIEW_DAILY_CAP` | 20 (at least 1) |
| `--model` | `CRW_REVIEW_MODEL` | `gemini-3.8-flash-high` |
| `--agy` | `CRW_REVIEW_AGY` | `agy` on `PATH` |
| `--gh` | `CRW_REVIEW_GH` | `gh` on `PATH` |
| `--lock` | `CRW_REVIEW_LOCK` | `crw/review/agy.lock` under the state home |
| `--lock-wait` | `CRW_REVIEW_LOCK_WAIT` | 30m (also bounds the post lock) |
| `--time-limit-floor`, `--time-limit-ceiling` | `CRW_REVIEW_TIME_LIMIT_FLOOR`, `CRW_REVIEW_TIME_LIMIT_CEILING` | the agy runner's (5m, 20m) |

## Not covered

The artifact's `agyVersion` is `unknown`: reading it would start an agy process outside the runner's host-wide lock. A `SIGKILL` of `crw` cannot be caught: agy, which holds no lock descriptor and whose time limit lives in the parent, may then run on, unbounded, while the locks are free; closing that gap belongs to the runner. The receipt field and the parent's reading of the artifact belong to the wiring around this command. The summary comment has only been exercised against a scripted forge and a fake `gh`, never against a live pull request.

The parent can combine saved findings with Devin/Codex observations in the separate [adjudication ledger](adjudication.md) and compare requested models over the same explicit PR heads. That append-only file convention and report do not change this command's artifact, execution ledger or merge rules.

## Tests

`go test ./internal/review/command` uses temporary Git repositories, a scripted runner, a scripted forge, a fake `gh` and one shell-script fake agy driven through the real runner; it never starts the real agy and never posts to a real pull request. `go test -tags agysmoke -run TestSmokeRealAgy -v ./internal/review/command` runs one small diff through the real agy once; run it by hand, since it uses the account agy is logged in with.
