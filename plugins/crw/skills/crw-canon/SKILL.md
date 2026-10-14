---
name: crw-canon
description: "Reference index for the rules that more than one CRW skill reads: the levels, the Linear model, the release gate, the schedule contract, delivery, execution roles, design explanation, repository governance and the skill family. Never invoked by itself; other skills link to its files."
---

# CRW Canon

The shared library of the plugin. It is reference-only: it has no procedure of its own and is never loaded because a request sounds like it (`allow_implicit_invocation` is off). A skill reads a file here through a link, and a rule that lives here is stated here once and linked everywhere else, so a passage in another skill that disagrees with a file below is out of date.

| File | What it decides | Read by |
|---|---|---|
| [hierarchy.md](references/hierarchy.md) | The layers (top session, parent, child, subagents), the one table that maps them to the relay role ids and Jun's Korean words, who owns and writes what at each layer, how messages and direct coordination between children work, and where each size is set | `crw-run`, `crw-plan`, `crw-define`, `crw-check`, `crw-status`, `crw-tidy`, `crw-loop`, `crw-pabcd` |
| [linear-model.md](references/linear-model.md) | How goals, projects and issues relate in Linear, which document is canonical where, who writes which record, when an issue is Done, how the implementation repository is resolved | `crw-define`, `crw-plan`, `crw-add-issue`, `crw-run`, `crw-status`, `crw-check`, `crw-tidy`, `crw-refactor` |
| [release-gate.md](references/release-gate.md) | When an issue may be released to an issue child, and the one bypass | `crw-add-issue`, `crw-plan`, `crw-run`, `crw-tidy` |
| [schedule-contract.md](references/schedule-contract.md) | The field names, sources and value words of a recorded schedule | `crw-plan`, `crw-status`, `crw-check` |
| [delivery.md](references/delivery.md) | Who owns each operation, how an issue child starts, how a delivery is integrated, what counts as evidence and handoff | `crw-run`, `crw-plan`, `crw-status`, `crw-check`, `crw-tidy` |
| [execution-roles.md](references/execution-roles.md) | The relay roles and the agent roles by policy key, and where each role's settings are decided (never a model name) | `crw-run`, `crw-plan`, `crw-refactor` |
| [design-explanation.md](references/design-explanation.md) | What an answer must carry when someone asks how a system works, what it is for, or whether a change helps | `crw-plan` |
| [repo-governance.md](references/repo-governance.md) | How a repository is set up and how it receives agent-authored pull requests: the two rules, with [repo-bootstrap.md](references/repo-bootstrap.md) (rulesets, merge settings, PR limits) and [agent-pr-intake.md](references/agent-pr-intake.md) (identity, draft-first, supersede, intake policy) as their owner files | the parent and the integrator; `crw-dev-devops` links it |
| [flow.md](references/flow.md) | The `crw flow` commands: start with its preview and five preflight facts, ps, show, watch, answer, pause, cancel, resume, recover and the owner thread's status line, and how a run process is taken again from its journal | `crw-run`, `crw-status` |
| [spawn-policy.md](references/spawn-policy.md) | Which agent calls a flow run makes by rule (the structural spawns, the signals and their thresholds, the work done inline), what is recorded for each decision, the two kinds of refusal no option overrides, and the QA calls of the final gate; the rule ids are the constants of `internal/flow/ops/rules.go` | `crw-run`, `crw-status`, `crw-pabcd` |
| [skill-family.md](references/skill-family.md) | Which skills exist together, who calls each, how a skill is named under each installation, and the invariants every skill follows | the reader of a skill name in any document, and `docs/plugin-packaging.md` |

Planning keeps its own rules in `crw-plan/references/integrations.md`: how a task resolves and binds its Linear project and how an issue's child pair is chosen.
