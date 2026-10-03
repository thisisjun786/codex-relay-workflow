package job

import (
	"fmt"
	"strings"
	"testing"
)

// removal.ts printed a checklist of the CXC repository; the CRW list is rewritten (name-substitution 4.3), so these tests pin its shape
// and the names it may carry, not the oracle's nine lines.
func TestRemovalTextCountsItsStepsAndNamesTheCrwSwitchOnly(t *testing.T) {
	steps, text := RemovalSteps(), RemovalText()
	if len(steps) == 0 {
		t.Fatal("no steps")
	}
	if want := fmt.Sprintf("crw relay job 제거 체크리스트 (%d단계)\n\n", len(steps)); !strings.HasPrefix(text, want) || strings.HasSuffix(text, "\n") {
		t.Errorf("header or end: %q", text)
	}
	for i, step := range steps {
		if !strings.HasPrefix(step, fmt.Sprintf("%d. ", i+1)) || !strings.Contains(text, "\n"+step+"\n") {
			t.Errorf("step %d: %q", i+1, step)
		}
	}
	for _, want := range []string{"crw relay job off", "export CRW_BGWAKE=0", ".crw/bg/", "make build && make test && make lint", "internal/relay/job/"} {
		if !strings.Contains(text, want) {
			t.Errorf("the text lacks %q", want)
		}
	}
	// contract/notes/cxc is this repository's own directory for the replay notes, not a former name.
	rest := strings.ToLower(strings.ReplaceAll(text, "contract/notes/cxc/", ""))
	for _, former := range []string{"codexclaw", "cxc", "bg-wake", "npm ", ".mjs"} {
		if strings.Contains(rest, former) {
			t.Errorf("the text carries the former name %q", former)
		}
	}
}
