# Codex Relay Workflow (CRW)

Codex skills for planning, delegating, and verifying work with Linear, CXC, and Paperthin. Keep product decisions in Linear, implementation in Git, and delivery evidence connected to the issue and PR.

CRW is a community project designed to work with CXC; it is not an official
OpenAI or Codex product. Its workflow connects child-task delegation, PR review
resolution, and parent-task verification and integration.

This is an experimental workflow built from a personal setup. It contains six
skill instruction sets, a symlink installer, and the two Python packages the
workflow delegates and reports through. CXC and Paperthin remain separate
dependencies. Having the package source here does not install or activate a
runtime, and offline contract checks do not establish live Codex hook or Desktop
compatibility.

| Skill | Purpose |
|---|---|
| [crw-focus](skills/crw-focus/SKILL.md) | Make this task a project's fixed management point and restore its recorded link |
| [crw-next](skills/crw-next/SKILL.md) | Choose one next action when starting or after finishing work, using readchk and nba |
| [crw-plan](skills/crw-plan/SKILL.md) | Turn product context into canonical Linear documents, milestones, and issues |
| [crw-run](skills/crw-run/SKILL.md) | Delegate ready work to independent Codex tasks and verify CXC execution and delivery |
| [crw-check](skills/crw-check/SKILL.md) | Verify delivery and return in-scope corrections to managed tasks |
| [crw-logic](skills/crw-logic/SKILL.md) | Find consequential contradictions using Paperthin checks and minimal counterexamples |

The shared [integration guide](skills/crw-plan/references/integrations.md) owns Linear document authority and CXC/Paperthin routing. Keep the skills together because their references link to one another.

| Package | Purpose |
|---|---|
| [codex-thread-bridge](packages/codex-thread-bridge/README.md) | An MCP server that creates and messages Codex sessions through the App Server running on the same host |
| [codex-session-relay](packages/codex-session-relay/README.md) | Durable same-host verification requests and completion reports between two independent Codex tasks |

Both are described in [packages/README.md](packages/README.md), including where they
were imported from and what was deliberately left behind.

## Sources of truth

- This repository owns the skill instructions and installation helper.
- Linear owns product specifications, plans, accepted decisions, and coordination documents.
- Product repositories own implementation, executable configuration, and repository policy.
- Private task locations hold raw receipts, logs, and sensitive verification evidence.

The skills use installed CXC/Paperthin and the available Linear connector; they do not bundle those tools or install credentials. Explicit-only Paperthin skills remain deliberate user choices.

## Install

Use Git, Python 3.10+, and a platform supporting directory symlinks. The default
branch `dev` contains ongoing integration work; `main` is reserved for authorized
release promotions. Clone into a location you will keep:

```sh
git clone --branch dev https://github.com/thisisjun786/codex-relay-workflow.git
cd codex-relay-workflow
python3 scripts/install.py --apply
python3 scripts/install.py --check
```

### Before using the skills

| Capability | What you need |
| --- | --- |
| Install or run repository checks | Python 3.10+; installation also requires directory symlinks |
| Work on the packages | Python 3.11+ and [uv](https://docs.astral.sh/uv/) |
| Plan and verify Linear work | Codex with local skill support and a connected Linear workspace you can access |
| Use the shared workflow | Separately installed CXC and Paperthin skills referenced by the [integration guide](skills/crw-plan/references/integrations.md) |
| Delegate independent tasks | A host exposing task creation and coordination tools, or an installed bridge |
| Receive automatic completion reports | An installed relay and verified host capability; installing these skills alone does not enable delivery |

Use your own Linear project and repository. The examples describe the maintainer's
workflow, including task names, model preferences, and worktree locations; adapt
them to your explicit instructions, repository policy, and available host tools.
Private maintainer projects are not required for installation or contributions.
There is no GitHub-only replacement for the Linear workflow in this version.

The [operations contract](skills/crw-run/references/operations.md) records
dependency and compatibility requirements. Automatic Codex hook activation and
adopting Desktop's native worktree creation as the default remain unverified;
see the [hook contract](skills/crw-run/references/hook-contract.md) and
[relay guide](skills/crw-run/references/relay.md). Do not treat their proposed
contracts or passing offline fixtures as a supported turnkey runtime.

### Acquire external dependencies

- CXC: follow the upstream [Codexclaw installation guide](https://github.com/lidge-jun/codexclaw#install).
- Paperthin: follow the upstream [Paperthin setup guide](https://github.com/LilMGenius/paperthin#readme), selecting Codex as the target agent.
- Bridge and relay: the source is in [packages/](packages/README.md) and you can
  build both wheels with `uv build`, but there is no published distribution,
  supported version, or installation procedure from this repository yet. Without an
  installed bridge or host-native task tools, independent delegation is unavailable;
  without an installed relay, automatic completion reporting is unavailable.

You can read, install, and validate the skill sources without these runtimes.
With Linear and the referenced CXC/Paperthin skills configured, planning and
manual delivery verification do not require relay delivery. The full unattended
workflow is not available from this repository alone. There is no certified
cross-component version matrix yet; inspect the installed interfaces against the
operations contract before enabling delegation, hooks, or automatic reporting.

### Installation behavior

The destination defaults to `$CODEX_HOME/skills`, or `~/.codex/skills`. Use `--dest /absolute/skills/path` for another Codex installation.

Installation creates a symlink per skill to this checkout. Repeating it preserves correct links. Existing directories or links to other locations are reported as conflicts and left untouched; compare and back them up before deliberately replacing them. There is no automatic deletion or overwrite option.

Edits in this checkout are visible through the installed paths immediately. Already-loaded conversation context may still contain an earlier version; read the updated skill or use a fresh task. Keep this checkout available while its skills are linked. If it moves, deliberately relink after checking the old destinations.

### Upgrade from linear-* to crw-*

The six skills now use CRW names. Their Linear, CXC, and Paperthin behavior is
unchanged; this rename does not add a GitHub-only workflow.

| Previous name | Current name |
| --- | --- |
| `linear-focus` | `crw-focus` |
| `linear-next` | `crw-next` |
| `linear-plan` | `crw-plan` |
| `linear-run` | `crw-run` |
| `linear-check` | `crw-check` |
| `linear-logic` | `crw-logic` |

Run `python3 scripts/install.py --check` after updating your checkout. It reports
`LEGACY` for any of these six old destination names, including dangling links,
ordinary files, directories, and links to another checkout. It never changes them.
`--check` exits nonzero while a canonical link is missing, a conflict exists, or
a legacy entry remains.

Run `--apply` to create the new links. It exits successfully when those links are
installed, even if it also warns about old entries. A conflict at a **new** name
prevents all planned link creation. Compare that destination with the intended
source before deliberately resolving it; the installer has no overwrite option.

Inspect each old entry and its original target. After verifying that a symlink
belongs to the installation you are retiring, move that link to a backup directory
outside Codex's scanned skill directories. Preserve foreign files, directories,
and links for their owner to reconcile. Do not infer ownership from the old name,
and do not move the linked source directory. Run `--check` again; a clean result
means all six new links point to this checkout and no known old names remain.

There are no old-name aliases or duplicate skill instruction folders. Refresh the
client's skill catalog or start a fresh task, then invoke `$crw-run`, for example.
An existing or compacted task may still carry `$linear-run` or an old path: use
the mapping above and read `skills/crw-run/SKILL.md` from the updated checkout.
The descriptions retain a former-name hint for discovery, but this does not make
an old explicit invocation resolve in an already loaded catalog. The rename does
not change task IDs, relay state, permissions, or running CXC workflows.

## Maintain

Edit `skills/`, review the diff, and commit the change. Use a scoped branch from `dev` and target `dev` for ordinary pull requests; an explicit dependent pull request may target its prerequisite branch instead. `main` receives explicitly authorized release promotions from `dev`. Read [repository policy](POLICY.md), [contribution steps](CONTRIBUTING.md) and [CI operation](docs/CI.md). Remote pushes remain user-authorized. Do not copy project state into the skills.

Repository checks need only Python 3.10+:

```sh
python3 scripts/ci/validate.py
python3 -m unittest discover -s scripts/ci/tests -v
python3 scripts/ci/contracts.py
git diff --check
```

Changing either package additionally needs Python 3.11+ and uv:

```sh
python3 scripts/ci/packages.py
```

When the bundled Codex skill validator is available:

```sh
for skill in skills/*; do
  python3 "${CODEX_HOME:-$HOME/.codex}/skills/.system/skill-creator/scripts/quick_validate.py" "$skill" || exit 1
done
python3 scripts/install.py --check
git diff --check
```

The bundled validator has its own dependencies. It checks skill structure, not the quality of workflow decisions. For meaningful instruction changes, use a scoped fresh-context review and realistic offline cases. Live Linear mutations or worker launches must be covered by the current assignment; reuse existing authorization rather than asking again.

## Usage

```text
$crw-focus [Linear project] 이 작업을 이 프로젝트의 고정 진행 관리 창구로 지정하고 연결을 기록해줘.
$crw-next [Linear project or product repository] 다음에 뭐 하지? 시작할 단계인지 끝난 뒤인지 확인하고 다음 행동 하나를 골라줘.
$crw-plan [Linear project or product repository] 정본 문서와 남은 실행 이슈를 정리해줘.
$crw-run [Linear project] 준비된 첫 묶음을 기존 담당 하위 Codex 작업에 맡기고, 없으면 새 독립 하위 Codex 작업을 생성해 CXC Loop로 실행·검증해줘.
$crw-check [Linear project or issue] 기획대로 구현됐는지 확인해줘.
$crw-logic [Linear document or project] 설계와 계산 규칙의 모순을 찾아줘.
```

These are invocation examples, not requests to execute while reading this file.

`crw-run` keeps implementation in the responsible independent child task for
one issue, an existing worktree, or a PR repair. The coordinator selects scope,
reviews results, checks CI, and performs authorized integration. Internal
subagents assist within those tasks and do not replace the independent child.

Independent child tasks run under the shared
[Default independent execution](skills/crw-plan/references/integrations.md#default-independent-execution)
settings unless the request chooses otherwise. A child running CXC Loop owns its own
goal and phases; an explicit non-Loop or no-goal assignment keeps the agreed workflow
without one. The coordinator's launch job is small: apply those settings through the
creation tool, send the bounded packet in the initial work prompt, invoke the
installed `cxc-loop` skill when Loop is the effective workflow, and verify the
settings that actually came back. A worker starts the assigned work without a routine
readiness handshake, and a mismatch found afterwards is reconciled on that same task
instead of adding an approval round.

Read a short request in its conversation: “진행해” accepting a concrete new-task
plan requests that plan's execution, and clear project delegation in an established
independent-task workflow carries that intent. No particular creation keyword is
required. Reuse the responsible task first and preserve the authorized scope and
settings, including successor batches and resumes after compaction.

If the conversation and prior authorization genuinely contain no creation intent
for this scope, prepare the packet and ask only for the missing request when a
new task is needed and the host requires it. Skill selection, unsubmitted UI text,
quoted examples, and permission for an unrelated task do not supply that intent.
A default prompt expressing creation/reuse intent counts when actually submitted.
Host restrictions and explicit current-task, read-only, or no-create limits still
apply. See [Independent implementation tasks](skills/crw-run/SKILL.md#independent-implementation-tasks).

`crw-focus` records the project and current task IDs in a linked Linear
document, sets the task title and sidebar pin when supported, and restores that
context for later requests. Management titles use a concise project summary;
the binding uses the stable project ID regardless of names or initiative membership.
It routes each request to its existing operation
owner. Designating the management task alone does not start project execution;
an accompanying execution request continues within its authorized scope.

`crw-plan` completes a requested full plan from initiatives through projects,
useful milestones, and executable issues in one operation. It reuses existing
items, creates missing ones within that request, and keeps narrow updates scoped.
Each implementation issue maps to one PR; work requiring several PRs is split
into dependent issues. Non-PR research or design keeps a verified result instead.
Initiatives represent goals; product family and related
repositories are separate project labels. Project names need no product prefix,
and views remain the user's choice. See the shared
[Linear operating model](skills/crw-plan/references/integrations.md#linear-operating-model).

`crw-next` distinguishes choosing a first step from choosing what follows a
delivery. It uses `readchk` to resolve ambiguous intent and `nba` to pick one
evidence-backed action with a clear completion condition. Standalone advice does
not launch work; a question during an authorized run does not pause that run.

During an existing delegated workflow, a completion check sends actionable
in-scope corrections to the responsible task and verifies the result without
another approval round. A capable child owns its commits, push, pull request and
the review handling on it, and reports once the current head's required checks and
reviews are clean. The coordinator updates the Linear record, verifies the pull
request against the Linear criteria and its latest diff, checks and review
resolution, then merges under Jun's standing authorization for this workflow and
confirms the landing. Release and deployment still require Jun. Skills, bridge and
relay all deliver to this repository on base `dev` for ordinary pull requests, with dependent pull
requests allowed to target their prerequisite branch, and promoting `dev` to `main` is a release that
needs Jun.
See [Default dev integration](skills/crw-plan/references/integrations.md#default-dev-integration)
for destination, delivery, and release boundaries. [Merge readiness](skills/crw-run/references/merge-readiness.md)
checks current CI, actual review coverage, and unresolved findings without requiring
a particular reviewer or repository-management app. Full-project execution continues
through the authorized batches; an explicit first-batch request remains bounded.
Standalone audits and explicit read-only or report-only requests remain observational.

Managed child tasks use `ISSUE-ID · short task name · workflow`, such as
`JUN-44 · 가격 조회 계약 · CXC Loop`. The coordinator verifies the actual app title;
the workflow suffix does not prove execution. See [Child task titles](skills/crw-run/references/task-packet.md#child-task-titles).

## Contribute and report problems

Open a [GitHub issue](https://github.com/thisisjun786/codex-relay-workflow/issues)
with a redacted reproduction, or follow [CONTRIBUTING.md](CONTRIBUTING.md) for a PR
against `dev`. Include the relevant skill, source commit, host version, expected
behavior, and observed behavior. Do not paste credentials or private task history.
For vulnerabilities, use the private route in [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE). External dependencies retain their own licenses and are not
redistributed by this repository.
