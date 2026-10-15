# Repository governance

Here the parent (repository session) is a layer of [the level table](hierarchy.md#the-levels).

The rules for how a repository is set up and how it receives pull requests from agents. They belong to a repository, not to a task: the parent or the integrator reads this file when a repository is created, when its settings are audited, or when its intake policy is decided. A `child` session (a code id, not a layer) never changes repository settings, and reading these rules authorizes none of the changes they describe (each owner file states its own authority).

CRW's own task branches open no pull request ([delivery](delivery.md#default-dev-integration)); the milestone pull request of a child (project session) ([PR-unit landing](delivery.md#pr-unit-landing)) is merged by the dev queue, never on the forge. So these rules apply to a repository whose delivery path is pull requests: an external contribution, or a repository that receives agent-authored PRs.

| Rule | Severity | Statement |
|------|----------|-----------|
| `DEVOPS-REPO-BOOTSTRAP-01` | DEFAULT | A repository that receives agent PRs is set up ruleset-first: protected integration lines, auto-delete on merge, closed-PR cleanup job, merge-method policy, PR limits, labels and template, each verified by a read-back command. Owner: [repo-bootstrap.md](repo-bootstrap.md). |
| `DEVOPS-AGENT-INTAKE-01` | DEFAULT | Agent-authored PRs enter through a declared intake policy (weak, medium or strong) that names identity, draft rule, review budget, supersede procedure and close conditions. Owner: [agent-pr-intake.md](agent-pr-intake.md). |

Branch hygiene, attached-worktree removal and local garbage collection (`DEVOPS-BRANCH-*`, `DEVOPS-WORKTREE-DIRTY-01`, `DEVOPS-LOCAL-GC-01`) stay with `crw-dev-devops` §2.9, whose rules a task applies while it works: see [branch-lifecycle.md](../../crw-dev-devops/references/branch-lifecycle.md) and [local-gc.md](../../crw-dev-devops/references/local-gc.md).
