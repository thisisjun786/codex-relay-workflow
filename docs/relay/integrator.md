# The integration queue: `crw manage integrate`

The sessions in this document are named by [the level table](../../plugins/crw/skills/crw-canon/references/hierarchy.md#the-levels): the parent is the repository session, the child is the project session, and a `child` session (a code id, not a layer) is no layer.

`crw manage integrate` is the product form of the shell queue runner that merges gated heads onto `dev`. It covers the
plain-head path, where the queue holds a commit per issue and the runner merges, verifies and pushes, and the pull request of a
milestone ([Pull requests](#pull-requests-pr-unit-landing)). It reads and writes no relay
record. A node's `merged` mark, the reach observation and the relationship and `child` session clean-up still belong to the project session that owns the
node (see [DAG scheduler](dag-scheduler.md), "Commit acceptance and local integration"); the queue does not replace them and does not decide
who may integrate. Code: `internal/integrator` (the logic) and `internal/manage/integrate.go` (the command).

## The queue

One directory, `queue_dir`, with one file per issue number in `pending/`, and, in a queue whose integrate section names
`pull_request_repo`, one file `pr-<N>` per pull request N of that repository ([Pull requests](#pull-requests-pr-unit-landing)). The file
holds the full 40-hex head (spaces and newlines are ignored). A pass moves a file out of `pending/` only after acting on it:

| Directory | Why a file is there |
|---|---|
| `done/` | the head is merged and pushed, or it is already in the branch; for a pull request also merged on GitHub. A pull request's entry is `done/pr-<N>-<head>`, one per head that landed, so a later head of the same pull request is a new entry |
| `conflict/` | the head conflicts with the branch, is not in the checkout, or could not be merged; its owner refreshes it. A pull request that is closed, or whose head moved after the enqueue and before the pass, is here too (enqueue it again) |
| `blocked/` | the gate is not `PASS`, the file does not hold a 40-hex head, or the head failed verification alone twice; a pull request also when its review says `needs_changes` (the reason names the verdict file), when it goes to another branch, or when its head branch is in another repository or in one GitHub no longer names (a deleted fork) |
| `retried/` | retry markers: `batch-<16 hex>` for a batch retried once, `<number>` holding the head retried once alone |
| `stop` | a file that ends `--watch` (and refuses a pass) |
| `solo-heads` | the heads (`node=head`, one per line) of a failed batch that have not yet had a run of their own; the file ends when none is pending any more |
| `runner.log` | one line per decision |

A file that changed while the run worked on it (the owner refreshed the head) stays in `pending/`: the file is moved to a private name in
its destination directory, read there, and put back as `pending/<name>` (never over a file written since) when it holds another head than the
run worked on. A coordinator should replace a pending file atomically (write a new file and rename it); a writer that changes the same file in
place after the move can still be missed. A file that can be neither moved on nor put back (the move to its destination and the link back
into `pending/` both fail) is never removed: it stays under its private name `.moving-<name>-<pid>` in the destination directory, the log
names it, `--dry-run` lists it as a claim, and the next pass puts it back into `pending/` under the lock before it reads the queue (a file
written to `pending/<name>` since wins and the claim goes). A name that is neither an issue number nor, with `pull_request_repo` set,
`pr-<N>`, a directory and a link are not entries; they are reported as ignored. A pull request entry whose head moved during the
verification or the review stays in `pending/` with the new head (`head-moved:<sha>`, below), and the next pass verifies and reviews it.

## One pass

1. A `stop` file ends the pass. An empty queue (no pending file and no claim left behind) is `idle` (the queue is listed once without the lock to decide this, as the shell runner's
   `ls` did; nothing is changed by that listing).
2. The lock (`lock_file`, a non-blocking `flock`, the lock every integration run takes) is taken as soon as the queue is not empty and held for
   the rest of the pass; a held lock is refused with status 2 before any gate, merge or file move. Claims an earlier pass left behind are
   put back into `pending/`, and the queue is read again under the lock.
3. The gate (`gate_command`, `{issue}` and `{head}` replaced, run in `checkout`) is re-checked for every pending head, in issue-number
   order: its last output line must start with `PASS` and its status must be 0. In solo mode the pass takes the first head that passes.
   A gate that ran and did not say `PASS` blocks the head. A gate that could not start or was killed is no verdict: the pass ends with
   status 2 (`error`) and the head and those behind it stay in `pending/`. A termination signal (`--watch` turns SIGINT and SIGTERM into a
   cancelled context) at any point of a pass, in the gates, the merges, the verification, or the second fetch and
   the push after a verification that passed, ends it as `cancelled` (status 2): what was decided before stays, nothing else is moved,
   retried or blocked, and no failed verification is counted. A push that had already landed when the signal came makes those heads
   `already-in-dev` on the next pass.
4. One integration run (tag `tag_prefix` plus the month, day and time, reserved by creating `runs_dir/<tag>.log` exclusively; a tag an
   earlier run used, as two passes in the same second do, gets the suffix `-2`, `-3`, ...) fetches the remote, makes the detached worktree
   `work_root/run-<tag>` on `refs/remotes/<remote>/<branch>`, and merges the heads in order with `merge --no-ff`.
   - A head that is an ancestor of the branch is `already-in-dev`; a commit missing from the checkout is `head-not-in-checkout`.
   - A head that conflicts is abandoned (`merge --abort`) and left out as `conflict`; the rest go on.
   - The plugin manifest's release version is a human choice; conflicting edits are ordinary conflicts and go back to their owner.
     The integration run makes no version-derivation commit. Digest suffixes are generated when installation artifacts are built.
     Any other conflict, in that file or beside it, is a plain conflict. Further generated files are not resolved here; a
     resolver for one is a separate, explicitly scoped and tested change.
   - What is verified is what is pushed: before the verification the worktree must be clean (`git status --porcelain` empty, so no
     uncommitted or untracked change beside the commit); otherwise the run ends with status 2, nothing is verified or pushed, and the heads
     stay in `pending/`. The same holds after the verification: `verify_command` is a program the operator configures, and a HEAD it
     moved or a change it left in the worktree (tracked, staged or untracked) ends the run with status 2 though it exited 0, because
     the tree it judged is no longer the commit that would be pushed.
   - The run's private temporary directory `work_root/tmp-<tag>` is created by the run itself (`mkdir`, not `mkdir -p`) and only a
     directory the run created is removed with it; a name that already exists ends the run with status 2 and is left untouched.
5. `verify_command` and the gate run with the inherited environment cleaned of Git routing variables
   (`GIT_DIR`, `GIT_WORK_TREE`, `GIT_INDEX_FILE` and the others `gitprobe.Sanitize` removes), so a command that asks Git for its
   repository finds the private worktree, never the shared checkout.
   `verify_command` runs in the worktree with `CRW_VERIFY_HEAD`, `CRW_VERIFY_BASE` and `CRW_VERIFY_RECORD` set; its status is the
   verdict, and its output is `runs_dir/<tag>.verify.log`.
6. The remote is fetched again. If the branch is no longer at the base the run ends `dev-moved:<sha>`. Otherwise the merged head is
   pushed as `<sha>:refs/heads/<branch>`: never forced, so a remote that moved refuses it and the run ends `push-failed`.
7. The worktree and the run's private temporary directory are removed, whatever the result.

Run files, in `runs_dir`: `<tag>.log`, `<tag>.out` and `<tag>.summary.json` (`tag`, `base`, `head`, `merged`, `left_out`, `record`,
`result`, the shell runner's own shape), `<tag>.verify.log`, and the verification record `<tag>.json` that `verify_command` writes.
Right before the push the run writes `"result": "pushing"` into its summary, a marker the run's own result overwrites. The marker must be on disk before the push starts: when `runs_dir` refuses the write (full or read-only), nothing is pushed, the pass reports `error` (status 2) and the entry stays in `pending/`; a push that landed with no
`pushed` summary behind it (the signal ended the call after git exited 0) leaves the marker, and the recovery of that landing accepts it when its head
is in the branch, so the run's verification record and review verdict are still found. The runs of the entry are read newest first (by tag) and the first that names a push
wins, a `pushed` summary or a marker whose head is in the branch; a marker whose head the checkout lacks was never pushed and is passed over for older evidence. A git that cannot answer whether the marker's head
is in the branch (its commit object is unreadable: the existence check `git cat-file -e <head>` finds the object, but the ancestor check fails) holds the entry (it stays in `pending/` with the reason in `held`, no record is written, the marker stays) and the next pass asks again. Only a marker newer than every other evidence can hold the entry this way: an unreadable older marker behind a newer `pushed` summary or a newer marker in the branch is never asked. `pushing` is a marker,
never a pass result.

### Exit status

| Status | Result | Meaning |
|---|---|---|
| 0 | `pushed`, `landed-before-record`, `comment-reposted`, `idle`, `stopped` | the merged heads were pushed, a pull request whose head was already in the branch got its landing record after the fact, a landing comment an earlier pass did not get to was posted, or there was nothing to do |
| 2 | `locked`, `error`, `cancelled` | usage, configuration, fetch or lock failure, a gate that could not run, the worktree already exists, a worktree not clean before the verification, or a termination signal; no head was judged by it |
| 3 | `verification-failed(rc=N)` | the merged tree failed verification; nothing was pushed |
| 4 | `dev-moved:<sha>`, `push-failed`, `head-moved:<sha>`, `pull-request-changed` | the branch moved during the run or the push was refused; for a pull request, its head moved, or it was closed, retargeted or could not be read again, between the merge and the move of the branch; nothing was pushed and the entry stays pending |
| 5 | `nothing-merged`, `nothing-to-integrate` | no head could be merged, or no head passed the gate |
| 6 | `review-needs-changes` | the review of a pull request's merged tree asked for changes; nothing was pushed |

### Retry after a failed verification

A failed batch is not bisected. It is retried once as the same batch (a load flake is the usual cause); if it fails again each of its
heads runs alone, and batching resumes when none of them is pending with that head any more (it was pushed, blocked, moved to `conflict/`,
or its owner refreshed or removed the file). A head that fails alone is retried once with the same head, then blocked; its retry does not
use up the run another member is owed. A new head under the same issue number starts again.

## Pull requests (PR-unit landing)

A project session lands its milestone through one pull request. The plan's target `base_ref` is the milestone integration branch, so
`crw.integrate` lands each node there exactly as it lands one on `dev` (merge commit, verification of the merged tree, compare-and-swap
of the branch). The project session then opens one pull request from that branch toward `dev`, naming the issues it carries
(`crw flow pr open`, which pushes the branch first). The parent decides it with
`crw flow pr decide --pr OWNER/NAME#N --head SHA --release-impact TEXT`; the equivalent queue entry command is
`crw manage integrate --enqueue-pr OWNER/NAME#N --head SHA --release-impact TEXT`.
Nobody merges the pull request on GitHub: the queue does, and the pull request is the place of its record and its review. The gate is
the verification-record of the merged tree; the review is a step of the queue the repository configures.

- Both commands hold the integration lock and check the expected head, remote base tip, successful head checks and overlapping
  earlier pending PRs, without reading diff content. `--update-base` permits a no-ff base merge and a non-forced push guarded
  by the advertised old head; that path answers `needs_ci` and does not enqueue. An overlapping earlier PR answers `hold_order`.
  The release impact is required and stored in a `crw-pr-decision/1` record (`--record PATH`, default
  `<runs_dir>/decisions/pr-N-SHA.json`). An all-green decision posts an idempotent `ready` comment before enqueueing;
  the final disk record says `enqueued` only after the queue write succeeds.
- `--enqueue-pr` uses that decision and writes its pinned head to `pending/pr-<N>`
  (written beside the queue and renamed in, replacing an earlier head of the same pull request). A pull request of another repository,
  one that is closed or merged, one toward another branch than `branch`, one whose head branch is in another repository (a fork), or
  one that names no head commit is refused and nothing is written. A queue without `pull_request_repo` takes no `pr-<N>` file: it is reported as ignored, as before. The adapter also lists the comments of a pull request (a GET with the token that already reads the pull request, paginated): a pass uses it to find a landing comment by its header before it posts one again.
- A pull request entry is keyed by its number and its head. A head that already landed (`done/pr-<N>-<head>`, or a landing record of that
  number and head that moved the branch) is refused by `--enqueue-pr`, and a pass moves such a pending file to `done/` with no run and no
  new record; the only comment it posts is the one its record still lacks (`comment` is `not written yet` and no comment with its header is on the pull request, see the record below).
  `--dry-run` reports such a file as `landed`, plans no run and posts nothing. `--enqueue-pr` may have posted its readiness comment
  before the queue detects a previously landed head; a refusal never records a successful enqueue. After new commits on its branch the project session
  enqueues the same pull request again and the queue lands the new head.
- A pass lands one pull request alone, the lowest number first, before the issue heads, and runs no `gate_command` for it. It reads the
  pull request again and fetches the branch. A head that is already in the branch with no landing record (a pass that ended after its
  push, or a pull request GitHub shows merged after the branch reached its head) gets one record after the fact, result
  `landed_before_record`: `dev_after` is the branch's first-parent commit that brought the head in, the verification and the review are
  taken from the files of the run that pushed it (`<tag>.summary.json`, the verification-record, `<tag>.review.json`) and stated as not
  recorded when they are gone; its comment is posted once and the file moves to `done/`. That recovery is the pass: its result is
  `landed-before-record` (status 0, the head is in the branch and nothing is wrong), the report's `pull_request_landing` is the recovered record, there
  is no summary (no run ran), and the pass ends there, so the pull requests and issue heads behind it wait for the next pass. Otherwise: merged moves the file to `done/`;
  closed, or a head that is no longer the one enqueued (the project session landed more and pushed before the pass), to `conflict/`
  (enqueue it again); toward another branch, from a fork, or from a repository GitHub no longer names, to `blocked/`. A pull request GitHub
  does not answer, or a fetch that fails, is no verdict: it stays in `pending/`, the report lists it under `held`, and the pass goes on with
  the pull requests and issue heads behind it. Status 2 (`error`) is for a pass that did nothing else; a pass that landed work (a run, or a
  recovered landing) keeps that result and its status, with `held` still in the report. A record whose comment says `not written yet` gets
  its comment on the next pass that finds the head landed and the entry moves to `done/`; when that is all the pass did, its result is
  `comment-reposted` (status 0, no summary, no new record), not `nothing-to-integrate`. Before it posts, that pass looks for the comment on the pull request by its header
  `[dev queue · <tag>]`: a comment that is already there is not posted a second time, the record is written as `written` and the entry moves to `done/`, but that pass is
  `nothing-to-integrate`, since no comment went up. A lookup the host does not answer holds the entry (it stays in `pending/`, the reason is in `held`, the record is left as it is and nothing is posted). A comment the host refuses is recorded as
  `not written: <error>` and the entry still moves to `done/`, but that pass is `nothing-to-integrate`: no comment went up.
- The run merges the head with `merge --no-ff -m "Merge pull request OWNER/NAME#N (<branch>) at <head8> into <branch>"` and verifies the
  merged tree as step 4 and 5 above. Only after a verification that passed does `review_command` run, in the worktree, on that tree:
  `{pr}`, `{pr_number}`, `{head}` (the pull request's head), `{merged}` (the verified commit), `{base}` (the branch before), `{verdict}`
  (`runs_dir/<tag>.review.json`), `{worktree}` and `{record}` are replaced, and `CRW_REVIEW_PR`, `CRW_REVIEW_HEAD`, `CRW_REVIEW_MERGED`,
  `CRW_REVIEW_BASE`, `CRW_REVIEW_VERDICT` and `CRW_VERIFY_RECORD` are set; its output is `runs_dir/<tag>.review.log`. What the review
  judges against is the review tool's own input: the coordinator's Argon review (`argon-v5.sh KEY WORKTREE HEAD OUT_JSON [BASE]`) reads
  the scope and end conditions from its key (`/state/crw/crw-<N>/criteria.json` for a key `CRW-<N>`) and reviews without end conditions
  when no such file exists, so the key configured in `review_command` must name a criteria file. It writes the
  verdict file, `{"verdict": "accept"}` or `{"verdict": "needs_changes"}`, at the top or under `structured_output` (the shape the
  coordinator's Argon review writes). `needs_changes` ends the run `review-needs-changes` (status 6), pushes nothing and moves the file to
  `blocked/` with the verdict file's path. No `review_command` is a warning: the run records `review not configured` and lands on the
  verification-record alone (CRW-1306). A review that cannot start or leaves no readable verdict is recorded and warned the same way and
  does not stop the landing. A review that moves HEAD or leaves a change in the worktree ends the run with status 2, as the verification
  does.
- Right before the branch moves the pull request is read once more. A head pushed during the verification or the review ends the run
  `head-moved:<sha>` (status 4): the branch does not move for the head that was verified, `pending/pr-<N>` is rewritten with the new head
  (unless the project session enqueued meanwhile), the record says `head_moved`, and the next pass verifies and reviews the new head. A
  pull request closed, merged or retargeted meanwhile, or one that cannot be read again, ends the run `pull-request-changed` (status 4,
  the reason is in the record's warnings) and the entry stays pending for the next pass to read.
- The branch then moves as step 6 above: the second fetch must find it at the base the tree was verified on, and the push is never forced.
  A move during the review ends the run `dev-moved:<sha>` and the pull request stays pending.
- Every run of a pull request writes `runs_dir/<tag>.pr.json` (`crw-pr-landing/1`): the pull request, its number, URL, head and branch, the
  issues of its `Carries:` line, `dev_before`, `dev_after` (empty when the branch did not move), the merged commit, the result, the
  verification (record path, its sha256, the command's status, a note when it is not recorded) and the review (configured, verdict,
  verdict file, status, note), `head_moved` when the head moved during the run, and its warnings. One (number, head) has one record that
  moved the branch: written by the run that pushed it, or, when that run ended before its record, by the pass that finds the head in the
  branch (`landed_before_record`). The same record is written on the pull request as a comment headed `[dev queue · <tag>]`; `comment` says whether it was:
  `not written yet` (the record is written first, so a pass that ended before the comment call leaves it), `written`, or `not written: <reason>`
  (GitHub refused; final). The next pass that finds the head landed looks for the comment of a `not written yet` record on the pull request by its header and, when it is not there, posts it once; it writes the file again either way
  (`written`). A `written` or refused comment is never posted again, and a pass that ends between the comment call and the second write of the record leaves a comment that the next pass finds
  instead of posting it a second time. A lookup that fails holds the entry like a queue with no tracking adapter, which cannot post or look for it: the entry stays in `pending/` with the reason in `held`
  and the record is left as it is.
- A failed verification is retried as for a head alone: once with the same head, then the file is blocked.

## Command

    crw manage integrate                      one pass
    crw manage integrate --watch              passes until the stop file (--interval <duration>, default interval_seconds)
    crw manage integrate --dry-run            read the queue and the checkout and report; --with-gate also runs the gate
    crw manage integrate --enqueue-pr REF --head SHA --release-impact TEXT
                                              decide and enqueue REF; optional --record PATH and --update-base

`--dry-run` makes nothing: no directory, no file move, no lock (it is only probed for `free`, `held` or `absent`), no fetch, no worktree.
It reports the pending heads with whether each commit is in the checkout and already in the last-fetched branch, the retry markers, the
solo state, the claims an earlier pass left behind and the heads the next pass would hand to one run (a pending pull request alone). The gate is run only with `--with-gate`, because a gate may fetch on its own; a dry run reads no pull request.

## Configuration

The `integrate` object of the `manage` section of the crw configuration file. Every host path and command is named here; none has a
default.

| Key | Meaning |
|---|---|
| `checkout` | the repository whose remote is merged onto; fetched into, never modified (the merge is in a worktree) |
| `queue_dir`, `lock_file`, `runs_dir`, `work_root` | absolute paths |
| `gate_command` | argv with `{issue}` (e.g. `CRW-1038`) and `{head}` |
| `verify_command` | argv; the merged-tree verification |
| `record_version_command`, `record_version_env` | retired (CRW-1582): accepted for compatibility, ignored; no version line is re-derived |
| `remote`, `branch`, `git`, `key_prefix`, `tag_prefix`, `plugin_dir`, `plugin_manifest`, `interval_seconds` | defaults: `origin`, `dev`, `git`, `CRW`, `cc-q`, `plugins/crw`, `plugins/crw/.codex-plugin/plugin.json`, `60` |
| `pull_request_repo` | `owner/name` of the repository whose pull requests the queue lands; empty: none (the token is `GH_TOKEN`, `GITHUB_TOKEN` or `gh auth token`) |
| `review_command` | argv; the review of a pull request's merged tree (placeholders above); empty: `review not configured`, warned and recorded. The review tool reads its criteria from the key the argv gives it (Argon V5: `/state/crw/crw-<N>/criteria.json` for `CRW-<N>`), so the configured key must name a criteria file |

## Switch from the shell runner

The shell runner (`cc-queue-runner.sh` with `integrate_git.sh`) stays the live runner. This command reads the same queue directories,
retry markers, stop file and lock, so the two can never integrate at once, and the scripts are not modified. The switch is an explicit
operator step after the cutover install:

1. Put the `integrate` section in the configuration; run `--dry-run` (and `--dry-run --with-gate`) on the live queue while the shell
   runner works, and compare the plan with what the shell runner then does.
2. Put a `stop` file in the queue; the shell runner exits at its next cycle. Remove the file and start `crw manage integrate --watch`
   under the service manager that ran the shell runner.
3. Keep the shell scripts in place until one full batch (merged order, verification record, summary, queue moves) matches what the
   shell runner would have produced.

Rollback: put a `stop` file in the queue (the watch ends), delete `solo-heads` if present, remove the file and start the shell runner. The
directories, markers and lock are the same, so no state needs converting.
