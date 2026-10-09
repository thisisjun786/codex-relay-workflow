package hook

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"testing"
)

// Recorded independently from CXC v0.2.40 commit 3c1459ac detector definitions.
// Inputs from hook.test.ts and memory-write-gate.test.ts use the declared CRW names.
// Additional rows pin the JavaScript text and lookaround boundaries. No Node is needed.
func TestDetectorsOracle(t *testing.T) {
	cases := []struct {
		prompt               string
		phase                state.Phase
		loop, search, memory bool
	}{
		{"detectTrigger: explicit CodexClaw phase requests map to phases", "", false, false, false},
		{"Use crw-pabcd to start Interview phase", "I", false, false, false},
		{"I", "", false, false, false},
		{"crw-pabcd로 인터뷰 시작해", "I", false, false, false},
		{"orchestrate I", "I", false, false, false},
		{"Use crw-pabcd to start Plan phase", "P", false, false, false},
		{"P", "", false, false, false},
		{"crw-pabcd로 계획 진행해", "P", false, false, false},
		{"orchestrate P now", "P", false, false, false},
		{"Run crw-pabcd Audit phase", "A", false, false, false},
		{"A", "", false, false, false},
		{"crw-pabcd로 감사 진행해", "A", false, false, false},
		{"orchestrate A", "A", false, false, false},
		{"Run crw-pabcd Build phase", "B", false, false, false},
		{"B", "", false, false, false},
		{"crw-pabcd로 구현 진행해", "B", false, false, false},
		{"orchestrate B", "B", false, false, false},
		{"Run crw-pabcd Check phase", "C", false, false, false},
		{"C", "", false, false, false},
		{"crw-pabcd로 검증 진행해", "C", false, false, false},
		{"orchestrate C", "C", false, false, false},
		{"issue 250: negated requests never arm or inject", "", false, false, false},
		{"Do not run crw-loop for this task", "", false, false, false},
		{"Do not use crw-pabcd to start Plan phase", "", false, false, false},
		{"crw-loop 돌리지 마", "", false, false, false},
		{"crw-pabcd 쓰지 말고 그냥 고쳐줘", "", false, false, false},
		{"Don't use `crw-loop` here", "", false, false, false},
		{"Please never run crw-loop", "", false, false, false},
		{"crw-loop 쓰지마", "", false, false, false},
		{"crw-loop 말고 그냥 고쳐줘", "", false, false, false},
		{"I don't want you to run crw-loop", "", false, false, false},
		{"I don't want you to use crw-pabcd to plan", "", false, false, false},
		{"I'd rather not use crw-loop", "", false, false, false},
		{"Stop using crw-loop", "", false, false, false},
		{"Can you not run crw-loop?", "", false, false, false},
		{"I would prefer not to use crw-pabcd for planning", "", false, false, false},
		{"negated", "", false, false, false},
		{"t1", "", false, false, false},
		{"", "", false, false, false},
		{"Run crw-loop for this task. Do not push.", "", true, false, false},
		{"Use crw-pabcd to plan, not build", "P", false, false, false},
		{"PABCD로, 단계별로 진행해", "P", true, false, false},
		{"Run crw-loop without asking me", "", true, false, false},
		{"Use crw-pabcd to plan without interviewing", "P", false, false, false},
		{"crw-loop으로 끝까지 마무리해", "", true, false, false},
		{"crw-loop으로 끝까지 해줘, 푸시는 하지 마", "", true, false, false},
		{"crw-loop 돌려줘. 커밋은 하지 말고", "", true, false, false},
		{"Don't run crw-loop, use crw-pabcd to plan instead", "P", false, false, false},
		{"detectTrigger: phase priority applies only within an explicit request line", "", false, false, false},
		{"Use crw-pabcd to start Interview then Plan phase", "I", false, false, false},
		{"Summarize interview notes\nUse crw-pabcd to start Plan phase", "P", false, false, false},
		{"detectTrigger: ordinary words stay silent", "", false, false, false},
		{"just a normal message", "", false, false, false},
		{"감사합니다", "", false, false, false},
		{"정말 감사해요 도와주셔서", "", false, false, false},
		{"계획을 세워줘", "", false, false, false},
		{"이거 감사해줘", "", false, false, false},
		{"기능 구현해줘", "", false, false, false},
		{"검증 좀 해줘", "", false, false, false},
		{"please interview me", "", false, false, false},
		{"order_line", "", false, false, false},
		{"Keep going until Done means holds. ...", "", false, false, false},
		{"wn_workflow_name", "", false, false, false},
		{"... workflow 인터뷰엔진 must keep its name.", "", false, false, false},
		{"author_ko_build", "", false, false, false},
		{"이 기능 구현해 두고 결과 보고해", "", false, false, false},
		{"author_ko_verify", "", false, false, false},
		{"검증해 보고 알려줘", "", false, false, false},
		{"author_ko_finish", "", false, false, false},
		{"끝까지 진행해", "", false, false, false},
		{"english_mention", "", false, false, false},
		{"Summarize the interview notes in file X", "", false, false, false},
		{"neg_thanks", "", false, false, false},
		{"neg_for_loop", "", false, false, false},
		{"fix the for loop bug in parser.ts", "", false, false, false},
		{"neg_plain", "", false, false, false},
		{"list the files in out/", "", false, false, false},
		{"issue 250: nine reported prompts stay silent", "", false, false, false},
		{"issue 250: explicit skill request arms once", "", false, false, false},
		{"Use [$crw-pabcd](skill://example/pabcd) to start Plan phase", "P", false, false, false},
		{"explicit-phase", "", false, false, false},
		{"Run crw-loop for this task", "", true, false, false},
		{"explicit-loop", "", false, false, false},
		{"issue 250: inline quoted requests are data", "", false, false, false},
		{"Summarize this quoted request: \"Use crw-pabcd to start plan phase\".", "", false, false, false},
		{"Summarize this quoted request: “Run crw-loop for this task”.", "", false, false, false},
		{"Summarize this quoted request: 'Use crw-pabcd to start plan phase'.", "", false, false, false},
		{"Summarize this quoted request: `Run crw-loop for this task`.", "", false, false, false},
		{"Summarize `crw-loop`로 written instructions.", "", false, false, false},
		{"Summarize: \"Run crw-loop for this task\" and \"Use crw-pabcd to start plan phase\".", "", false, false, false},
		{"> Run crw-loop for this task", "", false, false, false},
		{"- Use crw-pabcd to start Plan phase", "", false, false, false},
		{"```\nRun crw-loop for this task\nUse crw-pabcd to start Plan phase\n```", "", false, false, false},
		{"Explain how to run `crw-loop` from the README", "", false, false, false},
		{"quoted", "", false, false, false},
		{"issue 250: backtick command requests and mixed lines remain explicit", "", false, false, false},
		{"Run `crw-loop` for this task", "", true, false, false},
		{"Run `crw-loop` to update docs", "", true, false, false},
		{"Use `crw-pabcd` to start Plan phase", "P", false, false, false},
		{"Use `crw-pabcd` to plan the README", "P", false, false, false},
		{"orchestrate i", "I", false, false, false},
		{"README 계약에 맞게 기존 내부 메모 생성/목록 기능을 완성해줘. 네트워크 서버나 공개 API는 아니고 src/route.mjs와 src/service.mjs의 기존 빈 구현을 채우는 작업이야. src/store.mjs와 test/notes.test.mjs는 수정하지 마. 기존 번호 문서에 결과를 기록하고 node --test test/notes.test.mjs로 실제 검증해줘. 새 의존성/추상화/파일, goal/FSM 변경, 커밋, 서브에이전트 파견은 하지 마.", "", false, false, false},
		{"wp3: ordinary Korean C2 remains silent without a CodexClaw request", "", false, false, false},
		{"Use crw-pabcd to start Check phase.\n검증해줘. 읽기 전용으로 코드만 검토해. 수정, 테스트/빌드/타입검사, goal/FSM 변경, 서브에이전트 파견 금지.", "C", false, false, false},
		{"Use crw-pabcd to start Check phase.\nCheck this code by reading it only; no edits, no tests, no build, no typecheck, no goals, no FSM changes, no delegation.", "C", false, false, false},
		{"wp3: CHECK preserves separately allowed build and read-only state inspection", "", false, false, false},
		{"Use crw-pabcd to start Check phase.\nNo-tests, but npm run build is explicitly allowed. No delegation or goal/FSM mutations.", "C", false, false, false},
		{"crw-pabcd로 검증 진행해.\n테스트는 금지지만 빌드와 타입검사는 허용해. goal/FSM 생성과 변경은 금지하고 상태 조회는 허용해. 파견 금지.", "C", false, false, false},
		{"Use crw-pabcd to start Check phase read-only.\nNo-goal/no-FSM mutations; inspect get_goal and orchestrate status only. No edits, tests, build, typecheck or delegation.", "C", false, false, false},
		{"detectAgbrowseSearchRequest: Korean/English search requests, including typo, are detected", "", false, false, false},
		{"agbrowse를 통해서 질문해줘", "", false, true, false},
		{"agbrowe를 통해서 질문해줘", "", false, true, false},
		{"use agbrowse to verify this URL", "", false, true, false},
		{"agbrowse hook도 넣어야될듯", "", false, false, false},
		{"그냥 agbrowse 참조", "", false, false, false},
		{"Start goalplan for this task", "", true, false, false},
		{"골플랜부터 등록해", "", true, false, false},
		{"run PABCD repeatedly until this is fixed", "", true, false, false},
		{"PABCD를 여러 번 돌려서 이 문제 해결해라", "", true, false, false},
		{"ipabcd 사이클로 돌리자", "", true, false, false},
		{"crw-loop", "", false, false, false},
		{"goalplan", "", false, false, false},
		{"continue until done, no pauses", "", false, false, false},
		{"루프 돌려서 처리해", "", false, false, false},
		{"알아서 끝까지 해줘", "", false, false, false},
		{"멈추지 말고 진행해", "", false, false, false},
		{"여러 번 반복해서 해결해", "", false, false, false},
		{"fix the for loop in parser.ts", "", false, false, false},
		{"이 loop 버그 좀 봐줘", "", false, false, false},
		{"루프백 오디오 설정", "", false, false, false},
		{"계속해", "", false, false, false},
		{"what is pabcd?", "", false, false, false},
		{"pabcd 문서 다시 보여줘", "", false, false, false},
		{"explain how pabcd runs internally", "", false, false, false},
		{"pabcd가 뭐야? 계속 헷갈리네", "", false, false, false},
		{"이 함수 여러 번 호출되는 버그 고쳐", "", false, false, false},
		{"이 테스트 여러 번 실행해봐", "", false, false, false},
		{"앱 아이콘 여러 번 실행해도 안 열려", "", false, false, false},
		{"빌드 반복 실행해서 flaky 잡아줘", "", false, false, false},
		{"여러 번 진행된 마이그레이션 롤백해줘", "", false, false, false},
		{"Use crw-pabcd to start Plan phase read-only; no FSM mutations, goals, tests or delegation.", "P", false, false, false},
		{"crw-pabcd로 인터뷰 시작해. FSM 변경, goal 생성, 파일 수정, 테스트, 서브에이전트 파견 금지.", "I", false, false, false},
		{"IDLE", "", false, false, false},
		{"이건 기억해둬, 다음에 또 쓸 거야", "", false, false, true},
		{"remember this for later", "", false, false, true},
		{"please save that to memory", "", false, false, true},
		{"add this to your memories", "", false, false, true},
		{"don't forget the release date", "", false, false, true},
		{"make a note of the API key location", "", false, false, true},
		{"기억 안 나는데 그때 뭐 했지?", "", false, false, false},
		{"do you remember what we discussed?", "", false, false, false},
		{"search my memories for the release notes", "", false, false, false},
		{"fix the memory leak in the parser", "", false, false, false},
		{"메모리도 기록", "", false, false, true},
		{"메모리에 기록해줘", "", false, false, true},
		{"메모리에 남겨둬", "", false, false, true},
		{"메모리에 저장해", "", false, false, true},
		{"메모리에서 찾", "", false, false, false},
		{"메모리를 저장해", "", false, false, false},
		{"메모리를 기록하는 함수", "", false, false, false},
		{"Run `crw-loop`", "", true, false, false},
		{"`crw-loop`으로 진행해", "", true, false, false},
		{"좀 `crw-pabcd`로 계획 진행해", "P", false, false, false},
		{"Use `$crw:crw-pabcd` to plan", "P", false, false, false},
		{"Use [$crw-pabcd](skill://example/pabcd) to plan", "P", false, false, false},
		{"orchestrate D", "", false, false, false},
		{"orchestrate pp", "", false, false, false},
		{"orchestrate B", "B", false, false, false},
		{"Use crw-pabcd to plan\rUse crw-pabcd check", "P", false, false, false},
		{"\ufeffRun crw-loop", "", true, false, false},
		{"Run\u0085crw-loop", "", true, false, false},
		{"Run crw-loop", "", true, false, false},
		{"Uſe crw-loop", "", false, false, false},
		{"1) Run crw-loop", "", false, false, false},
		{"* Run crw-loop", "", false, false, false},
		{"~~~\nRun crw-loop\n~~~", "", true, false, false},
		{"Do not run crw-loop but start crw-loop", "", true, false, false},
		{"crw-loop 금지 하지만 crw-pabcd로 계획 진행해", "P", false, false, false},
		{"don't Run crw-loop", "", false, false, false},
		{"Summarize 'run crw-loop'", "", false, false, false},
		{"x'Run crw-loop'", "", true, false, false},
		{"Please explain crw-loop으로 진행", "", false, false, false},
		{"Run crw-loop, summarize plan", "", true, false, false},
		{"Run crw-loop, use crw-pabcd to check", "C", true, false, false},
		{"run /pabcd", "", false, false, false},
		{"run x-pabcd", "", false, false, false},
		{"run /ipabcd", "", false, false, false},
		{"run aipabcd", "", false, false, false},
		{"run ipabcd", "", true, false, false},
		{"Create goal plan", "", true, false, false},
		{"Do not use agbrowse to search", "", false, true, false},
		{"\"agbrowse search\"", "", false, true, false},
		{"İAGBROWSE search", "", false, true, false},
		{"agbrowe look up", "", false, true, false},
		{"Do not remember this", "", false, false, false},
		{"\"remember this\"", "", false, false, false},
		{"기억해", "", false, false, true},
		{"기억해\n", "", false, false, true},
		{"기억해?", "", false, false, false},
		{"잊지 말고", "", false, false, true},
		{"note this down", "", false, false, true},
		{"keep it in mind", "", false, false, true},
		{"record to my memory", "", false, false, true},
		{"Save........................ to memory", "", false, false, false},
		{"save xxxxxxxxxxxxxxxxxxx to memory", "", false, false, true},
		{"save 😀😀😀😀😀😀😀😀😀xx to memory", "", false, false, true},
		{"save 😀😀😀😀😀😀😀😀😀😀xx to memory", "", false, false, true},
		{"save to memory", "", false, false, true},
		{"save\u0085to memory", "", false, false, true},
		{"Make a note", "", false, false, true},
		{"dont forget", "", false, false, true},
		{"기억 해 둬", "", false, false, true},
		{"메모 기록해", "", false, false, true},
		{"메모리 넣어", "", false, false, true},
		{"기억해\r\n", "", false, false, true},
		{"Run crw-loop\r\nUse crw-pabcd to check", "C", true, false, false},
		{"Use crw-pabcd to audit and build", "A", false, false, false},
		{"record 😀😀😀😀😀😀😀😀😀😀 to memory", "", false, false, true},
		{"record 😀😀😀😀😀😀😀😀😀😀😀 to memory", "", false, false, true},
		{"deſcribe crw-loop으로 진행", "", true, false, false},
		{"a'Run crw-loop'", "", true, false, false},
		{"Run `crw-loop` then use `crw-pabcd` to plan", "", true, false, false},
		{"HOTL 모드로 돌려줘", "", true, false, false},
		{"crw-loop로 진행하자", "", true, false, false},
		{"record 😀😀😀😀😀😀😀😀😀😀😀x to memory", "", false, false, false},
	}
	for _, c := range cases {
		t.Run(c.prompt, func(t *testing.T) {
			if phase, ok := DetectTrigger(c.prompt); phase != c.phase || ok != (c.phase != "") {
				t.Errorf("DetectTrigger = %q, %v; want %q", phase, ok, c.phase)
			}
			for _, result := range []struct {
				name      string
				got, want bool
			}{
				{"loop", DetectLoopArmRequest(c.prompt), c.loop},
				{"search", DetectAgbrowseSearchRequest(c.prompt), c.search},
				{"memory", DetectMemoryWriteRequest(c.prompt), c.memory},
			} {
				if result.got != result.want {
					t.Errorf("%s = %v; want %v", result.name, result.got, result.want)
				}
			}
		})
	}
}

func TestOriginalModeSpellingsAreNotAliases(t *testing.T) {
	for _, p := range []string{"Run cxc-loop", "Use cxc-pabcd to plan", "Run codexclaw:cxc-loop", "Use [$cxc-pabcd](skill://example/pabcd) to plan"} {
		if phase, ok := DetectTrigger(p); ok || phase != "" {
			t.Errorf("old spelling %q: phase %q", p, phase)
		}
		if DetectLoopArmRequest(p) {
			t.Errorf("old spelling %q arms loop", p)
		}
	}
}

func TestRequestLines(t *testing.T) {
	cases := []struct {
		prompt string
		want   []string
	}{
		{"", []string{}},
		{"\ufeff  Run crw-loop\r\n", []string{"Run crw-loop"}},
		{"```ts\nRun crw-loop\n```\nUse crw-pabcd to plan", []string{"Use crw-pabcd to plan"}},
		{"~~~\nRun crw-loop\n~~~", []string{"~~~", "Run crw-loop", "~~~"}},
		{"> Run crw-loop\n- Run crw-loop\n* Run crw-loop\n1) Run crw-loop\n2. Run crw-loop", []string{}},
		{"Please explain how to run crw-loop", []string{}},
		{"좀 설명해 crw-loop으로 진행", []string{}},
		{"Run `crw-loop` to update docs", []string{"Run crw-loop to update docs"}},
		{"`crw-loop`으로 진행해", []string{"crw-loop으로 진행해"}},
		{"Use `orchestrate I`", []string{"Use orchestrate I"}},
		{"x `crw-loop`으로 진행", []string{"x  으로 진행"}},
		{"Summarize: \"Run crw-loop\" and “Use crw-pabcd to plan”.", []string{"Summarize:   and"}},
		{"x'Run crw-loop'", []string{"x'Run crw-loop'"}},
		{"'Run crw-loop'", []string{}},
		{"Do not run crw-loop, use crw-pabcd to plan instead", []string{"use crw-pabcd to plan instead"}},
		{"Run crw-loop without asking me", []string{"Run crw-loop without asking me"}},
		{"crw-loop 돌리지 마 하지만 crw-pabcd로 계획 진행해", []string{"crw-pabcd로 계획 진행해"}},
		{"Run crw-loop, but Use crw-pabcd to Check", []string{"Run crw-loop,", "Use crw-pabcd to Check"}},
		{"Run crw-loop, summarize plans", []string{"Run crw-loop, summarize plans"}},
		{"Run crw-loop but use crw-pabcd to check", []string{"Run crw-loop", "use crw-pabcd to check"}},
		{"Run crw-loop\u0085but use crw-pabcd to check", []string{"Run crw-loop\u0085but use crw-pabcd to check"}},
		{"Run crw-loop. Do not push! Use crw-pabcd to check?", []string{"Run crw-loop", "Use crw-pabcd to check"}},
		{"Run `crw-loop` then use `crw-pabcd` to plan", []string{"Run crw-loop then use   to plan"}},
		{"Use \"x\\\rcrw-loop\" crw-loop으로 진행", []string{"Use \"x\\\rcrw-loop\" crw-loop으로 진행"}},
	}
	for _, c := range cases {
		got := requestLines(c.prompt)
		if len(got) != len(c.want) {
			t.Errorf("%q: got %q, want %q", c.prompt, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%q: got %q, want %q", c.prompt, got, c.want)
				break
			}
		}
	}
}
