// Package delivery describes milestone criteria whose observations belong after
// publication. It never publishes a branch or changes a tracking record.
package delivery

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	trackingref "github.com/thisisjun786/codex-relay-workflow/internal/tracking/ref"
)

const (
	RemotePush      = "remote_push"
	IssuesClosed    = "issues_closed"
	MilestoneClosed = "milestone_closed"
	Passed          = "passed"
	Pending         = "pending"
	Unobserved      = "unobserved"
)

// Criterion refers to the original c1, c2, ... identifier; it does not replace or
// renumber the milestone's criteria. Every check must be observed.
type Criterion struct {
	ID     string  `json:"id"`
	Checks []Check `json:"checks"`
}

type Check struct {
	Kind string `json:"kind"`
	// Ref is required only for milestone_closed: OWNER/NAME#N. A flow milestone
	// key is not necessarily the identifier of a tracking milestone.
	Ref string `json:"ref,omitempty"`
}

// Valid returns only complete, unambiguous annotations. An invalid annotation
// does not exempt its criterion from the build crosscheck; its problem is a
// warning of compile/start, rather than a refusal of the handoff.
func Valid(criteria []string, proposed []Criterion) ([]Criterion, []string) {
	var valid []Criterion
	var problems []string
	counts := map[string]int{}
	for _, c := range proposed {
		counts[c.ID]++
	}
	for _, c := range proposed {
		n, err := strconv.Atoi(strings.TrimPrefix(c.ID, "c"))
		why := ""
		switch {
		case err != nil || n < 1 || n > len(criteria) || c.ID != "c"+strconv.Itoa(n):
			why = "does not name an existing criterion"
		case counts[c.ID] != 1:
			why = "is annotated more than once"
		case len(c.Checks) == 0:
			why = "names no delivery check"
		default:
			seen := map[Check]bool{}
			for _, check := range c.Checks {
				if seen[check] {
					why = "repeats a delivery check"
					break
				}
				seen[check] = true
				switch check.Kind {
				case RemotePush, IssuesClosed:
					if check.Ref != "" {
						why = check.Kind + " takes its target from the run, not a ref"
					}
				case MilestoneClosed:
					if _, err := trackingref.Parse(check.Ref); err != nil {
						why = "milestone_closed needs an OWNER/NAME#N ref"
					}
				default:
					why = "has an unknown delivery check " + check.Kind
				}
				if why != "" {
					break
				}
			}
		}
		if why != "" {
			problems = append(problems, fmt.Sprintf("%s: %s; kept in the build crosscheck", c.ID, why))
			continue
		}
		valid = append(valid, Criterion{ID: c.ID, Checks: append([]Check(nil), c.Checks...)})
	}
	return valid, problems
}

// Target is the closed run's publication target. Issues are canonical tracking
// references read from its milestone nodes, never a caller-selected subset.
type Target struct {
	Checkout string
	Branch   string
	Head     string
	Issues   []string
}

// Reader is readback only. Remote reads origin's exact refs/heads/<branch>;
// Ancestor proves that an observed newer tip contains the closed head. A missing
// object or an unavailable service is unobserved, never an affirmative answer.
type Reader struct {
	Remote    func(context.Context, string, string) (string, error)
	Ancestor  func(context.Context, string, string, string) (bool, error)
	Issue     func(context.Context, string) (state, reason string, err error)
	Milestone func(context.Context, string) (state string, err error)
}

type Observation struct {
	Criterion string `json:"criterion"`
	Kind      string `json:"kind"`
	Target    string `json:"target"`
	State     string `json:"state"`
	Evidence  string `json:"evidence"`
}

// Observe evaluates the explicit checks. Callers first establish the closed,
// crosschecked head and validate the annotations with Valid.
func Observe(ctx context.Context, criteria []Criterion, target Target, reader Reader) []Observation {
	var out []Observation
	add := func(id, kind, ref, state, evidence string) {
		out = append(out, Observation{id, kind, ref, state, evidence})
	}
	for _, criterion := range criteria {
		for _, check := range criterion.Checks {
			switch check.Kind {
			case RemotePush:
				ref := "origin/refs/heads/" + target.Branch
				state, evidence := Unobserved, "the run has no readable origin target"
				if target.Checkout != "" && target.Branch != "" && target.Head != "" && reader.Remote != nil {
					head, err := reader.Remote(ctx, target.Checkout, target.Branch)
					switch {
					case err != nil || head == "":
						evidence = "origin's branch could not be observed"
					case head == target.Head:
						state, evidence = Passed, "origin is at the closed head "+head
					case reader.Ancestor == nil:
						evidence = "origin is at " + head + "; containment of " + target.Head + " was not observed"
					default:
						contains, err := reader.Ancestor(ctx, target.Checkout, target.Head, head)
						switch {
						case err != nil:
							evidence = "origin is at " + head + "; containment of " + target.Head + " could not be read"
						case contains:
							state, evidence = Passed, "origin is at "+head+" and contains the closed head "+target.Head
						default:
							state, evidence = Pending, "origin is at "+head+" and does not contain the closed head "+target.Head
						}
					}
				}
				add(criterion.ID, check.Kind, ref, state, evidence)
			case IssuesClosed:
				if len(target.Issues) == 0 {
					add(criterion.ID, check.Kind, "", Unobserved, "the closed milestone names no issues")
				}
				for _, ref := range target.Issues {
					state, evidence := Unobserved, "the issue could not be observed"
					if reader.Issue != nil {
						got, reason, err := reader.Issue(ctx, ref)
						if err == nil {
							switch {
							case got == "open":
								state, evidence = Pending, "the issue is open"
							case got == "closed" && reason == "completed":
								state, evidence = Passed, "the issue is closed as completed"
							case got == "closed" && reason == "not_planned":
								state, evidence = Pending, "the issue is closed as not planned"
							case got == "closed":
								evidence = "the issue is closed; its completion reason was not observed"
							}
						}
					}
					add(criterion.ID, check.Kind, ref, state, evidence)
				}
			case MilestoneClosed:
				state, evidence := Unobserved, "the tracking milestone could not be observed"
				if reader.Milestone != nil {
					got, err := reader.Milestone(ctx, check.Ref)
					if err == nil {
						switch got {
						case "closed":
							state, evidence = Passed, "the tracking milestone is closed"
						case "open":
							state, evidence = Pending, "the tracking milestone is open"
						}
					}
				}
				add(criterion.ID, check.Kind, check.Ref, state, evidence)
			default:
				add(criterion.ID, check.Kind, check.Ref, Unobserved, "unsupported delivery check")
			}
		}
	}
	return out
}

// State never treats an empty set or an observation nobody could make as a pass.
func State(observations []Observation) string {
	if len(observations) == 0 {
		return Unobserved
	}
	state := Passed
	for _, o := range observations {
		switch o.State {
		case Passed:
		case Pending:
			if state == Passed {
				state = Pending
			}
		default:
			state = Unobserved
		}
	}
	return state
}
