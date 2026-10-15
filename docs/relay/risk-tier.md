# Risk tier of an issue branch head

This page is the canonical statement of the risk-tier policy (CRW-1041, part a): how the tier of a head is derived, what a decision record says, and how the effect of the tier is measured. The classifier is `internal/risktier`, and the record is `policy-decision/1` (`contract/schema/policy-decision.schema.json`).

## What this adds and what it leaves alone

The classifier and the record exist, and nothing consumes them yet. No gate reads the tier: the pre-merge evaluation, the independent review, the combined-tree verification of the child (project session) and the integrator's full verification run for every head as before, and `premerge-record/1` is unchanged. The record is never a pre-merge record: it has no grader and no score, and a head that is decided low still gets its own `premerge-record/1` and `dag-accept`.

The original request of CRW-1041 was lower gating for low issues: no per-issue evaluation or independent review, one combined verification per batch, and one fix per batch when it fails. That request is not implemented by this change and is not met by it. The decision of [the parent](../../plugins/crw/skills/crw-canon/references/hierarchy.md#the-levels) (repository session) of 10-11 reshapes it: the batch part becomes F15 of the 0.5.0-alpha design (candidate batch review of up to four low heads in one reviewer call, one verdict block each, a per-node `premerge-record/1` and `dag-accept` still produced for every member, a fix per failing member), which builds on this classifier and on F08. The reshaped request differs from the written one in what it still requires per node, and the Linear issue stays open until that difference is accepted in the issue itself.

## The tier

A head has one of two tiers, `low` and `high`.

- Low is a closed list. A path is low only when it is documentation prose (a Markdown file under `docs/`, no part of its path naming a corpus, fixture, golden, oracle, recorded set or security input) or a plain data file (`.json`, `.jsonl`, `.ndjson`, `.txt`, `.md`, `.toml`, `.yaml`, `.yml`, `.csv`) inside a testdata tree that is listed in `lowTestdata` (`internal/risktier/classify.go`), with no part of its path naming a corpus, fixture, golden, oracle, recorded set, security, replay, hook or shell input. The list holds `internal/skill/search/testdata`, whose files are ranking and catalog inputs that the package's tests feed to a pure function; a tree is added one reviewed line at a time, after reading how its tests use it (`TestLowTestdataTreesAreOnlyFedToPureTests` fails when a test of the owning package runs a command or production code reads the tree).
- A testdata file is not low for being under a `testdata` directory with a data extension. Tests replay such files as the expected answers of a security reader (`internal/role/testdata/dispatch/safety.json` for the dispatch ledger's symlink and containment refusals, `internal/pabcd/cli/testdata/boundary.json` for the receipt session, phase and epoch guards) and run their strings as commands (`internal/pabcd/source/session/testdata/scenarios.json`, whose world, setup and script strings go to `sh -ec`), and neither the name nor the extension says which. Every testdata file outside the list is high (rule `testdata_not_listed`), as is a testdata tree region outside it; `TestEveryTestdataFileIsHighUnlessListedLow` walks every testdata file of the repository against a hand-written list of the low ones.
- Everything else is high, including every path nobody listed. A small fix to a source file is not low. The decided answer also names sweeps and small edits outside the high paths as low; a path cannot say that a change is a sweep or small, so those are high until their tree is added to the allowlist, one reviewed line in `internal/risktier/classify.go`.
- Always high, whatever else matches: everything under `internal/relay`, `internal/runtime`, `internal/hookswitch` and `internal/policystore` (relay, runtime, install and cutover, policy, state and ledger code), including a `testdata` file or directory there; the security corpus, the recorded answers that tests replay against the security readers and gates (`internal/dev/cxcfuzz/testdata`, among them the worktree deletion guard and the memory write gate corpora, and `internal/pabcd/gate/testdata`), named by tree because their file names carry no reserved word; the shared contract surfaces (`contract/`, `internal/relay/argparse/specs.json`, any `testdata/golden`), executable instruction text (everything under `plugins/`: skills, their reference Markdown, agents, hook wiring; the root documents `AGENTS.md`, `CLAUDE.md`, `POLICY.md`, `SECURITY.md`, and an `AGENTS.md` or `CLAUDE.md` in any directory, under `docs/` or `testdata` too, whatever the case of its name), and a path that is not a clean path inside the repository.
- Reference Markdown of a skill is high because it is under `plugins/`. A page that changes a command procedure or an authority rule is not documentation, and a path cannot tell such a page from a table of names. A skill reference page that is only a table of names reaches a low execution tier through a recorded exception, never through the classification (CRW-700's packet is judged this way).
- A rename or a copy is judged on both paths, a type change is high, and an empty diff is high (it establishes nothing low).

The tier of a head is the larger of three things:

1. `declared_tier`, the author's declaration, or none. An issue that declares nothing is judged on the derived tier only; it is held by nothing in issue-ready.
2. `region_tier`, the highest tier of the declared edit regions. A tree is low only inside `docs/` or inside a listed testdata tree; a file or a symbol is judged as its file.
3. `changed_tier`, the highest tier of the actual changed paths.

`derived_tier` is the larger of the last two, so declared regions and the real diff can only raise each other, and an author can only raise the result. A diff that leaves the declared regions is judged from its actual paths: the paths no region covers are listed in `outside_regions`, and `reclassified` is true when that changed the derived tier from the regions' tier.

## The exception

`tier` is the classification and an exception never changes it. The parent may record an exception: who, why, when, and the `execution_tier` it sets. `execution_tier` is the tier whose gates run; it equals `tier` unless an exception says otherwise, and an exception that equals the tier is refused. An exception does not make a change low in the record: it makes the gates of a low change apply to it, with the name and the reason beside it. A record without a reason or a name is not a record.

## The record

`policy-decision/1` carries the identity (issue, node, head, dev commit, time, decider), the three tiers and the derived ones, every declared region and changed path with its tier and the rule that decided it, `outside_regions`, `reclassified`, the optional `exception`, and `skipped_gates`.

`skipped_gates` lists the gates that were actually skipped for the head, each with a reason, and nothing else. It is empty while no gate reads the tier, and always empty under execution tier high. `premerge_record` and `dag_accept` are never listed: every node still produces both. A shared reviewer call for a batch is not a skipped per-node review, and is not recorded as one.

`internal/risktier.Decode` accepts a record only when it is exactly the record `Decide` gives for the inputs the record carries. The tier, each verdict, the outside list, the execution tier and the skipped gates are recomputed, so a record cannot state a lower tier than its own paths give. A JSON `null` is refused everywhere except `declared_tier` and `region_tier`, which the schema lets be null; decoding does not turn a forbidden null into a false, an empty string or an absent member. Member names are matched exactly, in every object: a name that differs only in case (`TIER`, `PATH`) or an empty optional member (`old_path` or `key` of `""`) is refused as the schema refuses it, not read as the member it resembles or dropped. Where F15 stores the decision (`flow_step_decisions`) is that design's choice; the record is the same document.

What the classifier cannot do: it judges the paths it is given, and the caller must supply the complete diff of the head against the dev commit (an incomplete list can only understate the tier). It reads no file content.

## Measurement protocol for c2

The criterion is a record of the defect rate after merge of low-tier batches, compared with a baseline. It can only be evaluated once a low-tier policy is live.

- The first check is 14 days after the first live low-tier batch; 14 days alone do not prove quality, and the check records what the period was, not a verdict.
- The baseline is like for like: changes of similar kind and size, in the same throughput mode, merged under the full gates. A 30-day all-merge baseline is not comparable.
- A defect counts when it is found after merge and traced to a merged head: a P0 to P2 finding of a later evaluation, a revert, or a follow-up issue that fixes the head. The same counting rule applies to both sides.
- The result is a status artifact with the counts of both sides, their sizes and the rule used. It changes no tier automatically; whether the low-tier policy stays is decided by Jun and the parent.
