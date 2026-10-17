package cli

import (
	"strings"
	"testing"

	flowdelivery "github.com/thisisjun786/codex-relay-workflow/internal/flow/delivery"
)

func TestCRW1609StartRecordsDeliveryWarningsWithoutRefusal(t *testing.T) {
	w := newWorld(t)
	td := w.deps()
	in := testHandoff("plan-1", map[string][]string{"M1": {"CRW-11"}})
	ms := &in.Projects[0].Milestones[0]
	ms.Criteria = []string{"origin integration branch contains the changes", "the issues are closed"}
	ms.DeliveryCriteria = []flowdelivery.Criterion{{ID: "c1", Checks: []flowdelivery.Check{{Kind: flowdelivery.RemotePush}}}}
	code, out, errOut := td.do("start", "project-lane", "--backend", "app-server", "--thread", "thread-owner", "--state", w.state,
		"--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", writeHandoff(t, in))
	if code != 0 || len(td.spawned) != 1 {
		t.Fatalf("delivery warnings refused start: exit %d, %d processes\n%s%s", code, len(td.spawned), out, errOut)
	}
	spec, err := ReadSpec(w.state, td.spawned[0].Spec.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{"delivery_after_publish", "delivery_criterion_unmarked"} {
		found := false
		for _, fact := range spec.Preflight {
			if fact.Name == rule && fact.Level == LevelWarn && fact.Reading != "" {
				found = true
			}
		}
		if !found || !strings.Contains(out, rule) {
			t.Errorf("%s not both displayed and persisted: facts %+v, output %s", rule, spec.Preflight, out)
		}
	}
}
