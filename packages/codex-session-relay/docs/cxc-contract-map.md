# The CXC report contract, as this relay reads it

Jun's managed workflow runs CXC and this relay together, so both have a vocabulary for how
an execution ended. Where the two touch, one of them has to say what the other one means.
This page is that statement. It copies no CXC prose and rebuilds no part of its state
machine: every row points at the installed file that owns the rule, so a version bump is
re-read rather than trusted from a transcription that has quietly gone stale.

Read against **codexclaw 0.2.28+codex.20260914090142**, installed under
`$CODEX_HOME/plugins/cache/codexclaw/`. The version string is reported identically by
`.codex-plugin/plugin.json` and `inventory.json`. `codex_session_relay.cxc.provenance()`
returns the same record at runtime, including the digest of every file below, and
`cxc.affected_by()` names which rules a changed install actually touches, so a bump
re-verifies those rules and their scenarios rather than all of them.

| Rule | Owning file | Anchor | sha256 |
| --- | --- | --- | --- |
| report outcomes are not phases | `skills/loop/SKILL.md` | 146-148 | `61167d15...a44e5e2b` |
| terminal-state vocabulary | `skills/pabcd/references/loop-engineering.md` | 32-40 | `c5fb8c4e...5db67fc7` |
| REVIEW-SYNTHESIS-01 | `skills/pabcd/references/loop-engineering.md` | 41-60 | `c5fb8c4e...5db67fc7` |
| DISPATCH-TASK-01 | `skills/pabcd/references/delegation.md` | 20-22 | `ba773dbd...d0c578b2` |
| plan output, nine concepts | `skills/pabcd/references/plan-output.md` | 7-20 | `4535762a...366c7868` |
| REVIEW-OUTPUT-01 | `skills/dev-code-reviewer/SKILL.md` | 86-95 | `1d66f377...2e5d2d28` |
| ATTEST-EVIDENCE-01 | `skills/pabcd/references/phase-control.md` | 45-48 | `6982abdd...5a8b476a` |
| LOOP-WAIT-EVIDENCE-01 | `skills/loop/references/waiting.md` | 31-63 | `988451de...b15150c8` |
| DISPATCH-SURFACE-01 | `skills/pabcd/references/dispatch-surfaces.md` | 10-20 | `dd3e6721...611c26e98` |

The full digests live in `cxc.SOURCES`; the table abbreviates only for width.

## Report status against relay outcome

A CXC report status does not choose a relay outcome. The outcome is asserted by the child's
completion receipt, which is the only thing the frozen contract lets assert one, and the
status is recorded beside it and checked for compatibility. A status that picked the outcome
could overrule the evidence; a status checked against it cannot.

| CXC status | Means | Compatible relay outcome |
| --- | --- | --- |
| `DONE` | the child proved every recorded criterion | `ready_for_review` |
| `NOOP` | nothing needed doing | `ready_for_review` |
| `BLOCKED` | an external dependency is in the way | `blocked_needs_input` |
| `UNSAFE` | a human risk decision is required | `blocked_needs_input` |
| `NEEDS_HUMAN` | a judgment only the user can make | `blocked_needs_input` |
| `BUDGET_EXHAUSTED` | a stated bound ran out | `interrupted`, `failed` |

Three statuses land on one outcome because the frozen enum has a single slot for a human
having to decide something, and adding a second is not available to this layer. Nothing is
lost: the original word and the reason the child gave are stored and rendered, so a reader
can still tell a blocked dependency from a refused risk. A status this build does not know
is refused by name, with the accepted set and the contract version it was read against,
rather than being mapped to whichever neighbour comes first in a dictionary.

`NOOP` pairs with `ready_for_review` on purpose. A report that nothing needed doing is still
a finding, and the work that established it is the deliverable. This package already refuses
to complete a managed assignment verified against nothing; a no-change claim with nothing
behind it is the same shape of claim.

## The word both systems use for different things

CXC glosses `DONE` as verified success, meaning the child proved its own recorded criteria.
The relay's `verified` is the **parent's** disposition, written through `record_verdict`
against registered criteria on an acknowledged event. Same English word, two authorities.

`cxc.NOT_VERIFICATION` lists the facts that resemble verification and are not, each with the
reason it does not carry:

- a `DONE` report, the child proving its own criteria rather than the parent's verdict
- an opened pull request, a place to review rather than a review
- a review `PASS`, one reviewer's judgment rather than the parent's disposition
- a green required check, evidence for a verdict rather than a verdict
- a completed turn, the trigger to look, as protocol v1 section 2 already says
- a dispatched delivery, neither an acknowledgement nor a verification

## Review verdicts

REVIEW-OUTPUT-01 fixes the final line, and this relay renders exactly that: `VERDICT: PASS`,
`VERDICT: GO-WITH-FIXES (blockers=N)`, `VERDICT: FAIL`. A `GO-WITH-FIXES` without a blocker
count is refused, because it is a `PASS` wearing a hedge, and a count attached to `PASS` or
`FAIL` is refused as a number nobody can act on. A verdict line is rendered only for a
message that carries a review, so an ordinary progress or completion notice cannot be
dressed as a review judgment.

## Waiting

LOOP-WAIT-EVIDENCE-01 keeps five endings apart, and `cxc.classify_wait` returns exactly one
of them: `progress`, `suspected_stagnation`, `confirmed_failure`, `unobservable`,
`input_needed`, and `timed_out` for a wait that simply ended on its bound. A terminal error
outranks everything; a request for input is not a failure; fresh advancing evidence outranks
the clock. No result ever sets `authorisesRerun`, because running the work again is a
decision somebody makes from evidence, and these are the states in which that evidence does
not exist yet. This mirrors what the delivery layer has always done: protocol v1 section 3
says no delivery state is reachable by a timeout, and section 4 says elapsed time is not
affirmative evidence.

## Instruction shape

A revision request is an instruction, so it carries the DISPATCH-TASK-01 fields, `TASK`,
`SCOPE`, `MUST DO`, `MUST NOT`, `PROOF` and `RETURN FORMAT`, plus a decision boundary. It
leads with the criterion that was violated and the anchor that reproduces it, because a
correction whose first line is an identifier is a correction the child has to research
before it can start. The acknowledgement asymmetry is unchanged: contract v1 defines no
acknowledgement for the parent-to-child direction, so the message says so and tells the
child what does work instead.

## What this does not do

It builds no dispatch mechanism, no state machine and no provider retry ladder; CXC owns all
three. It adds no value to any frozen enum: not the receipt outcomes, not the delivery
states, not the verdict dispositions. It does not require the recovery daemon to run a CXC
cycle. And it neither reads nor depends on an installed CXC runtime at delivery time, since
the provenance record above is pinned data rather than a live lookup.
