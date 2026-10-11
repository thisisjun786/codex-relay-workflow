# crw-dev-visualizer: edit target and upstream tracking

`plugins/crw/skills/crw-dev-visualizer` is the only place the visualizer is edited. A fix to its
instructions, scripts, templates or tests is made and verified there, with the tests under
`plugins/crw/skills/crw-dev-visualizer/tests` (the `skill-scripts-node` CI job runs them). The skill is a ported
skill: every difference from the CXC v0.2.40 original is recorded in `port/cxc/records/crw-dev-visualizer.json`
by `skillport edits`, and `skillport check` verifies the copy against that record.

The skill has no dependency on a standalone visualizer repository. Its reader-document rules are the one file
`plugins/crw/skills/crw-dev/references/reader-documents.md`, which the skill links.

## Host visualize skill tracking

The host's own `visualize` skill owns inline delivery, and the skill delegates to it
(`references/visualize-contract.md`). Two files here record which version of that host skill was last read and
check the installed one against the record. They are not part of the skill and the skill does not link them.

| File | Role |
| --- | --- |
| `visualize-upstream.md` | The last inspection: the host skill's path rule, SHA-256, version and date |
| `sync-check.sh` | Compares the installed host skill with that record (`bash docs/port-cxc/visualizer/sync-check.sh`) |
| `sync-check.test.mjs` | The helper's test (`node --test docs/port-cxc/visualizer/sync-check.test.mjs`) |

A mismatch means the current host instructions have to be read again; they are never copied into the skill.
A missing local cache is an availability result and not a broken environment. The helper selects the host cache
with `CXC_VISUALIZE_ROOT`, a name the port's name table keeps.
