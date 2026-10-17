# Naming work

How a piece of work is called in a report, an instruction, a handoff, a comment, a commit, a branch and a pull request. Every session that points at work uses the one name below, so two sessions never take one piece of work for two and a person reading a report knows which work it means. The layers that write and read these names are in [hierarchy.md](hierarchy.md#the-levels); the tracking identifiers are in [tracking-model.md](tracking-model.md).

## The rules

1. The name of a piece of work is its identifier in the tracking repository: `OWNER/NAME#N` as [tracking-model.md](tracking-model.md) defines it, and `CRW-NNNN` while the work is tracked under that key. A report, an instruction, a handoff, a comment, a commit, a branch and a pull request title all use this name.
2. A short alias follows the identifier in parentheses and never stands alone: `CRW-1436(R2)`. No new alias is made outside an issue title.
3. A finding that has no issue yet is written `<source identifier>#F<number>`, where the number is the finding's number in the record of the source. Once the finding is registered it is called by the new identifier, and the issue body states the original F number.
4. Git carries the identifier too. A commit title is `<identifier>: <what>`, a lane branch is `codex/crw-NNNN-<short name>` (the identifier of the bundle when the lane carries a bundle), and a pull request title is `<identifier> <bundle title>`. A name that already exists is not changed.
5. What is said to a person (the top session to Jun, the parent to the top session) gives the identifier and a few words on what the work does, for example `CRW-1436(evidence handed to the reviewer)`. It names the layers by noun (top session, parent, child, subagent) as [the level table](hierarchy.md#the-levels) fixes them. A role id is a name the code keeps, not a name of work or of a layer, and does not appear in what a person reads.
6. A record mark is not a name of work: an integration tag such as `cc-q...` and a message mark stay as they are. Where the text points at the work itself, it follows rule 1.

## What the checks do

Rule 4 is checked for the commit title only. The commit-title test of `crw-dev ci validate` reads the titles of the commits a range adds and warns, in a line of its own for each title outside the rule, and never refuses the run: a commit title is cheap to correct and a hard gate on it would stop work that can be undone. Branch names and pull request titles are not read. The other rules are followed, not enforced.

## Scope

The rules apply to what is written after they are adopted. A record that is already written, a branch that already exists and an issue title are not rewritten to fit them.

## Task work names (CRW-1523)

These are the canonical **recommendations for owner confirmation** in CRW-1518.
They name one task's work and records. They do not activate new commands, skills,
hook names or paths: use the existing spellings until the migration is approved
and implemented. CRW-1523 changes this name map and its document check only.
CRW-1524 owns retention decisions and CRW-1525 owns migration and compatibility.
An archived receipt, oracle, upstream quotation or code identifier is not an
instruction to rename that artifact.

Keep the new flow's stage names separate: 수령, 탐색, 설계, 비평, 구현, 단위 점검,
크로스체크, PR and 머지 (in English discussion: intake, explore, design, critic,
implement, unit check, crosscheck, PR and merge). None of the recommendations below names a replacement for those
stages. The old letter labels are translated only when describing old single-task
work or its records; they do not prescribe a second workflow or revive the
retired loop. No new initialism replaces the old one.

### Inventory boundary

Counts are frozen at dev `73aa64cacba835eea3a8f1edf3908ff268b09a90`, before this
section was added. The input is the tracked regular text blobs of that Git tree;
binary blobs and symlinks are excluded. Counts are lexical occurrences, including
code, tests, fixtures and historical documents, not distinct behaviors or a list
of files to rename. A file is counted once per spelling; multiple matches on one
line count separately. Rows overlap and must not be added together.

The case-insensitive substring `pabcd` occurs in **1,315 files / 6,884 matches**
on 5,819 matching lines. The exact lowercase substring occurs 5,787 times. The
case-insensitive totals can be reproduced with `git grep -I -i -l -e pabcd <sha>`
for files and `git grep -I -i -o -e pabcd <sha>` for matches. In the tables, names
use case-insensitive token boundaries (letters, digits, underscore and hyphen
belong to the token), commands allow whitespace between words, and paths use
case-sensitive literal spelling. The phase table has its own narrower scope.

| Area containing `pabcd` | Files | Matches |
|---|---:|---:|
| `internal/pabcd/` | 400 | 1,844 |
| `contract/fixtures/` | 299 | 616 |
| Legacy port documentation | 172 | 1,331 |
| `plugins/crw/` | 60 | 319 |

`internal/pabcd/` still has 19 immediate Go package directories: attest, cli,
crwdir, evidence, fsm, gate, goalplan, hook, host, interview, metric, projectcfg,
reset, review, shellir, source, state, stateroot and text. Production Go files
under `internal/flow/` still import crwdir, evidence, host, source and text from
that namespace. This table does not rename those imports or change package boundaries.

### Public names, skills and records

The first column is also the coverage key for the document check. Capitalization
variants share a row; `crw:crw-pabcd` uses the `crw-pabcd` row. The skill prefix
qualification follows the existing installation convention.

| Legacy name | Canonical recommendation | Meaning / boundary | Other candidate (why not) | Uses (files / matches) |
|---|---|---|---|---|
| `PABCD` | `task work` | The work and completion evidence for one task. | `workflow`: confuses this scope with the project flow. | 934 / 3,363 |
| `IPABCD` | `task work` | The same task work when clarification was included. | `development cycle`: suggests retaining the retired cycle. | 39 / 111 |
| `pabcd-state` | `work-state` | The hook's task-state name; not a flow stage. | `flow-state`: collides with the flow run's state. | 520 / 1,350 |
| `pabcd-hooks` | `work hooks` | The human name for the task-state hook family. | `workflow hooks`: too broad for this family. | 1 / 1 |
| `pabcd-only` | `task work only` | A scope qualifier in text. | `phase only`: does not identify which scope. | 9 / 10 |
| `pabcd-phase` | `work step` | A label in a legacy task record. | `flow stage`: would claim a new-flow stage. | 2 / 2 |
| `crw-pabcd` | `crw-work` | The skill for one task's work. | `crw-flow`: confuses task guidance with the flow engine. | 99 / 399 |
| `crw-loop` | `crw-continue` | Continue the authorized task to its stated completion condition. | `crw-auto`: suggests authority the user did not grant. | 106 / 715 |
| `.crw/sessions` | `.crw/work/sessions` | Task records, scoped away from native chat-session storage. | `.crw/runs`: confuses these records with flow runs. | 48 / 221 |
| `ledger.jsonl` | `.crw/work/events.jsonl` | The legacy `.crw/ledger.jsonl` task-event history. | `history.jsonl`: hides that the rows are events. | 146 / 335 |

The ledger count is the literal filename anywhere in the tree; it is not a
claim that every file with that basename is this task ledger. Moving a path,
rewriting a persisted phase, changing an installed skill or a hook registration
requires the migration work; this recommendation performs none of those actions.

### Legacy phase labels

These counts cover only Markdown under
`plugins/crw/skills/crw-pabcd/` and `plugins/crw/skills/crw-loop/`.
Each is the case-sensitive standalone letter or `IDLE` token, using
the Python regular expression `(?<![A-Za-z0-9_])<label>(?![A-Za-z0-9_])`. They are reference-spelling
counts, not FSM transition counts; global English letters and Go identifiers
are not a sensible phase inventory.

| Legacy name | Canonical recommendation | Meaning / boundary | Other candidate (why not) | Uses (files / matches) |
|---|---|---|---|---|
| `I` | `clarify` | Resolve the task's requirements; old Interview. | `intake`: reserved for the new flow's reception stage. | 7 / 38 |
| `P` | `outline` | Describe this task's approach; old Plan. | `design`: already a new-flow stage. | 15 / 96 |
| `A` | `assess` | Judge that outline; old Audit. | `critic`: already a new-flow stage and role. | 16 / 188 |
| `B` | `change` | Make the task's changes; old Build. | `implement`: already a new-flow stage. | 10 / 43 |
| `C` | `validate` | Establish the task's completion evidence; old Check. | `unit check`: only one of the new flow's checks. | 9 / 49 |
| `D` | `finish` | Record completion and end this task step; old Done. | `merge`: completion does not itself authorize integration. | 9 / 50 |
| `IDLE` | `inactive` | No task step is active; a resting state, not an action. | `ready`: can imply readiness to dispatch work. | 5 / 21 |

### Command namespace

`internal/harness/pabcd.go` registers 13 current verbs. The table covers every
one; an existing plain word is kept when it already describes its job. The
recommendation changes the namespace and the four opaque or conflicting labels,
not the commands' authority, options or side effects. Arguments and compatibility
for the phase-control command belong to the migration specification.

| Legacy name | Canonical recommendation | Meaning / boundary | Other candidate (why not) | Uses (files / matches) |
|---|---|---|---|---|
| `crw pabcd` | `crw work` | The task-work command namespace, including its help. | `crw flow`: already owns project runs. | 170 / 593 |
| `crw pabcd orchestrate` | `crw work state` | Inspect or advance the stored task state. | `orchestrate`: suggests control of other sessions. | 75 / 197 |
| `crw pabcd loop` | `crw work continue` | Configure continuation of the authorized task. | `auto`: hides the scope and permission limits. | 41 / 118 |
| `crw pabcd plan` | `crw work outline` | Task-plan artifacts, not milestone decomposition. | `design`: already a new-flow stage. | 7 / 12 |
| `crw pabcd review-round` | `crw work assessment` | The outline's assessment-round record. | `critic`: would reuse the new-flow stage name. | 6 / 11 |
| `crw pabcd freeze` | `crw work freeze` | Freeze the relevant task artifact. | `lock`: confuses artifact freezing with process locks. | 4 / 10 |
| `crw pabcd receipt` | `crw work receipt` | Record or inspect a task receipt. | `proof`: wider than the receipt format. | 21 / 32 |
| `crw pabcd evidence` | `crw work evidence` | Task evidence and its assignment. | `check`: confuses stored evidence with a checking stage. | 9 / 17 |
| `crw pabcd memory` | `crw work memory` | The existing task-memory operation. | `recall`: already has a top-level command. | 10 / 66 |
| `crw pabcd reset` | `crw work reset` | The existing explicit reset operation. | `clean`: obscures that state can be reset. | 14 / 22 |
| `crw pabcd config` | `crw work config` | Task-work configuration. | `settings`: needless spelling drift for the same operation. | 8 / 40 |
| `crw pabcd scan` | `crw work scan` | The existing scan operation. | `explore`: already a new-flow stage. | 11 / 20 |
| `crw pabcd metric` | `crw work metric` | Record or inspect a task metric. | `score`: suggests every metric is an evaluation score. | 4 / 10 |
| `crw pabcd divergence` | `crw work divergence` | The existing alternative-direction record. | `replan`: confuses it with project replanning. | 4 / 14 |

### Document check

The CRW-1523 document test checks the required map, including every registered
command, and scans tracked Markdown for unmapped public spellings. It reports
unmapped names as `WARNING` with a source location; a missing required mapping or
empty recommendation is a document error. Mapped spellings pass. The scanner
keeps shell examples and visible labels, ignores Markdown link destinations,
source-code fences and path/code identifiers, and treats phase selectors only
in explicit `phase X` or `crw pabcd orchestrate X` context. It is an advisory
name inventory, not a runtime migration or proof of workflow behavior.

The frozen counts above are not regenerated by that check and do not include
this section. A later inventory must name its own base commit rather than
silently changing these numbers.
