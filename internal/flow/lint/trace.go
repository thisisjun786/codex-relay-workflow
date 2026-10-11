package lint

import (
	"errors"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

var errNoDir = errors.New("a script without a directory has no workflows to nest")

func errOutside(rel string) error { return fmt.Errorf("%q is outside the workflows directory", rel) }

// traceCall is what the journal's intent row says about one call.
type traceCall struct {
	path, label, role, family, stepDecision string
	explicit, continuation                  bool
}

// traceDiagnostics reads the journal of a dry run.
func traceDiagnostics(res *engine.Result, events []engine.Event, policy *engine.Policy) []Diagnostic {
	var out []Diagnostic
	add := func(rule, label, format string, args ...any) {
		out = append(out, Diagnostic{Rule: rule, Label: label, Message: fmt.Sprintf(format, args...), Source: sourceTrace})
	}
	labels := map[string]int{}
	lastAuthor := map[string]string{} // path -> role family of the last author call
	phases := map[string]uint8{}      // path -> lifecycle phase set
	for _, e := range events {
		if e.Kind != engine.KindCall {
			continue
		}
		switch {
		case e.Event == engine.EvPrepared && e.Op == "agent":
			c := preparedCall(e)
			if c.explicit {
				key := c.path + "\x00" + c.label
				labels[key]++
				if labels[key] == 2 {
					add(RuleLabelDuplicate, c.label, "label %q is used by two calls on path %s; labels are unique within a path", c.label, c.path)
				}
			}
			if c.continuation {
				continue
			}
			if c.explicit && strings.HasPrefix(strings.ToLower(c.label), "review") {
				if author := lastAuthor[c.path]; author != "" && author == c.family {
					add(RuleReviewerFamily, c.label, "the review call %q runs as %s, the same role family (%s) as the call that wrote the change on path %s", c.label, c.role, c.family, c.path)
				}
				continue
			}
			if c.family == "executor" {
				lastAuthor[c.path] = c.family
				phases[c.path] = traceEvent(phases[c.path], pBuilt, c.path, &out)
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
			if ev := lifecycleOp(strings.TrimPrefix(e.Op, "crw.")); ev != 0 {
				phases[e.Path] = traceEvent(phases[e.Path], ev, e.Path, &out)
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
	return traceCall{path: e.Path, label: e.Label, role: str("role"), family: str("family"), stepDecision: str("step_decision"),
		explicit: explicit, continuation: str("resume_of") != ""}
}

// traceEvent applies one lifecycle event of a path to its phase set, as the static walk does; a path that has seen no
// event starts at the build.
func traceEvent(phases, ev uint8, path string, out *[]Diagnostic) uint8 {
	if phases == 0 {
		phases = pInit
	}
	if phases&legalFrom[ev] == 0 {
		rule, msg := RuleLifecycleOrder, fmt.Sprintf("%s ran on path %s while the node was %s: the order is build, rounds of commit, verify, review, then land", eventNames[ev], path, phaseList(phases))
		switch {
		case ev == pLanded && phases == pLanded:
			rule, msg = RuleLandTwice, fmt.Sprintf("crw.land ran a second time on path %s", path)
		case ev == pLanded:
			rule, msg = RuleLandWithoutReview, fmt.Sprintf("crw.land ran on path %s while the node was %s, without a completed review", path, phaseList(phases))
		}
		*out = append(*out, Diagnostic{Rule: rule, Message: msg, Source: sourceTrace})
	}
	return ev
}
