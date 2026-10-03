# crw review

`crw review` runs the independent code review on one change and writes its artifact. It builds the context bundle (`internal/review/bundle`), runs the pipeline (`internal/review/pipeline`) with the agy runner (`internal/review/agy`), validates the result against schema v1 (`internal/review`) and writes `<head>.json` with its sha256. The review is a reference opinion, never a merge gate. The command is the top-level mode `review` of `crw` (the mode table in `cmd/crw/main.go`); the package contract is [doc.go](../../internal/review/command/doc.go).

```sh
crw review --base <sha> --head <sha> --issue CRW-N --out <dir>
```

`--base` and `--head` are commits or refs of the repository (`--repo`, default the current directory); the artifact names the resolved commits. The diff is the merge base to the head, read from committed Git objects only, with no system or global Git configuration and no external diff or textconv. `--issue` is recorded in the ledger and printed, and never reaches a prompt.

## Run rules

The rules keep agy, an unattended official CLI under one account, at a low and steady rate.

- **Once per patch-id.** A change is identified by its `git patch-id --stable`. Once a review of it has finished (any finished record in the ledger, whatever the artifact's status), the command does not review it again, even for another head or another `--out`; it prints `already_reviewed` and the artifact path recorded by the earlier review. An `unavailable` artifact counts too. To allow a patch again, an operator removes its `finished` line from `ledger.jsonl`.
- **A daily cap.** At most `--daily-cap` reviews start per UTC day (default 20). A review that would be the next one is refused before any agy call, with `daily_cap_reached` and the reason recorded as a `refused` line. A started review counts, finished or not; a refusal and an already reviewed answer do not.
- **One call at a time.** One review holds the run lock (`run.lock` in the state directory) from its start to its end, and agy's own host-wide lock admits one agy call at a time on the host. An invocation waits for the run lock up to `--lock-wait` (default 30 minutes; a negative value tries once) and then exits `busy`.

## Output

stdout is one JSON object. `outcome` is `reviewed`, `already_reviewed` or `daily_cap_reached`; `issue`, `base`, `head` and `patchId` identify the change.

| Outcome | Other fields |
| --- | --- |
| `reviewed` | `artifact` (absolute path), `sha256`, `status` and `reason` of the artifact (`complete`, `partial` or `unavailable`), `reviewers`, `findings`, `dropped`, `calls` |
| `already_reviewed` | `artifact` and `sha256` as the earlier review recorded them, `status`, `reviewedHead`, `artifactPresent` (whether the file is there now: false while another invocation is still writing it, after a failed write, or when it was moved) |
| `daily_cap_reached` | `reason`, `dailyCap`, `runsToday` |

| Status | Meaning |
| --- | --- |
| 0 | a review ran (an `unavailable` or `partial` artifact is a result), or the patch was already reviewed |
| 1 | an error: no changes between base and head, a Git or ledger failure, a file already at `<out>/<head>.json` that is no review of this patch, an artifact that could not be written |
| 2 | a usage error (the usage line and the reason go to stderr) |
| 3 | the run rules stopped it: `daily_cap_reached` (on stdout), or `busy` (on stderr only, nothing recorded) |
| 130 | interrupted by SIGINT, SIGTERM or SIGHUP (agy's process group is killed first, so no agy call outlives the locks) |

## Files

`<out>/<head>.json` is the artifact, validated before it is written. `<out>/<head>.json.sha256` holds `<hex>  <head>.json` and a newline, so `sha256sum -c <head>.json.sha256` run in `<out>` checks the artifact. Both are written through a temporary file in `<out>` and renamed, so a reader sees a whole file or none; a symlink at the final path is followed, so `<out>` belongs to the operator. The file name is the head's while the ledger key is the patch's, so the same head reviewed against another base would land on the same name: the command refuses (exit 1, before any agy call) when `<out>/<head>.json` already exists and the ledger holds no review of this patch; use another `--out`. The ledger records the review as finished before the two files are written, so a failure while writing them (exit 1, naming the file) still counts as the review: the patch is not reviewed again.

## Ledger and configuration

The state directory (`--state-dir`, default `crw/review` under `$XDG_STATE_HOME` or `~/.local/state`, next to agy's lock) holds `run.lock` and `ledger.jsonl`, one JSON object per line: `time`, `event` (`started`, `finished`, `failed`, `refused`), `patchId`, `base`, `head`, `issue` and, as they apply, `reason`, `artifact`, `sha256` and `status`. It is appended under the run lock. A line that is not a record stops the command and names the file and line; a trailing fragment without a newline, left by a torn append, is ignored and cut off by the next append.

Every setting comes from one place, `Config` in [config.go](../../internal/review/command/config.go): a flag, else the environment, else the default.

| Flag | Environment | Default |
| --- | --- | --- |
| `--state-dir` | `CRW_REVIEW_STATE_DIR` | `crw/review` under the state home |
| `--daily-cap` | `CRW_REVIEW_DAILY_CAP` | 20 (at least 1) |
| `--model` | `CRW_REVIEW_MODEL` | `gemini-3.8-flash-high` |
| `--agy` | `CRW_REVIEW_AGY` | `agy` on `PATH` |
| `--lock` | `CRW_REVIEW_LOCK` | `crw/review/agy.lock` under the state home |
| `--lock-wait` | `CRW_REVIEW_LOCK_WAIT` | 30m |
| `--time-limit-floor`, `--time-limit-ceiling` | `CRW_REVIEW_TIME_LIMIT_FLOOR`, `CRW_REVIEW_TIME_LIMIT_CEILING` | the agy runner's (5m, 20m) |

## Not covered

The artifact's `agyVersion` is `unknown`: reading it would start an agy process outside the runner's host-wide lock. A `SIGKILL` of `crw` cannot be caught: agy, which holds no lock descriptor, may then run on until its time limit while the locks are free; closing that gap belongs to the runner. Posting the result, the receipt field and the parent's reading of the artifact belong to the wiring around this command.

## Tests

`go test ./internal/review/command` uses temporary Git repositories, a scripted runner, and one shell-script fake agy driven through the real runner; it never starts the real agy. `go test -tags agysmoke -run TestSmokeRealAgy -v ./internal/review/command` runs one small diff through the real agy once; run it by hand, since it uses the account agy is logged in with.
