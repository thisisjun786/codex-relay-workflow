# Execution roles

The roles a task or a helper runs as, named by their policy keys, and where each one's settings are decided. This file names no model and no reasoning effort: a document that carries one is a second source of truth beside the host's execution policy. The levels the keys belong to are in [hierarchy.md](hierarchy.md).

## Execution settings by role

Each level runs on the model and reasoning effort decided for its role, and which pair belongs to which role is a recorded product decision rather than something a task infers. Two roles may be decided onto the same pair; each is still read and checked as its own role. The model of the `supervisor` role is Jun's own selection and is never propagated, changed by automation, or copied to the level below it; the pairs of `parent` and `child` come from the role policy, which may declare more than one pair for a role.

Two kinds of key are in the table. The relay roles `supervisor`, `parent` and `child` are the levels of [hierarchy.md](hierarchy.md): a task bound to an initiative, a project or an issue. The agent roles `architect`, `executor`, `explorer` and `reviewer` are the helpers a task dispatches with `crw role helper dispatch`; their prompts are `internal/role/agents/<key>.toml`, and a helper is never a level.

| Policy key | Kind | Role | Settings come from | Who decides |
|---|---|---|---|---|
| `supervisor` | relay role | initiative lead, and the manager's store seat | its own recorded setting | Jun, directly |
| `parent` | relay role | project orchestrator | the role policy's `parent` entry | the policy |
| `child` | relay role | issue child | one of the pairs the policy declares for `child`, chosen per issue | the policy for which pairs exist; [Child pair by issue type](../../crw-plan/references/integrations.md#child-pair-by-issue-type) for which issue gets which |
| `architect` | agent role | proposes a design and checks a plan against the repository, read-only | the policy's `architect` entry | the policy |
| `executor` | agent role | makes one bounded change inside a stated write scope | the policy's `executor` entry | the policy |
| `explorer` | agent role | answers a scoped question about the code, read-only | the policy's `explorer` entry | the policy |
| `reviewer` | agent role | reviews a diff or a plan adversarially, read-only | the policy's `reviewer` entry | the policy |

Read the pair rather than remember it. The host's execution policy declares the pair of each role, or several pairs for a role, and `get_capabilities` reports what it declares, so a creation states a pair that policy reports for the role it is creating (for a role that lists several, one entry of its `pairs`) and passes the role itself so the host checks the answer. Never carry a pair from memory, from another project, or from a document, this one included: a pair here would be named by a model and an effort, and this file carries neither. That habit is what the failure was made of: a coordinator created a project orchestrator on a model it remembered, and another retried a withheld send by changing the model and keeping the previous effort. Reasoning-effort names are catalog values belonging to their own model; two that look similar are not interchangeable, and nothing maps one onto another.

A role that lists several pairs ranks none of them. The policy says which pairs a task may run on and not which one it does run on; for `child` the issue decides, under [Child pair by issue type](../../crw-plan/references/integrations.md#child-pair-by-issue-type).

A role the host's policy does not declare, or a pair it does not list for the role, is a blocker to report, and its recovery is declaring it in that host's execution policy. There is no fallback pair, because a fallback is a default and a default is the second source of truth this arrangement exists to remove.

Changing an existing task's model is a user action plus a re-record, never something a message performs. After the user changes it, the authorization recorded for that task is re-recorded from a user-attributed source before the next send, because the record is what a send verifies against and a value observed on the host is evidence of what the task is running rather than a new approval. Until then the send is refused naming the record, and a task the host reports as not loaded is left alone rather than resumed under a pair that was never checked against its role.
