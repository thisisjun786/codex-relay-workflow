package engine

import (
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

// ChangedChild says why the child workflows a run opened are not the ones resolve gives now ("" when they are) (CRW-1461). A resume
// gates the entry script by its digest (notTheRuns); the child of a workflow() position is recorded in its workflow_opened row (label
// "workflow:" + name or scriptPath, data script_digest) and is gated here the same way: a run that ran another child, or a child this
// build does not have, is not this run's resume. A call reopened by later processes is judged by its last row, the child whose rows
// the run replays. The label holds a name or a scriptPath and does not say which: the name is resolved first and, when it resolves,
// is the child that is compared (a scriptPath that resolves to another file never accepts it); the scriptPath is tried only when
// the name does not resolve. A run that never opened a child is untouched.
func ChangedChild(events []Event, runID string, resolve func(name, scriptPath string) (*script.Program, error)) string {
	type opened struct{ child, digest string }
	var order []string
	last := map[string]opened{}
	for _, ev := range events {
		if ev.RunID != runID || ev.Kind != KindFlow || ev.Op != "workflow" || ev.Event != "workflow_opened" {
			continue
		}
		id := rowStr(ev.Data, "call_id")
		if id == "" {
			id = fmt.Sprintf("%s#%d", ev.Path, ev.Position)
		}
		if _, seen := last[id]; !seen {
			order = append(order, id)
		}
		last[id] = opened{child: strings.TrimPrefix(ev.Label, "workflow:"), digest: rowStr(ev.Data, "script_digest")}
	}
	for _, id := range order {
		o := last[id]
		var prog *script.Program
		var errs []string
		for _, ref := range [][2]string{{o.child, ""}, {"", o.child}} {
			p, err := resolve(ref[0], ref[1])
			if err == nil && p != nil {
				prog = p
				break
			}
			if err != nil {
				errs = append(errs, err.Error())
			}
		}
		switch {
		case prog == nil:
			why := ""
			if len(errs) > 0 {
				why = " (" + strings.Join(errs, "; ") + ")"
			}
			return fmt.Sprintf("run %s ran child workflow %s, which this build does not have%s", runID, o.child, why)
		case prog.Digest != o.digest:
			return fmt.Sprintf("the child workflow %s is not the one run %s ran (digest %s, the run's %s)", o.child, runID, prog.Digest, o.digest)
		}
	}
	return ""
}
