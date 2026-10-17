# Multi-session, multi-host execution

Status: **design proposal**, CRW-1615. The first delivery is this document. The
commands, envelopes and new packages below describe follow-up implementation;
they are not installed capabilities. The issue bodies in the appendix are
proposals for review, not updates to the tracking system.

The goal is to run independent work on several execution hosts without making
the operator assign terminals, copy reports or decide which duplicate CI run to
trust. A session waiting for an agent response consumes different resources
from a session compiling or testing. Placement must account for both, while
preserving the existing issue owner, edit-region rules and integration gate.

## 1. Evidence and its limits

An operating trial on 2026-10-17 ran the same warmed Go test bundle concurrently
at source revision `73aa64cac`. The bundle was
`go test -count=1 ./internal/flow/ops ./internal/crwconfig ./internal/relay/dagsched`.
The table retains the reported rounded values; it is not a new benchmark.
Host names and operational addresses are intentionally omitted.

| Host | Logical CPUs | Concurrent bundles | Per-job slowdown | Reported throughput approximation | Next measured concurrency / slowdown |
| --- | ---: | ---: | ---: | ---: | --- |
| A | 24 | 8 | 2.47× | 3.2× | 12 / 3.54× |
| B | 16 | 8 | 2.17× | 3.7× | 12 / 3.21× |
| C | 8 | 4 | 2.04× | 2.0× | 6 / 3.04× |

Those observations had zero test failures. The experiment stopped increasing
concurrency after slowdown exceeded 3×; that experimental stopping rule is not
a product latency target. These are cached test bundles, not cold builds,
complete project deliveries or measurements under the CRW-1591 CPU budget.

The accompanying operating summary reports approximately 90 MB per Devin
session and approximately 30% of session time spent testing. On a shared
20-CPU host with eight sessions and another trial, busy CPUs had median 1.0 and
maximum 6.6. These observations explain why session count alone is a poor CPU
limit. They do not establish memory requirements during builds, independent
test arrivals, provider account capacity or a safe session maximum.

Consequently:

- Keep session ceilings and heavy-command slots separate.
- Treat 8/8/4 concurrent bundles as candidates for a later controlled trial,
  not default session counts or evidence to raise the DAG's existing cap.
- Repeat the representative workload with the actual CPU budget, mixed
  session activity, memory pressure and CI queue time before enabling a
  higher host profile. Record completion rate, latency and failures together.
- Include edit-region availability and CI/integration backlog in capacity
  reports. More CPU cannot make overlapping work independent.

## 2. Current boundaries

The source inspected for this proposal is `969a4e589`.

| Current owner | Existing responsibility | Proposed extension |
| --- | --- | --- |
| [lane-dag.js](../../internal/flow/workflows/lib/lane-dag.js) | `crw.dag` limits concurrent lanes; its default `cap` is **3** and it checks admission-region overlap | Retain the run-wide limit; ask for a host assignment when a lane is admitted |
| [dagsched capacity](../../internal/relay/dagsched/capacity.go), [cap basis](../../internal/relay/dagsched/capbasis.go) | Initiative/project/store run ceilings; standing cap **6** without an applicable recorded basis | Add host assignment records and constraints without multiplying existing ceilings by host count |
| [relay capacity](../../internal/relay/capacity/capacity.go), [edit regions](../relay/dag-scheduler.md#edit-regions) | Transactional reservations and region ownership | Judge all hosts in the same authority store and repository namespace |
| [engine backend](../../internal/flow/engine/backend.go), [Codex adapter](../../internal/flow/backend/agent/agent.go) | Persistent App Server threads, turns, placement and reconciliation | Keep the real Codex contract; add an executor port at the execution boundary |
| [agent acceptance](../../internal/relay/dagsched/accept_agent.go) | Current execution, owner/epoch, independent review or unit check, pre-merge and commit verification | Collect external commits locally and supply genuine controller-side evidence to the same gate |
| [GitHub verifier](../../internal/flow/ops/verify_github.go), [verification records](../../internal/relay/dagsched/verification_record.go) | Hosted verification, record identity and constrained tree reuse | Coordinate one logical verification across hosts |
| [integrator](../../internal/integrator/) | Prepare, verify and land the candidate on the target | Preserve target serialization and re-verification after the target moves |
| [CPU budget](../../internal/dev/ci/local_cpu.go) | CRW-1591/1601 shared verification budget, heavy-gate re-entry and explicit fallback | Use the same enforcement and provenance for worker-managed heavy commands |

The two existing caps are different: `crw.dag` counts lanes within a run; the
relay judges its declared execution ceilings. A `dag_cap_basis` above the
standing cap requires the relevant limit revision and measured child wall time
W and parent serial time S. The test-concurrency measurements above are not W/S.
This proposal neither supplies invented W/S values nor raises either default.

`internal/provider` detects an inference-provider catalog; it is not an executor
registry. `internal/role/dispatchhost` is a read-only App Server connection used
by role-helper reconciliation; it is not the owner of remote execution hosts.

## 3. One authority store and several workers

The first implementation uses one controller and its existing relay store.
Each enrolled host runs a worker that owns local worktrees, provider processes,
command groups and a private artifact spool. Remote workers connect through an
authenticated SSH transport to a fixed worker command with structured input;
task text is never interpolated into a remote shell command.

```text
project owner / crw.dag
        |
        v
relay admission + host registry + execution journal
        |                           |
        v                           v
local or SSH worker          verification coordinator
        |                           |
Codex / Devin / Claude Code     GitHub Actions
        |
result envelope + immutable commit
        v
collector checkout -> existing review / verification / acceptance -> integration
```

The controller is the authority for assignments, capacity and accepted results.
Workers keep durable delivery journals, not replicas of the relay database.
Copying SQLite files, sharing them over a network filesystem, or allowing each
host to run an independent scheduler would create competing ownership facts.
Those alternatives are excluded from the first implementation.

Proposed module ownership:

- `internal/executor/` (new): versioned envelopes, identities, validation and
  adapter interfaces. It has no dependency on a live relay database or provider.
- `internal/executor/worker` and adapter/transport subpackages (new): process
  ownership, private files, SSH framing and provider-specific I/O.
- `internal/relay/dagsched` and `internal/relay/store`: assignment and capacity
  transactions, current execution and fencing. Add migrations/side tables;
  do not rewrite shipped schema statements.
- `internal/flow/engine` and `internal/flow/ops`: bridge an admitted executor
  invocation and its observed result into the flow journal. Inject the executor
  port; do not import network adapters into offline scheduler code.
- `internal/flow/cli` and `cmd/crw`: assemble the adapters and expose the proposed
  host/executor commands. Provider/model selection remains role-policy-owned.

External providers do not receive fabricated Codex thread IDs or forged
`flow_calls` reviewer rows. New execution provenance records identify their
adapter and native session ID in separate namespaces. Existing Codex executor
calls gain envelope provenance while retaining their actual thread/turn facts.
Review and unit-check evidence is produced through the real flow paths.

## 4. Hosts, tools and executor capabilities

These are **proposed** CLI surfaces:

| Surface | Effect |
| --- | --- |
| `crw host add` | Enroll an operator-approved host identity, SSH endpoint reference and managed roots, initially disabled |
| `crw host toolchain plan` / `apply` | Show a pinned, platform-specific change plan; apply that approved plan to a drained host |
| `crw host check` / `status` | Read tool/capability observations, health, assigned sessions, permits and pending work |
| `crw host enable` / `drain` | Enable a checked capacity profile; stop new assignments while retaining running work |
| `crw executor submit` / `status` / `cancel` / `collect` | Operate on a registered execution identity, not a terminal or pane |
| `crw executor result check` | Validate the received envelope and collected candidate; report which existing gates still need evidence |
| `crw worker serve` / `exec` | Serve the transport and supervise permitted command groups on an enrolled host |

A host record contains an opaque `host_id`, transport/credential references,
repository allowlist, managed-root configuration, toolchain profile digest,
capability revision, capacity profile and current incarnation. A worker
incarnation changes on a fresh worker tenure and records the machine boot
identity. A reinstalled machine does not inherit old grants just by reusing
an address.

Toolchain manifests pin versions and artifact digests per OS/architecture.
They include the runtime, provider CLI, repository-required build tools,
verification tools, hooks and skill assets. They do not require binaries for
different architectures to have identical hashes. Installation uses owned,
versioned locations and refuses foreign paths or symlinks; changing the active
profile waits for drain rather than modifying tools under a running command.
Missing privileged OS dependencies are explicit plan items for the operator.
Account tokens, signing keys and provider login state are not synchronized.

Host enrollment authenticates SSH host keys and the worker identity. Provider
workspace trust, authentication and launch permissions must already fit the
approved launch profile. The product does not press through a first-run trust
dialog or copy an unrestricted launch flag from an operator's terminal command.
Tool/profile digests and observed capabilities are part of eligibility.

The adapter contract is `Start`, `Inspect`, `Cancel`, `Collect`, with declared
optional resume/fork capabilities. `Start` is keyed by execution identity and
immutable task digest. `Inspect` distinguishes running, terminated, not-started
with proof, and unknown. `Cancel` acknowledges termination of the managed
process group/provider execution, not merely delivery of a signal.

| Executor | Initial integration |
| --- | --- |
| Codex subagent | Wrap the existing local App Server backend and preserve its native reconciliation |
| Devin local | Supervised CLI session and task/result files |
| Devin remote | The same Devin adapter behind an enrolled SSH worker |
| Claude Code | The same envelope/port contract with its own probed launch and lifecycle adapter |

Local versus remote is transport, not a different result schema. The installed
Devin CLI documentation exposes non-interactive `--print`, file prompts,
explicit resume IDs and conversation export. Those options do not prove
structured result correctness or safe retry after an unknown start. A provider
that cannot prove resume, fork or cancellation exposes that limitation; the
scheduler does not emulate it by starting another session. Conversation
exports and screen contents are private diagnostic material, not completion
receipts. A terminal multiplexer can display a session without owning its state.

## 5. Capacity is several independent resources

| Resource | Acquired | Returned |
| --- | --- | --- |
| Existing DAG execution slot and run lane | Existing release path | Existing node lifecycle rules |
| Host session grant | Atomic assignment before a provider start | Proven provider termination; result collection may continue |
| Host heavy-command permit | Before a supervised build/test/lint or other heavy command group starts | The whole command group has terminated |
| CI verification reservation | Before a new distinct verification is dispatched | Its authoritative terminal result is reconciled |
| Edit-region hold | Existing node release | Existing integration/closure rules, not a worker's “done” message |

A session waiting for a heavy permit remains a session. Waiting for inference
or hosted CI consumes no local heavy permit. Completing tests does not return a
DAG slot or a region hold early. Existing run ceilings continue to cover their
original subjects; new host counters do not count a slot twice.

Each host has explicit integer `session_limit`, `test_slots` and
`test_cpu_budget` settings. New hosts start disabled with provisional limits of
one session and one heavy slot. Enabling requires an operator-selected CPU
budget that fits observed affinity/quota and reserves room for other host work.
An unsatisfiable requested budget is reported before queueing. Limits are not
inferred from a logical-CPU count or the observed 30% test duty cycle.

For a host H, admission maintains both:

```text
held heavy command groups(H) <= test_slots(H)
sum(requested CPU budget of held groups(H)) <= test_cpu_budget(H)
```

These are reservation invariants, not a guarantee of physical CPU delivery.
CRW-1591's default budget of four CPUs remains a per-verification default;
an explicit task budget is frozen in its execution profile. The existing
shared cgroup guard also covers heavy gates that create another scope.
The Go fallback must retain its jobs × package-parallelism × GOMAXPROCS bound
and report its actual method; it is not a kernel cap on arbitrary native work.
A task requiring a kernel cap can only select a host that proves that
capability. Affinity/quota limitations remain visible in the result.

The permit is enforced by the worker's command launcher, including tests run
by a reviewer or provider helper. A nested `make`/Go process tree shares one
permit and CPU group; separate concurrent root tool commands need separate
permits. Unknown command classes are conservatively heavy, while a narrow
read-only class can bypass the heavy queue. Merely asking an agent to prepend
a wrapper is insufficient. A launch profile whose tool processes can bypass
the supervisor is ineligible for the bounded pool until that path is supported.

Placement preserves the ready-node ranking, `crw.dag` cap and existing region
checks. Among compatible, healthy hosts with session headroom, prefer the
lowest held-session/limit ratio, then the lowest queued-heavy-work pressure,
then stable host ID. Compare ratios without rounding. Record the host
observations and tie-break inputs in the pass so its choice can be explained.
Global and host reservations plus the execution assignment commit in one
controller-store transaction; network calls occur after commit.

The initial health profile proposes a 30-second heartbeat and 90-second
freshness window, explicitly as policy defaults rather than measured latency
guarantees. A worker samples its own effective CPU, available memory and memory
pressure. The controller's receipt time, worker incarnation and increasing
sample sequence distinguish a fresh observation from replayed data. Existing
memory thresholds keep their meanings, applied to the selected host rather
than accidentally to the controller. Missing required measurements or stale
heartbeats make a managed host ineligible for new placement; they do not mean
zero load or free capacity. The legacy un-enrolled local path is unchanged.

## 6. Task, result and recovery protocol

`crw-executor-task/1` is an immutable envelope. The controller hashes its
canonical bytes before admission. Mutable heartbeats and renewed grants are
separate records; a renewal does not change the task digest.

| Task field group | Required meaning |
| --- | --- |
| Identity | Task ID, execution ID, attempt, assignment generation; plan/node/revision and criteria digest |
| Repository | Registered forge/repository identity, base commit, target ref, assigned task branch; one implementation repository in the first version |
| Scope | Existing normalized region declarations and approved work kind |
| Executor | Host ID/incarnation, executor kind, capability revision, launch/toolchain profile digests |
| Work | Brief/artifact digests and typed execution input; no inline credentials |
| Checks | Approved command IDs/argument arrays, expected results and resource requirements; an explicit approved not-applicable reason for a design-only task |

`crw-executor-result/1` echoes the task digest and execution/assignment identity.
It carries a terminal outcome, branch and full Git object IDs, check results
and exit codes, evidence-file digests, actual tools and CPU method. Requested
and observed provider/model facts are separate; unsupported observations are
unknown. The worker constructs the outer identity and provenance from its
admission record, rather than trusting identity fields written by the model.
The agent's narrative and claimed pass/fail remain untrusted payload.

Task/result files are written to temporary files, flushed and atomically
renamed in a private spool. Collection accepts only bounded, relative artifact
paths under the assigned spool; traversal, symlinks and unexpected file types
are rejected. Transfer limits and retention policy are explicit host/profile
configuration. A digest proves integrity, not who sent it: authenticated
transport and the enrollment record bind a result to its host.

One execution has one final result identity. Repeating identical bytes returns
the stored acknowledgement; a different final digest for the same execution
is quarantined for reconciliation. A stale attempt or assignment generation
cannot replace the current result. A command string or URL supplied only by a
result never chooses what the collector executes or which repository it trusts.

The lifecycle separates uncertainty from failure:

1. Persist assignment, reservations and dispatch intent before sending `Start`.
   The worker journals an idempotent start before acknowledging its handle.
2. A lost reply enters `start_unknown`. Reconcile using the worker journal and
   provider/process identity. Retry creation only on proof that the original
   start did not occur; absence from a transient listing is not that proof.
3. A heartbeat timeout stops new placement but does **not** free an existing
   session grant, heavy permit or region hold. Existing work may still run.
4. Cancellation or recovery returns resources only after a terminal observation
   for the entire managed execution, or proof such as a changed boot identity
   that makes the old process tenure impossible. Until then it remains unknown.
5. Controller restart resumes from persisted assignments. Current owner/epoch
   checks and explicit handover fence control operations. A handover can adopt
   the same execution with a refreshed grant; it does not rewrite its task or
   start a replacement. Uncertainty may reduce availability, never justify an
   unrecorded duplicate.

This is an at-most-one admitted attempt with explicit reconciliation, not an
“exactly once” promise about an external provider API. Neither elapsed time nor
a pane closing proves that background tools have stopped.

## 7. Collection, verification and integration

The worker publishes the committed candidate to its assigned GitHub task
branch through normal pre-push checks. It has no authority to update an
integration target. Result delivery combines the envelope/private artifacts
with that branch and full commit ID; a patch file alone is not an accepted
candidate. Uncommitted or unpublished work is reported as incomplete.

The collector fetches the advertised object from the registered repository into
an isolated local checkout. It reads HEAD, tree, ancestry and changed paths
itself and compares them with the frozen base and envelope. It records the
collector checkout and its correspondence to the remote execution; it never
passes a remote absolute path as the acceptance gate's local checkout.
Moving a branch after collection cannot change the object under review.

The result-check command reports structural validity and missing gate evidence
separately. A valid envelope or a worker's local PASS is not a
`verification-record/1` PASS. Review/unit checks run on the collected candidate,
with the current criteria and genuine flow-journal provenance. The existing
pre-merge, verifier-record and commit checks then feed `dag-accept`; the current
plan owner and epoch remain authoritative. Source outside the declared scope
is reported through existing scope/risk handling and returned for an explicit
scope decision, not silently added to the assignment.

The configured verifier remains authoritative for local and remote executors.
Hosted CI is the full verifier for the current operating path. Integration
prepares the candidate over the then-current target and follows the existing
gate and compare-and-swap rules. If the target moves, verify the resulting
integration tree again as required. No worker label, report, result file or
green check alone authorizes a direct push to the target.

### One logical GitHub verification across hosts

The operating verifier's host-local lock and run lookup help callers on one
machine. They do not serialize independent machines. The product extension
uses a durable controller-side single-flight record keyed by:

```text
repository identity + full head object ID
+ workflow ID / workflow-source digest
+ normalized workflow inputs and verifier-rules/toolchain identity
```

All callers request or attach to that record; workers do not dispatch their
own competing workflow runs. The controller reserves the configured CI
capacity once for a new key, persists a dispatch intent and publishes an
immutable verification ref for that key. Different heads never reuse the same
ref while either run is active, so branch-based concurrency cancellation does
not cancel unrelated verifications. An existing ref with different contents
is a conflict, not a reason to force-push it.

The run ID, head, repository, workflow, event/ref, attempt and required job
results are read from GitHub and matched to the request. Completion is shared,
including failure. A permitted failed-job retry keeps the same logical record
and records the new attempt and original failure; it is not another worker's
new workflow dispatch.

If dispatch may have succeeded before the controller saved the run ID,
reconcile that intent against GitHub. An empty eventually consistent listing
does not authorize a second dispatch. Keep `dispatch_unknown` and its CI
reservation until the run is identified or an operator resolves the ambiguity
with evidence. A request nonce in workflow inputs/run identity is a proposed
future aid, not a capability assumed of today's workflow.

Keep completed-tree reuse within the existing verification-key checks.
Different base/head ranges still need their own current secret/blob/range
evidence; sharing hosted jobs does not share integration authority. An old
record with missing or incompatible keys cannot become a pass through this
new coordination layer.

## 8. Edit ownership across machines

Every assigned implementation node declares regions through the relay before
launch. Use the registered forge repository identity and one canonical
repository spelling, plus repository-relative paths. Different host checkout
roots cannot make the same file appear to belong to different repositories.
Resolve aliases at enrollment; do not merge unrelated local-only repositories
just because their directory names match.

Keep the current unknown-region, hotspot, rename/delete, grade and whole-repo
hold rules. Keep the conservative `crw.dag` overlap admission as well; adding a
host is not permission to relax it. Cross-plan ownership is judged in the same
store. The brief's manual “files held by another executor” list becomes a
generated view of those declarations and holders, with the observation
revision. A stale view cannot authorize a launch.

In the first version an issue/node has one active executor. CRW-1356 later
allows several executor partitions for one issue: each has its own execution
and host grants, disjoint declared regions or explicit ordered dependencies,
and a result pinned to the same parent plan/criteria revision. A composer
assembles their exact commits into one candidate, reviews and verifies that
combined tree, and produces one issue-level acceptance. A partial result
cannot close the issue. The parent remains one logical DAG node; physical
executor capacity must also count every partition, so `cap` cannot hide extra
executors inside one lane.

## 9. Operating-script migration map

| Operating surface | Product destination | Behavior retained or strengthened |
| --- | --- | --- |
| `fleet.sh start` | Host registry + executor submit/worker preparation | Approved repository/base, owned worktree and frozen task envelope |
| `fleet.sh launch` | Provider adapter + worker start journal | Idempotent identity, explicit trust/profile, observable lifecycle; terminal is a viewer |
| `fleet.sh pull` | Executor collect + controller candidate checkout | Full commit fetched from GitHub, validated envelope/artifacts and local commit proof |
| `fleet.sh status` | Host/executor status | Typed heartbeat, resource ownership, work state and unknown reasons |
| `fleet-sync.sh` | Host toolchain plan/apply | Pinned inventory, owned versioned installs, drain and readback; no credential copying |
| `fleet-check.sh` | Host check | Version/digest/capability comparison with the task profile, not one machine's mutable PATH |
| `gh-verify.sh` / ATTACH1017 | Existing Go GitHub verifier plus shared verification journal | One request/run across hosts, attempt provenance, range checks and existing record validator |
| Watch jobs | Worker heartbeat + controller reconcile | Restartable observations and delivery receipts; no completion inferred from screen text |
| Brief file-owner lists | Relay region declaration/status | One cross-host ownership view and existing overlap rules |
| Heavy-slot wrappers and CRW-1591/1601 | Worker heavy permits + shared CPU-budget runner | Independent session/test limits; nested processes share the actual recorded budget |

The shell scripts remain an operating fallback during rollout. The product
owns each surface only after its corresponding acceptance scenario passes;
copying their command strings into a new CLI is not the migration.

## 10. Delivery stages and acceptance

**First slice:** freeze CRW-1336's envelope/adapter contract and CRW-1339's host
registry, grants and command supervision; support the existing local Codex path
and one enrolled local/SSH Devin worker. Preserve one executor per issue. Use
explicit small profiles and existing run caps; prove reconnect and ambiguous
start behavior before increasing parallelism.

**Before declaring multi-host support for 1.0:** complete CRW-1337 result checks
and CRW-1338 collection/integration, shared verification, toolchain check/apply
and the failure scenarios below. Add Claude Code through the same contract and
require the same adapter tests before advertising that backend. A backend that
has not passed them remains disabled. Rollout progresses from local to one
remote host, then independent work on several hosts.

**After 1.0:** CRW-1356's multiple executors for one issue, automatic capacity
tuning, live execution migration, multiple active controllers, dynamic/cloud
host provisioning and new platform support. Static declared capacity and one
authority store are sufficient for the initial goal.

Required implementation scenarios:

1. Two admissions race for the last host session/CPU permit across different
   plans; only one grant commits, with no orphan global reservation.
2. Sessions can wait for inference concurrently while heavy commands remain
   bounded; reviewer tests and nested heavy gates cannot escape the budget.
3. Same repository/file on two hosts still conflicts; unknown/hotspot and
   cross-plan cases retain the existing decisions.
4. Disconnect before start, after start but before acknowledgement, during a
   test and after writing the final result; restart reconciles each without
   duplicate execution or premature resource release.
5. Replayed result, conflicting final bytes, stale generation, wrong repository
   or head, dirty work, missing Git objects, path traversal and toolchain drift
   cannot produce an accepted candidate.
6. Two hosts request the same verification; they share one observed run. A
   dispatch with an unknown response is held. Different heads use separate
   refs, and failed/cancelled/skipped required jobs are not a pass.
7. A target advance after verification requires the normal new integration
   proof. A worker or stale owner cannot land a result directly.
8. Every enabled adapter demonstrates the same envelope/check outcomes;
   unsupported lifecycle capabilities are explicit, not guessed.

The document-only delivery has no red test. Its checks are `make lint-changed`,
`go run -tags dev ./cmd/crw-dev ci validate`, one independent review and the
repository's configured GitHub CI for the merged task head.

## 11. Decisions still needing rollout evidence

- Approve per-host capacity profiles after repeating budgeted mixed workloads;
  gather actual W/S if a relay ceiling above six is proposed.
- Pin and probe the supported provider versions, workspace trust setup,
  process-group supervision and resume/cancel capabilities.
- Choose artifact size/retention quotas for the real workload. Partial transfer
  or quota exhaustion must be a visible incomplete delivery, not silent loss.
- Decide whether to add a dispatch nonce to the workflow under its own
  CI-control authorization. Until then ambiguous dispatches remain held.
- Reconcile the five appendix proposals with their existing tracking bodies
  and dependencies before registering or releasing implementation work.

## Appendix: proposed replacement issue bodies

These bodies use the release gate's five headings. New package paths and test
commands are implementation targets, not claims that those packages or tests
exist in this document-only delivery. The referenced old issues are not edited
by this proposal.

### CRW-1336 — 실행자 공통 과제·결과 봉투와 어댑터 경계

```markdown
## 기준

- Codex 서브에이전트, Devin 로컬/SSH, Claude Code가 같은 과제·결과 봉투를 사용한다.
- 과제·실행·시도·assignment generation과 계획/기준/저장소/편집 영역이 고정된다.
- 동일 결과 재전송은 같은 답을 주고, 충돌하거나 오래된 결과는 현 실행을 덮어쓰지 못한다.
- provider의 실제 식별자와 관찰 불가 항목을 보존하며 Codex thread 증거를 꾸미지 않는다.

## 편집 영역

- `internal/executor/` (새 공통 계약·검사기·어댑터 포트와 시험).
- `internal/flow/engine/`, `internal/flow/backend/agent/`, `internal/flow/ops/`의 실행 경계와 provenance 연결.
- 모델·노력 선택 정책, 통합 판정, 기존 이슈 본문 자동 수정은 범위 밖이다.

## 정한 답

- `docs/design/multi-host.md` 3·4·6절의 `crw-executor-task/1`, `crw-executor-result/1`을 구현한다.
- 불변 과제 digest와 갱신되는 grant/heartbeat를 분리한다. outer identity는 worker가 기록한다.
- Start/Inspect/Cancel/Collect와 선택적 resume/fork를 capability로 표현한다.
- 기존 Codex 경로의 실제 thread/turn 증거를 유지하고 외부 실행자 provenance를 별도로 기록한다.
- 동일 execution의 동일 digest는 재생, 다른 최종 digest는 격리한다. 파일 수신은 경로·형식·크기와 인증된 호스트를 검사한다.

## 먼저 실패하게 쓸 시험

- 세 provider의 합성 결과가 같은 정규화·검사 계약을 통과한다.
- 잘못된 schema/digest/시도, 중복·충돌 결과, 경로 탈출, 미지원 capability를 거절한다.
- 봉투의 임의 명령·URL을 검사기가 실행하지 않으며 원문 결과가 reviewer 증거로 변하지 않는다.

## 끝 조건

- `go test -count=1 ./internal/executor/... ./internal/flow/engine ./internal/flow/backend/agent ./internal/flow/ops`가 통과한다.
- `make lint-changed`와 최종 병합 head의 설정된 GitHub CI가 통과한다.
- provider별 미지원 기능과 합성 시험/실제 연결 확인의 경계를 보고한다.
```

### CRW-1339 — 실행 호스트·실행자 등록부와 호스트별 용량 배치

```markdown
## 기준

- 호스트 추가·도구 계획/적용·점검·활성화·drain이 제품 명령과 지속 기록으로 제공된다.
- 세션 상한과 무거운 명령 슬롯/CPU 예산을 독립적으로 지키며 호스트가 늘어도 전역 cap을 복제하지 않는다.
- 서로 다른 계획의 동시 배정도 마지막 슬롯을 중복으로 얻지 못한다.
- 끊긴 호스트는 신규 배정에서 빠지고, 기존 실행은 종료 증거 없이 회수·재기동하지 않는다.

## 편집 영역

- `internal/executor/worker/` 및 host/transport 어댑터 (새 파일 포함).
- `internal/relay/store/`, `internal/relay/dagsched/`, `internal/relay/capacity/`의 추가 기록과 배정 경계.
- `internal/crwconfig/`, `internal/flow/cli/`, `cmd/crw/`, `internal/flow/workflows/lib/lane-dag.js`의 설정·조립·배치 연결.
- 계정 토큰 복제, 기본 cap 인상, 실행 중 설치본 덮어쓰기와 다중 controller는 범위 밖이다.

## 정한 답

- CRW-1336의 실행 포트와 설계 3~6절을 사용한다. controller store 하나가 등록·배정을 소유한다.
- 신규 호스트는 disabled/세션1/시험슬롯1에서 시작하고, 점검된 명시적 CPU·용량 profile로 활성화한다.
- 전역/호스트 예약과 assignment를 한 transaction에 기록한 뒤 원격 Start를 보낸다.
- 현재 crw.dag 기본3과 relay standing cap6 및 근거 규칙을 유지한다. 지역 이름은 호스트 경로가 아닌 정규 저장소·상대 경로다.
- heartbeat 30초/신규 배정 freshness 90초를 초기 정책으로 두되, 시간 초과로 기존 grant를 반납하지 않는다.
- 도구는 소유한 버전 경로에 설치하고 drain·readback을 거친다. 인증과 workspace trust는 승인된 profile을 요구한다.

## 먼저 실패하게 쓸 시험

- 두 계획이 마지막 세션/시험/CPU 슬롯에 경합해 하나만 배정되고 실패한 transaction은 전역 예약을 남기지 않는다.
- 무거운 명령이 중첩되거나 reviewer에서 실행되어도 공유 CPU 예산을 벗어나지 않는다.
- stale heartbeat, worker 재시작, Start 응답 유실, 취소 확인 유실에서 중복 기동이나 조기 자원 반환이 없다.
- 다른 호스트의 같은 파일, hotspot, 미선언 영역이 기존 겹침 판단을 우회하지 못한다.

## 끝 조건

- `go test -count=1 ./internal/executor/... ./internal/relay/capacity ./internal/relay/dagsched ./internal/relay/store ./internal/crwconfig ./internal/flow/cli`가 통과한다.
- `make lint-changed`와 최종 병합 head의 설정된 GitHub CI가 통과한다.
- 승인된 시험 호스트에서 동시성·연결 단절·drain 시나리오를 확인하고 실제 계정/설치 변경 범위를 별도로 기록한다.
```

### CRW-1337 — 결과 봉투와 실제 후보를 검사하는 명령

```markdown
## 기준

- 결과 봉투의 구조적 유효성과 검증 게이트 통과를 구분한다.
- 실제로 가져온 Git commit/tree/ancestry와 과제·편집 영역·도구 증거를 대조한다.
- 실행자의 PASS 문구, 다른 head의 검증 기록, 누락된 시험이나 위조된 reviewer 결과로 수락하지 않는다.

## 편집 영역

- `internal/executor/`의 result 검사·후보 수집과 시험.
- `internal/flow/ops/`의 검사 호출 연결, `internal/flow/cli/`, `cmd/crw/`.
- 기존 판정기 호출에 필요한 `internal/relay/dagsched/` 연결과 시험.
- 기존 게이트 완화와 원격 이슈 상태 수정은 범위 밖이다.

## 정한 답

- CRW-1336 봉투와 CRW-1339의 신원/assignment 기록을 읽어 설계 6·7절대로 검사한다.
- 등록된 저장소에서 정확한 object를 수집한 로컬 checkout을 읽는다. 원격 절대 경로나 결과가 지정한 임의 URL을 신뢰하지 않는다.
- 결과의 명령 문자열을 실행하지 않는다. 승인된 과제의 check ID/예상 결과와 실제 결과를 대조한다.
- `verification-record/1`, pre-merge, 현재 기준의 실제 리뷰·unit-check 증거가 부족하면 필요한 게이트를 구조화해 보고한다.
- 재시도 가능한 수집 실패와 잘못된 결과를 구분하며, 유효한 봉투만으로 accepted 상태를 쓰지 않는다.

## 먼저 실패하게 쓸 시험

- 정상 합성 결과와 로컬 Git fixture는 검사되고, 같은 봉투의 다른 tree/base/repository는 거절된다.
- 오래된 generation, dirty/unpublished 작업, 없는 object, 손상된 log digest, 허위 PASS가 수락되지 않는다.
- 설계 전용 과제의 승인된 시험 비적용은 보존하고, 실행자가 임의로 추가한 비적용은 받아들이지 않는다.

## 끝 조건

- `go test -count=1 ./internal/executor/... ./internal/flow/ops ./internal/flow/cli ./internal/relay/dagsched`가 통과한다.
- `make lint-changed`와 최종 병합 head의 설정된 GitHub CI가 통과한다.
- 명령 출력에서 envelope-valid와 candidate-accepted가 별개이며 실제 gate 누락 이유가 드러난다.
```

### CRW-1338 — 외부 실행자 후보를 기존 검증·통합 게이트에 연결

```markdown
## 기준

- 외부 결과도 실제 controller checkout의 head와 현재 계획 기준으로 기존 수락·통합 게이트를 거친다.
- 서로 다른 호스트가 같은 검증을 요청하면 한 논리 기록과 실제 run을 공유한다.
- target 변경, 원격 브랜치 변경, 오래된 소유자, 불확실한 dispatch가 수락이나 직접 push를 허용하지 않는다.

## 편집 영역

- `internal/executor/`의 수집 provenance 연결.
- `internal/flow/engine/`, `internal/flow/ops/verify_github.go`와 관련 검증 호출.
- `internal/relay/store/`, `internal/relay/dagsched/`의 추가 검증 조정 기록과 기존 acceptance 연결.
- `internal/integrator/`의 외부 후보 입력과 시험. workflow/권한 변경은 별도 승인·이슈 범위다.

## 정한 답

- CRW-1336·1339·1337 위에 설계 7절을 구현한다. 실제 reviewer/unit-check 호출을 기록하며 외부 결과로 flow_calls를 꾸미지 않는다.
- 저장소·head·workflow/입력/규칙·도구 정체성의 단일 검증 키를 controller에서 원자적으로 차지한다.
- dispatch intent를 먼저 저장하고 같은 키는 attach한다. 서로 다른 head는 다른 불변 CI ref를 쓴다.
- 응답 유실은 dispatch_unknown으로 두고 GitHub의 실제 run/attempt/job을 대조한다. 빈 목록이나 시간 경과만으로 다시 dispatch하지 않는다.
- 현재 tree 재사용 판정기를 쓰고 범위별 비밀값/blob 검사는 새로 확인한다. target 이동 시 기존 통합 재검증 규칙을 유지한다.

## 먼저 실패하게 쓸 시험

- 두 호스트의 같은 head 요청이 한 dispatch만 만들고 같은 결과를 받는다.
- dispatch 성공 직후 응답 유실·controller 재시작에서도 확인 전 두 번째 dispatch가 없다.
- 다른 head끼리 CI ref 취소를 일으키지 않으며 failed/cancelled/skipped 필수 job은 PASS가 아니다.
- 다른 tree/base의 기록과 늦은 소유자 수락이 거절되고, target 이동은 새 통합 검증을 요구한다.

## 끝 조건

- `go test -count=1 ./internal/executor/... ./internal/flow/engine ./internal/flow/ops ./internal/relay/dagsched ./internal/integrator`가 통과한다.
- `make lint-changed`와 최종 병합 head의 설정된 GitHub CI가 통과한다.
- 승인된 GitHub 연동 시험에서 run 정체성·중복 요청·취소 격리를 확인하고 구현/설치/운영 결과를 구분한다.
```

### CRW-1356 — 이슈 하나의 여러 실행자와 단일 합성 결과 (1.0 이후)

```markdown
## 기준

- 이슈 하나를 여러 실행자가 나눠도 하나의 계획 기준·최종 후보·완료 판정을 유지한다.
- 각 partition의 host session과 무거운 명령을 모두 세며 한 lane 안에 실행자를 숨겨 상한을 우회하지 않는다.
- 부분 성공이나 이전 시도의 결과로 전체 이슈를 완료하지 않는다.

## 편집 영역

- `internal/executor/`의 partition 과제·결과 계약 확장.
- `internal/flow/workflows/lib/`, `internal/flow/ops/`, `internal/flow/engine/`의 분할 실행·합성 연결.
- `internal/relay/dagsched/`, `internal/relay/store/`의 partition provenance와 자원 기록.
- 추적 이슈 자동 분할·새 milestone 생성·개별 partition의 직접 dev push는 범위 밖이다.

## 정한 답

- CRW-1336~1339의 단일 실행자 경로가 안정화된 뒤 설계 8절을 확장한다.
- root issue/node는 하나이며 partition마다 별도 execution ID·시도·host grant·영역을 둔다.
- 영역이 겹치면 기존 규칙에 맞는 명시적 순서를 둔다. 같은 기준 revision에 선 결과만 합성한다.
- 지정된 composer가 각 결과의 정확한 commit을 하나의 후보로 합치고 합쳐진 tree를 리뷰·검증한다.
- root DAG slot과 물리 executor 수를 별도로 기록하고 전역/호스트 executor 한도를 모두 적용한다.

## 먼저 실패하게 쓸 시험

- 같은 이슈의 두 독립 partition이 다른 호스트에서 완료되어 하나의 후보로만 수락된다.
- 한 partition 실패·stale 기준·겹친 영역·조합 충돌은 전체 완료를 막거나 필요한 수정 단계로 보낸다.
- 같은 부분 결과의 재전송은 중복 merge/검증/완료를 만들지 않고, 물리 세션 상한을 넘지 않는다.

## 끝 조건

- `go test -count=1 ./internal/executor/... ./internal/flow/engine ./internal/flow/ops ./internal/relay/dagsched`가 통과한다.
- `make lint-changed`와 최종 병합 head의 설정된 GitHub CI가 통과한다.
- 전체 합성 tree의 검증과 단일 완료 기록을 실제 시나리오로 확인하고 부분 결과만의 PASS를 구별한다.
```
