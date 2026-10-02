package dagsched

import (
	"os"
	"strings"
	"testing"
)

// The scheduler's page names every reason a reading can give and every command the package registers, so the table cannot drift from the
// vocabulary the code emits.
func TestSchedulerPageNamesEveryReasonAndCommand(t *testing.T) {
	raw, err := os.ReadFile("../../../docs/relay/dag-scheduler.md")
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	for _, reason := range append(EmittedReasons(), WaitEdgePrefix+"<edge_id>") {
		if !strings.Contains(page, "`"+reason+"`") {
			t.Errorf("docs/relay/dag-scheduler.md does not name the reason %s", reason)
		}
	}
	for _, reserved := range ReservedReasons {
		if !strings.Contains(page, "`"+reserved+"`") {
			t.Errorf("docs/relay/dag-scheduler.md does not say that %s is reserved", reserved)
		}
	}
	for _, command := range []string{"dag-ready", "dag-region-declare", "dag-release", "dag-accept", "dag-integration-observe", "dag-decision-record", "dag-correct"} {
		if !strings.Contains(page, "`"+command) {
			t.Errorf("docs/relay/dag-scheduler.md does not describe %s", command)
		}
	}
}
