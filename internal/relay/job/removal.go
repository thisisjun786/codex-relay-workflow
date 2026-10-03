package job

import (
	"strconv"
	"strings"
)

// RemovalSteps is the uninstall checklist of the bg feature (REMOVAL_STEPS). The oracle's list described the CXC repository
// (plugins/codexclaw/components/bg-wake, build.mjs, package.json, the lockfile, inventory.mjs), which does not exist here, so the
// list is rewritten for this one (name-substitution.md 4.3: "rewritten, not substituted"). It names the places the plan gives the
// feature once it is whole: this package, the verb "crw relay job" and the three bg hook files, whose declarations sit in
// plugin.json. Those two arrive with later issues, so steps 2 to 4 are worded "if present" and stay true at every stage; the issue that
// lands the verb or the hooks reads these lines again against the files it added. It is a function, not a variable, so a caller cannot
// change the list for the next one.
func RemovalSteps() []string {
	return []string{
		"1. internal/relay/job/ 디렉터리 삭제 (bg 저장소와 그 위에 얹힌 코드 전부)",
		"2. crw relay job 동사가 연결돼 있으면 제거: internal/relay/cli 의 job 연결과 그 argparse 명세",
		"3. plugins/crw/wiring/hooks/ 에 bg 훅 파일 3개 (stop / user-prompt-submit / session-start)가 있으면 삭제하고, plugins/crw/.codex-plugin/plugin.json 의 hooks[] 에서 그 3줄 제거",
		"4. contract/notes/cxc/ 에 bg fixture (cli__bg__*, cli-help__bg__*, 백그라운드 완료 훅)를 맡은 claim 이 있으면 삭제 (그 fixture 는 pending 으로 돌아간다)",
		"5. go run -tags dev ./cmd/crw-dev ci plugin --record-version 으로 플러그인 digest 다시 기록",
	}
}

// RemovalText is what the removal verb prints (removalText): a header that counts the steps, the steps, how to check, what stays
// behind, and how to switch the wake off without removing anything. The oracle's claim that the list had been applied in full on
// 260909 is not carried, since nobody has applied this one. The text has no trailing newline, as the oracle's had none.
func RemovalText() string {
	steps := RemovalSteps()
	lines := append([]string{"crw relay job 제거 체크리스트 (" + strconv.Itoa(len(steps)) + "단계)", ""}, steps...)
	lines = append(lines,
		"",
		"확인: make build && make test && make lint",
		"",
		"작업공간의 .crw/bg/ 는 자동으로 지워지지 않는다. 필요하면 직접 삭제한다.",
		"",
		"끄기만 하려면 제거할 필요 없습니다.",
		"  즉시(이 워크트리):   crw relay job off",
		"  새 세션부터(전역):   export CRW_BGWAKE=0",
	)
	return strings.Join(lines, "\n")
}
