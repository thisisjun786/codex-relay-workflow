package pullrequest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/planinput"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

// The reasons `open --run` refuses with (exit 2), each printed as the first word of the refusal.
const (
	reasonRunUnknown         = "run_unknown"
	reasonRunNotClosed       = "run_not_closed"
	reasonCrosscheckMissing  = "crosscheck_missing"
	reasonCrosscheckMoved    = "crosscheck_head_moved"
	reasonCrosscheckRefused  = "crosscheck_not_accepted"
	reasonIssuesUnknown      = "issues_unknown"
	reasonIssuesNotMilestone = "issues_not_the_milestones"
	reasonIssueOtherRepo     = "issue_other_repo"
	reasonBodyLeak           = "body_leak"
)

// Source selects the relay store a run is read from: the state directory and the control socket `--state` and `--socket` name. Both
// empty is the relay's own selection (the default socket's state).
type Source struct{ State, Socket string }

// ErrNoRun is a run the selected store does not hold.
var ErrNoRun = errors.New("the store holds no such run")

// readJournal reads the rows of a run from the relay store src selects, read-only: nothing is created and no write gate is taken. It
// follows the sequence of flow/cli's reader and of compile's key binding reader, which this package cannot import (flow/cli imports
// this package). The rows are full (result bytes included): the facts of a run are in the results of its calls.
func readJournal(ctx context.Context, src Source, run string) ([]engine.Event, error) {
	socket := src.Socket
	if socket == "" {
		var err error
		if socket, err = store.DefaultSocket(); err != nil {
			return nil, err
		}
	}
	sel, err := store.ResolveStateDir(src.State, socket)
	if err != nil {
		return nil, err
	}
	db := sel.DBPath()
	if err := store.CheckReadable(db); err != nil {
		return nil, fmt.Errorf("there is no relay store at %s (%s): %w", db, sel.Detail, err)
	}
	st, err := store.OpenReadOnlyStore(ctx, db)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	// journal.Load reads the calls and the flow rows in separate queries, so a run resumed meanwhile could mix an old close with a new
	// finished row. The lean read is one snapshot; the journal is append-only, so a full read with the same number of rows is the same
	// rows. Any other count means the run changed while it was read: nothing is judged from it.
	snapshot, hasRun, err := journal.LoadLean(ctx, st, run)
	if err != nil {
		return nil, err
	}
	if !hasRun {
		return nil, fmt.Errorf("%w: %s", ErrNoRun, run)
	}
	events, _, err := journal.Load(ctx, st, run)
	if err != nil {
		return nil, err
	}
	if len(events) != len(snapshot) {
		return nil, fmt.Errorf("the run %s changed while it was read (%d rows, then %d): read it again", run, len(snapshot), len(events))
	}
	return events, nil
}

// RunFacts are the facts of a run the pull request of its milestone states.
type RunFacts struct {
	// Status is the status of the run's last finished row; Outcome its outcome member (nil when it has none).
	Status  string
	Outcome *engine.Outcome
	// Closed is whether the run is a closed run: finished completed, outcome state closed with a milestone, no later started row, and
	// a completed close of that milestone whose target had not moved. Why says what is missing when it is not.
	Closed bool
	Why    string
	// Plan and Nodes are the plan id and the nodes of the milestone's last crw.milestone row (a replan changes the nodes).
	Plan  string
	Nodes []ops.PlanNode
	// Moved is whether the close says the integration target moved after its gate.
	Moved bool
	// Body is what the body states: the milestone, the close tip, the gate and the crosscheck at that tip (filled when Closed).
	Body BodyFacts
	// Events are the rows the facts were read from, for the crosscheck reader.
	Events []engine.Event
}

// FactsOf reads the facts of a run from its journal rows. It reads the run-level facts from the flow rows themselves, as
// flow/cli's Summarize does (which this package cannot import): the LAST finished row, ignored when a later started row exists (a
// resumed run is not finished). A row it cannot read is an error; a run that did not close is not (Closed is false and Why says why).
func FactsOf(events []engine.Event) (RunFacts, error) {
	f := RunFacts{Events: events}
	lastStart, lastFinish := -1, -1
	for i, e := range events {
		if e.Kind != engine.KindRun {
			continue
		}
		switch e.Event {
		case "started":
			lastStart = i
		case "finished":
			lastFinish = i
			f.Status = str(e.Data, "status")
			f.Outcome = engine.FinishedOutcome(e.Data)
		}
	}
	switch {
	case lastFinish < 0:
		return f.notClosed("the run has not finished"), nil
	case lastStart > lastFinish:
		return f.notClosed("the run was resumed after its last finished row and has not finished again"), nil
	case f.Status != engine.RunCompleted:
		return f.notClosed("the run ended " + orUnknown(f.Status) + ", not completed"), nil
	case f.Outcome == nil:
		return f.notClosed("the run's finished row carries no outcome"), nil
	case f.Outcome.State != "closed":
		return f.notClosed("the run's outcome is " + f.Outcome.State + ", not closed"), nil
	case f.Outcome.Milestone == "":
		return f.notClosed("the run's outcome names no milestone"), nil
	}
	ms := f.Outcome.Milestone
	closeRow, ok := lastRow(events, ops.OpMilestoneClose, ms)
	if !ok {
		return f.notClosed("the run holds no completed close of milestone " + ms), nil
	}
	f.Moved = flagOf(closeRow.Data, "moved")
	if f.Moved {
		return f.notClosed("the integration target moved after the gate of milestone " + ms + " judged it"), nil
	}
	f.Body.Milestone, f.Body.Tip = ms, str(closeRow.Data, "tip")
	if f.Body.Tip == "" {
		return f.notClosed("the close of milestone " + ms + " names no tip"), nil
	}
	if res := resultOf(closeRow); len(res) > 0 {
		var closed struct {
			Gate *struct {
				State    string `json:"state"`
				Evidence []struct {
					Kind    string `json:"kind"`
					Command string `json:"command"`
					State   string `json:"state"`
					Digest  string `json:"digest"`
				} `json:"evidence"`
			} `json:"gate"`
		}
		if err := json.Unmarshal(res, &closed); err != nil {
			return f, fmt.Errorf("the result of the close of milestone %s cannot be read: %w", ms, err)
		}
		if g := closed.Gate; g != nil {
			f.Body.Gate.State = g.State
			for _, e := range g.Evidence {
				f.Body.Gate.Evidence = append(f.Body.Gate.Evidence, Evidence{Kind: e.Kind, Command: e.Command, State: e.State, Digest: e.Digest})
			}
		}
	}
	if msRow, ok := lastRow(events, ops.OpMilestone, ms); ok {
		f.Plan = str(msRow.Data, "plan")
		if res := resultOf(msRow); len(res) > 0 {
			var m ops.Milestone
			if err := json.Unmarshal(res, &m); err != nil {
				return f, fmt.Errorf("the result of the milestone row of %s cannot be read: %w", ms, err)
			}
			f.Nodes = m.Nodes
		}
	}
	f.Closed = true
	f.Body.Crosscheck, _ = crosscheckOf(events, ms, f.Body.Tip)
	return f, nil
}

func (f RunFacts) notClosed(why string) RunFacts {
	f.Closed, f.Why = false, why
	return f
}

func orUnknown(s string) string {
	if s == "" {
		return "with no status"
	}
	return s
}

// lastRow is the newest completed row of op whose data names the milestone.
func lastRow(events []engine.Event, op, milestone string) (engine.Event, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Kind == engine.KindCall && e.Op == op && e.Event == engine.EvCompleted && str(e.Data, "milestone") == milestone {
			return e, true
		}
	}
	return engine.Event{}, false
}

// resultOf is the JSON a completed call row answered (the journal reads it back as a string member of the row's data).
func resultOf(e engine.Event) json.RawMessage {
	s, _ := e.Data["result"].(string)
	return json.RawMessage(s)
}

// crosscheckOf judges the crosscheck of a milestone at a head from the journal's rows (R3's D-1): the newest completed crw.crosscheck
// row whose data names the milestone and the head. It is accepted when the row's state is verdict, its verdict object says verified
// and the object names the same head (and the same milestone when it names one). Any other state of that row (evidence included: a
// needs_changes waiting for evidence), a row without the object and a needs_changes are not accepted. With no row at the head the
// reason is head_moved when the milestone has a row at another head, else missing; a verified row of an older head never approves a
// head that moved. The Crosscheck is filled only when the reason is empty.
func crosscheckOf(events []engine.Event, milestone, head string) (Crosscheck, string) {
	anyOfMilestone := false
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Kind != engine.KindCall || e.Op != ops.OpCrosscheck || e.Event != engine.EvCompleted || str(e.Data, "milestone") != milestone {
			continue
		}
		anyOfMilestone = true
		if str(e.Data, "head") != head {
			continue
		}
		v, ok := e.Data["verdict"].(map[string]any)
		if !ok || str(e.Data, "state") != "verdict" || str(v, "verdict") != "verified" || str(v, "head") != head ||
			(str(v, "milestone") != "" && str(v, "milestone") != milestone) {
			return Crosscheck{}, reasonCrosscheckRefused
		}
		c := Crosscheck{Verdict: "verified", Head: head, EngineFamily: str(v, "engine_family"), Model: str(v, "model"), GuideRev: str(v, "guide_rev"),
			Round: intOf(v["round"])}
		if list, ok := v["follow_ups"].([]any); ok {
			for _, item := range list {
				if s, ok := item.(string); ok {
					c.FollowUps = append(c.FollowUps, s)
				}
			}
		}
		return c, ""
	}
	if anyOfMilestone {
		return Crosscheck{}, reasonCrosscheckMoved
	}
	return Crosscheck{}, reasonCrosscheckMissing
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func flagOf(m map[string]any, key string) bool {
	b, _ := m[key].(bool)
	return b
}

// intOf reads a number of a row's data: an int in memory, a float64 after the JSON round trip of the store.
func intOf(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

// carriedRefs are the issues the pull request of the milestone carries, from its nodes and the --issue values the caller gave (D-3):
//   - every node's issue decodes (planinput.DecodeKey, OWNER:NAME:N) and no --issue: the decoded refs, in node order, once each;
//   - every node decodes and --issue is given: the two sets must be equal, else issues_not_the_milestones;
//   - some node does not decode (a Linear key, a hand-written key) or the milestone has no node: --issue as given, and without it
//     issues_unknown.
//
// Every carried ref must be of the repository the pull request is opened in (issue_other_repo). The reason is empty when the refs can
// be used, and the message is one line.
func carriedRefs(nodes []ops.PlanNode, given []tracking.Ref, repo string) (refs []tracking.Ref, reason, message string) {
	// GitHub names are case-insensitive and DecodeKey keeps the spelling, so refs are compared by refKey
	var decoded []tracking.Ref
	seen := map[tracking.Ref]bool{}
	all := len(nodes) > 0
	for _, n := range nodes {
		ref, err := planinput.DecodeKey(n.Issue)
		if err != nil {
			all = false
			break
		}
		if !seen[refKey(ref)] {
			seen[refKey(ref)] = true
			decoded = append(decoded, ref)
		}
	}
	switch {
	case all && len(given) == 0:
		refs = decoded
	case all:
		want := map[tracking.Ref]bool{}
		for _, r := range given {
			want[refKey(r)] = true
		}
		equal := len(want) == len(seen)
		for r := range want {
			equal = equal && seen[r]
		}
		if !equal {
			return nil, reasonIssuesNotMilestone, "--issue does not name the issues the milestone's nodes carry: leave --issue out to take them from the run"
		}
		refs = decoded
	case len(given) == 0:
		return nil, reasonIssuesUnknown, "give --issue: the milestone's keys are not OWNER:NAME:N"
	default:
		refs = given
	}
	for _, r := range refs {
		if !strings.EqualFold(r.Repo, repo) {
			return nil, reasonIssueOtherRepo, fmt.Sprintf("%s is not an issue of %s, the repository the pull request is opened in", r, repo)
		}
	}
	return refs, "", ""
}

// refKey is a ref with the repository in lower case, the key refs are compared by.
func refKey(r tracking.Ref) tracking.Ref {
	r.Repo = strings.ToLower(r.Repo)
	return r
}

// leakMessage is the one-line refusal of a title and body that leak: the kinds and a 12-character prefix of each match, never a whole
// match.
func leakMessage(leaks []Leak) string {
	var parts []string
	for _, l := range leaks {
		parts = append(parts, l.Short())
	}
	return "the title or the body would publish what must not reach a public repository: " + strings.Join(parts, "; ")
}
