# Skill family

The skills of the plugin by the job they do and the level that calls them, and how a skill is named under each installation. A skill that names another links to its directory; this file is the one place that says which skills exist together.

| Family | Skills | Called by |
|---|---|---|
| Define and plan | `crw-define`, `crw-plan`, `crw-add-issue` | the initiative lead, the manager, or the user; `crw-add-issue` also by the project orchestrator |
| Run | `crw-run` | an initiative lead or a project orchestrator, at the level its task is bound to |
| Report and check | `crw-next`, `crw-status`, `crw-check`, `crw-logic`, `crw-tidy`, `crw-refactor` | any level, read-only unless the skill says what it writes |
| One task | `crw-loop`, `crw-pabcd`, `crw-interview` | an issue child, or a task without a parent |
| Development surfaces | `crw-dev`, the `crw-dev-*` skills, `crw-qa` | the task that edits the code |
| Tools | `crw-search`, `crw-lunasearch`, `crw-recall`, `crw-repo-map`, `crw-ast-grep`, `crw-kwrite`, `crw-worktree-guardian` | any task that needs the tool |
| Shared references | `crw-canon` | never called: it is read through the links of the other skills |

## Skill names under each installation

These documents name the skills without a prefix, which is how a linked
installation exposes them. A plugin installation namespaces every skill under the
plugin, so `crw-run` is offered to the model as `crw:crw-run` and the same mapping
applies to each name above. Read a name written here as whichever spelling the
current installation exposes, and use the exposed spelling when invoking a skill
or telling a user how to invoke one. The directory name, the file layout, and the
relative links between these documents are identical in both installations.
