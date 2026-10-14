# Check records in a relay store

Where a relay holds the assignment, a check's verdicts and summary are recorded against that store, and this file is
how. The procedure that produces the verdicts is [crw-check](../../crw-check/SKILL.md); the commands are in
[codex-session-relay](relay.md). Whether a relay holds the assignment is determined once, by
[Whether a relay holds this assignment](../../crw-canon/references/delivery.md#whether-a-relay-holds-this-assignment),
and not decided again by the check: a check that decides it for itself can reach the opposite answer from the
coordinator running the same assignment, and then one of the two is comparing against a store the other never
registered in. Without a relay, the check compares as `crw-check` describes and none of this applies.

## Pin the comparison to the current revision

Pin the comparison to the revision the relay reports as current, and register the issue's criteria as the canonical set so each verdict is recorded against agreed obligations and names the set it was reviewed against. A superseded revision is not the thing to certify, a newer revision arriving mid-check restarts the comparison at the new current revision rather than completing the old one, and a criteria edit after a review invalidates that review instead of passing it. Read the same revision again against the set now in force and rule on that same event; a fresh execution generation is for when the artifact itself has to change or the event is no longer the current revision.

## Map the dispositions to the registered criteria

The three recordable per-criterion dispositions are verified, needs_changes and unverified
([crw-check](../../crw-check/SKILL.md#compare-criterion-by-criterion) maps each classification to one). What a
relay adds:

An agreed change maps to verified too, but only once the registered criterion itself carries the accepted wording, because a relay review binds to the criteria set as it stood at claim time: certifying the revised obligation against a criterion that still states the original one records a verdict for the wrong obligation. Register the revised wording before claiming. Once an event has been claimed, editing a criterion invalidates that review whether or not a verdict has settled, and claiming is idempotent with one exception: where the set moved under a review of the revision the assignment still stands on, and any verdict already recorded for it is `verified`, a second claim returns `proceed` and rebinds the review to the set now in force, so the same event is read again and ruled again. Where a verdict had already settled, that re-review names the set it read with `--expect-criteria-digest` and records only verified or needs_changes. Everywhere else the second claim still returns `already_claimed` and a fresh execution generation is the route, as [codex-session-relay](relay.md) describes. Leaving a required criterion unmapped is not neutral: a managed `verified` verdict needs every required criterion recorded verified and is refused otherwise. Stopping an assignment is aborted, which is an overall verdict recorded with its reason and never a per-criterion disposition; a re-review cannot record it, so stop the assignment on the relationship instead.

## Queue the audit summary

An audit summary under an existing management assignment goes to the coordination issue of the milestone ([crw-check](../../crw-check/SKILL.md#in-workflow-auditor)). Where a relay holds the assignment, it QUEUES that summary with the event, revision, and verdict it describes; the write itself is the coordinator's own `gh` operation, since the relay holds no credential. Confirm it by reading the comment back and checking this job's own structured record rather than searching the text for words. A failed write is retried on its own and never re-runs the check or re-sends the correction.

## A quiet child is not a finding

A child sitting idle with no goal is not a finding. Since the goal-free default a child ends its turn when only waiting remains, so an idle child whose recorded start policy and wake-path readiness hold is operating normally and its quiet is the design working. What turns that into a finding is the delivery side: a readiness fact that no longer holds, a delivery held terminally, or a recipient whose own goal status the relay treats as blocking. Check those before reporting a stalled child, and keep its idleness and its delivery path as two readings rather than one.

## Missing reach is not a failed criterion

Where a check finds an installation or runtime state it cannot observe, it records that state as unverified, as
[crw-check](../../crw-check/SKILL.md#report-delivery-reach-in-the-check-result) says. Where a relay holds the
assignment, a `needs_changes` verdict requires a registered criterion marked that way with a note, so an observation
request sent through the correction path is either refused or recorded as a criterion failing that nothing failed. Ask
for the observation through that path only where a registered criterion or a separately authorized operation already
covers it.
