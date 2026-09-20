# Start policy and child cap

One adjudication settles how a run starts. It happens after the project baseline and before the
first child is created, and what it decides is recorded where the next session will read it. A
value the adjudication can settle is never put to the user as a question; a value it cannot settle
is asked before anything is created rather than after.

This reference belongs to [crw-run](../SKILL.md#prepare-and-dispatch). The standing cap and its
precedence are in [Default parent start policy](../../crw-plan/references/integrations.md#default-parent-start-policy),
and the goal side of the compatibility field is in
[Parent goal lifecycle](../../crw-loop/references/parent-goal.md#record-the-start-adjudication).
Nothing here adds a store, a questionnaire, a planning document or a scheduler: the fields below
are written under the settings and authorization entries the
[coordination record](task-packet.md#coordination-record) already keeps, and the facts that reach
past this project are read from the records that already own them.

## What is settled, and what is recorded with it

| Field | What it holds |
| --- | --- |
| `run_mode` | `loop`, `goal-free-run` or `blocked`, in the vocabulary [Parent goal lifecycle](../../crw-loop/references/parent-goal.md#record-the-start-adjudication) defines. |
| `child_cap` | The ceiling in force, and every bound that produced the number actually dispatched. |
| `host_compatibility` | The preflight outcome, the installed identities it was read at, and the issue that owns an unresolved blocker. |
| `observation_path` | The waiting mode selected under [OPS-8.1](operations.md#ops-81-parent-continuation-and-waiting), with the evidence that it is available. |
| `operating_scope` | What identifies the scope those host facts were read on: host, OS user and App Server under [OPS-3.1](operations.md#ops-31-the-operating-scope-is-the-sharing-unit). Recorded apart from what identifies this run. |

Every field carries three more things, because a field without them cannot be restored later:
nobody reading it can tell what it was allowed to survive.

- **source:** a host or tool restriction, an explicit limit in force for this request, the
  user's choice for this scope, a decision already recorded for this same project, or the
  standing default.
- **scope:** `host`, `project`, `this-run` or `standing`.
- **valid_while:** the conditions the value stands on, written so a later reader can check them.

## Where a decision reaches, and where it stops

A `host` fact describes the machine, its OS user, the installed components, the hook registration
and its observed rule, and the permission profile. It holds for any project on that operating
scope while the identities it was read at still match.

A `project` decision is that project's: its run mode, and a cap that differs from the standing one.
It holds across sessions of the same project and crosses to no other.

A `this-run` allowance was granted for one execution and expires with it. It is restored while that
run continues and is never promoted. Reading one in another project is reading a decision somebody
made about different work.

A `standing` default applies wherever nothing narrower is recorded. Only the user changes one, and
that change is recorded as a decision at that scope. An allowance granted once does not become one.

Because a project's coordination record is that project's, it is not where another project looks
for a host fact. It holds this run's copy and a pointer. The cross-project home of an unresolved
host blocker is the issue that owns it together with its accepted decision, which is also the
record that says whether the blocker has cleared. Another project reads that owning record.

## Re-read on every entry; re-decide only on a change

A new session, a resume after compaction and a later batch are all re-read events. Each re-reads
the identities in `valid_while` and the limits in force, and that read answers one question: does
anything the recorded adjudication stands on differ now?

Where nothing differs, the recorded values are restored and the run continues. Where something
differs, only the affected field is adjudicated again, under the same precedence, and the
superseded value is kept beside what changed.

A field is adjudicated again when an installed component's version, path or digest differs from the
recorded one; when the hook registration or its observed rule differs; when the permission profile
or the exposed tool set differs; when the operating scope differs; when the user states a limit
that did not apply before; or when the issue owning an unresolved blocker records its resolution.
A new session, a compaction and a later batch satisfy none of those by themselves. A recorded
blocker whose owning record nobody has re-read is a blocker that still stands, not an unknown.

## Ask before, never after

Every field is one of two things, and the run states which.

A field the precedence settles is applied and recorded without a question. Asking the user to
confirm a host restriction, a limit already in force, a choice already made for this scope, a
still-valid decision recorded for this same project, or the standing default is asking again for
an approval already given.

A field the precedence does not settle needs a new decision, and it is asked before the first
creation call. Only the action waiting on the answer is held: reading the baseline, preparing
packets, inspecting existing owners and read-only diagnosis continue while it is pending.

A question raised after the action it governs is a defect to record, not an approval obtained.
The reverse is no remedy either. Nothing here reduces what a run may dispatch, and holding a
question until the work is already done is not a way of avoiding it.

## What bounds the number actually dispatched

The children started in one pass are the lower of the ready independent issues the baseline found
and the cap in force minus this parent's children already live, then reduced by whatever the
parent observed in its operating scope. Record each bound and which one decided the result: a pass
limited by ready work and a pass limited by capacity look identical afterwards and recover
differently.

This parent's own live children always count against its cap. Other parents' children never do;
they inform the observation instead.

That observation is a reduction and only ever lowers the number. Where nothing was observed, no
reduction applies and the observation is recorded as `unmeasured`, which states what was read
rather than claiming the host is safe at the cap. Where an observation shows pressure, such as
another parent's live children, a resource reading, or a tool or permission constraint, the parent
lowers or holds dispatch and records the observation that caused it.

Three claims this does not support:

- The standing cap is a policy default, not a demonstration that a host runs that many children
  safely. [OPS-8.4](operations.md#ops-84-stating-the-scale-that-was-actually-verified) governs
  what may be said about scale.
- A raised file-descriptor limit is a changed ceiling, not evidence that a higher cap is safe. The
  exhaustion's cause and repair belong to [CRW-38](https://linear.app/jun786/issue/CRW-38).
- A parent counting its own slots is bookkeeping inside one parent, not an atomic host-global
  guarantee. Two parents each inside their own cap can jointly exceed what either observed, and
  the per-parent bound in
  [OPS-8.3](operations.md#ops-83-fairness-limits-and-error-isolation-proposed) is proposed rather
  than installed.

## Cases this policy is accepted against

These are the cases the policy is judged by. D is the number from the section above, and no case
creates anything before the adjudication: that count is zero in every row.

| # | Case | Question | Creations after | Effective mode and cap | On the next entry |
| --- | --- | --- | --- | --- | --- |
| 1 | New project, the default applies | none | D | as requested, standing cap | recorded before the first creation |
| 2 | The same conditions in a later session | none | D, the same as case 1 | identical, decided again by nobody | identities re-read, values unchanged |
| 3 | The same project resumed or compacted | none | D | the restored values | restored from the record, not re-decided |
| 4 | A different project on the same host | none for the cap | D | standing cap; case 5 does not reach here | host facts carry, project decisions do not |
| 5 | The user states a limit of four | none | D with the cap in force at four | source is the explicit limit, scope `this-run` | restored while the run lasts, never promoted |
| 6a | `no-create` in force | none | 0 | the mode as adjudicated, nothing created | the limit recorded as the precedence that applied |
| 6b | `no-goal` in force and the request separately covers goal-free Run | none | D | `goal-free-run` under the cap in force | the limit restored; it bars the goal, not the work |
| 7a | The goal is unsupported and no substitute is approved | none | 0 | `blocked`, owning issue cited | the blocker preserved, not re-asked as new |
| 7b | A genuinely new decision is required | asked before any creation | 0 until answered | that action alone held | baseline, packets and read-only diagnosis continue |
| 8 | An installed version or hook rule changed | only if the re-read forces one | D | the changed field re-adjudicated, the rest restored | the changed identity recorded against the superseded value |
| 9 | Children of this parent are already live | none | D, which subtracts them | standing cap minus the live children | the existing owners preserved, never replaced |

An edit does not alter a turn that has already loaded these instructions. It does not stop there,
though: where an installation links this checkout, a later read loads the edited text, and the
re-read events above are exactly such reads, so a run spanning sessions can continue under
instructions it did not start with. The local edit, the pull request, the integration and what is
actually installed and running stay separate facts and are reported separately.
