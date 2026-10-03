package job

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// removal.ts printed a checklist of the CXC repository; the CRW list is rewritten (name-substitution 4.3), so these tests pin its
// shape and the names it may carry, not the nine CXC lines.
func TestRemovalTextCountsAndNumbersItsSteps(t *testing.T) {
	steps, text := RemovalSteps(), RemovalText()
	if len(steps) == 0 {
		t.Fatal("no steps")
	}
	if want := fmt.Sprintf("crw relay job 제거 체크리스트 (%d단계)\n\n", len(steps)); !strings.HasPrefix(text, want) {
		t.Errorf("header: %q", strings.SplitN(text, "\n", 2)[0])
	}
	for i, step := range steps {
		if !strings.HasPrefix(step, fmt.Sprintf("%d. ", i+1)) || !strings.Contains(text, "\n"+step+"\n") {
			t.Errorf("step %d: %q", i+1, step)
		}
	}
	if strings.HasSuffix(text, "\n") {
		t.Error("the oracle's removalText ends without a newline")
	}
}

func TestRemovalTextNamesTheCrwSwitchOnly(t *testing.T) {
	text := RemovalText()
	for _, want := range []string{"crw relay job off", "export CRW_BGWAKE=0", ".crw/bg/", "make build && make test && make lint",
		"internal/relay/job/", "go run -tags dev ./cmd/crw-dev ci plugin --record-version"} {
		if !strings.Contains(text, want) {
			t.Errorf("the text lacks %q", want)
		}
	}
	for _, former := range []string{"codexclaw", "cxc", "bg-wake", "CXC_", "npm ", ".mjs"} {
		if strings.Contains(strings.ToLower(text), strings.ToLower(former)) {
			t.Errorf("the text carries the former name %q", former)
		}
	}
}

func TestRemovalStepsAreACopy(t *testing.T) {
	first := RemovalSteps()
	if len(first) == 0 {
		t.Fatal("no steps")
	}
	want := slices.Clone(first)
	first[0] = "changed"
	if !slices.Equal(RemovalSteps(), want) {
		t.Error("a caller changed the steps")
	}
}
