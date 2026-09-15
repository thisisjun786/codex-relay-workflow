# Official Codex worktree verification

Read before proposing the official Codex managed worktree as a creation path for
child tasks, or before recording a verdict about it. This reference owns the probe
procedure, the evidence format and the judgment rule. It does not select a default
creation path, and it does not restate bridge launch or the relay procedure, which
stay in [bridge.md](bridge.md) and [relay.md](relay.md).

Every measured observation must carry its date, client and version, because a
published default or a feature stage can change between builds. The measurements
reported below were taken on 2026-09-15 against Codex 0.154.0, on a remote-SSH Linux
host whose desktop client was not inspectable from that host. Record your own
environment beside your result.

Re-run the probe rather than inheriting a verdict, but only when the current scope
authorizes what it does: this procedure creates and resumes tasks, writes and pushes,
performs relay mutations and triggers retention cleanup. Under a plan-only, read-only
or no-create scope, reuse still-applicable evidence or report the path as unverified.
Unverified is an allowed outcome here; running the probe outside the authorized scope
is not, and nothing below overrides the caller's restriction.

## Keep four surfaces separate

A negative on one surface is not a negative on the product. Name the surface before
recording any availability claim.

| Surface | Who performs creation | Reachable from inside a task |
|---|---|---|
| Desktop client worktree flow | The ChatGPT desktop app, from the new chat composer | No. Client-side and not inspectable from a remote host |
| Model-visible app tools | Whatever dynamic tools the host exposes to the running model | Only when that namespace is actually exposed |
| App Server thread protocol | The backend that accepts thread creation and resume | Yes, and its generated schema is checkable |
| CLI experimental feature | The local Codex binary under an experimental flag | Yes, when the flag is passed for that invocation |

An absent tool namespace is an exposure limit. A schema without a field is a
statement about that protocol version. Neither one proves the product lacks the
feature. Confirm product capability against the official documentation, and mark
anything the access path cannot reach as unverified rather than absent.

## Distinguish the identifiers

App Server `projectId` on a thread record, the desktop saved-project identity, the
working directory, and a user-defined sidebar section are four different things.
Grouping into a custom section, pinning, or renaming a task is not project
association. When `projectId` reads null for every thread on a host, including tasks
created directly in the desktop app, that field proves nothing in either direction
and the grouping question must be settled on the client.

## Probe procedure

Run against a disposable fixture repository, never a real checkout.

1. Record the client and binary version, and the effective state of the worktree
   feature. Read the feature stage as well as its value.
2. Generate the App Server protocol schema from the installed build and search it
   for any worktree field. Record the file count searched alongside the result.
3. Create the fixture: one commit, then a staged change, an unstaged change, an
   untracked file, and a gitignored private file. Record the fixture HEAD. Also create
   a throwaway bare repository, add it as the fixture's remote, and confirm one push
   to it succeeds before probing, so that a later push failure cannot be a defect of
   the fixture.
4. Create the worktree through the surface you are actually testing, and record that
   surface and its exact invocation next to every result it produces: for a
   command-line surface, the command and the per-invocation feature flag; for a client
   surface, the UI action taken. Enable the feature for that one invocation only and
   never write it into the shared configuration. Do not carry a result from one
   surface to another.
5. Record the resolved worktree path, the checked out commit, whether HEAD is
   detached, and that the source repository lists the new worktree.
6. Compare the fixture before and after. The source must be unchanged.
7. Inspect the new checkout for each of the four fixture file states, and for the
   ignored file specifically.
8. Read the created thread record for cwd, `projectId`, section, model, reasoning
   effort, approval policy, sandbox, originator and source, and record the workspace
   roots, source repository, base commit, branch and owner too, since the Setting
   preservation verdict uses all of them.
9. With client access, observe where the created task actually appears in the client
   and record the project it is grouped under, kept distinct from any section, pin or
   rename. Without client access, record Project association as unverified: the
   App Server fields in step 8 cannot settle it in either direction.
10. In the created worktree, write a file, commit it, and push the NEW commit to an
    explicit disposable ref on the throwaway remote, naming the destination instead of
    relying on an upstream. A managed worktree is detached, as step 5 records, so a
    bare push fails with a not-currently-on-a-branch error that is easy to misrecord as
    a worktree or permission failure when publish access was in fact available. Record
    which of the three succeeded. Do not infer this from the checkout being writable,
    and do not count a push that only republished a commit that already existed, which
    can succeed while committing cannot.
11. Resume the task through the transport a relay would use, then re-read the FULL set
    the Setting preservation verdict uses rather than only some of the step 8 fields:
    working directory, model, reasoning effort, approval policy, sandbox, workspace
    roots, source repository, base commit, branch and owner. Compare each against its
    recorded creation value, and confirm the working directory is byte-for-byte the
    path creation resolved, not merely some managed worktree and not the source
    checkout.
    Repeat the artifact-production check after resume with a fresh change and a new
    commit, pushed to a second explicit disposable ref so the two attempts stay
    distinguishable on the remote, and record both attempts separately.
12. Exercise the retention path you intend to rely on, on a disposable fixture only:
    trigger the cleanup condition the policy names, then attempt the documented
    recovery, and record what was removed and what came back. If you do not exercise
    it, the lifecycle verdict stays unverified; a documented policy you have not run is
    not a recovery path you have.

Run the whole sequence a second time with an explicit model and effort override, and
carry that run through the resume comparison in step 11 rather than stopping at
creation. An override that is applied at creation and silently lost on resume passes
every creation-only check, and inherited defaults that happen to stay stable will hide
it.

## Write capability inside a managed worktree

A managed worktree keeps its git metadata in the source repository rather than in
the checkout, so editing a file and recording a commit touch different filesystem
locations that a sandbox can treat differently. Measure every permission profile you
would actually give a child, because the outcome differs between them and a single
profile's result does not describe the path.

The measurements in this section come from the command-line surface. The worktree was
created with `codex exec --enable worktrees --worktree`, that is the `worktrees` feature
enabled to true for that invocation only, on Codex 0.154.0, with the permission profile
named in each paragraph below; the resume went through the App Server transport. None
of them was produced on a client surface, so none may be transferred to one, and none
of them describes a different feature configuration.

Measured there with a broad profile, full access and approvals never: the child
created a commit inside the managed worktree, and after being resumed it created a
further commit and published that new commit to a remote. Its approval policy and
sandbox mode were unchanged across the resume. Under that profile, on that host, the
path carried both commit and publish. That records one measured profile and is not a
recommendation to grant it: the agreed per-task permission profile still governs, and
permissions are never widened to make a launch or a delivery succeed.

Measured on the same host and the same command-line surface, with a profile scoped to
the checkout plus temporary directories: writing a file in the checkout returned 0, while staging and committing
returned 128 with a read-only filesystem error on the per-worktree index lock inside
the source repository. Pushing an already-existing ref still succeeded, including
writing a remote-tracking ref into that same source repository, so the restriction is
not uniform across that git directory and the precise rule was not isolated. Treat the
mechanism as unexplained and the outcomes as the finding.

Read the restricted result as a conditional compatibility limit of that profile rather
than as a property of managed worktrees, since the broad profile committed and
published from the same path. Record the write, commit and push outcomes separately
with the profile in force, because they fail independently and a success in one says
nothing about the others. The portable requirement is effective write permission to
every git path the delivery actually needs, demonstrated by producing the artifact
under the profile the child will really run with, rather than inferred from a granted
parent directory.

## Evidence to capture

Keep raw receipts outside the repository and record only the shape here.

- The experiment identity every other receipt inherits: the measurement date, the host
  or environment it ran on, the client and its version, the surface that was tested, and
  the exact invocation used, together with the feature stage and its effective value.
  The host belongs here because the adoption gate is scoped to the current host and the
  permission behaviour recorded above was host-specific. Without these the receipts read as
  portable and can be reused against a different client or creation surface, with
  nothing in them revealing that the verdict no longer applies.
- Schema search result, with the number of definitions searched.
- Creation header showing workdir, model, reasoning effort, approval and sandbox.
- Worktree listing from the source repository, and the resolved checkout path.
- The worktree owner record that binds the checkout to its owning task.
- Three separately labelled snapshots, not two: the source checkout before creation,
  the source checkout after creation, and the NEW checkout after creation. The third is
  what the gate actually consumes, and each snapshot must cover all four fixture states,
  the staged change, the unstaged change, the ordinary untracked file and the ignored
  file. Proving the source was untouched says nothing about what was copied into the
  child. Without the new-checkout snapshot, isolation stays unverified.
- The INTENDED value of every field that verdict consumes, recorded before creation
  from the request, alongside the actual thread record fields at creation and again
  after resume. Without that baseline a wrong value that stays wrong reads as
  preservation, so the verdict stays unverified when the intended values are absent.
  Two fields are exceptions, because creation generates them: the working directory and
  the owning task. For each, record the intended PROPERTY beforehand, then the resolved
  value at creation, then the value after resume. The working directory's property is a
  new managed checkout under the configured root, distinct from the source checkout;
  the owner's property is ownership by the newly created task and by no other. Never
  backfill a value you learned from the run as though it had been requested.
- The effective configured worktree root, read before creation. Without it the
  property above cannot be checked: a registered managed worktree proves only that some
  root was used, not that it was the intended one, and a surface that ignores the
  configuration looks identical in the receipts.
- The client project the task was INTENDED to belong to, recorded before creation,
  and the project it was actually observed under afterwards. Without the pre-creation
  identity, whatever project the task lands in reads as the original one. If client
  access is unavailable, record that and leave Project association unverified; the
  thread record alone cannot stand in for either half.
- The number of managed worktrees present, the surface that created them, and the
  configured retention limit, since the retention verdict depends on all three and a
  bare count settles nothing.
- Two labelled sets of write, commit and push outcomes, one before resume and one
  after, each with the sandbox profile in force when it was attempted. A single
  undifferentiated set cannot satisfy a gate that requires production on both sides of
  the resume. For each set keep the new commit identifier, the destination ref it was
  pushed to, and a readback of that ref from the remote. A recorded success that names
  neither the commit nor where it landed cannot show that the NEW commit reached the
  remote, which is what the gate requires.
- The relay and bridge runtime identity behind every relay receipt: implementation,
  version, how that version was determined, and the consumer interface used, since
  these are external dependencies whose behaviour is not established by the Codex
  version alone.
- The created task identifier and its store or host identity, captured at creation and
  again after resume, since the verdict requires a stable native identifier in the same
  store and a successful resume alone does not show the resumed task is that one.
- Relay receipts for every leg the verdict consumes, not only the outer ones:
  relationship and generation, the issue identity and scope reference the relationship
  was registered under, using the fields the registration contract actually stores, the
  relay's own parent and child settings records, which are separate from the thread's
  creation and resume settings and are consumed by the same verdict, the criteria digest
  the review was bound to, the resume
  result, the emitted event with its revision
  and manifest, the terminal proof and observed turn status, the delivery state with
  attempt count and dispatch evidence, the recorded recipient lifecycle, the claim
  result and acknowledgement, the verdict, and the correction and re-verification
  records. A narrower set cannot support a round-trip pass, and without the criteria
  digest the verdict stays unverified, because a pass that cannot be tied to the
  criteria actually in force at claim time is not a pass.
- Retention: how long this run's evidence must survive, stated before the judgment,
  then the owner the policy assigns, the exact cleanup condition triggered, the
  recovery mechanism used, and whether the policy is even established for the surface
  that created the checkout, together with what was removed and what came back. A
  removal you triggered by hand proves nothing about which owner or policy applies, so
  the lifecycle verdict stays unverified without all of these, and compatible is
  meaningless without the horizon it is compatible with.
- When the gate is satisfied by preserving evidence outside the checkout instead of by
  compatible retention, the completed copy's location, an integrity check for it, and
  the destination's own retention owner, expiry or cleanup conditions and recovery
  guarantees, shown to be compatible with the horizon recorded above. An intention to
  preserve evidence is not preservation, and a copy that was intact when written but
  sits in a temporary or auto-cleaned store has only moved the expiry, not removed it.

## Judgment

Record four verdicts separately. A single summary verdict hides the one that fails.

**Project association.** Passes only when the created task appears under the
original project in the client, with the working directory in the separate
worktree. A section, a pin or a renamed task does not count. When the client cannot
be inspected from the running host, record unverified and name the access that
would settle it.

**Setting preservation.** Passes when the model, reasoning effort, approval policy,
sandbox, workspace roots, source repository, base commit and branch are the intended
ones at creation and unchanged after resume.

The working directory and the owning task are judged differently, because creation
generates both. Their intent is a property, not a literal value: for the checkout, a
new managed checkout under the configured worktree root and distinct from the source
checkout, compared against the root read before creation; for the owner, ownership by
the newly created task and by no other. Check each property against what creation
actually resolved, then require exact equality between those resolved values and the
ones seen after resume. A resume that moves the child back to the source checkout defeats
the isolation the worktree exists for while every other field stays put, and that is
what this catches. Unchanged is not sufficient on
its own: a task created on an unintended branch that stays on it still fails. A default that happens to match is not
preservation. Confirm an explicit override separately from the inherited default.

**Relay round trip.** Passes when the officially created task keeps a stable native
identifier in the same store, accepts a resume through the relay transport, carries
its recorded settings, exposes its artifacts, and completes the sequence of child
delivery, parent acknowledgement, verdict, correction and re-verification. Staged is
not delivered. When delivery stalls, diagnose it with the
delivery preconditions below rather than recording only that it was not delivered. Test the minimum number of round trips the decision needs.

**Lifecycle and retention.** Passes when the retention owner, the automatic cleanup
conditions and the recovery path are known and compatible with how long the evidence
must survive. An automatically managed checkout that can be removed on archive, or
trimmed to a fixed recent count, is unsuitable for evidence that must outlive the
task unless the evidence is preserved outside it.

## Delivery has preconditions beyond emitting

Treat "the receipt was not delivered" as an unfinished diagnosis. Record which
precondition stopped it.

The relay behaviour below was produced by two external runtimes, not by Codex itself,
so each carries its own identity, recorded separately because their versions came from
different places and they were driven through different interfaces.

- Session relay, version 0.1.0, taken from its package metadata because the tool
  exposes no version flag. Consumer interface: its command-line interface, used
  store-only for most commands and with the same-host App Server control socket for the
  commands that require a host connection.
- Thread bridge, version 0.1.0, taken from the version string in its own capability
  report rather than from package metadata. Consumer interface: its MCP tools, used to
  read and resume a thread and to wait on a turn, over that same control socket.

Record the version source per runtime, since one may report its version while the other
does not. [relay.md](relay.md) owns the rule that you check the version before relying
on a refusal reason or a field name; what this reference adds is that the identity
travels with the receipt, because an observation from a different relay or bridge build
does not transfer to yours.

Observed on the same command-line surface, and stated as a sequence rather than a
controlled comparison,
since the later attempt also changed the artifact and its revision hash: a receipt
emitted without a host connection recorded the emitting turn as still in progress and
had no delivery queued for it; re-emitting through a host connection recorded that
turn as completed; and a further attempt reached a final stage with a delivery entry
for the recipient. Whether the turn had already finished at the first emission was
not independently established.

That delivery entry then recorded no send attempt and no dispatch evidence, while the
recipient's settings record was complete and usable, and the relay journal recorded
the withholding against an unknown recipient lifecycle, with the recipient's runtime
status not loaded and its ability to accept input unknown. A separate earlier refusal
blocked acknowledgement because the event was not delivered while the recipient was
busy. Both conditions are recorded distinctly and both concern recipient readiness;
the evidence does not show independent underlying causes, and it does not establish
which process performed the checks or owns sending.

The practical consequence for a child task: settings completeness is not recipient
readiness. Here, the relay recorded the recipient as not loaded, left deliverability
unknown, and withheld sending. Capture, separately: whether the
emitting turn was observed terminal, whether a delivery entry exists, the recorded
recipient lifecycle, and only then the acknowledgement outcome.

### Legs to run, and the receipt each one owes

A verdict on the round trip needs every leg attempted, in order, each leaving its own
receipt. Naming the leg that stopped is the result; "not delivered" is not.

1. Register the relationship, naming BOTH sides as allowed recipients, under the stable
   identity and scope reference that [relay.md](relay.md) requires rather than a
   displayed issue key, which is not unique across workspaces. That contract is owned
   there and is not restated here. Receipt: the relationship identifier, the identity
   and scope reference it was registered under, and the recorded settings for each side.
2. Bind the criteria the review will be judged against. Receipt: the criteria digest.
3. Have the child emit its outcome with the artifact. Receipt: event identifier,
   revision hash, artifact manifest and the stage reached.
4. Establish the emitting turn's terminal status through a host connection. Receipt:
   the terminal proof and the observed turn status.
5. Deliver to the recipient. Receipt: delivery state, attempt count, dispatch
   evidence, and the recipient lifecycle recorded at that moment.
6. Have the parent claim and acknowledge. Receipt: the claim result, the
   acknowledgement proof over the parent's own turn, and the acknowledgement record.
7. Record a verdict. Receipt: the settled verdict and its per-criterion dispositions.
8. For a correction, open the next generation and have the SAME child re-emit.
   Receipt: the new generation, the superseded revision and the new head.
9. Re-verify the new head. Receipt: the settled verdict against the current revision.

Identify the recipient before starting, and use a recipient that is genuinely
reachable. A parent that is itself mid-turn, or a task recorded as not loaded, will
stop the sequence for a reason that has nothing to do with the worktree under test.

## Retention differs by owner

The managed column below is documented policy, not measured behavior. Treat it as
the published contract for the surface it describes, and verify separately whether
it governs worktrees created through any other surface. A count above the documented
limit is not by itself evidence that cleanup is inactive, because the limit is
configurable and some worktrees are exempt. Record the observed count, the creating
surface and the configured limit together, and leave applicability unknown until an
actual removal or an actual retention is observed.

Source for the managed column: the official Worktrees documentation at
learn.chatgpt.com/docs/environments/git-worktrees, section "Worktree cleanup",
retrieved 2026-09-15. Re-read it when re-running the probe, and record the retrieval
date next to the verdict, because a published default can change between versions.

| | Codex managed worktree (documented desktop-app policy) | Task-owned checkout convention |
|---|---|---|
| Location | Under the Codex home worktree root, or a configured root | The agreed worktree directory for the project |
| Cleanup owner | Codex, automatically | The task and its coordinator, manually |
| Automatic removal | Documented: on archive of the associated chat, or to stay within a recent-count limit | None |
| Protection | Documented: pinned, in progress, or permanent worktrees are kept | Explicit retention and locking |

Because the automatic path can remove a checkout, evidence a later reviewer needs
must be written outside it before the task ends. That obligation belongs in the task
packet, not in the checkout.

## Adoption gate

Adopt the official path as a default only when project association, setting
preservation and relay round trip all pass on the current host and version, and
retention is either compatible or covered by preserving evidence outside the
checkout. When association is
unverified, keep the existing creation path and record the exact access that would
settle it. Do not record an unverified leg as a pass, and do not simulate an
official result through another creation path.

Adoption further requires the isolation results to MEET their expectations rather
than merely be recorded. The source checkout must be unchanged after creation, and the
new checkout must contain none of the fixture's uncommitted state, neither the staged
change, the unstaged change, the ordinary untracked file, nor the gitignored private
file, unless that propagation was deliberately requested for this path. A run that
altered the source, that copied a private file, or that carried any uncommitted work
into the child's checkout fails the gate whatever the other verdicts say. The first two
are confidentiality and isolation failures; the third is worse in a default creation
path, because a child that starts from a contaminated baseline can commit source work
nobody asked it to touch.

Adoption also requires the child to actually produce its delivery artifact under the
permissions it will really run with, and to produce it again after a resume. When
delivery includes publishing, require the newly created commit to reach the remote,
not merely a push that republished an existing one. A relay round trip that carries a
diff can pass while a child that owes commits cannot make them, so the delivery
capability is a separate gate rather than something the round trip implies.
