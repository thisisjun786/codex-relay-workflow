# Control group: CRW-116 and CRW-124 acceptance scenarios

Replay list for todo 42 (fenced cutover on Jun's host) and todo 45 (release acceptance). Each
scenario is one acceptance criterion as Linear states it, followed by the observable pass
condition and the evidence the Python build produced for it. A criterion the Python build never
closed stays a replay obligation; it doesn't become optional under Go.

## Provenance and read-back

Source: the live Linear issue bodies and every comment, fetched read-only by the orchestrator
on 2026-09-25 and handed to the todo-5 executor as files. Criterion text below is copied byte
for byte from the `description` field, including Linear's inline `<issue ...>` markup; nothing
is paraphrased. Comment text is quoted where a comment amends or decides a criterion.

| Issue | Linear id | updatedAt | File | sha256 |
|---|---|---|---|---|
| CRW-116 | b93d1a90-f419-4005-930e-f762ec0e8440 | 2026-09-25T04:48:35.211Z | `/tmp/crw-linear-CRW-116-issue.txt` | `1153e3aac20d588f377db08431a016cd3531c96ab0897fc8910ce515a6294648` |
| CRW-116 comments | 25 comments | latest 2026-09-25T00:43:48.134Z | `/tmp/crw-linear-CRW-116-comments.txt` (pointer) and `/home/jun/.omo/agent/tmp/mcp-out/linear-1790318482146-2000604894008-0.txt` (full) | `8d6688cdb17ad001401e491c176fc32d4f7179180a68114490c4413e74769925` / `ad27a867ea26d15ff51d2ae4e5acb3b63a32f78b7ed336d3405cd2ed914344ae` |
| CRW-124 | 4449d89e-298a-45dd-926b-3937a51b9f97 | 2026-09-25T04:48:35.079Z | `/tmp/crw-linear-CRW-124-issue.txt` | `bc1b972ada8abb351ddefb9fb51837ece7c252418ca721c247b37bdef9c07a60` |
| CRW-124 comments | 18 comments | latest 2026-09-25T03:23:46.296Z | `/tmp/crw-linear-CRW-124-comments.txt` | `b953368d7d306a041b8859a1cdfe13a3925978e55c74cbf6c65be99c36c67546` |

Read-back of counts (what Linear contains versus what this document carries):

| Issue | Linear source | Count in Linear | Headings in this doc | Match |
|---|---|---|---|---|
| CRW-116 | `완료 기준:` bullets in the body | 7 | CRW-116-1 .. CRW-116-7 | yes |
| CRW-116 | comments that add a criterion (2026-09-23 comment 00bfeace: CRW-212 one record per Stop event added to the canonical criteria) | 1 | CRW-116-8 | yes |
| CRW-124 | `## 완료 기준` bullets | 7 | CRW-124-1 .. CRW-124-7 | yes |
| CRW-124 | `## 완료 기준` closing paragraph (emission rules) | 1 | CRW-124-8 | yes |
| CRW-124 | `## 추가 완료 기준 — 실제 부모 간 협업 반영 (2026-09-19)` paragraph | 1 | CRW-124-9 | yes |
| CRW-124 | `## 유휴 보고의 필수 회귀 조건` paragraph | 1 | CRW-124-10 | yes |
| CRW-124 | `## 실제 하네스 수용 보강 — 2026-09-21 Jun`: lead paragraph, `필수 대조군:` bullets, closing paragraph | 1 + 6 + 1 | CRW-124-11 .. CRW-124-18 | yes |
| CRW-124 | comments that decide a criterion's reading (2026-09-24 comment c142e568: Jun's two decisions) | 1 | CRW-124-19 | yes |
| Totals | | CRW-116 8, CRW-124 19 | CRW-116 8, CRW-124 19 | yes |

The `### 일정 정합화 · 2026-09-22` sections of both bodies change due dates only and add no
criterion, so they carry no heading. Comments that report measurements are cited under the
criterion they measure; comments that only hand a parent over or announce an install are not
scenarios.

Verdict vocabulary used by the Python-era measurements: TRUE, FALSE, UNREADABLE, NOT_RUN,
NOT_APPLICABLE, plus the Linear result-table marks verified / needs_changes / unverified.

## CRW-116: 플러그인 설치본의 실제 왕복 검증

Body preamble, verbatim:

> 프로젝트 정본: [https://linear.app/jun786/project/713522f38044](<https://linear.app/jun786/project/713522f38044>)

> 대상 저장소: [https://github.com/thisisjun786/codex-relay-workflow](<https://github.com/thisisjun786/codex-relay-workflow>)

> 비 PR 운영 수용 검증. 구현 PR 통합과 실제 설치 성공을 구분하고, 실행 시 승인된 격리/호스트 범위에서 플러그인 사용 경로를 끝까지 검증한다. 이번 계획 등록만으로 운영 호스트 전환이나 공개 배포를 실행하지 않는다.

### CRW-116-1: body criterion 1

Linear text, verbatim:

> * 새 설치와 기존 설치 전환 모두 Codex 플러그인 화면·스킬 발견·MCP 실제 호출·Stop 훅 발화를 확인한다. 플러그인 등록만으로 통과하지 않는다.

Pass when: on the same host and install, all four observations are read for BOTH a new install (start state has neither `[plugins."crw@crw"]` nor `[marketplaces.crw]`, then marketplace add, plugin add; coordinator ruling Q1, comment 5b57634f) and an existing-install transition: `codex plugin list --json` lists `crw@<marketplace>`; `codex debug prompt-input` lists exactly the crw skills; a bridge MCP tool call in a real session returns a capability-shaped non-error response; and one Stop hook firing leaves exactly one plugin-owned journal record carrying that session and turn id (ruling Q2). A plugin listing alone never passes.

Under Python: TRUE for the transition half and for a new install in an isolated home; the real-host new install is open. Comment 4632327f (R2a f29a656e, 2026-09-24): '기준 1: TRUE. Desktop 플러그인 화면은 Jun 이 확인했습니다. 스킬 10개는 0.4.0+3e072ce20e86 에서 적재되고, 브리지 MCP get_capabilities 실제 호출이 정책 7e021234 를 냈으며 ... 격리 홈 신규 설치와 9265ab89→R2a 전환도 TRUE 입니다. 운영 호스트 신규 설치만 Jun 의 결정으로 남습니다.' Plugin screen seen by Jun: comment f02a2374. Earlier host row: comment 78e3b0ed (S1.screen/skill/mcp/hook.transition TRUE, journal session 01a0c046-6ee8 / turn 01a0c046-6f6e, owner=plugin). Evidence root `/state/crw/crw-116/revisions/<rev>/`.

### CRW-116-2: body criterion 2

Linear text, verbatim:

> * 부모 검증 → 같은 자식 수정 → 재검증을 사람 중계 없이 완료하고 실제 세션·회차·PR/head 또는 비 PR 산출물·판정 근거를 대조한다.

Pass when: one relay relationship shows the sequence parent verdict needs_changes -> correction delivered to the same child -> child's next generation report -> parent verdict verified with zero operator-authored messages in between, and the four recorded values (session id, turn id, PR head or non-PR artifact digest, judgment basis) agree pairwise between the relay records and the host turns.

Under Python: TRUE. Comment 4632327f: '기준 2 ... TRUE. 시험 관계 rel-c345bae07339ebd6 에서 1세대 보고 d34ca781 을 부모가 needs_changes 로 판정했고(09:03:12Z), 수정 요청 63037ab9 가 같은 자식에게 2세대로 갔으며, 2세대 보고 7efa5cbe 를 부모가 파일을 직접 읽고 verified 로 판정했습니다(09:05:45Z). 사람 중계는 없었습니다.' Same shape earlier at 4120e2a0 (comment 94b83594) and b19b3fd2 (comment cb10798d).

### CRW-116-3: body criterion 3

Linear text, verbatim:

> * active steer의 접수와 실제 반영, CXC 지시/보고/리뷰 계약, idle 부모 깨우기를 확인한다. CXC의 자체 상태를 릴레이 상태로 덮어쓰지 않는다.

Pass when: a steer sent into a running parent turn is accepted for that turn AND its instruction shows up in the parent's own later work; the CXC dispatch packet, child report and reviewer verdict each carry the fields their contract requires; an idle parent gains a new turn caused by a relay delivery with zero tokens spent while idle; and the CXC's own state has a last writer that is not the relay path.

Under Python: TRUE at R2a. Comment 4632327f: '기준 3: 모두 TRUE. 실행 중 steer 는 전달로 깨어난 부모 턴에 받아들여졌고 ... 쉬는 부모는 517초 동안 rollout 기록 0건, 토큰 증가 없이 있다가 전달로 깨어났습니다 ... CXC 계약은 배정 문구, 보고 줄, 판정이 모두 맞았고 ... CXC 상태는 덮어쓰이지 않았습니다.' The contract cell was PARTIAL at 4120e2a0 (comment 94b83594: correction request lacked the five CRW-149 sections) and was fixed by CRW-149 F4 before R2a.

### CRW-116-4: body criterion 4

Linear text, verbatim:

> * 업데이트·재시작 후 지원되는 복구 경로, 비활성화·제거 뒤 기존 데이터 보존과 중복 훅/데몬 부재를 확인한다. 소유한 시험 자원만 정리한다.

Pass when: after update and after service restart the supported recovery path exits 0 and the declared execution policy is unchanged; after disable and after remove the relay store, bridge ledger and hook journal rows are unchanged or grown only by the run; exactly one journal record exists per (session, turn) Stop pair; exactly one relay service exists for the scope; the cleanup diff is a subset of the run-owned resource ledger.

Under Python: TRUE at R2a. Comment 4632327f: '기준 4: TRUE. R2a 설치 때 서비스가 선언 정책 7e021234 로 다시 떴고(감독 3971159, 워커 3971178), 격리 홈에서 비활성화·제거 뒤 저장소·원장·저널이 남고 제거는 캐시와 설정만 건드렸습니다. 운영 저장소는 R1, R2a 모두 127개 그대로(이전 없음, 같은 inode)입니다.' The duplicate-hook half was FALSE at 174eacd (comment 3ae91526: 4 of 69 pairs, all stopHookActive re-firings) and became CRW-212; see CRW-116-8.

### CRW-116-5: body criterion 5

Linear text, verbatim:

> * <issue id="d05f4bca-527e-4dbc-b780-a9d41ebde7f1" href="https://linear.app/jun786/issue/CRW-111/실구동-시험의-준비-조건과-시작-순서를-고정">CRW-111</issue> 준비 조건과 기존 유효 근거를 재사용하고 신규 패키징/호스트 변경으로 무효화된 부분만 재검증한다.

Pass when: the reuse set and the rerun set of prior evidence atoms are disjoint and together exhaustive, and every atom invalidated by the new packaging or host change has a fresh judged record on the current install.

Under Python: 17 of 19 invalidated atoms reconfirmed at R2a; the real-host new install remains open. Comment 4632327f: '기준 5: 9265ab89→R2a 로 무효화된 원자 19개 중 17개를 R2a 에서 다시 확인했고, 안내서는 부모 판정으로 충족, 운영 호스트 신규 설치는 Jun 의 결정입니다.' Reuse/rerun partition at `~/.local/state/crw-run/crw-116-roundtrip-prep/reuse-sets.json`.

### CRW-116-6: body criterion 6

Linear text, verbatim:

> * 사용자가 따라 할 수 있는 설치·업데이트·복구·제거 안내, 확인한 버전·호스트·제약과 미검증 항목을 남긴다. 필요 소스/문서 수정은 해당 구현 이슈로 반환한다.

Pass when: a reader who is not the author follows the guide alone and reaches the recorded verdicts; the guide states the verified version, host, constraints and the unverified items; every required source or doc change names its owning implementation issue.

Under Python: satisfied by parent judgment. Comment 10fd303a: '**부모 판정 · 기준 6: 충족.** 근거는 열세 번째 독자가 15판의 여섯 주요 흐름을 이 호스트에서 모두 따라갈 수 있다고 본 것(A-main 0)이다.' Comment 4632327f: '기준 6: 부모 판정으로 충족(04:17Z). 부모가 R1·R2a 설치에서 안내서 16판 3.2 순서와 스크립트를 그대로 따랐고 ... 17판은 버전과 호스트 사실만 R2a 에 맞췄습니다.' Evidence `/state/crw/crw-116/revisions/9265ab89/evidence/C6_fresh_reader11~13_report.md`.

### CRW-116-7: body criterion 7

Linear text, verbatim:

> * 공개 배포가 필요하면 검증된 산출물과 구체적인 배포 절차를 준비해 별도 승인 경계를 명시한다.

Pass when: if publication is required: verified artifacts, concrete deployment steps and the named approval boundary are all present; if not required: recorded as NOT_APPLICABLE with the reason.

Under Python: NOT_APPLICABLE. Comment 4632327f: '부모 판단: 기준 2·3·4·6 은 충족, 기준 7 은 해당 없음이다.' The v0.4.0 prerelease (comment 09aa97d7) was an install-test artifact, not the publication this criterion speaks of.

### CRW-116-8: comment-added criterion (CRW-212, one record per Stop event)

Linear text, verbatim (last paragraph of comment 00bfeace, 2026-09-23):

> CRW-212는 Stop 이벤트당 1건으로 결정 완료. 같은 턴의 서로 다른 정상 재발화는 허용하고 동일 이벤트의 중복 수용·효과는 대조군으로 검증한다. 정본에 새 결정과 완료 기준을 추가했다.

Pass when: replaying one accepted Stop event more than once (`replay_stop.py --times 2` against the same eventKey) yields exit 0, empty stdout and a `duplicate_invocation` record with no guard call for every copy after the first; the per-event judge reports `eventsWithMoreThanOneAcceptance []` over the window since the pointer swap; distinct natural re-firings within one turn (stopHookActive) are allowed and each leaves exactly one accepted record.

Under Python: TRUE. Comment 4632327f: 'CRW-212: 같은 사건 재생(replay_stop.py --times 2)이 시험 부모 첫 턴에서 중복 두 줄(duplicate_invocation, 가드 호출 없음, 같은 eventKey)을 냈고, 사건 단위 판정기는 08:20:47Z 이후 사건 23건에서 TRUE 입니다.' Comment 10fd303a: judge over 9 events, `eventsWithMoreThanOneAcceptance []`, `turnsWithMoreThanOneEvent 1`. Judge is `scripts/stop_events.py` (Python; its Go port is todo 48).

## CRW-124: 설치본에서 감독·부모·자식의 무개입 운영을 실증

Body preamble, verbatim:

> 참조 저장소: codex-relay-workflow. 비 PR 검증이며 코드 수정·새 실행 배정은 아직 하지 않는다.

> 코드 변경 없는 비 PR 실사용 검증. 실제 설치본의 버전·소스·설정과 재현 절차, 기준별 판정, 비공개 실행 증거를 남긴다. 빈 PR을 만들지 않는다. 발견한 수정은 실제 소유 이슈의 한 PR로 반환한다.

> 앞선 계층 구현이 통합된 정상 설치 산출물을 사용한다. 플러그인 전환 <issue id="b93d1a90-f419-4005-930e-f762ec0e8440" href="https://linear.app/jun786/issue/CRW-116/플러그인-설치본의-실제-왕복-검증">CRW-116</issue>의 실제 설치/왕복 증거와 <issue id="d05f4bca-527e-4dbc-b780-a9d41ebde7f1" href="https://linear.app/jun786/issue/CRW-111/실구동-시험의-준비-조건과-시작-순서를-고정">CRW-111</issue>의 준비 절차를 재사용하되 같은 결과라고 간주하지 않는다. 운영 환경 변경은 실제 실행 시 이미 허용된 범위에서 수행하고 다른 살아 있는 작업을 보존한다. native goal 모드라면 <issue id="4c4d346a-7457-4410-8076-aa549474eb19" href="https://linear.app/jun786/issue/CRW-29/프로젝트-부모의-조정-완료와-cxc-소스-변경-게이트-충돌-규명">CRW-29</issue>의 지원 경로가 먼저 검증돼야 한다.

### CRW-124-1: 완료 기준 bullet 1

Linear text, verbatim:

> * 감독 1개, 서로 다른 프로젝트 부모 2개 이상, 부모별 자식으로 실제 바인딩·배정·검증·수정·통합·후속 진행을 수행한다. 문서상의 이름이나 스텁 성공으로 대체하지 않는다.

Pass when: one supervisor, two or more parents on different projects, and per-parent children run bind, assign, verify, correct, integrate and follow-up on the installed build; every step is observed as a relay record plus a host turn, never as a documented name or a stub success; all lifecycle steps for one parent's child share one assignmentId and projectKey.

Under Python: verified at R3/R4 with defects routed out. Comment 1a81180f (G1, 2026-09-24): 'S 가 이니셔티브를 묶고 두 프로젝트를 넘겼다. PA·PB 는 프로젝트를 묶고 A-B 연결을 등록했다. 관리 시작으로 자식을 띄웠고 ... A1·B1 은 merge turn 으로 로컬 origin 에 랜딩했고, A2·B2 는 시작해 검증까지 했다. 후속 랜딩은 F-G1-3 에 막혔다.' Landing then completed at R4 via CRW-229 (comment 33c50e83, 'G1c, 개입 0'). Evidence `/state/crw/crw-124/phase-b/g1-result.json`, `r4-remediation-result.json`.

### CRW-124-2: 완료 기준 bullet 2

Linear text, verbatim:

> * 양 계층에서 active steer와 idle 재개를 실제로 관측한다. 감독/부모가 각각 유휴인 구간과 수정 왕복 구간에 인간의 재개 지시·중계가 0회인 증거를 남긴다. 사람이 보정한 준비 구간과 무개입 판정 구간을 나눈다.

Pass when: for both layers (supervisor and parent) an active steer and an idle resume are observed; for each idle interval and each correction round trip the enumeration of human resume instructions and relays over the full channel set is empty with cursor continuity proven; the human-corrected preparation segment and the unattended judgment segment are recorded as separate segments.

Under Python: verified. Comment 1a81180f: 'relay 가 유휴 부모와 감독을 9번 깨웠다(각 1회 시도, 모두 ACK). 활성 턴 steer 3건이 반영됐다 ... 유휴 구간 13:29:47~13:32:48Z: S·PA·PB 모두 새 턴·rollout 항목·토큰·OpenCodex 사용 행이 0 이었다.' Comment 600e8f79 (G2): '판정 구간 사람 중계는 0회이고, 유휴 구간 4분 22초 동안 S·PA·PB·SR의 턴·토큰·사용량 행 증가는 0이었습니다.' Comment 6e068ef0 (R5): idle window 21:28-21:50Z re-observed.

### CRW-124-3: 완료 기준 bullet 3

Linear text, verbatim:

> * 하위 실행 중 감독/부모의 압축·턴 종료·지원된 재시작을 겪고 같은 작업·PR·관계·미완료 결과가 복구된다. 영수증 재전송과 늦은 결과가 중복 실행을 만들지 않는다.

Pass when: during a child's execution the supervisor or parent undergoes compaction, turn end and a supported restart, and afterwards the same task, PR, relationship and unfinished result are recovered under the same ids; a resent receipt and a late result each produce zero additional executions (late-result marker and replay marker present).

Under Python: verified for compaction, turn end, relay restart, ACK loss; restart measured on an isolated App Server per the Jun decision in CRW-124-19. Comment 600e8f79 (G2): '공유 relay를 7분 53초 멈춘 사이 B8 보고가 staged됐다가 재시작 뒤 정확히 한 번 전달돼 압축된 PB를 깨웠고, PB가 B7 ACK를 복구하고 B8을 판정했습니다. 멈춤 전에 이름을 적어 둔 40개 미완료 전달은 모두 남아 있었습니다.' and 'W5에서는 B7 전달 turn을 끊어 ACK 유실을 만들었는데 재전송 replay가 두 번째 깨움 없이 수렴했습니다.' Defect found on the way: CRW-224 (comment 3374feb9, H0-F1) fixed and re-run at R4 (comment 33c50e83, K5c·K5u·K5·K5b·W5a·H7 pass).

### CRW-124-4: 완료 기준 bullet 4

Linear text, verbatim:

> * 공유 프로젝트 참조·동시 머지 요청·상위 pause/cancel·범위 축소·오래된 revision을 시험하고, 일부 하위만 완료된 상태에서는 상위 전체를 완료하지 않는다.

Pass when: shared project reference, concurrent merge requests, upper-level pause/cancel, scope reduction and a stale revision are each exercised on the installed build and recorded; while only some children are complete the parent is not marked complete (state diff shows the parent incomplete).

Under Python: verified at G3 (R4). Comment 034ffff5: '동시 머지 요청은 강제로 겹치게 했습니다. ... 소유자는 늘 하나였고 중복 landing도 없었습니다.' Comment 600e8f79 (G2): 'W4a/W4b에서는 사용자 중지로 B4 보고가 staged 상태로 멈췄고 B6는 취소된 뒤 되살아나지 않았습니다. 재개 후 보고는 한 번 전달됐고, Z 요구로 needs_changes를 거쳐 2세대가 검증·landing됐습니다.' Stale revision, comment 034ffff5: 'PA의 늦은 수락은 agreement_revision_stale로 거부됐고, 현재 revision으로 reaffirm한 뒤 합의됐습니다.' The parent decision in comment 034ffff5 accepts the '동시 머지 요청, 늦은 ACK·기반 변경' rows as R4-verified.

### CRW-124-5: 완료 기준 bullet 5

Linear text, verbatim:

> * 과거 실제 실패인 리뷰 100개 페이지 경계, 상태 쓰기 오류 payload, 긴 wait 셀로 인한 steer 적용 지연, 문맥 증가를 이유로 한 조기 종료를 회귀 사례에 포함한다. 주입 자료의 검증과 실제 호스트 관측을 구분한다.

Pass when: each of the four named past failures (100-review page boundary, status-write error payload, steer delay from a long wait cell, early termination on context growth) has its own regression case with provenance to the historical failure, and each case records whether its observation came from injected material or from the real host.

Under Python: partially observed. Steer delay from a long wait cell: comment 1a81180f 'S→PA steer 는 PA 가 최대 175초짜리 wait 셀 안에 있어 137초 뒤 반영됐다(C5 긴 wait 셀 회귀 사례로 기록, 기준 문턱 없음).' Review page boundary: reused from the merge-review page source test (comment 6b32d12b, phase-A reuse list). Status-write error payload: the original case was still being identified at phase A (comment 6b32d12b, D-P11). Early termination on context growth: no comment records it. Injected-vs-host split kept per row (comment 6e068ef0 lists shared-host vs isolated-host usage).

### CRW-124-6: 완료 기준 bullet 6

Linear text, verbatim:

> * 프로젝트 단독과 이슈 단독 실행도 기존대로 작동한다. 기존 작업 강제 이관·원 설계 덮어쓰기·가짜 source delta·훅 비활성화가 없다.

Pass when: a project-only run and an issue-only run both complete as before on the installed build; no forced migration of existing work, no overwrite of the original design, no fake source delta and no hook disabling is observed.

Under Python: verified at G3 (R4). Comment 034ffff5: '프로젝트 단독(C1)과 이슈 단독(U1) 실행, 관계 없는 direct 실행(TD)도 기대대로 동작했습니다.' Parent decision: 'C6(프로젝트 단독·이슈 단독) ... 행을 R4 검증으로 받아들입니다.' No forced migration / design overwrite / fake delta / hook disable was reported in any comment.

### CRW-124-7: 완료 기준 bullet 7

Linear text, verbatim:

> * 원격 통합, 설치 등록, 실제 실행, 판정된 완료를 분리한 최종 결과표를 작성하고 각 기준은 verified/needs_changes/unverified로 표시한다. 필요한 항목이 unverified이면 프로젝트 완료로 처리하지 않는다.

Pass when: the final table separates remote integration, install registration, real execution and adjudicated completion into four columns; every criterion carries exactly one of verified / needs_changes / unverified derived from its observations; project completion is refused while any required item is unverified.

Under Python: not yet emitted. Comment 6e068ef0 (R5): '최종 결과표는 CRW-235와 CRW-237이 설치된 R6 재검 뒤에 씁니다.' Comment 2ccea63d (2026-09-25 stop checkpoint): '재개하면 CRW-238 병합, R6 설치 한 번, 그다음 영향받는 설치본 행(231 K5ctl, 235 bridge 적재, 238 K2)만 다시 확인합니다.' The table's column and mark rules are frozen in `~/.local/state/crw-run/project-71acafb6/crw-124-prep/devlog/_plan/260920_crw124_prep/registry.json` (emissionObligations c7.*).

### CRW-124-8: 완료 기준 closing paragraph (emission rules)

Linear text, verbatim:

> 현 세션의 직접 감독 사례는 재현 출발점이지 이 이슈의 통과 증거가 아니다. 실제 전체 토큰·시간과 상위/하위 사용량은 구분하고 검증되지 않은 절감률은 쓰지 않는다.

Pass when: the direct-supervision example from the authoring session appears only in a separate candidates list with no verdict column and is cited by no deciding cell; the result table reports total tokens and wall time and splits upper-layer from lower-layer usage; no savings percentage appears without its own measurement.

Under Python: applied as a rule, not a measured row. Comment 6b32d12b (phase A): '유휴 구간은 rollout 카운터와 OpenCodex usage.jsonl ... 로 이중 측정한다.' Comment 6e068ef0: '사용량: 공유 host는 K2 자식 3턴, PB 1턴, S 2턴이고, 격리 host는 39턴(P1 21턴)입니다.' Candidates kept separate in `~/.local/state/crw-run/project-71acafb6/crw-124-prep/candidates/`.

### CRW-124-9: 추가 완료 기준 — 실제 부모 간 협업 반영 (2026-09-19)

Linear text, verbatim:

> 설치본에서 부모↔부모의 실제 공유 영역 협상도 필수로 실증한다. 제안 → 조건부 수락 → 소유 부모의 자식 지시 → 관측된 변경 → 상대의 검증까지 message ID·합의 revision·증거를 연결한다. 실제 <issue id="e5792c57-3f85-4593-bca2-796cfe9c078d" href="https://linear.app/jun786/issue/CRW-100/판독이-부재의-원인을-구분하지-못하고-진단이-잔여-경로를-내주지-않는다">CRW-100</issue>/115 사례처럼 진단 동작과 설치 위치가 만나는 조건, 임시 중복의 동작 일치, 범위 밖 제거 약속을 구분하는 시나리오를 재현한다. 거절, 조건 변경, 늦은 ACK, 기반 변경, 미배정 후속과 상위 복구를 넣고 독립 영역 진행·중복 지시 0회·사람 중계 0회를 확인한다. 운송 수락과 부모의 전달 주장만으로 자식 효과를 통과시키지 않는다. 메시지 수·중복 본문·요청부터 합의/반영/검증까지 시간을 기록하되 새로운 임의 성능 문턱이나 절감률을 만들지 않는다.

Pass when: on the installed build two parents negotiate a shared region: proposal -> conditional acceptance -> owning parent's child instruction -> observed change -> counterpart's verification, with message ids, agreed revision and evidence linked across the chain; rejection, condition change, late ACK, base change, unassigned successor and upstream recovery are each exercised; independent-region progress is observed; duplicate instructions = 0 and human relays = 0 over the full channel set; a transport acceptance or a parent's delivery claim never counts as a child effect; message count, duplicate bodies and request-to-agreement/apply/verify timings are recorded with no new thresholds or savings rates.

Under Python: verified at G2/G3 (R4) with two defects routed out. Comment 600e8f79 (G2): 'A의 첫 제안(agr-72bd967d)을 B가 거절하면서 임시 방식을 제시했고, A가 merge 순서를 조건으로 수락했습니다. A3이 먼저 landing하고, B3를 A3 위로 rebase해 landing한 뒤, B가 바뀐 조건을 새 base에서 다시 기록했습니다(agr-d27f4e1e). ... 어느 이슈에도 속하지 않는 후속 작업은 S에게 올라갔고, S가 A 소유로 정했습니다(A3-pointer).' Comment 034ffff5 (G3): late ACK and base change forced and re-observed; 'reaffirm이 제안자·조건·수락을 바꿔 버리는 문제(F-G3-2)' -> CRW-237; F-G2-1 -> CRW-235. Diagnostic-meets-install condition (CRW-100/115 style) is not reported as a separate row in any comment.

### CRW-124-10: 유휴 보고의 필수 회귀 조건

Linear text, verbatim:

> <issue id="9ad7d703-d205-40b8-8846-c818b0c3073e" href="https://linear.app/jun786/issue/CRW-122/감독과-부모의-결과-전달재개완료-집계를-연결">CRW-122</issue>에 기록한 실제 unsupported_approval_policy 결함을 설치본에서 재현·검증한다. on-request 감독/부모가 idle인 경우와 앱 서버 재시작 후에, 승인 정책·sandbox를 유지하면서 보고 수신과 후속 조정까지 사람 중계 없이 이어져야 한다. active steer 성공, 영속 저장만 성공, 감독 수동 재개만으로 통과시키지 않는다. 실제 승인이 필요한 행동은 기존 승인 경로를 유지하고 미승인 실행이 없어야 한다.

Pass when: the unsupported_approval_policy defect recorded in CRW-122 is reproduced on the installed build; with an on-request supervisor/parent idle, and again after an App Server restart, report receipt and the follow-up coordination proceed with zero human relays while approval policy and sandbox are preserved; a run that shows only active-steer success, only persistence success, or only a manual supervisor resume does not pass; every action that needs real approval goes through the existing approval path and no unapproved execution occurs.

Under Python: partially verified; the on-request half is deferred behind CRW-225. Comment c142e568 (Jun, 2026-09-24): 'on-request 감독·부모의 무개입 수신(R1):** on-request 지원을 새 제품 이슈 CRW-225 로 만들었다. CRW-124 는 그 뒤로 미룬다.' Comment ed568202 (R3): 'R1 행(on-request): 부모·감독·재시작 수신을 R3 에서 확인했다. 근거는 CRW-225 설치본 실측(사람 중계 0회)과 격리 H0-R3 이다. 재시작 쪽은 Jun 결정대로 격리 App Server 재시작으로 쟀다. 승인 요청이 소유자 화면에 보이는지는 CRW-226(Jun 확인)을 기다린다.' Comment 034ffff5 (G3): 'on-request 부모가 바쁠 때 온 자식 보고는 deferred_busy를 거쳐 turn/start가 정확히 한 번 일어났습니다.' Evidence `/state/crw/crw-124/phase-b/h0-r3-result.json`.

### CRW-124-11: 실제 하네스 수용 보강 — 2026-09-21 Jun: lead paragraph

Linear text, verbatim:

> <issue id="b114b01c-5754-4247-a6b8-1e1c144fe8c2" href="https://linear.app/jun786/issue/CRW-177/test-rolepolicy-픽스처의-부모-쌍을-swe-복원에-맞춰-갱신">CRW-177</issue>의 직접 send → 부모 수신/merge 성공은 업무 완료 사례이며 relay 자동 인계 통과 사례가 아니다. <issue id="1bff733b-5baf-4ff6-8536-a66babeb62db" href="https://linear.app/jun786/issue/CRW-180/관리-배정-등록과-미보고-종료-탐지를-하네스에-연결">CRW-180</issue>의 관리 진입/미보고 탐지와 <issue id="15166873-9593-4fe4-a565-6f68e260e573" href="https://linear.app/jun786/issue/CRW-165/부모를-목표-없는-릴레이-이벤트-실행으로-전환">CRW-165</issue>의 목표 없는 이벤트 수신을 정상 설치본에서 함께 소비한다. 기존 기준과 증거는 보존한다.

Pass when: the CRW-177 direct send -> parent receive/merge case is recorded as a business completion and never as a relay auto-handoff pass; CRW-180 managed entry and unreported-exit detection and CRW-165 goal-less event receipt are both consumed on the same normal install; prior criteria and evidence stay in place.

Under Python: consumed at phase A and G1+. Comment 6b32d12b: 'CRW-180·165·116·214·215·205 실측은 delivery·packets·receiver·supervisorchannel 과 제공 스킬이 바뀌어 방법·선례로만 쓰고 통합 실행에서 다시 관측한다.' Comment 034ffff5 (G3): '관계 없는 direct 실행(TD)도 기대대로 동작했습니다' recorded as a separate row, not as relay automation.

### CRW-124-12: 필수 대조군 bullet 1

Linear text, verbatim:

> * 정상 관리 배정에서 LLM의 직접 send/steer 호출 없이 구조화된 결과가 영속 큐 → 부모 실제 턴 → ACK/판정/후속 행동으로 이어짐.

Pass when: in a normal managed assignment, the child's structured result reaches the parent via the persistent queue -> a real parent turn -> ACK/verdict/follow-up action with zero direct send/steer calls by any LLM in that chain.

Under Python: verified. Comment 1a81180f (G1): 'K1: 자식 완료 6건이 영속 큐를 거쳐 부모 자기 턴으로 갔다. LLM 의 직접 send 로 간 완료는 없다.' Comment 6e068ef0 (R5): normal delivery, ACK and verdict re-observed.

### CRW-124-13: 필수 대조군 bullet 2

Linear text, verbatim:

> * 보고/disposition을 의도적으로 생략한 자식의 종료는 완료가 아니라 미보고로 탐지되며, 원 담당 복구 또는 명시적 보류가 남음. 무한 Stop/목표 루프를 쓰지 않음.

Pass when: a child that deliberately ends without emit and disposition is derived as unreported (not complete) by the relay itself, an original-owner recovery or an explicit hold is left behind, and no unbounded Stop or goal loop is used.

Under Python: needs_changes at R5, fix in CRW-238 (blocks). Comment 6e068ef0: '보고 생략 셀(K2, D-G3-1) ... 20분(omission grace 300초의 4배) 동안 relay는 아무것도 도출하지 못했습니다 ... 원인은 daemon이 그 턴을 settle하는 경로입니다 ... F-R5-2(보고 생략 도출이 약 91분 걸림) → 새 이슈 CRW-238, 이 이슈를 막음.' Earlier F-G3-1 in comment 034ffff5.

### CRW-124-14: 필수 대조군 bullet 3

Linear text, verbatim:

> * 관계 없는 명시적 direct 실행은 정상 지원하되 relay 자동성으로 집계하지 않음.

Pass when: an explicit direct execution with no relationship works, and the result table counts it outside relay automation.

Under Python: verified at G3. Comment 034ffff5: '관계 없는 direct 실행(TD)도 기대대로 동작했습니다.'

### CRW-124-15: 필수 대조군 bullet 4

Linear text, verbatim:

> * goal absent/paused/blocked, 사용자 중단/보관, 부모 active/idle/notLoaded를 별도 행으로 기록. 대기를 blocked로 이름만 바꿔 자동성에 통과시키지 않음.

Pass when: goal absent/paused/blocked, user stop/archive, and parent active/idle/notLoaded each have their own row with the observed delivery outcome; a wait is never relabelled blocked to pass automation.

Under Python: verified at G3 (R4). Comment 034ffff5: 'goal blocked인 부모에는 한 번 전달됐고, archived 부모는 recipient_archived로 보류된 채 전송이 0회였습니다. paused 부모는 recipient_paused로 보류됐다가 goal을 해제하자 한 번 전달됐습니다.' Parent decision accepts W8a-c rows.

### CRW-124-16: 필수 대조군 bullet 5

Linear text, verbatim:

> * 자식 receipt가 아직 inProgress 턴을 가리키는 경우, 오래된 turn·새 generation·전송 불명·ACK 유실을 구별하고 전달 보류 이유를 조회할 수 있음.

Pass when: for a child receipt that still points at an inProgress turn, the relay distinguishes stale turn, new generation, transport unknown and lost ACK, and the hold reason is queryable (`assignment-show` / `reporting-show`).

Under Python: verified at R4/R5 after CRW-224 and CRW-231. Comment 3374feb9 (H0): defect H0-F1 'App Server 가 받아들인 턴을 잃으면 릴레이가 자식 보고를 조용히 잃는다. ACK 유실과 구별도 안 된다' -> CRW-224. Comment 33c50e83 (R4): 'K5c·K5u·K5·K5b·W5a·정상 ACK·H7 통과.' and 'H0R4-F1(전송 결과 불명 뒤 턴 소실 시 held_uncertain 무기한)과 O-H0R4-2(시간당 한도 보류 이유 미표시) → CRW-231 신규, 이 이슈를 막음.' Comment 6e068ef0 (R5): 'K5b·K5c·W5a 는 host_lost_turn 코드가 R4→R5 에서 바뀌지 않아 R4 판정을 유지합니다'; F-R5-1 (held fault stays degraded) still open in CRW-231.

### CRW-124-17: 필수 대조군 bullet 6

Linear text, verbatim:

> * 반복 이벤트·재시작에서도 동일 이슈/PR의 조정 행동이 중복되지 않음. 같은 이벤트의 생산/큐/전송/수신/ACK/실제 반영/수락/landing/Linear 동기화와 다음 담당을 구별함.

Pass when: across repeated events and restarts the coordination action for the same issue/PR is not duplicated, and production, queue, transport, receipt, ACK, actual application, acceptance, landing, Linear sync and next owner are recorded as distinct facts for one event.

Under Python: verified for the observed windows. Comment 600e8f79 (G2): 'B7 전달 turn을 끊어 ACK 유실을 만들었는데 재전송 replay가 두 번째 깨움 없이 수렴했습니다. ... B8 보고가 staged됐다가 재시작 뒤 정확히 한 번 전달돼 압축된 PB를 깨웠고, PB가 B7 ACK를 복구하고 B8을 판정했습니다.' Comment 034ffff5 (G3): 'deferred_busy를 거쳐 turn/start가 정확히 한 번 일어났습니다.' Linear sync column recorded as n/a for the relay-test project per parent decision (comment 6b32d12b).

### CRW-124-18: 실제 하네스 수용 보강: closing paragraph

Linear text, verbatim:

> 기계적으로 보장된 절차, LLM의 의미 판단, 수동 개입, 지원되지 않은 host 경계를 결과표에서 구분한다. 더 풍부한 메시지 양식·정책 재주입·머지 조율 개선 전체를 이 이슈의 새 선행으로 무조건 추가하지 않는다. <issue id="1bff733b-5baf-4ff6-8536-a66babeb62db" href="https://linear.app/jun786/issue/CRW-180/관리-배정-등록과-미보고-종료-탐지를-하네스에-연결">CRW-180</issue>/165의 핵심 자동 인계가 이번 Go 선행 수용 경계다.

Pass when: the result table marks each row as mechanically guaranteed procedure, LLM semantic judgment, manual intervention, or unsupported host boundary; no richer message formats, policy re-injection or merge-coordination improvements are added as new prerequisites of this issue; the CRW-180/165 core auto-handoff rows are the Go-prerequisite acceptance boundary.

Under Python: applied as a table rule; O-G1c-1 recorded as a documented host boundary (comment 33c50e83: 'O-G1c-1(부모가 적재되지 않은 감독에게 직접 메시지를 보낼 수 없음)은 G2 에서 문서화된 host 경계로 기록합니다.'). Final table pending R6 (CRW-124-7).

### CRW-124-19: comment decision (Jun, 2026-09-24): App Server restart cell and on-request R1

Linear text, verbatim (full body of comment c142e568, 2026-09-24):

> ## Jun 결정 두 건 (2026-09-24, 부모 01a0c514 기록)
>
> - **앱 서버 재시작 칸:** 격리 App Server(같은 설치본, 별도 CODEX_HOME)에서 잰 재시작 증거를 이 칸의 충족으로 인정한다. 공유 App Server 재시작은 하지 않는다.
> - **on-request 감독·부모의 무개입 수신(R1):** on-request 지원을 새 제품 이슈 CRW-225 로 만들었다. CRW-124 는 그 뒤로 미룬다. CRW-225 는 CRW-124 를 막는 관계로 걸었다.
>
> 이에 따라 2단계의 공유 쓰기 창 G1~G3 은 CRW-224(호스트가 잃은 턴의 보고 복구)와 CRW-225 가 병합·설치된 뒤에 연다. H0 격리 리허설 결과와 부모 결정(D-P1~D-P10, D-H0-1~4)은 그대로 유효하다. 설치 리비전이 바뀌면 차이 규칙으로 영향받은 행만 다시 돌린다. 이 이슈는 In Progress 를 유지한다.

Pass when: the 'after App Server restart' cell of CRW-124-10 is satisfied by a restart of an isolated App Server (same install, separate CODEX_HOME), never by restarting the shared App Server; the on-request unattended receipt (R1) is judged only after CRW-225 is merged and installed; the isolated H0 results and parent decisions D-P1..D-P10, D-H0-1..4 stay valid; when the installed revision changes only the rows touched by the diff are re-run.

Under Python: applied from R3 on. Comment ed568202: '재시작 쪽은 Jun 결정대로 격리 App Server 재시작으로 쟀다.' Comment 33c50e83: 'R3→R4 에서 바뀐 relay 모듈 기준으로 relay 경유 행을 모두 R4 에서 다시 관측했고, host 전용·LLM 행동 행은 R3 개정을 밝혀 이어 씁니다.'

## Replay obligations for the Go build

Todo 42 replays every heading above on the fenced Go owner and records the same vocabulary.
Rows the Python build closed (CRW-116-2/3/4/6/8, CRW-124-1/2/3/4/6/9/12/14/15/16/17) must read
TRUE or verified again; rows Python left open (CRW-116-1 real-host new install, CRW-116-5,
CRW-124-5 two regression cases, CRW-124-7 final table, CRW-124-10 on-request half, CRW-124-13
unreported-exit derivation pending CRW-238) are replayed as written and reported with the
blocking issue if still open. The Python-only judge `scripts/stop_events.py` used by CRW-116-8
is a todo-48 port; until then the replay records its Python invocation as a dev tool, not a
product path.
