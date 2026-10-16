package lint

import (
	"errors"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

var errNoDir = errors.New("a script without a directory has no workflows to nest")

func errOutside(rel string) error { return fmt.Errorf("%q is outside the workflows directory", rel) }

// traceCall is what the journal's intent row says about one call.
type traceCall struct {
	path, label, role, family, node, stepDecision string
	// modelFamily is the vendor family of the model the call's thread was configured with.
	modelFamily            string
	explicit, continuation bool
}

// traceDiagnostics reads the journal of a dry run.
func traceDiagnostics(res *engine.Result, events []engine.Event, policy *engine.Policy) []Diagnostic {
	var out []Diagnostic
	add := func(rule, label, format string, args ...any) {
		out = append(out, Diagnostic{Rule: rule, Label: label, Message: fmt.Sprintf(format, args...), Source: sourceTrace})
	}
	labels := map[string]int{}
	lastAuthor := map[string]string{} // path -> model family of the last author call
	phases := map[string]uint8{}      // path and node -> lifecycle phase set
	checked := map[string]bool{}      // path and node -> a unit check holds for the node's current tree
	decided := map[string]bool{}      // path and step id -> crw.tierStep returned it on the path
	for _, e := range events {
		if e.Kind != engine.KindCall {
			continue
		}
		switch {
		case e.Event == engine.EvCompleted && e.Op == "crw.tierStep":
			if id, _ := e.Data["step_decision"].(string); id != "" {
				decided[e.Path+"\x00"+id] = true
			}
		case e.Event == engine.EvPrepared && e.Op == "agent":
			c := preparedCall(e)
			if c.explicit {
				key := c.path + "\x00" + c.label
				labels[key]++
				if labels[key] == 2 {
					add(RuleLabelDuplicate, c.label, "label %q is used by two calls on path %s; labels are unique within a path", c.label, c.path)
				}
			}
			if c.role == "executor-t1" && !c.continuation && c.stepDecision != "" && !decided[c.path+"\x00"+c.stepDecision] {
				add(RuleTierStepDecision, c.label, "the executor-t1 call %q carries stepDecision %q, which crw.tierStep did not return earlier on path %s", c.label, c.stepDecision, c.path)
			}
			// A review is independent of the call that wrote the change by its model family, a continuation too: the
			// thread keeps the pair it was configured with.
			if c.explicit && strings.HasPrefix(strings.ToLower(c.label), "review") {
				if author := lastAuthor[c.path]; author != "" && c.modelFamily != "" && author == c.modelFamily {
					add(RuleReviewerFamily, c.label, "the review call %q runs as %s on the %s model family, the family of the call that wrote the change on path %s", c.label, c.role, c.modelFamily, c.path)
				}
				continue
			}
			if c.continuation {
				if c.family == "executor" {
					lastAuthor[c.path] = c.modelFamily
					// A continuation of an executor thread changes the tree a unit check saw: of its node, or, when the
					// row names none, of any node of the path.
					for key := range checked {
						if key == c.path+"\x00"+c.node || c.node == "" && strings.HasPrefix(key, c.path+"\x00") {
							delete(checked, key)
						}
					}
				}
				continue
			}
			if c.family == "executor" {
				lastAuthor[c.path] = c.modelFamily
				key := c.path + "\x00" + c.node
				phases[key] = traceEvent(phases[key], false, buildEvent, c.path, &out)
				delete(checked, key)
			}
		case e.Event == engine.EvRefused:
			reason, _ := e.Data["reason"].(string)
			cause, _ := e.Data["cause"].(string)
			detail, _ := e.Data["detail"].(string)
			switch {
			case cause == "alias_ambiguous":
				add(RuleAliasAmbiguous, e.Label, "%s", strings.TrimPrefix(detail, "alias_ambiguous: "))
			case reason == engine.ReasonMissingStepDecision:
				add(RuleTierStepDecision, e.Label, "the executor-t1 call %q has no stepDecision from crw.tierStep: %s", e.Label, detail)
			default:
				add(RuleCallRefused, e.Label, "the call %q was refused with %s: %s", e.Label, reason, detail)
			}
		case strings.HasPrefix(e.Op, "crw.") && e.Event == engine.EvCompleted:
			op := strings.TrimPrefix(e.Op, "crw.")
			kind, _ := e.Data["kind"].(string)
			released, _ := e.Data["released"].(bool)
			le := lifecycleEvent(op, op == "release" && kind == "land" && released)
			// a unit check checks the node only when the tool judged the tree: role unit-check, state ok and a verdict. A check held
			// for the tool's quota or a blank answer has the role too, and a check that was not run has none
			if le.check {
				role, _ := e.Data["role"].(string)
				state, _ := e.Data["state"].(string)
				verdict, _ := e.Data["verdict"].(string)
				if role != execution.AgentUnitCheck || state != "ok" || verdict == "" {
					le = lifeEvent{}
				}
			}
			if le.from != 0 {
				key := e.Path + "\x00" + eventNode(e)
				phases[key] = traceEvent(phases[key], checked[key], le, e.Path, &out)
				switch le {
				case unitCheckEvent:
					checked[key] = true
				case landReleaseEvent:
					delete(checked, key)
				}
			}
		}
	}
	if res.Status != engine.RunCompleted {
		switch {
		case res.ErrorKind == engine.ErrKindPathConcurrency:
			add(RulePathConcurrency, "", "%s", res.Error)
		case strings.Contains(res.Error, "crw.classify takes"):
			add(RuleClassifyArg, "", "%s", res.Error)
		case strings.Contains(res.Error, "resume takes"):
			add(RuleResumeArg, "", "%s", res.Error)
		case strings.Contains(res.Error, "not available in a CRW workflow script"):
			add(RuleDeterminism, "", "%s", res.Error)
		}
	}
	return out
}

func preparedCall(e engine.Event) traceCall {
	str := func(k string) string { s, _ := e.Data[k].(string); return s }
	explicit, _ := e.Data["explicit_label"].(bool)
	model := str("configured_model")
	if model == "" {
		model = str("model")
	}
	c := traceCall{path: e.Path, label: e.Label, role: str("role"), family: str("family"), node: str("node"), stepDecision: str("step_decision"),
		explicit: explicit, continuation: str("resume_of") != ""}
	if model != "" {
		c.modelFamily = execution.ModelFamily(model)
	}
	return c
}

// eventNode is the node a lifecycle operation of the journal names: its node field, else the first of its arguments
// when that is a string.
func eventNode(e engine.Event) string {
	if n, _ := e.Data["node"].(string); n != "" {
		return n
	}
	if args, ok := e.Data["args"].([]any); ok && len(args) > 0 {
		if n, ok := args[0].(string); ok {
			return n
		}
	}
	return ""
}

// traceEvent applies one lifecycle event of a path to its phase set, as the static walk does; a path that has seen no
// event starts before the build. checked says whether a unit check holds for the node's current tree (a land is legal
// from verified then); a unit check leaves the phase set where it is, its caller sets the flag.
func traceEvent(phases uint8, checked bool, le lifeEvent, path string, out *[]Diagnostic) uint8 {
	if phases == 0 {
		phases = pInit
	}
	ev := le.to
	if phases&landFrom(le, checked) == 0 {
		rule, msg := RuleLifecycleOrder, fmt.Sprintf("%s ran on path %s while the node was %s: the order is build, rounds of commit, verify, review (a unit check before the commit or after the verify), then land", le.name, path, phaseList(phases))
		switch {
		case ev == pLanded && phases == pLanded:
			rule, msg = RuleLandTwice, fmt.Sprintf("crw.land ran a second time on path %s", path)
		case ev == pLanded:
			rule, msg = RuleLandWithoutReview, fmt.Sprintf("crw.land ran on path %s while the node was %s, without a completed review or a unit check of the head", path, phaseList(phases))
		}
		*out = append(*out, Diagnostic{Rule: rule, Message: msg, Source: sourceTrace})
	}
	if le.check {
		return phases
	}
	return ev
}
