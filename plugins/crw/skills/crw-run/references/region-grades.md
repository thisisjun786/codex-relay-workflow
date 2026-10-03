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
directory) are `exclusive` at their own place and nowhere else: they hold the same path (whatever kind or symbol the other region names), the trees that cover it and, for a tree they delete or rename, what lies under it. Two nodes on the
same `.sql` file or the same deleted path run one after the other; a node that deletes a file, one that adds a `.sql` file and one that changes a workflow file are released together with each other and with every node that does not share their place.
A rename is held at the path it leaves, so declare the destination as a region of its own when another node could write it. Only a region declared with `"exclusive": true` holds the whole repository (a repository-wide rename, say): state it
where the work needs it. The shared contract surfaces (`contract/schema`, `contract/golden`, `contract/fixtures`, any `testdata/golden` directory and the CLI spec
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
and a pair released as `local-optimistic` is the pair to watch when the first of them lands.

## At merge time

### Decide the merge order

The queue is first in, first out until it is measured. A departure needs a reason in the coordination record, and the usual ones are these: a candidate whose change another
candidate must read first goes ahead of it (a shared interface, a command, a field), and of two candidates that overlap the one with fewer overlapping hunks goes first, since the other
one's refresh is then the smaller. Read the `release` rule of each. A grade is a declaration and not a forecast: a `mechanical` pair can still conflict as text and a `local` pair can merge cleanly. What the grade says
is who settles a conflict and how, a rule for a mechanical overlap and the child of the later pull request for a local one.
A landing leaves every other open pull request behind; only the candidate about to merge is refreshed ([Refresh the base yourself when only the base
moved](merge-readiness.md#refresh-the-base-yourself-when-only-the-base-moved)).

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

For every merge, write down in the coordination record the grade the overlap was released under, the number of conflicting files and hunks the refresh met, and how long it took to settle.
Those numbers are what a later change of the grades is calibrated on.
