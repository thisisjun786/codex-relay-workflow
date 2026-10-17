# Codex Relay Workflow (CRW)

Codex skills for planning, delegating, and verifying work with Linear. Keep product decisions in Linear, implementation in Git, and delivery evidence connected to the issue and PR.

CRW is a community project; it is not an official
OpenAI or Codex product. Its workflow connects child-task delegation, PR review
resolution, and parent-task verification and integration.

This is an experimental workflow built from a personal setup. It contains the skill
instruction sets listed below, the `crw` runtime the workflow delegates and reports
through (one Go binary serving the task bridge, the session relay and the completion
hook) and a symlink installer for development. The two Python packages that runtime was
ported from left the repository in todo 44, after the cutover; the bridge's upstream
provenance and licence stay in [packages/codex-thread-bridge](packages/codex-thread-bridge/PROVENANCE.md).
CXC v0.2.40 is the origin of the port ([NOTICE](NOTICE),
[docs/port-cxc/provenance.md](docs/port-cxc/provenance.md)), and CRW runs without it. Having the source here
does not install or activate a runtime, and offline contract checks do not establish
live Codex hook or Desktop compatibility.

| Skill | Purpose |
|---|---|
| [crw-define](plugins/crw/skills/crw-define/SKILL.md) | Settle a milestone and its issues before they are registered: purpose, desired change, success evidence, scope and open decisions, and whether a question is really needed |
| [crw-plan](plugins/crw/skills/crw-plan/SKILL.md) | Decompose an agreed goal into projects, milestones, and one-delivery issues |
| [crw-add-issue](plugins/crw/skills/crw-add-issue/SKILL.md) | Turn one short request into one ready issue: criteria, edit regions, decided answer, tests to write first and done condition, with an open design question held as design first |
| [crw-run](plugins/crw/skills/crw-run/SKILL.md) | Bind the child (project session) and execute one project's agreed scope, including parallel issue delivery and successors, without a goal unless goal mode is explicitly requested; and the work of the parent (repository session): project order, decisions, reports and the midpoint check |
| [crw-status](plugins/crw/skills/crw-status/SKILL.md) | Read-only report of where work stands by level, with progress against the agreed schedule, and the one next action when asked |
| [crw-check](plugins/crw/skills/crw-check/SKILL.md) | Verify delivery, find contradictions with minimal counterexamples, judge the schedule, and return in-scope corrections to managed tasks |
| [crw-refactor](plugins/crw/skills/crw-refactor/SKILL.md) | Diagnose post-cycle structural debt, read-only, and recommend bounded, behavior-preserving repairs; a chosen repair becomes a Linear issue through crw-add-issue |

The shared references in [crw-canon](plugins/crw/skills/crw-canon/SKILL.md) own Linear document authority and the other rules that more than one skill reads. Keep the skills together because their references link to one another.

| Component of `crw` | Purpose |
|---|---|
| `codex-thread-bridge` ([provenance](packages/codex-thread-bridge/PROVENANCE.md)) | An MCP server that creates and messages Codex sessions through the App Server running on the same host |
| [codex-session-relay](docs/relay/README.md) | Durable same-host verification requests and completion reports between two independent Codex tasks |

Both began as Python packages imported into this repository;
[the bridge's provenance](packages/codex-thread-bridge/PROVENANCE.md) records where they came
from, what was deliberately left behind, and where their Go ports live.

## Sources of truth

- This repository owns the skill instructions and installation helper.
- Linear owns product specifications, plans, accepted decisions, and coordination documents.
- Product repositories own implementation, executable configuration, and repository policy.
- Private task locations hold raw receipts, logs, and sensitive verification evidence.

The skills use the available Linear connector; they do not bundle it or install credentials.

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

Or install the same skills as a versioned plugin, which needs no checkout to stay
in place. The plugin also declares an MCP server and a Stop hook that run the installed
runtime, so on a host that will use them [install the runtime](#install-the-runtime)
first and add the plugin after it: runtime first, then payload
([turning the wired surfaces on](docs/plugin-packaging.md#turning-the-wired-surfaces-on)).
A host whose runtime is still the Python one does not add or update the plugin before
[the cutover](docs/port/cutover.md). This payload's declared commands cannot start a
Python runtime, so every Stop would be released without a record and the bridge would
not start. The cutover's Python half (the fence release and its installer) left the
repository in todo 44, so such a host runs it from a revision before that.

The committed manifest names a fixed release; the version an installation knows carries the
payload's digest and is stamped into a generated local marketplace (CRW-1582). A direct Git or
source clone has no build step, so it cannot carry that identity - the generated marketplace is
the supported content-addressed install path.

```sh
# From a checkout of the revision to package; destination must not exist.
go run -tags dev ./cmd/crw-dev ci plugin --stage /path/outside-checkout/crw-marketplace --revision HEAD
go run -tags dev ./cmd/crw-dev ci plugin --payload /path/outside-checkout/crw-marketplace/plugins/crw
codex plugin marketplace add /path/outside-checkout/crw-marketplace
codex plugin add crw@crw
```

Codex copies the stamped payload into a versioned cache directory, so updates and rollbacks
stage a fresh marketplace of their revision; no version suffix is committed to the source.

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
pairs. `register-mcp` never overwrites a record that says something else, so a record that
already names a policy is re-registered with
`register-mcp --re-register-policy --execution-policy <policy-file>`, which replaces that one
field and backs the record up first
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
still on the Python runtime moves through [the cutover](docs/port/cutover.md) first, from a
revision before todo 44 removed the Python half of it. That runbook moves the store's
ownership; `crw install install` moves the pointer (and on the relay host's first Go install
replaces its Python-era Stop settings, once).

### Before using the skills

| Capability | What you need |
| --- | --- |
| Link the skills from a checkout | The Go toolchain `go.mod` names, and directory symlinks |
| Install the runtime | A release archive for Linux (amd64 or arm64); darwin/arm64 is built but unvalidated |
| Run repository checks | The Go toolchain `go.mod` names |
| Plan and verify Linear work | Codex with local skill support and a connected Linear workspace you can access |
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

- Bridge, relay and completion hook: the `crw` runtime above, from a release that
  carries its archives. Without an installed bridge or host-native task tools,
  independent delegation is unavailable; without an installed relay and a trusted
  Stop hook, automatic completion reporting is unavailable.

You can read, install, and validate the skill sources without these runtimes.
With Linear configured, planning and
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

The first line is the revision this checkout holds. `readlink` gives the checkout an installed skill actually resolves to, which is not always the one you just edited. `--check` reports whether the links belong to the checkout you run it from and prints `CONFLICT` when they point elsewhere; it does not print targets, so read the link itself when the answer matters, and point both commands at the destination you installed with. A plugin installation has no link to read: the cache holds one published version per plugin, and an edit here reaches it only after a marketplace is staged from the revision and the plugin is installed. That version carries the digest of the packaged bytes, so the version `codex plugin list` shows names the payload and not only the release.

None of that is the same as using the change. A conversation that already read a skill keeps the text it read, so the shortest confirmation is to start a fresh task, invoke the skill on a real request, and compare what it does with the behavior the change describes. The skills apply the same rule when they report their own delivery; see [Delivery reach and current usability](plugins/crw/skills/crw-canon/references/delivery.md#delivery-reach-and-current-usability).

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
parts CI splits that into). The Python packages the runtime was ported from left the
repository in todo 44, the CI checks' Python twins in refactor R3 and the port checkers in
todo 48. The runtime, installer and CI do not depend on Python, and `crw-dev ci validate` refuses a `.py` file outside a skill's `scripts/` and `examples/` directories.

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
$crw-run [Linear project] 실행하지 말고 이 프로젝트의 프로젝트 세션으로 연결만 해줘.
$crw-status [Linear project or product repository] 다음에 뭐 하지? 시작할 단계인지 끝난 뒤인지 확인하고 다음 행동 하나를 골라줘.
$crw-run [Linear initiative link] 저장소 세션으로서 승인된 프로젝트들을 기존 프로젝트 세션을 통해 끝까지 진행해줘.
$crw-define [idea or milestone] 목적·성공 증거·범위·열린 결정을 정리해 Linear 마일스톤에 반영해줘.
$crw-plan [defined initiative or existing project] 프로젝트·마일스톤·이슈와 의존성을 계획해 Linear에 반영해줘.
$crw-run [Linear project link]
$crw-run [Linear project link] 프로젝트 세션의 goal을 열어서 끝까지 진행해줘. (goal mode)
$crw-check [Linear project or issue] 기획대로 구현됐는지 확인해줘.
$crw-status 지금 어디까지 됐는지, 막힌 이유와 내 결정이 필요한 부분을 근거와 함께 짧게 알려줘.
$crw-run 중간점검. 승인된 프로젝트들을 확인하고 이미 승인된 범위의 대기 중인 일을 진행시킨 뒤 보고해줘.
$crw-check [Linear document or project] 설계와 계산 규칙의 모순을 찾아줘.
$crw-refactor [initiative cycle or project] 완료 근거와 변경 코드를 확인하고 구조적 부채 후보를 3개 이내로 추천해줘. 진단과 계획까지만.
$crw-refactor 추천한 1번을 crw-add-issue로 Linear 이슈로 만들어줘. 관련 없는 개선은 남겨두고 머지와 배포는 하지 마.
```

These are invocation examples, not requests to execute while reading this file.

A submitted `$crw-run <Linear project link>` executes one project's agreed scope:
restore the fixed parent, reuse/create independent `child` sessions, verify/integrate
their deliveries and start newly ready successors as capacity opens. Run creates no
parent goal; the first ready batch is not its finish boundary. An explicit issue,
milestone or batch request narrows the assignment. Status-only requests remain read-only.

Ask `$crw-run <Linear project link>` explicitly for a parent goal to get goal mode: a
native parent goal and automatic continuation of that same project execution. Run owns
scheduling and delivery; goal mode creates or restores the parent's goal and keeps it
across continuations. Both fill available capacity and serialize shared-target merges.
`$crw:crw-continue <Linear project link>` is not a second way in: it starts no loop and hands the
request to goal mode. Continuation (formerly `crw-loop`) completes the
task work (formerly PABCD/IPABCD) of one task under `$crw:crw-work`; the project
parent never follows it. Children keep their own issue goals and `$crw:crw-continue` implementation
workflow. Parent completion uses verified scoped deliveries, without requiring a
parent-local diff or task work implementation steps.

Goal activation requires the [parent goal preflight](plugins/crw/skills/crw-run/references/goal-mode.md#preflight-before-starting-workers),
including compatible goal/Stop hooks and a supported observation/continuation path.
A hook that forces every active goal into task work steps blocks activation even in a fresh
parent. Goal mode does not install a wake service or silently migrate existing goals.
An explicit no-goal limit prevents goal mode; goal-free Run remains a separately
authorized option. Keep narrower scope, pause and delivery limits in either mode.

`crw-run` keeps implementation in the responsible independent child task for
one issue, including that issue’s worktree and PR repairs. Each child (project session)
runs one project and each `child` session one issue. The coordinator selects scope,
reviews results, checks CI, and performs authorized integration. Internal
subagents assist within those tasks and do not replace the independent child.

Independent child tasks run under the shared
[Default independent execution](plugins/crw/skills/crw-canon/references/delivery.md#default-independent-execution)
settings unless the request chooses otherwise. A child running `$crw:crw-continue` owns its own
goal and phases; an explicit non-Loop or no-goal assignment keeps the agreed workflow
without one. The coordinator's launch job is small: apply those settings through the
creation tool, send the bounded packet in the initial work prompt, invoke the
`$crw:crw-continue` skill when it is the effective workflow, and verify the
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

Run, goal mode included, uses the shared [project binding procedure](plugins/crw/skills/crw-plan/references/integrations.md#project-parent-binding)
to record project/task IDs in Linear, set the task title and sidebar pin when
supported, and restore context. Management titles lead with the linked project's
product-family label in brackets, spelled exactly as Linear spells it, followed by a
concise project summary; where that label is absent or ambiguous, or the user fixed
the title, the title stands without a prefix and the gap is recorded. The binding
uses the stable project ID regardless of names or initiative membership.
The requesting skill remains the operation owner.
Designating the management task alone does not start project execution;
an accompanying execution request continues within its authorized scope.

`crw-define` settles a milestone and its issues before they are registered: the
top session (today Sion) or [the parent](plugins/crw/skills/crw-canon/references/hierarchy.md#the-levels) (repository session) organizes purpose,
desired change, success evidence, scope and open decisions, and asks one question
at a time only when one is really needed. `crw-plan` then decomposes the agreed goal into projects, useful
milestones, and executable issues in one operation. A request for both chains
them; definition-only stops before project and issue creation. It reuses existing
items, creates missing ones within that request, and keeps narrow updates scoped.
Consultation and draft-only planning do not write to Linear.
Each implementation issue maps to one delivery; work requiring several deliveries is split
into dependent issues. Non-PR research or design keeps a verified result instead.
Initiatives represent goals; product family is a project label, while an issue's
repository label identifies its actual edit target. Project names need no product prefix,
and views remain the user's choice. See the shared
[tracking operating model](plugins/crw/skills/crw-canon/references/tracking-model.md#tracking-operating-model).

`crw-status` reports where the work actually is, by level from the parent (repository session) down to the issue,
and sends a gap one level down to its owner. When asked what to do next it distinguishes choosing
a first step from choosing what follows a delivery, restates an ambiguous request to resolve
intent and picks one evidence-backed action with a clear completion condition. A portfolio
question keeps every project's one-line status while only the detail narrows. It separates source,
checks, an actual merge, installation and observed behavior, shows progress against the agreed
schedule using `crw-check`'s judgment, and reports what it could not verify. It is read-only: it
writes nothing and wakes nobody, standalone advice launches no work, and a question during an
authorized run does not pause that run.

The parent's section of `crw-run` owns the midpoint check. A bare 중간점검 in the parent
walks the initiative binding down to its children and moves the work that Jun's
approval already covers, then reports what it actually did; under an explicit report-only,
read-only, pause or no-contact limit, or without that binding, the request is a `crw-status`
read. Either way it returns control to whoever is executing and starts nothing that was not
approved.

During an existing delegated workflow, a completion check sends actionable
in-scope corrections to the responsible task and verifies the result without
another approval round. A capable child owns its commits and push, and, where the
delivery is a pull request, the review handling on it; it reports once the
repository's named verification and the applicable reviews are clean. Under the
push-only procedure internal work integrates by one integrator, who merges the
candidate into a local integration tree over `dev`, verifies that tree with the
local full verification (`verification-record/1` with `result: pass`) and fast-forwards
`dev` to it; the coordinator updates the Linear record and verifies the criteria
against the delivered tree and confirms the landing. That is direct landing. PR-unit
landing is the project session's way to land a milestone: its plan's target `base_ref`
names the milestone integration branch, the run lands each node there as it would on
`dev`, and the project session opens one pull request from that branch toward `dev`
naming the issues it carries (`crw flow pr open`) and enqueues it (`crw manage integrate
--enqueue-pr`). The dev queue merges the pull request's head onto `dev` in its own tree,
verifies it, runs the repository's configured review command (the coordinator configures
its Argon review there) and fast-forwards `dev`; the pull request holds the record and the
review, nobody merges it on GitHub, and the `verification-record/1` stays the gate. See
[the integration queue](docs/relay/integrator.md#pull-requests-pr-unit-landing). Release and deployment still
require Jun. Skills, bridge and relay all deliver to this repository on base `dev`;
an external contribution arrives as an ordinary pull request, and a dependent pull
request may target its prerequisite branch. Releasing a verified dev commit and
advancing main to it requires Jun's explicit release authorization.
See [Default dev integration](plugins/crw/skills/crw-canon/references/delivery.md#default-dev-integration)
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
