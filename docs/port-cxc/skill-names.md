# CXC skill names

The CXC v0.2.40 skill names appear here once. A CRW skill never cites them: its folder is `plugins/crw/skills/crw-<name>`
and its text names only CRW skills. The name table that maps the original names is
[`contract/schema/cxc/name-substitution.json`](../../contract/schema/cxc/name-substitution.json) (`skills`), and the
difference of each ported skill from its original is the record `port/cxc/records/crw-<name>.json`
(`go run -tags dev ./internal/dev/skillport/cmd/skillport check` verifies the copies against the records).

## Ported skills

Every ported skill takes its CXC name with the `cxc-` prefix replaced by `crw-`: `ast-grep`, `dev`, `dev-architecture`,
`dev-backend`, `dev-code-reviewer`, `dev-data`, `dev-debugging`, `dev-devops`, `dev-frontend`, `dev-scaffolding`,
`dev-security`, `dev-testing`, `dev-uiux-design`, `dev-visualizer`, `interview`, `kwrite`, `loop`, `pabcd`, `qa`, `recall`,
`repo-map`, `search`, `worktree-guardian`. The CXC `lunasearch` is a reference of `crw-search`
(`references/lunasearch.md`) and has no skill of its own.

The CXC `loop` is merged into the existing CRW `crw-loop`, which runs the PABCD loop of one task; the parent-goal
procedure of the original is `crw-run` goal mode.

## CXC skills with no CRW counterpart

| CXC skill | In CRW |
| --- | --- |
| `goalplan` | A deprecated redirect stub in the original; not ported. Its subject is merged into `crw-pabcd` and `crw-loop`. |
| `orchestrate` | A deprecated redirect stub in the original; not ported. Its subject is merged into `crw-pabcd` and `crw-loop`. |
| `skill-hub` | A redirect stub in the original; not ported. |
| `dev-diagram-viewer` | A redirect stub in the original; not ported. Its subject is absorbed into `crw-dev-visualizer`. |
| `remote` | Out of scope. |

## Skills CRW wrote

`crw-add-issue`, `crw-canon`, `crw-check`, `crw-define`, `crw-plan`, `crw-refactor`, `crw-run`, `crw-run-initiative`,
`crw-status` and `crw-tidy` have no original and no record. They are listed in `OwnSkills` in
`internal/dev/skillport/record.go`; `TestSkillDirectoryCountAtMost34` holds the number of skill directories at 34 or below.
