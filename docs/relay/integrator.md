# The integration queue: `crw manage integrate`

`crw manage integrate` is the product form of the shell queue runner that merges gated heads onto `dev`. It covers the
plain-head path only: the queue holds a commit per issue, and the runner merges, verifies and pushes. It reads and writes no relay
record. A node's `merged` mark, the reach observation and the relationship and child clean-up still belong to the parent that owns the
node (see [DAG scheduler](dag-scheduler.md), "Commit acceptance and local integration"); the queue does not replace them and does not decide
who may integrate. Code: `internal/integrator` (the logic) and `internal/manage/integrate.go` (the command).

## The queue

One directory, `queue_dir`, with one file per issue number in `pending/`. The file holds the full 40-hex head (spaces and newlines are
ignored). A pass moves a file out of `pending/` only after acting on it:

| Directory | Why a file is there |
|---|---|
| `done/` | the head is merged and pushed, or it is already in the branch |
| `conflict/` | the head conflicts with the branch, is not in the checkout, or could not be merged; its owner refreshes it |
| `blocked/` | the gate is not `PASS`, the file does not hold a 40-hex head, or the head failed verification alone twice |
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
written to `pending/<name>` since wins and the claim goes). A name that is not an issue number, a
directory and a link are not entries; they are reported as ignored.

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
   cancelled context) at any point of a pass, in the gates, the merges, the record command, the verification, or the second fetch and
   the push after a verification that passed, ends it as `cancelled` (status 2): what was decided before stays, nothing else is moved,
   retried or blocked, and no failed verification is counted. A push that had already landed when the signal came makes those heads
   `already-in-dev` on the next pass.
4. One integration run (tag `tag_prefix` plus the month, day and time, reserved by creating `runs_dir/<tag>.log` exclusively; a tag an
   earlier run used, as two passes in the same second do, gets the suffix `-2`, `-3`, ...) fetches the remote, makes the detached worktree
   `work_root/run-<tag>` on `refs/remotes/<remote>/<branch>`, and merges the heads in order with `merge --no-ff`.
   - A head that is an ancestor of the branch is `already-in-dev`; a commit missing from the checkout is `head-not-in-checkout`.
   - A head that conflicts is abandoned (`merge --abort`) and left out as `conflict`; the rest go on.
   - A conflict that is **only** the `version` line of the plugin manifest (`plugin_manifest`) is the one automatic resolution: every
     conflict block must have exactly one line on each side and both must be `"version"` lines; the current side is kept and the merge is
     committed. Any other conflict, in that file or beside it, is a plain conflict. Further generated files are not resolved here; a
     resolver for one is a separate, explicitly scoped and tested change.
   - When a version conflict was resolved, or the merged tree differs from the base under `plugin_dir`, `record_version_command` re-derives
     the manifest's version line in the worktree and the change is committed as `integration <tag>: re-derive the plugin version line ...`.
     A change that cannot be added or committed (a refusing hook, for example) ends the run with status 2 before the verification.
   - What is verified is what is pushed: before the verification the worktree must be clean (`git status --porcelain` empty, so no
     uncommitted or untracked change beside the commit); otherwise the run ends with status 2, nothing is verified or pushed, and the heads
     stay in `pending/`. The same holds after the verification: `verify_command` is a program the operator configures, and a HEAD it
     moved or a change it left in the worktree (tracked, staged or untracked) ends the run with status 2 though it exited 0, because
     the tree it judged is no longer the commit that would be pushed.
   - The run's private temporary directory `work_root/tmp-<tag>` is created by the run itself (`mkdir`, not `mkdir -p`) and only a
     directory the run created is removed with it; a name that already exists ends the run with status 2 and is left untouched.
5. `record_version_command`, `verify_command` and the gate run with the inherited environment cleaned of Git routing variables
   (`GIT_DIR`, `GIT_WORK_TREE`, `GIT_INDEX_FILE` and the others `gitprobe.Sanitize` removes), so a command that asks Git for its
   repository finds the private worktree, never the shared checkout. `record_version_env` is the operator's explicit word and is applied
   after that cleanup.
   `verify_command` runs in the worktree with `CRW_VERIFY_HEAD`, `CRW_VERIFY_BASE` and `CRW_VERIFY_RECORD` set; its status is the
   verdict, and its output is `runs_dir/<tag>.verify.log`.
6. The remote is fetched again. If the branch is no longer at the base the run ends `dev-moved:<sha>`. Otherwise the merged head is
   pushed as `<sha>:refs/heads/<branch>`: never forced, so a remote that moved refuses it and the run ends `push-failed`.
7. The worktree and the run's private temporary directory are removed, whatever the result.

Run files, in `runs_dir`: `<tag>.log`, `<tag>.out` and `<tag>.summary.json` (`tag`, `base`, `head`, `merged`, `left_out`, `record`,
`result`, the shell runner's own shape), `<tag>.verify.log`, and the verification record `<tag>.json` that `verify_command` writes.

### Exit status

| Status | Result | Meaning |
|---|---|---|
| 0 | `pushed`, `idle`, `stopped` | the merged heads were pushed, or there was nothing to do |
| 2 | `locked`, `error`, `cancelled` | usage, configuration, fetch or lock failure, a gate that could not run, the worktree already exists, a re-derived version line that could not be committed, a worktree not clean before the verification, or a termination signal; no head was judged by it |
| 3 | `verification-failed(rc=N)` | the merged tree failed verification; nothing was pushed |
| 4 | `dev-moved:<sha>`, `push-failed` | the branch moved during the run or the push was refused; nothing was pushed |
| 5 | `nothing-merged`, `nothing-to-integrate` | no head could be merged, or no head passed the gate |

### Retry after a failed verification

A failed batch is not bisected. It is retried once as the same batch (a load flake is the usual cause); if it fails again each of its
heads runs alone, and batching resumes when none of them is pending with that head any more (it was pushed, blocked, moved to `conflict/`,
or its owner refreshed or removed the file). A head that fails alone is retried once with the same head, then blocked; its retry does not
use up the run another member is owed. A new head under the same issue number starts again.

## Command

    crw manage integrate                      one pass
    crw manage integrate --watch              passes until the stop file (--interval <duration>, default interval_seconds)
    crw manage integrate --dry-run            read the queue and the checkout and report; --with-gate also runs the gate

`--dry-run` makes nothing: no directory, no file move, no lock (it is only probed for `free`, `held` or `absent`), no fetch, no worktree.
It reports the pending heads with whether each commit is in the checkout and already in the last-fetched branch, the retry markers, the
solo state, the claims an earlier pass left behind and the heads the next pass would hand to one run. The gate is run only with `--with-gate`, because a gate may fetch on its own.

## Configuration

The `integrate` object of the `manage` section of the crw configuration file. Every host path and command is named here; none has a
default.

| Key | Meaning |
|---|---|
| `checkout` | the repository whose remote is merged onto; fetched into, never modified (the merge is in a worktree) |
| `queue_dir`, `lock_file`, `runs_dir`, `work_root` | absolute paths |
| `gate_command` | argv with `{issue}` (e.g. `CRW-1038`) and `{head}` |
| `verify_command` | argv; the merged-tree verification |
| `record_version_command`, `record_version_env` | argv and extra environment; `{tmpdir}` is the run's private directory below `work_root` |
| `remote`, `branch`, `git`, `key_prefix`, `tag_prefix`, `plugin_dir`, `plugin_manifest`, `interval_seconds` | defaults: `origin`, `dev`, `git`, `CRW`, `cc-q`, `plugins/crw`, `plugins/crw/.codex-plugin/plugin.json`, `60` |

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
