# Codex Relay Workflow (CRW)

Codex skills for planning, delegating, and verifying work with Linear, CXC, and Paperthin. Keep product decisions in Linear, implementation in Git, and delivery evidence connected to the issue and PR.

CRW is a community project designed to work with CXC; it is not an official
OpenAI or Codex product. Its workflow connects child-task delegation, PR review
resolution, and parent-task verification and integration.

This is an experimental workflow built from a personal setup. It contains the skill
instruction sets listed below, the `crw` runtime the workflow delegates and reports
through (one Go binary serving the task bridge, the session relay and the completion
hook), a symlink installer for development, and the two Python packages that runtime
was ported from, kept as the development and rollback path until the Python execution
path is removed. CXC and Paperthin remain separate dependencies. Having the source here
does not install or activate a runtime, and offline contract checks do not establish
live Codex hook or Desktop compatibility.

| Skill | Purpose |
|---|---|
| [crw-next](plugins/crw/skills/crw-next/SKILL.md) | Choose one next action when starting or after finishing work, using readchk and nba |
| [crw-define](plugins/crw/skills/crw-define/SKILL.md) | Explore intent and define an initiative goal, success evidence, and scope |
| [crw-plan](plugins/crw/skills/crw-plan/SKILL.md) | Decompose an agreed goal into projects, milestones, and one-PR issues |
| [crw-run](plugins/crw/skills/crw-run/SKILL.md) | Bind the parent and execute one project's agreed scope, including parallel issue delivery and successors, without a parent goal |
| [crw-loop](plugins/crw/skills/crw-loop/SKILL.md) | Add a parent goal and automatic continuation to the same Run project execution |
| [crw-status](plugins/crw/skills/crw-status/SKILL.md) | Report where work stands, including the supervisor midpoint check and progress against the agreed schedule |
| [crw-check](plugins/crw/skills/crw-check/SKILL.md) | Verify delivery and return in-scope corrections to managed tasks |
| [crw-logic](plugins/crw/skills/crw-logic/SKILL.md) | Find consequential contradictions using Paperthin checks and minimal counterexamples |
| [crw-refactor](plugins/crw/skills/crw-refactor/SKILL.md) | Diagnose post-cycle structural debt and plan bounded, behavior-preserving repairs |

The shared [integration guide](plugins/crw/skills/crw-plan/references/integrations.md) owns Linear document authority and CXC/Paperthin routing. Keep the skills together because their references link to one another.

| Package | Purpose |
|---|---|
| [codex-thread-bridge](packages/codex-thread-bridge/README.md) | An MCP server that creates and messages Codex sessions through the App Server running on the same host |
| [codex-session-relay](docs/relay/README.md) | Durable same-host verification requests and completion reports between two independent Codex tasks |

Both are described in [packages/README.md](packages/README.md), including where they
were imported from and what was deliberately left behind.

## Sources of truth

- This repository owns the skill instructions and installation helper.
- Linear owns product specifications, plans, accepted decisions, and coordination documents.
- Product repositories own implementation, executable configuration, and repository policy.
- Private task locations hold raw receipts, logs, and sensitive verification evidence.

The skills use installed CXC/Paperthin and the available Linear connector; they do not bundle those tools or install credentials. Explicit-only Paperthin skills remain deliberate user choices.

## Install

Use Git, the Go toolchain `go.mod` names, and a platform supporting directory
symlinks. The default branch `dev` contains ongoing integration work; `main`
identifies the last owner-authorized source release. Clone into a location you will
keep:

```sh
git clone --branch dev https://github.com/thisisjun786/codex-relay-workflow.git
cd codex-relay-workflow
go run -tags dev ./cmd/crw-dev skills link --apply
go run -tags dev ./cmd/crw-dev skills link --check
```

`crw-dev` is the repository's development binary; the linker is in it because it
links a checkout, and a release archive has none. Its Python predecessor,
`scripts/install.py`, left with the Python execution path in todo 44.

### Retired skill migration

Skills installed under their retired names (`linear-focus`, `linear-next`,
`linear-plan`, `linear-run`, `linear-check`, `linear-logic`, `crw-focus`) are not
removed by any linker, and `crw-dev skills link` does not look for them (the Python
linker that reported them as `LEGACY` lines left in todo 44): inspect who owns each
entry, then move it outside the skills directory Codex discovers
(`$CODEX_HOME/skills`, or `~/.codex/skills`).

Or install the same skills as a versioned plugin, which needs no checkout to stay
in place. The plugin also declares an MCP server and a Stop hook that run the installed
runtime, so on a host that will use them [install the runtime](#install-the-runtime)
first and add the plugin after it: runtime first, then payload
([turning the wired surfaces on](docs/plugin-packaging.md#turning-the-wired-surfaces-on)).
A host whose runtime is still the Python one does not add or update the plugin before
[the cutover](docs/port/cutover.md). This payload's declared commands cannot start a
Python runtime, so every Stop would be released without a record and the bridge would
not start.

```sh
codex plugin marketplace add thisisjun786/codex-relay-workflow --ref dev
codex plugin add crw@crw
```

A plugin installation offers the skills as `crw:crw-run`, `crw:crw-plan`, and so on,
while a linked installation offers them unprefixed. Both read the same source. See
[plugin packaging](docs/plugin-packaging.md) for the package layout, updates, and
what installing does not do.

### Install the runtime

The skills plan and verify without a runtime. Delegation through the task bridge and
automatic completion reports need the `crw` runtime, which a release publishes as
`crw_<version>_<os>_<arch>.tar.gz` beside a `SHA256SUMS` ([releases](docs/releases.md#binary-assets)).
Unpack the archive anywhere and let its `crw` install itself, on the host whose App
Server it will serve, because the install exercises the new runtime against it. Name that
App Server's socket with `--socket`: the relay has no default socket, so without it the
exercise fails and nothing is promoted (exit 1):

```sh
tar -xzf crw_<version>_linux_amd64.tar.gz -C <scratch>
<scratch>/crw install install --from crw_<version>_linux_amd64.tar.gz --socket <app-server-socket>
crw=~/.local/share/crw-runtime/current/bin/crw
# The bridge record the plugin's server reads, naming the host's execution policy:
"$crw" install register-mcp --owner plugin --execution-policy <policy-file>
"$crw" install hook --owner plugin           # the Stop settings the plugin's hook reads
"$crw" doctor                                # read what landed
```

Without `--execution-policy` the bridge record is version 1 and the bridge checks no role
pairs. `register-mcp` never overwrites a record that says something else, so naming a policy
later means moving that record aside by hand first
([the execution policy](docs/runtime-install.md#the-execution-policy-the-plugin-bridge-runs-under)).

The runtime goes to `~/.local/share/crw-runtime`, the path the plugin's declared server
and Stop hook name, and `crw install` has no other destination. Nothing puts its
`current/bin/` on `PATH`, which the relay commands in the skills need. The Stop hook
then has to be trusted in Codex before it fires. [Runtime installation](docs/runtime-install.md)
covers updating, rolling back, removing and reading an installation.

The runtime this procedure leaves serves the store in the relay's default state directory with
no variable set: the relay commands the skills run and the Stop hook's guard open it there. Until
todo 43 the Go build refused that directory unless `CRW_ALLOW_LIVE_STATE=1` was set; the guard
now refuses it only under test isolation
([the live-state guard](docs/port/cutover.md#the-live-state-guard-test-isolation-only)). A host
still on the Python runtime moves through [the cutover](docs/port/cutover.md) first. That runbook
moves the store's ownership. Where the pointer move (`crw install install`, which on the relay host's first Go install
also replaces its Python-era Stop settings, once) falls among its steps is not written yet: todo 42
settles it.

### Before using the skills

| Capability | What you need |
| --- | --- |
| Link the skills from a checkout | The Go toolchain `go.mod` names, and directory symlinks |
| Install the runtime | A release archive for Linux (amd64 or arm64); darwin/arm64 is built but unvalidated |
| Run repository checks | The Go toolchain, and Python 3.10+ for the Python checks that remain until the Python path is removed (developer-only) |
| Plan and verify Linear work | Codex with local skill support and a connected Linear workspace you can access |
| Use the shared workflow | Separately installed CXC and Paperthin skills referenced by the [integration guide](plugins/crw/skills/crw-plan/references/integrations.md) |
| Delegate independent tasks | A host exposing task creation and coordination tools, or an installed bridge |
| Receive automatic completion reports | An installed runtime, a trusted Stop hook and verified host capability; installing these skills alone does not enable delivery |

Use your own Linear project and repository. The examples describe the maintainer's
workflow, including task names, model preferences, and worktree locations; adapt
them to your explicit instructions, repository policy, and available host tools.
Private maintainer projects are not required for installation or contributions.
There is no GitHub-only replacement for the Linear workflow in this version.

The [operations contract](plugins/crw/skills/crw-run/references/operations.md) records
dependency and compatibility requirements. Automatic Codex hook activation and
adopting Desktop's native worktree creation as the default remain unverified;
see the [hook contract](plugins/crw/skills/crw-run/references/hook-contract.md) and
[relay guide](plugins/crw/skills/crw-run/references/relay.md). Do not treat their proposed
contracts or passing offline fixtures as a supported turnkey runtime.

### Acquire external dependencies

- CXC: follow the upstream [Codexclaw installation guide](https://github.com/lidge-jun/codexclaw#install).
- Paperthin: follow the upstream [Paperthin setup guide](https://github.com/LilMGenius/paperthin#readme), selecting Codex as the target agent.
- Bridge, relay and completion hook: the `crw` runtime above, from a release that
  carries its archives. Without an installed bridge or host-native task tools,
  independent delegation is unavailable; without an installed relay and a trusted
  Stop hook, automatic completion reporting is unavailable.

You can read, install, and validate the skill sources without these runtimes.
With Linear and the referenced CXC/Paperthin skills configured, planning and
manual delivery verification do not require relay delivery. The full unattended
workflow is not available from this repository alone. There is no certified
cross-component version matrix yet; inspect the installed interfaces against the
operations contract before enabling delegation, hooks, or automatic reporting.

### Installation behavior

The destination defaults to `$CODEX_HOME/skills`, or `~/.codex/skills`; a leading `~` in `CODEX_HOME` or `--dest` is expanded. Use `--dest /absolute/skills/path` for another Codex installation. Run it from inside the checkout: it finds the checkout with Git and links the skills directory its plugin manifest declares.

Installation creates a symlink per skill to this checkout. Repeating it preserves correct links. Existing directories or links to other locations are reported as conflicts and left untouched; compare and back them up before deliberately replacing them. There is no automatic deletion or overwrite option.

Edits in this checkout are visible through the installed paths immediately. Already-loaded conversation context may still contain an earlier version; read the updated skill or use a fresh task. Keep this checkout available while its skills are linked. If it moves, deliberately relink after checking the old destinations.

### Confirm what you can use now

A change reaches you in stages, and a report naming only the last stage it completed is easy to misread. After updating, check the stage you actually depend on, from this checkout:

```sh
git log --oneline -1
skills_dest="${CODEX_HOME:-$HOME/.codex}/skills"   # or the --dest you installed with
readlink "$skills_dest/crw-run"
go run -tags dev ./cmd/crw-dev skills link --dest "$skills_dest" --check
```

The first line is the revision this checkout holds. `readlink` gives the checkout an installed skill actually resolves to, which is not always the one you just edited. `--check` reports whether the links belong to the checkout you run it from and prints `CONFLICT` when they point elsewhere; it does not print targets, so read the link itself when the answer matters, and point both commands at the destination you installed with. A plugin installation has no link to read: the cache holds one published version per plugin, and an edit here reaches it only after the manifest version is recorded again and the plugin is installed. That version carries the digest of the packaged bytes, so the version `codex plugin list` shows names the payload and not only the release.

None of that is the same as using the change. A conversation that already read a skill keeps the text it read, so the shortest confirmation is to start a fresh task, invoke the skill on a real request, and compare what it does with the behavior the change describes. The skills apply the same rule when they report their own delivery; see [Delivery reach and current usability](plugins/crw/skills/crw-plan/references/integrations.md#delivery-reach-and-current-usability).

## Maintain

Everything in this section is developer-only: it needs a checkout.

Edit `plugins/crw/skills/`, review the diff, and commit the change. Use a scoped branch from `dev` and target `dev` for ordinary pull requests; an explicit dependent pull request may target its prerequisite branch instead. The owner releases a verified `dev` commit and fast-forwards `main` to it with the [release workflow](docs/releases.md); there is no promotion PR. Read [repository policy](POLICY.md), [contribution steps](CONTRIBUTING.md) and [CI operation](docs/CI.md). Remote pushes remain user-authorized. Do not copy project state into the skills.

Repository checks run from the development binary:

```sh
go run -tags dev ./cmd/crw-dev ci validate
go run -tags dev ./cmd/crw-dev ci plugin
go run -tags dev ./cmd/crw-dev ci contracts
git diff --check
```

The runtime itself is checked with `make lint test` ([CI operation](docs/CI.md) lists the
parts CI splits that into). Until the Python execution path is removed, the Python checks
remain and CI runs them too; they need Python 3.10+:

```sh
python3 scripts/ci/validate.py
python3 scripts/ci/plugin.py
python3 -m unittest discover -s scripts/ci/tests -v
```

The Python packages under `packages/` are no longer built or tested; todo 44 removes them.

When the bundled Codex skill validator is available:

```sh
for skill in plugins/crw/skills/*; do
  python3 "${CODEX_HOME:-$HOME/.codex}/skills/.system/skill-creator/scripts/quick_validate.py" "$skill" || exit 1
done
go run -tags dev ./cmd/crw-dev skills link --check
git diff --check
```

The bundled validator has its own dependencies. It checks skill structure, not the quality of workflow decisions. For meaningful instruction changes, use a scoped fresh-context review and realistic offline cases. Live Linear mutations or worker launches must be covered by the current assignment; reuse existing authorization rather than asking again.

## Usage

```text
$crw-run [Linear project] 실행하지 말고 이 프로젝트 부모로 연결만 해줘.
$crw-next [Linear project or product repository] 다음에 뭐 하지? 시작할 단계인지 끝난 뒤인지 확인하고 다음 행동 하나를 골라줘.
$crw-define [idea or initiative] 목표·완료 기준·범위를 정의해 Linear에 반영해줘.
$crw-plan [defined initiative or existing project] 프로젝트·마일스톤·이슈와 의존성을 계획해 Linear에 반영해줘.
$crw-run [Linear project link]
$crw-loop [Linear project link]
$crw-check [Linear project or issue] 기획대로 구현됐는지 확인해줘.
$crw-status 중간점검. 지금 어디까지 됐는지, 막힌 이유와 내 결정이 필요한 부분을 근거와 함께 짧게 알려줘.
$crw-logic [Linear document or project] 설계와 계산 규칙의 모순을 찾아줘.
$crw-refactor [initiative cycle or project] 완료 근거와 변경 코드를 확인하고 구조적 부채 후보를 3개 이내로 추천해줘. 진단과 계획까지만.
$crw-refactor 추천한 1번을 구현하고 검증해줘. 관련 없는 개선은 남겨두고 머지와 배포는 하지 마.
```

These are invocation examples, not requests to execute while reading this file.

A submitted `$crw-run <Linear project link>` executes one project's agreed scope:
restore the fixed parent, reuse/create independent issue children, verify/integrate
their deliveries and start newly ready successors as capacity opens. Run creates no
parent goal; the first ready batch is not its finish boundary. An explicit issue,
milestone or batch request narrows the assignment. Status-only requests remain read-only.

Use `$crw-loop <Linear project link>` to explicitly request a parent goal and automatic
continuation of that same project execution. Run owns scheduling and delivery; Loop
creates or restores the parent's goal and keeps it across continuations. Both fill
available capacity and serialize shared-target merges. Run inside Loop returns to
the same owner. Children keep their own issue
goals and CXC implementation workflow. Parent completion uses verified scoped deliveries,
without requiring a parent-local diff or CXC implementation phases.

Goal activation requires the [parent goal preflight](plugins/crw/skills/crw-loop/references/parent-goal.md),
including compatible goal/Stop hooks and a supported observation/continuation path.
A hook that forces every active goal into CXC phases blocks activation even in a fresh
parent. This skill does not install a wake service or silently migrate existing goals.
An explicit no-goal limit prevents Loop activation; goal-free Run remains a separately
authorized option. Keep narrower scope, pause and delivery limits in either mode.

`crw-run` keeps implementation in the responsible independent child task for
one issue, including that issue’s worktree and PR repairs. Each parent orchestrates
one project and each child one issue. The coordinator selects scope,
reviews results, checks CI, and performs authorized integration. Internal
subagents assist within those tasks and do not replace the independent child.

Independent child tasks run under the shared
[Default independent execution](plugins/crw/skills/crw-plan/references/integrations.md#default-independent-execution)
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
new task is needed and the host requires it. Automatic skill selection, unsubmitted
UI text, quoted examples, and permission for an unrelated task do not supply that
intent. The submitted project-run shorthand above does express that intent.
Host restrictions and explicit current-task, read-only, or no-create limits still
apply. See [Independent implementation tasks](plugins/crw/skills/crw-run/SKILL.md#independent-implementation-tasks).

Run and Loop use the shared [project binding procedure](plugins/crw/skills/crw-plan/references/integrations.md#project-parent-binding)
to record project/task IDs in Linear, set the task title and sidebar pin when
supported, and restore context. Management titles lead with the linked project's
product-family label in brackets, spelled exactly as Linear spells it, followed by a
concise project summary; where that label is absent or ambiguous, or the user fixed
the title, the title stands without a prefix and the gap is recorded. The binding
uses the stable project ID regardless of names or initiative membership.
The requesting skill remains the operation owner.
Designating the management task alone does not start project execution;
an accompanying execution request continues within its authorized scope.

`crw-define` explores intent and defines the initiative’s goal, finish condition
and scope. `crw-plan` then decomposes the agreed goal into projects, useful
milestones, and executable issues in one operation. A request for both chains
them; definition-only stops before project and issue creation. It reuses existing
items, creates missing ones within that request, and keeps narrow updates scoped.
Consultation and draft-only planning do not write to Linear.
Each implementation issue maps to one PR; work requiring several PRs is split
into dependent issues. Non-PR research or design keeps a verified result instead.
Initiatives represent goals; product family is a project label, while an issue's
repository label identifies its actual edit target. Project names need no product prefix,
and views remain the user's choice. See the shared
[Linear operating model](plugins/crw/skills/crw-plan/references/integrations.md#linear-operating-model).

`crw-next` distinguishes choosing a first step from choosing what follows a
delivery. It uses `readchk` to resolve ambiguous intent and `nba` to pick one
evidence-backed action with a clear completion condition. Standalone advice does
not launch work; a question during an authorized run does not pause that run.

`crw-status` answers the other half of that question: not what to do next, but where
the work actually is. A bare 중간점검 in a supervision task walks the initiative binding
down to its project parents and issue children, and a portfolio question keeps every
project's one-line status while only the detail narrows. It separates source, checks,
an actual merge, installation and observed behavior, judges progress against the agreed
schedule, and reports what it could not verify. A standalone status call, and any call carrying an
explicit report-only, read-only, pause or no-contact limit, writes nothing and wakes nobody. A
중간점검 inside an initiative execution Jun already approved is the other branch: it checks the
responsible parents and moves the work that approval already covers, then reports what it actually
did. Either way it returns control to whoever is executing and starts nothing that was not approved.

During an existing delegated workflow, a completion check sends actionable
in-scope corrections to the responsible task and verifies the result without
another approval round. A capable child owns its commits, push, pull request and
the review handling on it, and reports once the current head's required checks and
reviews are clean. The coordinator updates the Linear record, verifies the pull
request against the Linear criteria and its latest diff, checks and review
resolution, then merges under Jun's standing authorization for this workflow and
confirms the landing. Release and deployment still require Jun. Skills, bridge and
relay all deliver to this repository on base `dev` for ordinary pull requests, with dependent pull
requests allowed to target their prerequisite branch. Releasing a verified dev commit and
advancing main to it requires Jun's explicit release authorization.
See [Default dev integration](plugins/crw/skills/crw-plan/references/integrations.md#default-dev-integration)
for destination, delivery, and release boundaries. [Merge readiness](plugins/crw/skills/crw-run/references/merge-readiness.md)
checks current CI, actual review coverage, and unresolved findings without requiring
a particular reviewer or repository-management app. Full-project execution continues
through the authorized batches; an explicit first-batch request remains bounded.
Standalone audits and explicit read-only or report-only requests remain observational.

Managed issue tasks use `ISSUE-ID · descriptive task title`, such as
`JUN-44 · 가격 조회 결과를 검증한다`. Use up to 20 characters for the title text,
including spaces, without over-abbreviating; exclude the issue code and separator
from that count. Keep workflow and PR metadata out of the title.
The coordinator verifies the actual app title. See [Child task titles](plugins/crw/skills/crw-run/references/task-packet.md#child-task-titles).

## Contribute and report problems

Open a [GitHub issue](https://github.com/thisisjun786/codex-relay-workflow/issues)
with a redacted reproduction, or follow [CONTRIBUTING.md](CONTRIBUTING.md) for a PR
against `dev`. Include the relevant skill, source commit, host version, expected
behavior, and observed behavior. Do not paste credentials or private task history.
For vulnerabilities, use the private route in [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE). External dependencies retain their own licenses and are not
redistributed by this repository.
