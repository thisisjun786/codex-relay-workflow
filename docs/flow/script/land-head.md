
## Landing a head made outside a run

The child decides how the work of an issue is made: a project-lane run, or its own subagents outside any run (CRW-1316 §18.3). A head made
outside a run lands through the gates of a run, never past them: `internal/flow/workflows/land-head.js` (CRW-1354) is the gate-only
workflow, `crw flow start land-head --handoff FILE --args '{"plan": P, "milestone": M, "issue": KEY, "head": COMMIT-OR-BRANCH, "authors":
[THREAD, ...]}'`, where `authors` are the subagent threads that wrote the head. It makes no executor, architect, design-audit or reviewer
call and fixes nothing. Its steps:

- `crw.release(plan, node, milestone, {pinned: true, land: {head, authors}})` releases the node to the run with no DESIGN: the head is
  resolved in the run's repository, each author thread is read back from the host (`thread/read`, the configured pair; `Threads` provides
  `ThreadConfig`), the plan the relay holds is compared with the plan the run's `crw.milestone` row read (`plan_changed` otherwise), and
  `Relay.Release` is the agent release with the node's slice and the pair the authors ran on, which the execution row records
  (`author_model`, `author_effort`; `AgentReleaseOptions` of `ReleaseAgent`, the stored form unchanged). The head is then checked out,
  detached, in `<state>/flow/<run>/land-wt/<node>-<commit>`, the worktree the row names and the execution's handle points at, and the row
  records the commit, its tree, its base (the merge-base with the integration tip), the paths it changes and the authors with their pairs.
- The gates are the lane's: `crw.verify` of that head (verification-record/1 of the exact tree), one unit check in the committed mode,
  `crw.unitCheck(node, null, {commit: head, base: head.base})` (CRW-1468): the review tool run in Go in a separate tool invocation over
  `base..head` of the land checkout, with no reviewer call or agent thread id; a head made outside a run has no executor, so the check's row records the receipt `no_executor`. A same-thread agent review, if actually observed, is warning-only and recorded, and the run continues. Model family is provenance only. The
  tool is a Codex review by default, `codex review --base <base>` in the clean checkout of the head (Codex reviews the diff
  from the merge-base of base and HEAD, which in that checkout is `base..head`; its answer does not name the range, and it is given no
  prompt), and
  Bugbot on a Codex quota, with the same settings and fallback record as the node lane's check (CRW-1517). Bugbot's committed reader
  takes an answer only when it names the requested range `base..head` by its commit ids and no other range, so a clean Bugbot answer
  that names no range or another one fails closed as `unit_check_invalid`. Then
  `crw.land` (`crw.accept` on that check, the relay's agent acceptance; model family is provenance only, while the review uses a separate
  tool invocation. An observed same-thread review is warning-only and recorded, and acceptance continues; then `crw.integrate`). They write the rows they write for a node of a project lane.
- The only refusals are input errors (no head, a head git does not resolve, and an issue that is not a node of the milestone) and the gates.
  Historical records may still contain `independence_refused`, `independence_unproven` or `land_author_families_mixed`; readers preserve
  those reasons as recorded outcomes, without applying them to new work. A land one of whose named authors is not read back records no author pair on
  the execution (`land_author_pair_unrecorded`, a warning of the row: the observed pair would stand for authors the run does not know),
  and missing author model information is kept as a warning/evidence; it does not create a family-based independence refusal. Historical
  `independence_unproven` remains readable. The pairs the host reads back are trimmed. A verification that fails, a check that
  finds a blocking defect and a land the relay refuses end the run escalated for the child (`land_verification_failed`,
  `land_review_needs_changes` with the ids of the check's findings carried on the escalation, the relay's reason); a check that gives no
  judgment (`unit_check_<state>`) escalates as a configuration fault. The findings of a verified check
  that block nothing land as premerge defects. An author whose pair is not read back, no
  author named and a path outside the node's regions are warnings of the release row (`land_author_unobserved`, `land_authors_unnamed`,
  `land_out_of_scope`). A lost epoch ends the run `stopped` with `epoch_lost`.
- The gate-only lane fixes nothing: the child changes the head and lands it again, in a new run. The fix rounds (CRW-1490):
  `args.fix_round` (0 when not given) is the round of the child's fix this land is, a non-negative integer; any other value (a string,
  a negative or fractional number, null) ends the run `refused` with `fix_round_invalid` at its start, before the milestone is read or
  the node released. A check that needs changes at a round under
  `LAND_FIX_ROUNDS` (2) ends `land_review_needs_changes` with no ask (the child acts), the round and the carried ids on the escalation
  and in the run's value, and the value hands the child a fix packet (`fix`, `landFixPacket`): the findings by id with file, line and
  text, the range as a `git diff` command, the node's criteria, and the land of the fix, a new `crw flow start land-head` run of the
  same node with `fix_round` one more and the authors of this head together with the fixer's thread, since the fix branches from this
  head and its range holds this head's work too. A check that needs changes at `LAND_FIX_ROUNDS` ends `land_fix_rounds` with ask
  `land_fix_rounds`, a question for the parent, and no fix packet. A land that ends escalated gives its
  release back - the escalation carries `end_land`, and `crw.escalate` asks the relay to close the node's execution with `land_ended`
  (`cancelled`, the slot returned in the same transaction) - so the next land is a new release: a new execution of its run, with its
  worktree and the pair of the authors it names. Only the relay's refusal to close an execution that has to stay (its accepted head
  landed or may be in the target, a `disposition_conflict`) is the row's `close_refused`; any other relay error is asked again in place,
  and one that keeps failing is the op's error, so the run ends on it rather than leaving its execution current under a run that has
  ended (CRW-1397). A hold - a verifier the host cannot run now (`verify_unavailable`), or a check the tool gave no judgment for its
  quota or no answer (`crw.hold` with source `unit_check`) - keeps the release, and the run goes on with `crw flow resume`, which asks
  the tool again, or gives the release up with `crw flow cancel`. A node whose current execution another run holds is not released
  to the land: `crw.release` answers `land_node_held` with that run and its execution, before the run claims the plan's coordinator
  epoch, and no gate runs. The answer names what settles it: `crw flow resume` of a run that has not ended, and `crw flow cancel` of
  one that has (the row records `held_run_state` and the warning `land_node_held_by_ended_run`), whose cancel gives up the executions
  the run still holds - the ones its rows show, and the ones the relay records under its run id that its rows do not name. A run that
  ended cancelled no longer holds its node: its run process gave its executions up at its cancelled end (CRW-1573), so a land after
  a cancel during the verification is released, not `land_node_held`.
- A land-head run is a run of the plan: its release claims the plan's coordinator epoch like any run's, so a run of the same plan that
  is still going stops `epoch_lost` at its next fenced write (one run orchestrates one plan). A head made outside a run is landed when
  no other run of its plan is going: `crw flow start land-head` warns of one with the preflight fact `plan_live_run`, which names the
  run, is printed and is recorded in the run's spec, and the start goes on (CRW-1397).
