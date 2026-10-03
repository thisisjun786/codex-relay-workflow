# Release by region grade

A DAG plan releases its nodes in parallel. This page says what a declared edit region means for that release, how to declare one, and what the parent does about an overlap once
both branches exist. The scheduler's own description is `docs/relay/dag-scheduler.md` ("Edit regions"); this page is the procedure the parent follows around it.

## The principle

The parent exists to run work in parallel. A region is a signal for the merge order and for the handling of a conflict, not a gate on release. Writing the same file is not a
reason to run two nodes one after the other. What holds a release back is a change two branches cannot both make: the same file renamed or deleted, the same function body rewritten in
two directions, a shared contract surface, or an overlap whose resolution would cost more than the work. Every other overlap is released and settled when the branches meet.
The parent orders the merges and settles the mechanical conflicts by their rule; the child of the later pull request resolves the rest when it refreshes its base.

## The four grades

A region carries a grade. The grade says how an overlap on that place is settled, so declare the grade the change deserves, not the one that gets the node released.

| Grade | Declare it when | Released against an overlapping node | Settled at merge time |
| --- | --- | --- | --- |
| `independent` (the default) | nobody else edits this place | not released: an overlap contradicts the claim and is judged `exclusive` | nothing to settle |
| `mechanical` | the result of the overlap is fixed by a rule: `union` (both sides add rows or lines to one list), `renumber` (a clash of ids or numbers is renumbered) or `regenerate:<command>` (the file is derived; a command rebuilds it) | yes, and left out of the overlap count | the rule is applied to the merged tree and checked by a command |
| `local` | the same file, but another clause, symbol or a small hunk | yes, as `local-optimistic` | the child of the later pull request resolves it when it refreshes its base |
| `exclusive` | a rename or delete of the place, the same function body rewritten differently, a shared contract, or a registry key both add | not released: `defer:edit_overlap` | serial: the later node waits for the earlier head to land |

A rule belongs to a mechanical region and is required: a mechanical region without one is refused. Two mechanical regions on one place are one mechanical overlap only when they name
the same rule; with two rules the overlap is `local`, because neither rule covers the other side's change.

Two lists are applied whatever grade is declared. A rename, a delete and the hotspots (lockfiles, `.github/`, a Makefile or Dockerfile, `.sql` files, a `schema` or `migrations`
directory) hold the whole repository. The shared contract surfaces (`contract/schema`, `contract/golden`, `contract/fixtures`, any `testdata/golden` directory and the CLI spec
`internal/relay/argparse/specs.json`) are `exclusive` on their own place: a command of the CLI spec is the CLI spec, and an overlap whose common place is a tree that holds one of them is
exclusive. Two trees above the CLI spec, or two `testdata` directories, are one such overlap, while a tree and an unlisted file under it keep their own grades. The scheduler reads declarations and not
the repository, so a tree above a package (which may hold a `testdata/golden`) is not looked into: it claims everything under it. Declare a tree that stops short of those paths, or list the files,
where the work does not touch them.

## Declare the regions

Declare before the release, with the grade of each region:

    codex-session-relay --state "$RELAY_STATE" dag-region-declare --plan <plan> --node <node> --actor <you> --regions @regions.json

    [{"repository": "owner/name", "path": "plugins/crw/.codex-plugin/plugin.json", "kind": "file", "grade": "mechanical", "rule": "regenerate:crw-dev ci plugin --record-version"},
     {"repository": "owner/name", "path": "docs/port/refactor-backlog.md", "kind": "file", "grade": "mechanical", "rule": "union"},
     {"repository": "owner/name", "path": "internal/relay/dagsched/ready.go", "kind": "symbol", "key": "Ready", "grade": "local"}]

A region without a grade is `independent`, which is how every earlier declaration reads. A declaration is made before the release; once a node is released its regions are held until its
head lands and a different declaration is refused. A node with no declaration is unknown, and an unknown node overlaps every other.

## Read the judgement

`dag-ready` gives every implementation candidate a `release` object: the `rule` (`independent`, `mechanical`, `local-optimistic` or `defer`), the `overlaps` by grade (the
holders it overlaps, each by the worst grade of their overlap) and the `basis` rows it rests on. A row names the holder, the repository and place, the grade of the overlap, the
grade and rule each side was judged at, and the recent conflict observations of the plan on that place: how many of the latest twenty there are (`recent_observations`), how many
conflicted on it (`recent_conflicts`), and how many conflicted without naming a file in that repository (`unattributed_conflicts`). The pass object adds `overlaps` and `overlap_count` (the local and exclusive overlaps; mechanical ones
are left out of it). `dag-ready --record` keeps the same in the pass.

The observations are evidence and not a second gate: the rule follows the grades. A place that keeps conflicting in the basis is a reason to regrade it at the next declaration,
and a pair released as `local-optimistic` is the pair to watch when the first of them lands. Once the running branches are measured, `dag-ready` also gives the nodes of a conflicting pair a `merge_order` object
([Read the merge order](#read-the-merge-order)).

## At merge time

### Decide the merge order

The queue is first in, first out until it is measured, and a measured conflict is the exception ([Read the merge order](#read-the-merge-order)). A departure needs a reason in the coordination record, and the usual ones are these: a candidate whose change another
candidate must read first goes ahead of it (a shared interface, a command, a field), and of two candidates that overlap the one with fewer overlapping hunks goes first, since the other
one's refresh is then the smaller. Read the `release` rule of each. A grade is a declaration and not a forecast: a `mechanical` pair can still conflict as text and a `local` pair can merge cleanly. What the grade says
is who settles a conflict and how, a rule for a mechanical overlap and the child of the later pull request for a local one.
A landing leaves every other open pull request behind; only the candidate about to merge is refreshed ([Refresh the base yourself when only the base
moved](merge-readiness.md#refresh-the-base-yourself-when-only-the-base-moved)).

### Measure at every landing and every receipt

A grade is a declaration and not a forecast, so the relay measures what the branches really do, with git merge-tree: every pair of the live heads and each live head against the tip of the branch it lands on, recorded with a ledger row, and a conflict on a path a node did not declare marked as
drift. `dag-integration-observe` runs the sweep a landing owes and `dag-accept` the sweep of the receipt it takes in, and `dag-conflict-sweep` runs one by hand. The relay never fetches, so you fetch first, and receipt arrival is approximated by these steps: delivery does not run the scheduler.

1. When a receipt wakes you, run `git fetch` in your checkout, then `dag-conflict-sweep --plan <plan> --actor <you> --trigger receipt --node <node> --target <owner/name>@dev`. A receipt you accept is swept by `dag-accept` as well, but a receipt you do not accept yet (a correction round) is measured only by
   this command. The relay takes a running child's head from the checkout it works in and an accepted node's from its acceptance; `--head <node>=<sha>` names one yourself.
2. After a landing, run `git fetch`, then `dag-integration-observe`: its answer carries `conflict_sweep`. Every member with `status: unmeasured` names what is missing: `commit_missing` (a head the checkout lacks), `tip_unreadable` (the tip is not in the checkout), `head_unknown` (a node whose head the relay cannot know), `checkout_mismatch`
   (the child's checkout is another repository). Fix what it names and run `dag-conflict-sweep` again; measuring the same heads again is a replay, not a second observation.
3. A conflict on a path a node did not declare is `drift` for that node: the declaration did not describe the work. Name the path to the child with the base refresh and declare it for the next node.

### Read the merge order

When the latest measurement of two live nodes shows a conflict that no rule both declared settles (a file settled by `union`, `renumber` or the same `regenerate` command on both sides is not one; a mechanical file with a local symbol inside it is), `dag-ready` puts them in an order: the node with the later place in the merge lane
carries `merge_order.after` (the nodes that land before it) and the earlier one `merge_order.before`, each row with the observation, the conflicting files, the grade, the nodes that did not declare a file, `heads_current` and the lane of the other node. `merge_order.tip` is a node's own conflict with the tip. `pass.order_constraints` counts the pairs, and the
recorded pass keeps each node's object. The order is the merge lane's: an open merge turn by `requested_at`, then an accepted result by when it was accepted, then every other node that holds regions by when its work began. A node the plan paused, and an accepted result that is no longer the node's current one (a correction is open), are in the last group: they cannot be merged as they are.

It is a constraint on the merge and on the base refresh, not on the work. Nothing running is stopped: the children keep their state, disposition and reason, and the relay refuses nothing for it. You act on it when you order the merges:

- Merge the nodes in the order the reading gives, one at a time. Do not merge a node while a node in its `after` has not landed; a different order needs its reason in the coordination record (the order is advisory: first in, first out until measured, and a measured conflict is the exception).
- When the earlier node has landed, send the later candidate back to its child for a base refresh onto the landed head, naming the files of its `after` row. The child resolves a `local` conflict there; a place a rule settles is handled as in [Settle a mechanical overlap](#settle-a-mechanical-overlap). With several nodes in `after`, the refresh comes after the last of them has landed.
- The relay does not tell the children: there is no sibling channel in this build, so the base refresh you send is the notice.
- `heads_current: no` says the measurement was made at other heads than the store holds: measure again before relying on it. `unknown` is every node that is not accepted yet, because the relay does not store a running child's head.
- The constraint goes away by itself when the earlier node has landed, or when a later measurement of the pair is clean. A conflict nobody measured is not seen: without a sweep `dag-ready` shows none.

### Settle a mechanical overlap

Refresh the base as that section says. A clean merge needs nothing more. When the forge reports a conflict, the update did not happen and the parent does not resolve a conflicted merge
itself: the correction goes to the child and names the rule of the region for each conflicting place, which is "the rule the assignment names for that overlap" of the `mechanical`
kind in the [base refresh kinds](task-packet.md#what-a-handoff-discloses). The child resolves each hunk by that rule only and lists the hunks, and the parent reproduces them
([the check on a refresh the child made](merge-readiness.md)): for `union` every row of both sides is still present and none is added; for `renumber` no id clashes and no other id moved;
for `regenerate:<command>` the command is run on the merged tree and its output equals the file byte for byte. A hunk no declared rule covers is `manual`.

### Send a candidate back to its child

A candidate goes back for a conflict in a `local` or `exclusive` place, a mechanical hunk that its rule does not cover or that does not reproduce, a conflict in a file outside
every declared region (the declaration did not describe the work), and a refusal of the refresh check. The correction is a base refresh and not new scope, and names the landed head and
the places that conflict.

Two candidates that changed the same function body in two directions are not a merge problem. Stop the later one, give its child the landed head as the new baseline, and decide with the plan's owner whether
the two nodes become one, one node is redefined, or the later work is dropped as a duplicate. Do not merge both.

### Record what happened

For every merge, write down in the coordination record the grade the overlap was released under, the observation the order rested on (the `observation_id` of its `merge_order` row), the number of conflicting files and hunks the refresh met, and how long it took to settle.
Those numbers are what a later change of the grades is calibrated on.
