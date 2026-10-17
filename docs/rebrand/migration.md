# 저장 상태 리브랜딩 이관 설계 (CRW-1525)

상태: **승인을 위한 설계**. 이 문서는 런타임이나 저장 데이터를 변경하지 않는다. CRW-1518의 이름·옛 루프 정리를 위한 저장 호환 계약이며, 구현은 이 설계 승인 뒤 CRW-1532에서 시작한다. CRW-1533·CRW-1534도 아래 단계와 시험을 공통 인계 기준으로 사용한다. 새 이름은 모두 `<새 이름: CRW-1523>`이며, 실제 철자는 CRW-1523의 승인된 대응표에서만 가져온다. 패키지를 남기거나 합치거나 지우는 판단은 CRW-1524가 맡는다.

## 1. 조사 기준과 실제 저장 경계

2026-10-17 조사 기준은 `origin/dev`의 `73aa64cacba835eea3a8f1edf3908ff268b09a90`이다. 다음 명령의 결과를 고정했다.

```sh
git grep -I -i -o -n pabcd 73aa64cacba835eea3a8f1edf3908ff268b09a90 --
git ls-tree -r --name-only 73aa64cacba835eea3a8f1edf3908ff268b09a90 -- internal/pabcd
```

첫 명령은 추적된 text blob의 대소문자 구분 없는 부분 문자열 **출현 수**다. 같은 줄의 여러 출현도 따로 세며 파일 수는 경로의 중복을 제거한다. 바이너리·gzip 내부·untracked 파일은 포함하지 않는다. 예전 집계와 정의가 같다고 가정하지 않는다.

| 범위 | 일치 파일 | 출현 |
| --- | ---: | ---: |
| 저장소 전체 | 1,315 | 6,884 |
| `internal/pabcd/` | 400 | 1,844 |
| `internal/flow/` | 26 | 34 |
| `internal/relay/` | 19 | 39 |
| `contract/fixtures/` | 299 | 616 |
| `docs/port-cxc/` | 172 | 1,331 |
| `plugins/crw/` | 60 | 319 |

`internal/pabcd`의 Go 파일이 속한 첫 하위 디렉터리는 19개다: attest, cli, crwdir, evidence, fsm, gate, goalplan, hook, host, interview, metric, projectcfg, reset, review, shellir, source, state, stateroot, text. 중첩된 Go import package 전체 개수는 아니다. 이름 검색 결과 전체를 데이터 이관 대상으로 취급하지 않는다.

| 저장 대상 | 현재 계약과 근거 | 이관 결정 |
| --- | --- | --- |
| 프로젝트 세션 | [state.go](../../internal/pabcd/state/state.go)의 `StatePath`: `<native cwd>/.crw/sessions/<canonical id>.json`. 상태는 최상위 `phase`, `sessionId`, epoch·receipt·source 필드를 가지며 현재 세션 파일에는 통합 schema/version 필드가 없다. | 세션 디렉터리의 새 이름은 CRW-1523에서 받는다. 세션 ID와 native cwd를 유지하고, 변환된 복사본을 새 경로에 게시한다. 옛 파일은 검증된 백업과 함께 보존한다. |
| 전이 ledger | [ledger.go](../../internal/pabcd/state/ledger.go)의 `.crw/ledger.jsonl`; `.crw/interviews/<session>.jsonl`은 별도 scan ledger다. [outbox.go](../../internal/pabcd/state/outbox.go)는 `<state file>.ledger-outbox/`의 event ID·원문 줄·byte offset·state digest로 중복 append와 누락을 막는다. | ledger의 새 파일명도 CRW-1523에서 받는다. 이전 줄은 순서·중복·바이트를 그대로 복사한다. outbox가 미해결이면 이관을 거절한다. scan ledger는 참조 의존성으로 같은 snapshot에 포함한다. |
| 상태 루트 | [stateroot.go](../../internal/pabcd/stateroot/stateroot.go)는 실행 thread의 native cwd를 지킨다. [anchor.go](../../internal/pabcd/stateroot/anchor.go)는 `CRW_HOME` 또는 `~/.crw` 아래 `state-roots/<session>.json`에 version 1의 `sessionId`, `nativeCwd`를 기록한다. | 경로 이름 변경과 cwd 이전을 구별한다. anchor의 이름을 바꾸더라도 `nativeCwd`는 그대로다. 다른 worktree를 source로 바인딩한 사실은 실행 루트를 바꿀 근거가 아니다. |
| 공유 evidence·goalplan | [evidence.go](../../internal/pabcd/evidence/evidence.go)의 `.crw/evidence`, attempts; [assignment.go](../../internal/pabcd/evidence/assignment.go)의 `.crw/evidence-assignments`는 native version 1과 flow version 2를 구별한다. [goalplan/types.go](../../internal/pabcd/goalplan/types.go)는 `.crw/goalplans/<slug>/goalplan.json`과 별도 ledger, 최대 schemaVersion 3을 규정한다. | `.crw` 전체를 통째로 이름 바꾸지 않는다. source·crwdir·evidence를 쓰는 flow가 있다. 공유 경로·기존 receipt bytes·root identity는 유지한다. 보존/통합 대상의 참조는 snapshot에서 검사하고, 제거 대상으로 정해져도 이관 중 삭제하지 않는다. |
| relay·flow 기록 | [flow journal](../../internal/flow/engine/journal.go)은 append-only이고 [영속 journal](../../internal/flow/journal/journal.go)은 relay store의 `flow_runs`, `flow_calls`, `flow_events`를 쓴다. 현 조사에서 이 범위의 제품 코드에 `pabcd`라는 저장 JSON key나 schema 문자열은 확인되지 않았다. import 경로·주석은 저장 필드가 아니다. | 원문 행·run/call/request ID·seq·digest는 변경하지 않는다. 실제로 확인된 옛 식별자는 읽기 adapter와 명시적 대응표로 해석한다. 신규 사건만 새 명칭으로 기록한다. 가정한 `pabcd_*` 열을 만들거나 SQL 문자열 치환을 하지 않는다. |
| 실제 옛 이름 필드·관찰 | [projectcfg/policy.go](../../internal/pabcd/projectcfg/policy.go)의 `crw.json`에 `pabcd.enabled`, 환경에 `CRW_PABCD`; [hook observation](../../internal/harness/observation.go)의 `component: "pabcd-state"`, `event`, schemaVersion 1 및 entrypoint digest. relay의 [report_compose.go](../../internal/relay/delivery/report_compose.go)에 옛 skill pointer가 있다. | config key·환경·component/event·표시 pointer는 승인된 정확한 키/값 단위의 alias로 처리한다. 과거 hook 관찰을 새 바이너리가 실행된 증거로 바꾸지 않는다. 관찰 slot은 component/event/entrypoint의 digest를 포함하므로 과거 파일명을 재계산해 덮어쓰지 않는다. |
| contract fixtures | [CXC corpus](../../contract/schema/cxc/README.md)는 원본 v0.2.40 fixture를 고정하고 [name-substitution](../../contract/schema/cxc/name-substitution.json)을 replay 때 적용한다. | raw oracle fixture·provenance·기존 원본 digest는 보존한다. 새 runtime의 읽기/쓰기 계약은 새 합성 migration fixture와 명시적인 replay 변환으로 시험한다. 기존 fixture를 일괄 검색·치환하여 녹화 사실을 바꾸지 않는다. |

## 2. 바꾸지 말아야 할 의미

이관은 이름과 저장 위치의 변환이다. `I/P/A/B/C/D/IDLE`을 새 flow의 수령·탐색·설계·비평·구현·점검·PR·머지 완료 상태로 추정해서 변환하지 않는다. 옛 phase와 epoch, 원문 receipt·goalplan 연결은 호환 기록으로 보존한다. 옛 non-IDLE 세션은 새 이름 아래에서도 `handover_required`로 표시하고 자동 실행하지 않는다. IDLE의 읽기도 새 flow의 성공이나 검증 통과를 뜻하지 않는다. 실제 새 run의 생성과 명시적 인계는 별도 실행이며, 그 결과의 run ID를 연결할 수는 있어도 옛 기록에서 만들어내지 않는다.

`state.ReadStateStrict`는 모르는 필드를 떨어뜨리거나 잘못된 값을 기본값으로 읽을 수 있다. 따라서 이 함수의 결과를 다시 encode하여 migration 원본을 대신하지 않는다. 원본 bytes를 먼저 보관하고, 정확한 key/token 위치만 바꾸는 lossless 변환을 사용한다. 알려지지 않은 필드·null/absent·숫자 표기·배열 순서·JSON 문자열 내용은 보존한다. duplicate key, 잘못된 UTF-8, lone surrogate, 잘못된 session ID, 미래 schema처럼 해석을 하나로 결정할 수 없는 경우는 `blocked`로 남기며 새 IDLE 파일로 덮지 않는다.

보존해야 할 식별자는 session/run/call/attempt ID, coordinator/plan/check epoch, request id, outbox event id, 원문 증거의 digest다. 이름 치환으로 이들을 재생성하지 않는다. 새 파일 bytes의 digest는 별도로 기록하며 원문 digest와 같다고 주장하지 않는다.

## 3. 이름과 형식의 대응표

구현의 입력은 CRW-1523의 승인된 대응표다. 이 표의 hash를 migration manifest에 고정한다. 새 이름이 미정인 현재 문서는 아래 기호만 정한다. 같은 placeholder를 썼다고 디렉터리·파일·schema·config key가 모두 같은 문자열이라는 뜻은 아니다.

| 기호 | 구 경로/이름 | 새 값의 출처와 동작 |
| --- | --- | --- |
| `SESSION_DIR_NEW` | `.crw/sessions/` | `.crw/<새 이름: CRW-1523>/`; 세션 파일 basename은 같은 canonical ID |
| `LEDGER_NEW` | `.crw/ledger.jsonl` | `.crw/<새 이름: CRW-1523>`; 역사 줄의 bytes와 순서 유지 |
| `ANCHOR_DIR_NEW` | `<CRW home>/state-roots/` | `<CRW home>/<새 이름: CRW-1523>/`; 같은 ID와 native cwd |
| `SESSION_SCHEMA_NEW` | schema 없는 구 세션 object | `<새 이름: CRW-1523>/session/1`; 구 schema 부재는 **구 경로에서만** legacy format 0으로 인정 |
| `CONFIG_KEY_NEW`, `ENV_NEW` | `pabcd`, `CRW_PABCD` | 각각 `<새 이름: CRW-1523>`; 새 writer는 새 이름만 사용 |
| typed identifier aliases | `pabcd-state`, 옛 hook event/CLI/skill token | `<새 이름: CRW-1523>` 대응표의 정확한 값만 해석; 사용자 prose·SQL·receipt 문자열의 부분 문자열 치환 금지 |

새 세션 파일은 `schema`, 동일한 `sessionId`, `migrationId`, `legacyState`와 `handover`를 가진 호환 envelope다. `legacyState`는 알려진 이름만 lossless 변환한 옛 상태 object이며 실행 가능한 새 flow 상태가 아니다. `handover.state`는 non-IDLE이면 `required`, IDLE이면 `not_required`; 미인계 `targetRunId`는 null이다. 최초 snapshot의 원문 hash와 새 파일 hash를 manifest에서 연결한다. migration converter의 version과 각 artifact의 schema version은 독립적으로 기록한다. anchor는 새 형식 version을 구별하고 원래 native cwd와 해당 root의 migration ID를 함께 가진다. 기존 evidence version 1/2와 goalplan schemaVersion 1/2/3은 이번 layout version으로 덮어쓰지 않는다.

`crw.json`은 추적될 수 있는 프로젝트 설정이다. 첫 실행 migration이 자동 수정하거나 commit하지 않는다. 호환 reader를 먼저 제공하고 config 명시적 변환은 같은 alias 규칙의 별도 preview/apply로 수행한다. 한 object에 구 key와 새 key가 함께 있으면 둘이 같은 값일 때만 하나로 읽고 신규 저장은 새 key만 한다. 값이 다르거나 타입이 다르면 거절한다. 환경 변수도 동시에 설정된 유효 값이 다르면 거절한다. 옛 `false`가 새 이름의 기본값 때문에 enabled로 바뀌면 안 된다.

## 4. 읽기·쓰기 선택과 혼재 규칙

아래의 'commit marker'는 §5의 검증된 root manifest의 `committed` 상태다. 폴더 존재·mtime·파일 크기로 승자를 정하지 않는다.

| 관찰한 상태 | 읽기 | 쓰기/조치 |
| --- | --- | --- |
| 옛 것만 있고 marker 없음 | 구 decoder로 읽되 legacy 출처 표시 | 새 런타임의 첫 상태 변경 전에 backup과 migration을 완료한다. 그 전에는 mutation을 거절하고 필요한 절차를 안내한다. |
| 아무 상태도 없고 anchor도 없음 | 새 세션 생성 가능 | 빈 원본 목록을 가진 새 layout manifest를 게시한 뒤 새 경로만 쓴다. 권한 오류·깨진 anchor를 '없음'으로 취급하지 않는다. |
| 새 경로만 있고 유효한 committed marker 있음 | 선언된 새 schema만 읽음 | 새 경로만 씀 |
| 새 경로만 있으나 marker 없음 | 정상 이관으로 추정하지 않음 | orphan/partial로 거절. recover가 manifest와 inventory를 확인할 때까지 생성·쓰기 금지 |
| 구·신 둘 다 있고 committed marker 있음 | 새 경로가 정본; 구 경로는 그 manifest가 고정한 보존본 | 구 경로가 원본 hash와 달라졌으면 구 writer의 분기 가능성으로 전체 mutation을 거절. 새 파일이 손상/누락되어도 구 파일로 조용히 fallback하지 않음 |
| 구·신 둘 다 있고 marker 없음 | 어느 쪽도 임의로 정본 선택 안 함 | 완전한 manifest를 가진 중단 이관만 recover. 별도로 만들어진 두 상태는 내용이 같아도 충돌 목록과 명시적 adoption이 필요. 차이는 운영자가 풀어야 하며 자동 병합 금지 |
| `preparing`/`prepared`/`rolling_back` marker | 명시적인 진단 읽기만 허용 | 정상 writer는 거절. recover/rollback만 같은 migration ID로 진행 |
| 알 수 없는 version/alias map, 손상 manifest 또는 다른 root/session | 추측하지 않음 | bytes 보존하고 `unsupported` 또는 `conflict`로 거절 |

이관 후에는 옛 경로로 새 데이터를 쓰지 않고, 구·신 경로를 동시에 쓰는 기간도 두지 않는다. 옛 CLI 이름을 유지할지 없앨지는 CRW-1523·1524의 결정이지만, 유지되는 alias는 반드시 같은 새 resolver와 writer를 거쳐야 한다. alias가 옛 저장소나 옛 루프를 다시 여는 것은 허용하지 않는다. layout을 읽을 수 있는 호환 runtime이 배포되기 전에는 새 layout을 활성화하지 않는다.

## 5. 첫 실행 backup과 게시 프로토콜

이 절의 inspect/apply/recover/rollback은 **구현할 연산의 이름**이며 현재 사용 가능한 CLI 명령이 아니다. CLI의 실제 명칭은 승인된 이름표를 사용한다. `inspect(root)`는 읽기만 하고 inventory·충돌·필요한 정지·예상 공간을 반환한다. 첫 상태 쓰기는 이관 필요를 감지하여 apply를 완료하거나 명확히 거절한다. help/status 같은 읽기 호출로 저장소를 몰래 바꾸지 않는다. 첫 이관의 원본 backup이 durable하기 전에는 어떤 원본 artifact도 변경하지 않는다.

이관 단위는 하나의 **native root의 세션 전체와 공유 전이 ledger, scan ledger, outbox, 참조 목록 및 그 root를 가리키는 anchor**다. 전이 ledger가 공유되므로 세션 한 개만 독립적으로 전환하지 않는다. CRW home이 root와 다른 filesystem이어도 같은 manifest의 participant다. 공유 evidence·goalplan과 relay/flow store는 변경하지 않지만 참조 확인과 복구에 필요한 일관된 snapshot 대상으로 inventory에 명시한다. native Codex session/goal SQLite는 읽는 외부 데이터이며 수정·복원 대상에 넣지 않는다.

제어 경로는 brand와 무관한 고정 이름을 사용한다: root의 `.crw/migration.json`은 선택 중인 layout과 migration ID를 나타내며, `.crw/migrations/<id>/`에 manifest 이력·backup·staging inventory를 둔다. 이동되는 session/ledger 경로 안에 migration lock을 두지 않는다. manifest 형식은 `crw-state-migration/1`이며 다음 필드가 필수다.

- migration ID, native root의 정규 경로와 filesystem identity, session ID 목록, CRW home·anchor 위치와 participant 목록.
- source layout 0, target layout 1, converter version, 승인된 alias map hash, 최소 reader/writer format capability.
- 각 상대 source/destination 경로, 종류·mode·byte 수·원본 SHA-256·변환본 SHA-256, schema, backup 경로. manifest 경로는 해당 root/backup 범위를 벗어나지 못한다.
- `preparing`, `backed_up`, `prepared`, `committed`, `rolling_back`, `rolled_back` 중 상태, 작업 단계, 각 participant의 준비 완료 hash, 첫 새 writer가 진행했는지 확인할 durable generation/dirty marker.
- ledger의 byte 길이와 전체 hash, 각 파일의 session identity, 관련 journal의 원래 ID·최종 seq. 시각은 감사용이며 commit 판단 기준이 아니다.

**순서와 commit point**

1. 모든 쓰기 진입점(직접 CLI, hook, relay daemon/session resume, flow 증거 writer)이 새 layout을 이해하는 호환 runtime인지 확인한다. 옛 프로세스·등록된 옛 hook·서비스를 정지하고 쓰기 없는 유지보수 상태를 만든다. 실행 중인지 증명할 수 없는 구 writer가 있으면 진행하지 않는다. 새 lock은 모르는 옛 바이너리를 저절로 막지 못한다. 문서의 안전성은 참여 writer가 이 절차를 지킨다는 조건이다.
2. 고정 위치의 root migration gate를 독점 획득하고, CRW home의 anchor gate 및 필요한 relay store write gate를 일관된 정렬 순서로 획득한다. session/goalplan 기존 lock도 그 아래서 잡으며 살아 있는 lock을 timeout만으로 훔치거나 파일을 지우지 않는다. 호환 runtime의 모든 normal writer는 같은 migration gate를 공유 획득하고 marker를 확인해야 한다. 기존 [store](../../internal/relay/store/store.go)의 `write-gate.lock` 계약과 세션 lock을 재사용하되, root 전체를 막는 gate는 새로 구현해야 한다.
3. no-follow descriptor로 모든 source와 parent를 확인한다. symlink·device·FIFO·공유 mutable hardlink·경로 이탈·ID 불일치·읽기 권한 실패는 거절한다. 다른 filesystem으로 rename하지 않는다. 옛 ledger outbox가 있거나 D-close followup이 미해결이면 이관을 보류한다. 먼저 원문 backup을 남긴 뒤 **기존 recovery 절차**로 해결하고 새 snapshot을 만들어 다시 시작한다. 이관기가 followup을 추측해 실행하거나 offset을 재계산하지 않는다.
4. `preparing` intent와 새 migration ID를 durable하게 기록한 후 원본 bytes를 exclusive 생성한 backup 디렉터리에 복사한다. 원본과 hardlink하지 않는다. 디렉터리는 0700, 파일은 0600으로 접근을 제한하고 원래 mode는 metadata에 기록한다. 각 파일과 부모 디렉터리를 fsync하고 재독·hash 검증 후 `backed_up`을 게시한다. 기존 backup을 덮지 않는다. SQLite가 대상이면 연결을 정지·flush한 일관된 SQLite backup API snapshot을 사용하고 integrity/foreign-key 검사 결과를 남긴다. 열린 WAL DB의 본 파일만 `cp`하지 않는다. snapshot 중 원본이 변하면 거절한다.
5. backup에서 새 형식과 새 경로를 만든다. 임시 파일은 각각 목적지 filesystem의 같은 부모 아래 만들고 fsync한다. schema·ID·참조·원문 보존·alias 충돌·byte 길이와 digest를 검증한다. 각 새 destination을 no-replace로 게시하되 아직 사용할 수 없게 두고, 게시 뒤 해당 부모 디렉터리를 fsync한다. 이 과정에서 새로 만든 디렉터리가 있으면 그 이름을 담은 상위 디렉터리도 아래에서 위로 이미 durable한 조상까지 fsync하여 새 경로의 모든 디렉터리 엔트리를 영속화한다. 같은 ID의 재시도는 inventory에 기록된 정확한 hash만 재사용하며 이 디렉터리 fsync도 완료하고, 기존 다른 파일은 덮지 않는다. anchor도 새 경로에 준비하며 원래 native cwd와 이 root의 migration ID를 기록하고 같은 게시·fsync 순서를 따른다. 모든 participant의 게시와 필요한 fsync가 성공한 뒤에만 manifest를 `prepared`로 만들고 단계 6의 `committed` 게시로 진행한다.
6. 검증된 root `.crw/migration.json`을 같은 디렉터리의 temp→atomic rename→부모 fsync로 `committed`로 게시한다. **이 marker가 유일한 활성화 지점**이다. 프로젝트 reader와 새 home anchor reader 모두 이 marker와 migration ID를 확인한 뒤 새 경로를 선택한다. 다른 filesystem의 anchor 게시를 원자적이라고 가정하지 않는다. 어느 participant라도 빠지면 commit하지 않는다. marker rename 뒤 fsync 실패는 성공으로 보고하지 않고 recover가 실제 durable 상태를 판정한다.
7. mutation을 허용하기 전에 모든 진입점의 resolver로 새 상태를 재독한다. 첫 새 mutation은 실제 artifact 변경보다 먼저 generation/dirty marker를 durable하게 올린다. 이 marker를 못 쓰면 mutation도 하지 않는다. root gate를 놓은 뒤의 정상 쓰기는 새 schema·새 경로만 사용한다. backup·옛 상태는 자동 삭제하지 않으며 별도 승인된 보존 정책이 정해질 때까지 남긴다.

파일 fsync만으로 no-replace 게시된 이름이나 새 디렉터리의 상위 엔트리가 정전 뒤에도 남는다고 보장할 수 없다. 따라서 위 순서를 지키면 파일·디렉터리 fsync의 내구성 보장 아래 durable한 `committed` marker가 아직 영속화되지 않은 destination을 가리키지 않는다. 게시 뒤 디렉터리 fsync 완료 전에 전원이 끊기거나 fsync가 실패하면 `prepared`/`committed`로 진행하거나 성공을 보고하지 않으며, 원본과 검증된 backup을 보존하고 정상 mutation을 차단한 채 같은 migration ID의 recover에서 inventory·hash와 게시 상태를 재확인하고 필요한 게시·fsync를 완료한다. 순서를 어겨 marker가 먼저 영속화되었다면 destination의 존재는 보장할 수 없으므로 marker만으로 새 layout을 활성화하지 않고 모든 participant를 검증하며, 하나라도 누락되거나 불일치하면 `blocked`로 남기고 임의로 old fallback하지 않는다.

세션 ledger 줄의 변환은 기존 줄을 재인코딩하지 않는 예외다. 원본 append-only 구간을 그대로 두고, 새 사건부터 명시적인 새 row schema/version으로 기록한다. reader는 legacy row와 새 row를 구분하며 알 수 없는 새 version을 무시하여 성공으로 처리하지 않는다. 원본 outbox를 비운 상태에서 시작하므로 기존 byte offset의 의미를 바꾸지 않는다.

## 6. 중단·실패·되돌리기

recover는 manifest를 먼저 읽고, 같은 migration ID와 source hash에 대해서만 수행한다. stage 파일이 있다는 사실만으로 단계를 건너뛰지 않는다. disk full, permission denied, process kill, partial copy, sync 실패, 참여 경로 하나의 장애는 모두 old 또는 new 전체 집합 중 하나만 선택하도록 처리한다.

| 중단 지점 | 재시작 행동 |
| --- | --- |
| backup 완료 전 | 새 정상 writer를 열지 않는다. 원본은 그대로이며 부분 backup을 complete로 표시하지 않는다. 소유가 확인된 임시 파일만 제거하거나 같은 ID로 재시도한다. |
| backed_up/prepared, commit 전 | checksum을 다시 확인하고 남은 staging만 이어 간다. 취소하면 이번 ID가 만든 정확한 destination만 제거하며 원본은 보존한다. 다른 프로세스의 파일이면 제거하지 않고 충돌로 멈춘다. |
| commit marker 게시 경계 | marker와 모든 participant의 hash/version을 함께 확인한다. 일관되면 committed로 복구하고, 불완전하면 mutation을 막는다. 임의로 old fallback하지 않는다. |
| committed, 첫 새 mutation 전 | 아래 rollback 절차를 통해 legacy 정본으로 되돌릴 수 있다. 원본 snapshot과 완전히 같은 상태인지 먼저 검증한다. |
| 새 mutation intent가 기록됨 | 자동 rollback 금지. 새 사건·receipt·외부 부작용을 없던 것으로 만들 수 없다. 새 bytes를 추가 backup하고 호환 runtime으로 복구한다. 역변환이 필요하면 모든 새 데이터 보존과 외부 동작 대조를 증명하는 별도 명시적 복구 작업으로 수행한다. |

**rollback(migration ID, expected generation) 절차:** 쓰기를 모두 멈추고 같은 gate들을 잡는다 → source backup·현재 destination·manifest·새 mutation marker와 participant identity를 검증한다 → 변경 후 데이터가 없을 때만 `rolling_back`을 durable 기록한다 → 원본이 그대로면 그대로 사용하고, 복원할 파일은 검증된 backup에서 no-follow/no-replace 또는 예상 hash CAS로 되돌린다 → anchor를 같은 native cwd의 legacy 해석으로 복구하고 모든 participant를 검사한다 → marker를 `rolled_back`으로 원자 게시한 뒤 legacy 호환 reader/writer를 연다. 복원 도중 죽으면 `rolling_back`으로 계속 차단하며 이 절차를 재개한다. 새 경로와 실패 산출물은 소유·hash를 확인한 뒤 격리할 수 있으나 backup은 삭제하지 않는다.

여기서 legacy writer는 migration marker를 이해하는 호환 release다. 더 오래된 바이너리로 downgrade하려면 모든 writer를 정지하고 rollback 완료를 먼저 증명해야 한다. 바이너리만 바꾸거나, anchor만 옮기거나, 새 디렉터리를 지우는 것은 rollback이 아니다. 공유 relay/flow DB를 현재 상태 위에 과거 snapshot으로 덮어쓰는 작업도 금지한다. 본 설계는 그 DB의 과거 행을 변경하지 않으므로 이관 rollback에서 DB 전체 복원이 필요하지 않아야 한다.

## 7. relay·flow와 schema 호환

새 import 경로와 저장 schema는 별개의 계약이다. source/crwdir/evidence의 package 이동이 저장 JSON key 변경을 자동으로 일으키지 않게 한다. flow가 journal에 기록한 evidence cwd, assignment ID, 원문 receipt 경로·digest와 version을 그대로 해석할 수 있어야 한다. 옛 루프의 상태를 이름 변경만으로 flow receipt로 승격하지 않는다.

실제로 저장되는 옛 식별자의 mapping entry는 `(record kind, schema version, exact field path, old token, new token)`으로 제한한다. 읽기 때만 원문 위에 새 이름의 view를 만들고 원문과 view를 구분한다. 새 writer는 그 record kind의 새 schema/version만 기록한다. 하나의 payload에 구·신 key가 충돌하면 거절한다. future schema와 모르는 enum을 기본 성공/완료로 바꾸지 않는다. schema 문자열에 옛 이름이 실제로 있는 경우에만 승인된 entry를 추가한다. `flow_runs` 등의 테이블 이름이나 전역 store SchemaVersion을 브랜드 교체의 핑계로 변경하지 않는다.

재개 중인 기존 run은 기존 op 이름·request id·seq를 그대로 replay한다. dispatcher의 alias는 실행 전에 동일한 canonical operation으로 해석하되 기존 idempotency key를 다시 계산하지 않는다. 변환할 수 없는 행이 있으면 run을 held로 남기고 해당 행과 이유를 제시한다. 오래된 hook observation은 진단 이력으로 읽을 수 있지만 새 hook/entrypoint/manifest가 실제 실행되었다는 근거로 사용하지 않는다. 새 hook 실행이 새 token으로 별도 관찰을 남긴다.

## 8. CRW-1532·1533·1534 구현 순서

다음은 세 구현 이슈가 공유할 순서와 인수 조건이다. 이슈 제목이나 다른 작업자의 파일 소유권을 새로 정하는 표가 아니다. CRW-1532는 이관 구현을 이 계약에 맞추며, CRW-1533·1534는 자신이 바꾸는 호출부·문서·시험에서 같은 계약과 아래 fixture를 사용한다.

1. **고정 입력:** CRW-1523 승인된 이름표 hash, CRW-1524 처분표, 이 설계의 승인 revision을 연결한다. placeholder가 남거나 삭제될 데이터의 보존 방침이 없는 상태로 apply를 배포하지 않는다.
2. **호환 읽기와 차단 먼저:** 구 경로·구 field/schema를 정확히 읽는 decoder, 새 형식 decoder, 혼재 판단, migration gate를 구현한다. 구 release의 raw fixture와 forward-version 거절을 먼저 red로 만든다. CLI/hook/relay/flow 모두 같은 resolver를 쓰게 한다. writer가 아직 옛 경로를 쓰면 cutover 기능은 비활성으로 둔다.
3. **이관 연산:** inspect, backup, apply, status, recover, rollback을 구현한다. apply 입력은 root, 예상 source inventory hash, 승인된 alias map hash, migration ID다. inventory가 바뀌면 다시 inspect해야 하며 자동 수락하지 않는다. 출력은 단계·선택된 layout·각 artifact 출처/hash·거절 사유·다음 가능한 연산을 기계가 읽는 구조와 사람용 요약으로 제공한다.
4. **writer 전환:** 새 snapshot·새 사건만 새 형식으로 기록한다. 살아 있는 old writer 차단과 처음부터 신규 root인 경우를 함께 시험한다. 옛 명령 alias의 존치 여부와 별개로 저장 writer는 하나다. pending outbox/기존 in-flight session은 위에서 정한 보류·인계 정책을 따른다.
5. **consumer와 fixture 전환:** relay/flow replay·hook observation·evidence 경계를 연결한다. 원본 CXC fixture는 유지하고 replay mapping과 CRW 합성 migration fixture를 추가한다. 삭제·스킬·명령 변경은 각 소유자의 범위에서 진행하며 이관 primitive를 복사해서 따로 만들지 않는다.
6. **배포 인수:** 아래 fault/upgrade/rollback matrix를 격리된 임시 저장소에서 통과시킨다. 소스 CI 통과와 실제 배포·상태 이관을 별도 사실로 기록한다. 사용자 상태의 실제 적용은 inspect 결과와 backup·복구 절차를 갖춘 별도 배포 단계다. 호환 reader/backup 보존을 없애는 일은 이 세 구현의 기본 완료 조건에 포함하지 않는다.

## 9. 구현 때 먼저 실패하게 만들 시험 목록

이 문서 자체에는 새 실행 코드나 red 시험이 없다. 다음은 CRW-1532·1533·1534가 구현 전에 작성할 동작 시험이다. 단순 이름 검색 성공을 데이터 안전성의 증거로 쓰지 않는다.

| 시험 | 합성 입력/고장 | 필수 assertion |
| --- | --- | --- |
| 구 형식 기본 이관 | legacy-only 세션·전이/scan ledger·anchor | backup hash가 원본 bytes와 같음; 새 session ID/native cwd 동일; 새 경로만 정상 writer 대상 |
| 비활성/진행 상태 | IDLE, I/P/A/B/C/D, epoch·receipt·D-close marker | IDLE을 완료로 승격하지 않음; non-IDLE은 handover required; 새 run이나 성공 receipt 자동 생성 없음 |
| lossless | unknown key, null/absent, 큰 수, 순서; duplicate key, invalid UTF-8, lone surrogate | 허용된 이름 외 object 의미·원문 backup 유지; 모호한 입력은 거절; 기본 상태 덮어쓰기 없음 |
| 공유 ledger | 여러 세션·동일한 두 줄·개행 없는 마지막 줄 | 순서·중복·byte offset 보존, 세션 일부만 cutover 불가; 기존 append 계약 유지 |
| 미해결 전이 | 각 outbox publish/append/followup 경계 | backup 가능, apply는 blocked; 기존 복구 완료 전 이름 변경·event 소실·중복 append 없음 |
| 혼재와 충돌 | §4 모든 행, 동일/다른 구·신 config 값 | 선택 규칙대로만 읽음; mtime 우선·묵시적 병합·old fallback 없음; false 설정 유지 |
| 미래 형식 | 더 높은 schema, 다른 alias hash, malformed manifest | unsupported/conflict, 원본 불변, 신규 상태 생성 없음 |
| 재실행 | 같은 migration ID·같은 hash로 apply/recover 반복 | 효과 1회, backup 덮어쓰기 없음, 이미 게시된 파일 hash 검증, 원본 바뀌면 거절 |
| 경로·identity | native cwd alias, 다른 worktree, 잘못된 ID, symlink/FIFO/hardlink/상위 경로 | 정상 alias는 같은 root로 인식; 다른 root 자동이전 없음; 특수 경로 거절 및 외부 bytes 불변 |
| 경쟁 writer | hook/CLI/relay/flow 동시 진입, 살아 있는 lock, 옛 writer 발견 | 정상 writer는 gate/marker 확인; 이관 중 쓰기 불가; lock 강제탈취 없음; 옛 writer면 cutover 거절 |
| 저장 실패 | backup/stage/marker의 write·rename·fsync 실패, ENOSPC/권한/취소 | complete marker가 미완료 backup을 가리키지 않음; 원본 보존; 성공 오보고 없음 |
| crash matrix | §5 각 durable 단계 직전/직후 kill, root/home 별도 filesystem | 재시작은 하나의 일관된 layout 또는 명시적 blocked; 부분 anchor/session 조합으로 정상 실행 없음 |
| SQLite snapshot | WAL에 미반영 commit, 다른 run 기록 | 일관된 backup 검증, ledger/flow ID·seq 보존; 본 파일만 복사하거나 타 run을 과거로 되돌리지 않음 |
| rollback 전후 | commit 전, commit 후 첫 새 mutation 전, mutation intent 후, rollback 중 kill | 앞 두 경우 원본 복원 검증; 새 mutation 후 자동 rollback 거절; rolling_back 재개·backup 보존 |
| flow 재개 | 옛 op/field/schema 행과 receipt가 있는 중단 run | 과거 행·digest 불변, dispatch 중복 없음, 현재 evidence gating 유지; 변환 불가면 held |
| 관찰·계약 | 옛 hook slot/원본 oracle fixture와 새 token | 옛 관찰을 새 실행 증거로 쓰지 않음; raw fixture bytes 유지; 새 synthetic fixture가 양방향 읽기와 새 쓰기를 검증 |
| 설정·downgrade | 구 config만, 구·신 값 충돌, 옛 runtime 재실행 | tracked crw.json 자동수정 없음; 모순된 설정 거절; layout 미지원 writer 차단/유지보수 요구 |
| clean install | state/anchor/marker 모두 없음 | 새 layout만 생성; legacy 폴더·가짜 migration backup이나 과거 run 생성 없음 |

합격 증거에는 각 fixture의 source/target inventory hash, 사용한 format/map revision, fault 지점, 예상/실제 marker와 재개 결과를 남긴다. 동작 시험은 실제 사용자 root·Codex DB·운영 relay DB를 사용하지 않는다. 승인된 이름표를 대입한 뒤에도 이 matrix의 의미는 달라지지 않는다.
