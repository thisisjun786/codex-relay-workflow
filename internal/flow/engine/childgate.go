package engine

import (
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

// The reference kinds of a child workflow, as its workflow_opened row records them (data "ref"): the run named the child
// (workflow(name)) or gave its path (workflow({scriptPath})). A row written before the kind was recorded has none.
const (
	WorkflowRefName   = "name"
	WorkflowRefScript = "script"
)

// ChangedChild says why the child workflows a run opened are not the ones resolve gives now ("" when they are) (CRW-1461). A resume
// gates the entry script by its digest (notTheRuns); the child of a workflow() position is recorded in its workflow_opened row (label
// "workflow:" + name or scriptPath, data script_digest and ref) and is gated here the same way: a run that ran another child, or a child
// this build does not have, is not this run's resume. A call reopened by later processes is judged by its last row, the child whose
// rows the run replays. The child is resolved the way the row says the run referred to it (ref): by its name, or by its scriptPath. A
// row that does not say (one written before ref was recorded) is resolved both ways: when both resolve and read different sources, the
// row cannot tell which one the run ran, and the resume is refused with both named rather than guessed; when only one resolves, or both
// read the same source, that source is compared. A run that never opened a child is untouched.
func ChangedChild(events []Event, runID string, resolve func(name, scriptPath string) (*script.Program, error)) string {
	type opened struct{ child, digest, ref string }
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
		last[id] = opened{child: strings.TrimPrefix(ev.Label, "workflow:"), digest: rowStr(ev.Data, "script_digest"), ref: rowStr(ev.Data, "ref")}
	}
	for _, id := range order {
		o := last[id]
		byName := o.ref != WorkflowRefScript
		byPath := o.ref != WorkflowRefName
		var named, pathed *script.Program
		var errs []string
		if byName {
			named, errs = resolveChild(resolve, o.child, "", errs)
		}
		if byPath {
			pathed, errs = resolveChild(resolve, "", o.child, errs)
		}
		prog := named
		if prog == nil {
			prog = pathed
		}
		switch {
		case prog == nil:
			why := ""
			if len(errs) > 0 {
				why = " (" + strings.Join(errs, "; ") + ")"
			}
			return fmt.Sprintf("run %s ran child workflow %s, which this build does not have%s", runID, o.child, why)
		case named != nil && pathed != nil && named.Digest != pathed.Digest:
			// only a row without ref resolves both ways
			return fmt.Sprintf("run %s ran child workflow %s, and its record (written before the reference kind was recorded) does not say whether the run named it or gave its scriptPath: "+
				"they read different sources, %s by the name (digest %s) and %s by the scriptPath (digest %s), the run's %s; move or rename one of the two so that only the one the run ran resolves and resume again, or start a new run",
				runID, o.child, childFile(named, "workflow('"+o.child+"')"), named.Digest, childFile(pathed, "workflow({scriptPath: '"+o.child+"'})"), pathed.Digest, o.digest)
		case prog.Digest != o.digest:
			return fmt.Sprintf("the child workflow %s is not the one run %s ran (digest %s, the run's %s)", o.child, runID, prog.Digest, o.digest)
		}
	}
	return ""
}

// resolveChild resolves one reference of a child, adding the reason to errs when it does not resolve.
func resolveChild(resolve func(name, scriptPath string) (*script.Program, error), name, scriptPath string, errs []string) (*script.Program, []string) {
	p, err := resolve(name, scriptPath)
	if err != nil {
		return nil, append(errs, err.Error())
	}
	return p, errs
}

// childFile names the source a reference resolved to: the program's name, or the reference when the program has none.
func childFile(p *script.Program, ref string) string {
	if p.Name != "" {
		return p.Name
	}
	return ref
}
