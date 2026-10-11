---
name: crw-canon
description: "Reference index for the rules that more than one CRW skill reads: the levels, the Linear model, the release gate, the schedule contract, delivery, execution roles, design explanation, repository governance and the skill family. Never invoked by itself; other skills link to its files."
---

# CRW Canon

The shared library of the plugin. It is reference-only: it has no procedure of its own and is never loaded because a request sounds like it (`allow_implicit_invocation` is off). A skill reads a file here through a link, and a rule that lives here is stated here once and linked everywhere else, so a passage in another skill that disagrees with a file below is out of date.

| File | What it decides | Read by |
|---|---|---|
| [hierarchy.md](references/hierarchy.md) | The levels (manager, initiative lead, project orchestrator, issue child), the one table that maps them to the relay role ids and Jun's Korean words, who owns and writes what at each level, how messages and direct coordination between project orchestrators work, and where each level's size is set | `crw-run`, `crw-run-initiative`, `crw-plan`, `crw-define`, `crw-check`, `crw-status`, `crw-tidy`, `crw-loop`, `crw-pabcd` |
| [linear-model.md](references/linear-model.md) | How goals, projects and issues relate in Linear, which document is canonical where, who writes which record, when an issue is Done, how the implementation repository is resolved | `crw-define`, `crw-plan`, `crw-add-issue`, `crw-run`, `crw-run-initiative`, `crw-status`, `crw-check`, `crw-tidy`, `crw-refactor` |
| [release-gate.md](references/release-gate.md) | When an issue may be released to an issue child, and the one bypass | `crw-add-issue`, `crw-plan`, `crw-run`, `crw-tidy` |
| [schedule-contract.md](references/schedule-contract.md) | The field names, sources and value words of a recorded schedule | `crw-plan`, `crw-status`, `crw-check` |
| [delivery.md](references/delivery.md) | Who owns each operation, how an issue child starts, how a delivery is integrated, what counts as evidence and handoff | `crw-run`, `crw-run-initiative`, `crw-plan`, `crw-status`, `crw-check`, `crw-tidy` |
| [execution-roles.md](references/execution-roles.md) | The relay roles and the agent roles by policy key, and where each role's settings are decided (never a model name) | `crw-run`, `crw-plan`, `crw-refactor` |
| [design-explanation.md](references/design-explanation.md) | What an answer must carry when someone asks how a system works, what it is for, or whether a change helps | `crw-plan` |
| [repo-governance.md](references/repo-governance.md) | How a repository is set up and how it receives agent-authored pull requests: the two rules, with [repo-bootstrap.md](references/repo-bootstrap.md) (rulesets, merge settings, PR limits) and [agent-pr-intake.md](references/agent-pr-intake.md) (identity, draft-first, supersede, intake policy) as their owner files | the manager and the integrator; `crw-dev-devops` links it |
| [skill-family.md](references/skill-family.md) | Which skills exist together, who calls each, how a skill is named under each installation, and the invariants every skill follows | the reader of a skill name in any document, and `docs/plugin-packaging.md` |

Planning keeps its own rules in `crw-plan/references/integrations.md`: how a task resolves and binds its Linear project and how an issue's child pair is chosen.
