# Scenarios

The situations the issue requires, each resolved against [the operations contract](../operations.md).
Every scenario names what was observed, the clause that decides it, the required action, and what
must survive. These are fixtures for `scripts/check_operations_contract.py`, which verifies that each
one cites a real clause and that every normative section of the contract is exercised by at least
one of them.

## S1 New installation on a host that has nothing

Observed: no relay console script, no MCP registration, no state directory, and the current CRW skills not
yet linked.

Clauses: OPS-2.3 for the skill links, OPS-1.1 and OPS-1.3 for the combination, OPS-2.4 for the
install order, OPS-3.1 and OPS-3.2 for where the store goes, OPS-6.1 for the result.

Action: link the skills with the existing standard-library installer, install the runtime as a
separate step from the single compatibility record, register the MCP server through supported
configuration, then fill all six check fields. The store is created once for the whole operating
scope at the default location under the state home, not inside a repository and not in a temporary
directory.

Preserved: nothing exists yet to preserve, which is the only reason this case is simple. It is
listed because every later scenario is defined by what it must not destroy.

## S2 Running the installation again with nothing changed

Observed: every entry point resolves into a recorded checkout, each checkout is on the recorded
commit and tree, `git status --porcelain` is empty for both, every digest matches the record, the
combination each install runs under carries a measured point, and the Codex configuration registers
the command the record names. All four signals are read, because three of them agreeing is what the
next scenario shows is not enough, and a digest agreeing without a point is what S22 shows is not
enough either.

Clauses: OPS-2.2 `own`, OPS-2.3, OPS-1.4.

Action: reuse everything and write nothing. The skill installer reports each link as already linked
and creates nothing, which is what makes rerunning safe. The digests are still re-measured, because
OPS-1.4 ties validity to the event rather than to a date, and the measurement is attached to the
receipt.

Preserved: the store, the existing links, the MCP registration, and every hook trusted hash. A
second run is not a reinstall and must not become one.

This is the positive case the next scenario is contrasted against: `own` is reached here only
because nothing else matched first.

## S3 A component installed by somebody else

Observed: the relay console script resolves to a path outside every recorded checkout, for example
a `pipx` environment or another user's directory.

Clauses: OPS-2.1, OPS-2.2 `foreign`, OPS-6.1.

Action: never overwrite it. Read its digest and the combination it runs under, because sitting
outside the recorded paths does not exempt it from the evidence test. It can be used only when the
digest matches the record and that combination carries a measured point. A digest that matches while
the interpreter differs is a different combination and is reported `unmeasured`, and a digest that
does not match at all is reported `unverified`, in both cases with the expected and the observed
location. Installing a second copy to win the race is not a repair, because the entry point that
actually runs is the one on the path, not the one most recently written.

Preserved: the foreign installation, untouched and unrenamed, and the user's `PATH`.

## S4 A local fork of a dependency

Observed: the recorded checkout is on a different commit, or its working tree is dirty. The digest
may or may not have changed, and assuming it changed is the mistake this scenario exists to
prevent. An uncommitted edit to a README, a test, or anything else outside the package directory
leaves the package digest exactly equal to the digest the record carries for that install, so entry
point, digest and
configuration all look like `own` while the checkout is no longer the revision the record names.

Clauses: OPS-2.2 `fork`, OPS-1.3, OPS-4.1.

Action: read cleanliness and the commit and tree as signals in their own right, not as things the
digest would have told you. Precedence then sends this to `fork` before `own` can match, which is
the whole reason `own` is the residual class. Preserve the fork exactly as it is and never update,
reset, clean or stash it to make a check pass. The class is `fork` and not `own`, so OPS-4.1 refuses
a service start. Reading the refusal off the class is what makes it hold here, because the digest
still matches and a gate keyed on digest mismatch alone would have let this start.

Clearing it means reaching `own`, and recording a measured point alone does not get there: the tree
is still dirty, so precedence sends it to `fork` again before `own` can match. Either the entry point
moves to a recorded install that is already clean and pointed, which leaves this checkout entirely
alone, or the user commits the change themselves and the resulting revision is recorded with its own
point. Both routes keep the rule that verification never mutates a working tree, and neither is the
installer deciding what happens to somebody else's work. The report names the expected revision, the
observed revision, and whether the tree was dirty.

Preserved: the user's uncommitted work, their branch, and their local commits. This is the scenario
where an installer is most tempted to destroy work, so the rule is that verification never mutates
a working tree.

## S5 A runtime update that fails halfway

Observed: a new combination was being installed, one component is replaced, the next step failed,
and an assignment is mid-handover.

Clauses: OPS-2.4, OPS-4.4, OPS-4.5, OPS-1.4.

Action: the entry point is `unverified`, so a service start is blocked rather than attempted with a
half-updated runtime. Reconcile open attempts with the runtime that created them, then either
complete the update and record the new measured point, or restore the previous runtime and confirm
its digest still matches the record and that the combination it restores carries an exercised point.
Those are two readings: the digest says the bytes came back, and only the point says the combination
was ever run. The store is not touched in either direction.

Preserved: the store and every in-flight assignment belonging to every parent, the previous runtime
until the replacement passes its checks, and the user's local forks. A failed update is a runtime
problem, and it must never be resolved by recreating a database.

## S6 The store cannot be reached

Observed: a process cannot write the state directory, or `doctor` reports a `relationships` count of
zero where the packet says an assignment exists.

Clauses: OPS-3.3, OPS-3.4, OPS-3.5, OPS-8.1.

Action: distinguish the two causes before doing anything, because they look alike and their repairs
are opposite. A permission failure means the state directory is outside this task's writable roots,
and that removes every relay command rather than only the ones needing a socket, because each
command opens the store read-write as it starts. So this task runs no relay command at all and an
authorized host-capable process owns every store operation for it, including emitting on its
behalf, with the report saying so rather than implying the task reported for itself. The lasting
repair is to grant the directory at task creation, not to widen permissions afterwards. An empty
store is the opposite problem: the process can write, it is simply pointed somewhere else, since
every command creates a store on open and a mistyped path therefore yields a silent empty one
rather than an error. Confirm the resolved absolute path, and confirm both the environment variable
and the flag agree, since the flag alone moves the store while leaving the adapter's ledger behind.

Preserved: the real store, wherever it is. Nothing is recreated, copied over, or migrated to make a
command succeed, and a receipt that is staged rather than delivered is reported as staged.

## S7 Assigning a checkout to an implementation task

Observed: a coordinator is about to dispatch an implementation task for one issue, and a retained
checkout for that task either exists or must be created.

Clauses: OPS-5.1, OPS-5.2, OPS-5.3, OPS-5.4.

Action: place the checkout at `/home/jun/code-worktrees/<original-project>/<task>` with branch
`codex/<task>`, deriving the project segment from the original repository rather than from the
directory name of the checkout currently open, and reuse an existing checkout belonging to the same
task instead of creating a second one. Record the ownership columns separately from the path: who
created it, which task edits source in it, who owns its git metadata, who owns retention, and which
cleanup is authorized.

Then settle the write split before dispatch rather than discovering it mid-task. If the child's
writable roots cover the checkout but not the repository's git metadata, the coordinator creates
the branch and makes the commits, and the child returns a frozen diff with per-file hashes. The
child does not build an alternative commit store to get around the boundary.

Preserved: dirty work and local forks in a reused checkout, every other worktree's checked-out
branch, and the retained checkout itself, since nothing deletes or archives it automatically and
retention belongs to the parent that asked for it.

## S8 Two parents, two projects, one service

Observed: a parent coordinating one Linear project finishes its last assignment while another
parent, working in a different repository and a different project, still has work in flight on the
same host. The finishing parent would like to shut things down, and a third process tries to start
a service because it sees none of its own.

Clauses: OPS-3.1, OPS-4.1, OPS-4.2, OPS-4.3, OPS-7.1, OPS-7.2, OPS-7.3, OPS-8.3.

Action: the finishing parent marks its own assignment complete and stops there. It does not stop
the service, because the service belongs to the operating scope and another parent is still using
it, and only the scope operator may stop it. The third process does not get a second service: the
lock refuses the start, and that refusal is the correct outcome rather than a reason to pick a
different state directory, which would split the scope into two stores that cannot see each other.

Routing between the two parents uses the bound identifiers, never a display name, a branch name or
a working directory, and both parents may legitimately be working in the same repository. Each
completion goes only to its own assignment's registered parent and each correction only to its
registered child, and neither parent's permissions are consulted for the other's delivery.

What this scenario does not get is a fairness guarantee. OPS-8.3 proposes that delivery be fair per
parent so the parent with several assignments does not starve the parent with one, and that is
the intended behaviour. OPS-12.10 requires current implementation and measurement evidence before
claiming a bound. Routing separates the parents; do not infer a scheduling guarantee from this
scenario or from the historical state of a maintainer's host.

Preserved: the shared service and store, the other parent's in-flight assignments, and both
parents' separate authorized settings. The one thing that must never happen here is one parent's
completion silently ending another parent's work.

## S9 The parent selects a verified waiting mode

Observed: a CRW coordinator has selected event-driven idle handoff for an independent task.
Its registered assignment, running delivery service and parent-resume path are verified.
Later the delivery arrives while the parent is mid-turn, and a second assignment's parent
has meanwhile been paused by the user. Separately, a CRW parent in active observation has a running child
using recorded non-relay dispatch, and its first bounded transport wait times out. Another
issue is already relay-registered to an active parent on a relay that defers busy recipients.

Clauses: OPS-8.1, OPS-8.2, OPS-8.3, OPS-8.4.

Action: the event-driven coordinator finishes its response and returns to idle. When the child's
turn ends normally, a host-capable observer promotes its staged receipt to a delivery and the
service delivers it to that assignment's registered parent, which is what wakes the parent. If the
parent is mid-turn the delivery waits and is retried rather than interrupting it. The paused
parent's delivery also waits, and nothing resumes that task automatically, because resuming work a
user deliberately stopped is the one thing a retry cannot undo.

A native subagent completes inside its parent's own turn and is not this independent-task case.
The active CRW parent keeps its coordination record and continues bounded transport waits
using actual child identifiers. The timeout does not finish the project or justify a resend.
After verifying and integrating the child's delivery, it dispatches the next ready issue within
the agreed scope. A blocked issue holds its dependents, not independent ready work. Without a
usable observation path it records the blocker and resume step instead of promising automatic
progress. A service installation or staged receipt alone does not qualify for idle handoff.

The already registered issue has a delivery-mode blocker: reading its child's result does not
deliver or acknowledge its pending event. Preserve its relationship, owner and artifact, record
the required supported handoff for recovery, and do not bypass its relay verdict or keep waiting
as if time alone could resolve it. For new unassigned issues, inspect ownership before choosing
non-relay dispatch so they never acquire that incompatible registration.

Preserved: the user's decision to pause, the running child when a wait times out, and the honest
distinction between a staged receipt and a delivered one. Any claim about how many parents and
concurrent deliveries this handles states what was actually exercised, and anything else stays
`unmeasured`.

## S10 Two services in one scope, and two scopes that are not in conflict

Observed: on one host, one operator starts a service passing one state directory while another
passes a different one. Separately, a genuinely different host or App Server endpoint starts its own
service. In this synthetic scenario the installed version has only a per-store lock.

Clauses: OPS-4.2, OPS-4.6, OPS-3.1, OPS-12.11.

Action: read the current installation evidence before relying on a guarantee. Given the
scenario's per-store lock, the two
starts in one scope both succeed and neither is refused, while a second start against the same
directory is correctly refused. That is the gap OPS-4.6 is written to close, and until it is
implemented the scope guarantee comes from every participant carrying the same recorded state path,
not from the runtime. Under OPS-4.6 the first case is a refusal naming the scope id, the
authoritative store and the store the caller gave, and competing starts resolve atomically to one
winner rather than to whoever reached a different path first.

The different-scope case is the contrast and it is not a conflict at all: separate scopes run
separate services with separate stores by design, and contention is per scope rather than per host.
A socket path that is renamed or symlinked is still the same scope and must not mint a second one.

Preserved: both existing stores. Closing this gap never merges or rewrites a database, and a scope
that already has two stores is reconciled as a deliberate migration with its own backup.

## S11 A verdict exists, and that is not the same as verification being complete

Observed: a verdict is recorded at the head of an assignment, and someone wants to report
`verificationComplete`.

Clauses: OPS-6.4, OPS-6.1.

Action: test all five conditions rather than the existence of the verdict. It passes when the
disposition is `verified`, every required criterion is recorded verified, the event, revision and
generation are the current head, the criteria digest bound at claim time is the digest in force now,
and the acknowledgement was verified by a host rather than recorded offline as intent. It fails in
three ways worth naming: a verdict of `unverified` or `aborted` is a real judgement that nothing was
verified, so the field is not_verified rather than unknown; a verdict whose criteria digest no
longer matches certified wording nobody is judging by now, so it is re-reviewed against the set in
force before the field passes; and a verdict left with one required criterion at needs_changes
withholds the whole field rather than covering the rest.

Integration is judged separately. Verified work may never be integrated and integrated work may
never have carried this field, so neither is inferred from the other.

Preserved: the record of what was judged. A re-review replaces what the assignment stands on, not
the history of deciding it: the replaced ruling keeps the set it was decided against and is
journalled with both digests, so a later reader can still see which wording was certified when.

## S12 The review lands after the push

Observed: the child has pushed its branch, opened its pull request and marked it ready for review.
Checks are running, and a reviewer leaves findings while the child is still in its own turn.

Clauses: OPS-9.1, OPS-9.2, OPS-5.3.

Action: the child handles the review itself, on the pull request that produced it. It collects the
findings, triages them, fixes what needs fixing with commits on the same branch, replies to a
finding that does not apply and says why, and rechecks. It does not hand a list of unaddressed
findings to the parent and call that delivery, and it does not open a second pull request to escape
the thread, which would leave the reasoning behind and restart the review.

Each finding carries its own trail: the finding, the commit that addressed it, and the recheck that
confirms it. A report that says review was addressed without that trail is not evidence, because the
whole question is which findings were actually answered.

Preserved: the pull request as the single place the review history lives, the reviewer's threads
alongside the commits that answered them, and its ready state, since ordinary fixes are what review
is for and returning to draft would withdraw the request being answered.

## S13 A new head outruns the review

Observed: review passed, and then another commit lands on the branch, whether the child's own fix or
a base update.

Clauses: OPS-9.4, OPS-9.2, OPS-6.4.

Action: the review evidence that the new head supersedes is invalid, and only that evidence. Re-run
what the change actually touched rather than repeating the whole review from the start, and never
present the earlier green result as covering a head it never saw. Completion is judged against the
current head, so a child that reported completion before this commit reports again.

The same rule reaches the relay: a verdict is bound to the revision it reviewed, so a newer revision
means the older verdict certifies something that is no longer current.

Preserved: the earlier review's findings and their answers, which stay valid as history even when
their green result no longer covers the tip.

## S14 A mandatory check never ran

Observed: an optional reviewer is unavailable, and separately a required check has not completed on
the current head.

Clauses: OPS-9.2, OPS-9.3.

Action: the two are not treated alike. The optional reviewer is recorded as unavailable, sufficient
independent review is obtained under the repository's policy, and the work continues; waiting
indefinitely for an optional reviewer is not diligence and produces nothing. The missing mandatory
check is blocked, and the child reports blocked rather than reporting completion with a caveat
attached, because the caveat is the part that gets skimmed. The parent does not merge on a head
whose required checks have not passed.

Preserved: the distinction between what the repository requires and what it merely offers, which is
read from the repository's actual configuration rather than from which reviewers happen to be
present.

## S15 The parent merges, and stops before the release

Observed: the child has reported completion, the pull request is green on its current head, and
merging this branch is known to trigger a deployment.

Clauses: OPS-9.3, OPS-10, OPS-12.14.

Action: the parent checks the Linear criteria and the pull request's latest diff, base, head, checks
and review resolution. Then, because merging this branch is known to trigger a deployment, it
obtains the user's approval for that deployment before merging. The order is the substance of this
scenario: the merge is what fires the trigger, so approval asked afterwards is approval for
something that already happened. Merge authority was granted for this workflow and deployment
authority was not, and one does not imply the other.

With that approval in hand the parent merges without asking again about the merge itself, and then
verifies the landing rather than trusting an accepted merge request. Where a merge triggers nothing,
the question never arises and the parent merges on its own authority under OPS-12.14, so the trigger
is read from the repository rather than assumed in either direction. A green pull request is not a
relay verdict either; verification happens in the relay against the registered criteria.

Preserved: the user's control over release and deployment, and the separation between a merge that
the parent may decide and a deployment that it may not.

## S16 Creating the next child

Observed: the coordinator is about to create an independent implementation child for an issue whose
delivery is a pull request.

Clauses: OPS-5.5, OPS-5.2, OPS-5.1.

Action: give it enough capability to finish: its checkout and evidence, the git metadata its branch
and commits need, and the network its push, pull request and checks require. Choose a narrower
profile only when that assignment calls for it. The worktree, branch, scope and ownership columns
organize concurrent work; they do not constrain access outside the assigned checkout. The effective
sandbox and permission profile define that access boundary. A narrow sandbox does reduce what the task can
reach, which is a real safety property, so treat it as a trade rather than a free win: a task that
cannot commit also cannot finish its delivery, and the remaining work returns as coordinator effort.

Apply the settings through the creation tool's real arguments and read the returned profile back,
because a prompt asking for capability is not capability. Settle a setting the creation path cannot
apply before the task exists rather than downgrading it quietly.

Preserved: every already-running task's settings, which this clause does not touch.

## S17 A task that is already running under the old restriction

Observed: a task created earlier with workspace-write and no network is mid-assignment. Its writable
roots cover its checkout and its evidence directory, and its worktree's git metadata sits outside
them, so it cannot create a branch or commit.

Clauses: OPS-5.3, OPS-5.5, OPS-9.1.

Action: use the fallback rather than changing anything about the running task. The coordinator
prepares the git metadata and makes the commits, and the child returns a frozen diff against the
recorded baseline with the SHA-256 of that diff and of each changed file, which is then the reviewed
artifact. Where the task also cannot reach a network, the pull request cannot be created from it, so
the title, body and diff references are prepared as private files and reported truthfully as
unpublished rather than described as a pull request that exists.

The new default does not reach backwards. Nobody widens this task's permissions mid-assignment,
nobody bypasses its sandbox, and nobody builds an alternative commit store to get around the
boundary.

Preserved: the running task's agreed settings, its progress, and the honest distinction between
prepared and published.

## S18 The workflow is missing, and the run is not called normal

Observed: a managed assignment is about to run, and the CXC workflow the assignment assumes is not
installed, or its contract is absent, or the installed version does not match what the packet was
written against.

Clauses: OPS-10.2, OPS-10.1, OPS-10.3.

Action: report the condition and stop treating the run as ordinary. Managed execution here always
carries that workflow, so proceeding without it produces work that looks normal while carrying none
of the evidence the workflow exists to produce, which is worse than a visible failure because nobody
goes looking for it. Name what is missing, the version observed against the version expected, and
what that blocks.

Reporting it requires nothing to be installed, upgraded or tested mid-assignment and changes no
permission. Receipt reading, acknowledgement and recovery continue exactly as before. If a format
question comes up while resolving it, read the owning CXC file rather than a paraphrase, since the
packet and report formats are defined there and the adapter that carries them is a separate issue.

Preserved: the running assignment's settings and its existing receipts, acknowledgements and
recovery path. Nothing here authorizes an install, a permission change, or a mid-flight upgrade.

## S19 Each side keeps its own conclusion

Observed: an independent task's own workflow reaches a done state, its pull request is green, and its
parent has not yet claimed or judged the receipt. This CRW coordinator has selected
event-driven idle handoff and verified its delivery and resume path as in S9.

Clauses: OPS-10.1, OPS-8.1, OPS-6.4.

Action: read each fact from its owner. The task's workflow state is that task's conclusion about its
own work and its goal, and the pull request is evidence about the code. Neither is a relay verdict,
which happens against the registered criteria and belongs to the parent. A report that presents an
internal done state as verification is claiming something nobody has decided yet.

This coordinator can stay idle until a meaningful handoff. Parents in active observation mode instead use the
compatible waiting mode in OPS-8.1 and S9; a native subagent wait is not a substitute for either
independent-task path.

Preserved: the separation between a task's own state and the relay's, and the parent's idle time,
which is the point of delegating in the first place.

## S20 Three components, one destination

Observed: an implementation issue is ready for the relay, and another for the bridge. Both need a
place to send a pull request, and the bridge's code originally came from another author's
repository.

Clauses: OPS-11.1, OPS-11.2, OPS-1.5, OPS-12.2.

Action: send both to `github.com/thisisjun786/codex-relay-workflow` on base `dev` after the
owner-authorized snapshot publication has established that destination. Creating the clean public
repository is a separate publication operation, not a reason to create one per component. Preserve
private history separately and never push its branches into the public repository. `dev` is where
pull requests integrate; reaching `main` is a
promotion of `dev` and a release decision, so it is not part of sending a change for review.

Keep the components distinct inside it. Each package keeps its own module and command names, its own
project file and its own tests, so it remains separately buildable, and each keeps its upstream
provenance and licence notice, including the MIT notice of the component that came from elsewhere. A
licence travels with the code, not with the repository it lands in. Workflow instructions stay under
skills and runtime code under packages, and a rule does not migrate between them merely because they
now share a commit.

Identity needs more than the repository commit once three components share it, since every package
moves whenever any of them changes. Record the subdirectory path and its tree hash alongside the
repository commit, the retained upstream provenance, and the digest of the bytes actually installed.

Preserved: each package's separate build and test surface, its upstream anchor, and its licence.

## S21 The destination is decided and nothing has moved

Observed: the destination decision has landed. Someone asks whether the relay is now installed from
the monorepo, and whether the durable store moved with it.

Clauses: OPS-11.3, OPS-11.4, OPS-2.4, OPS-4.5.

Action: answer the four stages separately, because only the first has happened. The source
destination is decided. The source migration that would put code under packages has not run. Runtime
installation and activation are unchanged, so the installed entry points still resolve to the
external checkouts, and a real compatibility record still describes them as dated observations of those
paths. The store has not moved and is not part of any of this.

When the migration does run it preserves the original checkouts, any dirty work and local forks in
them, and their git history, because those are the only record of what the code was before it moved
and the fork classification still needs them. It imports no virtual environment, no session data, no
database and no receipts, since those are operational state that belongs outside every repository and
importing them would publish a person's working history.

Reinstalling from the new location afterwards is a separate update with its own measurement, and
moving the store is a separate migration with its own backup.

Preserved: the external checkouts and their history, the current installation, and the store.

## S22 A recorded install with no qualifying evidence

Observed: the bridge is installed a second time inside the relay's virtual environment. Nothing
conflicts, the entry point resolves into a recorded path, the commit and tree match the record, the
working tree is clean, and the digest equals the one the record carries for that install. The
compatibility record holds no point for the interpreter that environment runs. That is a statement
about the evidence rather than about the past: somebody may well have run it, but no recorded run
binds an execution to these bytes under that interpreter, and an unrecorded run is not evidence.

Clauses: OPS-2.2 `unmeasured`, OPS-1.3, OPS-4.1, OPS-6.1.

Action: classify `unmeasured` and stop there. Precedence has already ruled out `conflict`, `fork` and
`foreign`, and `own` cannot be reached because it needs a measured point and not a matching digest.
So the install is not reused for a host-required command, `installed` reports `not_verified` naming
the combination that has no point, and OPS-4.1 refuses a service start. What clears this install is
running the bridge under that interpreter and recording the point. That clears this install only:
OPS-4.1 refuses on any service-required component that is not `own`, so if a sibling component is
also unmeasured the start stays refused until its combination is exercised too. Adding a point
without running it, or reading the matching digest as the point, is the shortcut OPS-1.3 exists to
refuse, and reinstalling the same bytes would not have produced the missing evidence either. This is
not a fault report: no recorded expectation was contradicted, which is what separates it from S4.

Preserved: both installs untouched, the environment as it stands, the compatibility record unedited,
and the store. The state is left honest rather than converted into a verdict.

## S23 Ready for review comes before the review

Observed: a child has pushed a complete change and wants a hosted review. The pull request is still
a draft, and the plan is to request the review now and mark it ready once the findings are resolved.

Clauses: OPS-9.1, OPS-9.2, OPS-6.1.

Action: mark it ready first, then request the review. The order carries the whole meaning, because a
reviewer that reads a draft as not yet asking will not run, and the absent review is then taken for a
pass nobody gave, which is exactly what OPS-9.2 refuses when it calls a missing mandatory review
blocked rather than complete. Ready for review claims only that the change can be read: it is not the
merge readiness the parent judges later against the criteria and the current head, and it is not the
relay outcome. So the pull request stays ready while reviews are pending and while ordinary fixes
land on it, and it returns to draft only if the change genuinely stops being reviewable, never as a
routine step between rounds.

Preserved: the review request and its threads, the findings already collected on that pull request,
and the separation between entering review, being merge ready, and reporting an outcome.

## S24 Parent coordination finishes without a local implementation diff

Observed: a default CRW parent has no CXC implementation FSM. Every child in its agreed
scope has delivered verified results, required PRs have landed, and no owned work or
receipt remains pending. Its own checkout is unchanged. Separately, another parent
explicitly chose CXC and has a blocked FSM with no supported transition to CRW.

Clauses: OPS-8.1, OPS-8.4, OPS-10.1; lifecycle decisions belong to
[crw-loop](../../../crw-loop/SKILL.md).

Action: the default CRW parent completes its coordination record on the verified scoped
deliveries, without manufacturing a parent-local code change. Child completion alone is
insufficient if any required result, correction, receipt or integration remains unresolved.
The explicit CXC parent retains its installed lifecycle and cannot start CRW execution
while its transition is unsupported. Record the transition blocker and exact supported
resume requirement; do not reset the FSM, edit phases or report a new loop as armed.

Preserved: child identities and delivery evidence, the distinct parent completion boundary,
and the existing CXC parent's binding, goalplan, pending obligations and recovery evidence.
