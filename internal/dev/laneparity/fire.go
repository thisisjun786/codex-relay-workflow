//go:build dev

package laneparity

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contracttest"
	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
)

// Faults the harness can inject to show that a wrong run does not pass. Each one is a way a cell
// could report success without the hook having done its work.
const (
	FaultNone = ""
	// Registration faults: the declaration changes.
	FaultWrongEvent  = "wrong-event"  // the first leg is declared under another event name
	FaultMissingHook = "missing-hook" // the first leg is not declared at all
	FaultNoop        = "noop"         // every declared command does nothing and succeeds
	FaultKill        = "kill"         // every declared command is killed before it answers
	// Transport faults: what comes back changes.
	FaultDropStdout   = "drop-stdout"   // every answer is lost on its way back
	FaultReverseSteps = "reverse-steps" // a fixture's payloads arrive in the opposite order
	// Receipt faults: the record changes.
	FaultStaleReceipt  = "stale-receipt"  // receipts of an earlier run
	FaultOtherPlugin   = "other-plugin"   // receipts of another plugin that declares the same events
	FaultOtherBuild    = "other-build"    // receipts of another crw build (head)
	FaultReceiptEvent  = "receipt-event"  // receipts naming another event
	FaultReceiptAgent  = "receipt-agent"  // receipts naming another agent
	FaultReceiptSkill  = "receipt-skill"  // receipts naming another skill
	FaultLostReceipt   = "lost-receipt"   // one receipt is missing
	FaultDoubleReceipt = "double-receipt" // one firing is recorded twice
)

// Faults lists every fault name, for the flag's help and the tests.
var Faults = []string{FaultWrongEvent, FaultMissingHook, FaultNoop, FaultKill, FaultDropStdout, FaultReverseSteps,
	FaultStaleReceipt, FaultOtherPlugin, FaultOtherBuild, FaultReceiptEvent, FaultReceiptAgent, FaultReceiptSkill, FaultLostReceipt, FaultDoubleReceipt}

// FireOptions is one firing of the corpus through a plugin root.
type FireOptions struct {
	Root    string         // the repository holding contract/
	CRW     string         // the crw build under test (absolute)
	Plugin  string         // the plugin root to fire
	Scratch string         // where case roots live
	Only    *regexp.Regexp // restrict the fixtures
	Fault   string
	Run     string // the run id (a fresh one when empty)
}

// FixtureFire is the firing and effect of one fixture.
type FixtureFire struct {
	ID      string `json:"id"`
	Leg     string `json:"leg"`
	State   string `json:"state"`
	Run     bool   `json:"run"`
	OK      bool   `json:"ok"`
	Silent  bool   `json:"silent,omitempty"`
	Problem string `json:"problem,omitempty"`
}

// LegFire is the firing and effect cell of one leg.
type LegFire struct {
	Leg     string `json:"leg"`
	Matched int    `json:"matched"` // fixtures that ran and equal their expectation
	Failed  int    `json:"failed"`
	Pending int    `json:"pending"` // fixtures no status file claims for a port yet
	// Positive counts matched fixtures that expect an answer or a nonzero exit: a command that does
	// nothing passes the others, so only these show the handler ran.
	Positive int  `json:"positive"`
	OK       bool `json:"ok"`
	// Witnessed is whether at least one positive fixture matched.
	Witnessed bool   `json:"witnessed"`
	Note      string `json:"note,omitempty"`
}

// FireReport is the firing, effect and receipt cells of a run.
type FireReport struct {
	Run             string        `json:"run"`
	Plugin          string        `json:"pluginDigest"`
	Binary          string        `json:"binarySha256"`
	Fault           string        `json:"fault,omitempty"`
	Legs            []LegFire     `json:"legs"`
	Fixtures        []FixtureFire `json:"fixtures"`
	Probes          []ProbeFire   `json:"probes"`
	Receipts        []Receipt     `json:"receipts"`
	ReceiptProblems []string      `json:"receiptProblems,omitempty"`
	// Unverified names what a green run does not show: legs no fixture witnesses.
	Unverified []string `json:"unverified,omitempty"`
	OK         bool     `json:"ok"`
}

// Fire fires every claimed hook fixture of the corpus, plus CRW's own probes, through the commands
// the plugin root declares, and returns the firing, effect and receipt cells.
func Fire(o FireOptions) (FireReport, error) {
	rep := FireReport{Run: o.Run, Fault: o.Fault}
	if rep.Run == "" {
		rep.Run = NewRunID()
	}
	if !slices.Contains(append([]string{FaultNone}, Faults...), o.Fault) {
		return rep, fmt.Errorf("unknown fault %q", o.Fault)
	}
	want, err := ExpectedLegs(o.Root)
	if err != nil {
		return rep, err
	}
	_, registered, err := ReadRegistered(o.Plugin)
	if err != nil {
		return rep, err
	}
	registered = injectRegistration(o.Fault, want, registered)
	declared := Declared(registered, want)
	if rep.Plugin, err = PluginDigest(o.Plugin); err != nil {
		return rep, err
	}
	if rep.Binary, err = FileDigest(o.CRW); err != nil {
		return rep, err
	}
	// A receipt names the build the declared command starts, not the file --crw names: a root whose
	// commands start another executable shows it there.
	builds := map[string]string{}
	for leg, command := range declared {
		builds[leg], _ = CommandBuild(command, o.CRW)
	}
	scratch := o.Scratch
	if scratch == "" {
		if scratch, err = os.MkdirTemp("", "crw-parity-"); err != nil {
			return rep, err
		}
		defer os.RemoveAll(scratch)
	}
	in := contracttest.HookFireInput{Root: o.Root, CRW: o.CRW, Plugin: o.Plugin, Declared: declared, Scratch: scratch, Only: o.Only}
	switch o.Fault {
	case FaultDropStdout:
		in.Mutate = func(_ string, got *cxccorpus.Expect) {
			for i := range got.Steps {
				got.Steps[i].Stdout, got.Steps[i].StdoutJSON, got.Steps[i].StdoutJSONL, got.Steps[i].StdoutForm = nil, nil, nil, "empty"
			}
		}
	case FaultReverseSteps:
		in.Steps = func(_ string, steps []cxccorpus.Step) []cxccorpus.Step {
			slices.Reverse(steps)
			return steps
		}
	}
	results, err := contracttest.FireHooks(in)
	if err != nil {
		return rep, err
	}
	wantLeg := map[string]Leg{}
	for _, l := range want {
		wantLeg[l.Leg] = l
	}
	perLeg := map[string]*LegFire{}
	legOf := func(name string) *LegFire {
		if perLeg[name] == nil {
			perLeg[name] = &LegFire{Leg: name}
		}
		return perLeg[name]
	}
	var wants []Want
	for _, res := range results {
		ff := FixtureFire{ID: res.ID, Leg: res.Leg, State: res.State, Run: res.Run, Silent: res.Silent}
		leg := legOf(res.Leg)
		switch {
		case res.State == "pending":
			leg.Pending++
		case res.Err != nil:
			ff.Problem = res.Err.Error()
			leg.Failed++
		default:
			ff.OK = true
			leg.Matched++
			if !res.Silent {
				leg.Positive++
			}
		}
		rep.Fixtures = append(rep.Fixtures, ff)
		if res.Run && res.Observed != nil {
			ws, rs := stepReceipts(rep, wantLeg, declared, builds, res)
			wants, rep.Receipts = append(wants, ws...), append(rep.Receipts, rs...)
		}
	}
	rep.OK = true
	if rep.Probes, err = fireOwn(o, in, wantLeg, declared, builds, &rep, &wants, perLeg); err != nil {
		return rep, err
	}
	for _, p := range rep.Probes {
		rep.OK = rep.OK && p.OK
	}
	rep.Receipts = injectReceipts(o.Fault, rep, rep.Receipts)
	rep.ReceiptProblems = VerifyReceipts(rep.Run, rep.Plugin, rep.Binary, wants, rep.Receipts)
	rep.OK = rep.OK && len(rep.ReceiptProblems) == 0
	names := make([]string, 0, len(perLeg))
	for name := range perLeg {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		leg := perLeg[name]
		leg.Witnessed = leg.Positive > 0
		leg.OK = leg.Failed == 0
		switch {
		case leg.Failed > 0:
			leg.Note = fmt.Sprintf("%d fixture(s) differ from the expectation", leg.Failed)
		case leg.Matched == 0:
			leg.Note = fmt.Sprintf("no fixture ran: %d pending", leg.Pending)
			rep.Unverified = append(rep.Unverified, fmt.Sprintf("%s: effect not shown (%s)", name, leg.Note))
		case !leg.Witnessed:
			leg.Note = "only silent fixtures matched: a command that does nothing passes them too"
			rep.Unverified = append(rep.Unverified, fmt.Sprintf("%s: effect not shown (%s)", name, leg.Note))
		}
		rep.OK = rep.OK && leg.OK
		rep.Legs = append(rep.Legs, *leg)
	}
	return rep, nil
}

// stepReceipts makes the receipts of a fixture's hook steps, and what each must name.
func stepReceipts(rep FireReport, wantLeg map[string]Leg, declared, builds map[string]string, res contracttest.HookFireResult) ([]Want, []Receipt) {
	var wants []Want
	var receipts []Receipt
	for i, step := range res.Steps {
		if step.Leg == "" || i >= len(res.Observed.Steps) {
			continue
		}
		event := wantLeg[step.Leg].Event
		var expectedAnswer, observedAnswer string
		if i < len(res.Expected.Steps) {
			expectedAnswer = res.Expected.Steps[i].StdoutBytes()
		}
		observed := res.Observed.Steps[i]
		observedAnswer = observed.StdoutBytes()
		subject := SubjectOf(step.Payload, expectedAnswer)
		seen := subject
		seen.Skills = SkillsIn(observedAnswer)
		if res.State != "identical" { // a changed claim may rename what the answer names
			subject.Skills, seen.Skills = nil, nil
		}
		fixture := res.ID
		wants = append(wants, Want{Fixture: fixture, Step: i, Leg: step.Leg, Event: event, Subject: subject})
		receipts = append(receipts, Receipt{
			Run: rep.Run, Plugin: rep.Plugin, Binary: builds[step.Leg], Fixture: fixture, Step: i, Leg: step.Leg, Event: event,
			Command: declared[step.Leg], Session: seen.Session, Turn: seen.Turn, ToolUse: seen.ToolUse, ToolName: seen.ToolName,
			Agent: seen.Agent, Skills: seen.Skills, Exit: observed.Exit, StdoutSHA256: StdoutDigest(observedAnswer),
		})
	}
	return wants, receipts
}

// injectRegistration changes what the root declares, in memory, for the registration faults.
func injectRegistration(fault string, want []Leg, got []Registered) []Registered {
	got = slices.Clone(got)
	if len(want) == 0 {
		return got
	}
	first := want[0].Leg
	switch fault {
	case FaultWrongEvent:
		for i := range got {
			if got[i].Leg == first {
				got[i].Event = "Stop"
				if want[0].Event == "Stop" {
					got[i].Event = "SessionStart"
				}
			}
		}
	case FaultMissingHook:
		got = slices.DeleteFunc(got, func(r Registered) bool { return r.Leg == first })
	case FaultNoop, FaultKill: // Leg keeps what the original command started, so the leg is still fired
		for i := range got {
			got[i].Command = map[string]string{FaultNoop: "exit 0", FaultKill: "kill -9 $$"}[fault]
		}
	}
	return got
}

// injectReceipts changes the receipts of a run, for the receipt faults.
func injectReceipts(fault string, rep FireReport, rs []Receipt) []Receipt {
	rs = slices.Clone(rs)
	if len(rs) == 0 {
		return rs
	}
	switch fault {
	case FaultStaleReceipt:
		for i := range rs {
			rs[i].Run = "0000000000000000"
		}
	case FaultOtherPlugin:
		for i := range rs {
			rs[i].Plugin = strings.Repeat("0", 64)
		}
	case FaultOtherBuild:
		for i := range rs {
			rs[i].Binary = strings.Repeat("f", 64)
		}
	case FaultReceiptEvent:
		for i := range rs {
			rs[i].Event = "Notification"
		}
	case FaultReceiptAgent:
		for i := range rs {
			rs[i].Agent = "other-agent"
		}
	case FaultReceiptSkill:
		for i := range rs {
			rs[i].Skills = append(slices.Clone(rs[i].Skills), "crw:crw-not-this-skill")
		}
	case FaultLostReceipt:
		rs = rs[1:]
	case FaultDoubleReceipt:
		rs = append(rs, rs[0])
	}
	return rs
}

// ProbeFire is the outcome of one of CRW's own probes.
type ProbeFire struct {
	ID      string `json:"id"`
	Leg     string `json:"leg"`
	OK      bool   `json:"ok"`
	Silent  bool   `json:"silent,omitempty"`
	Problem string `json:"problem,omitempty"`
}

// fireOwn fires the probes for the registrations K1 does not hold, folding them into the leg cells
// and the receipts.
func fireOwn(o FireOptions, in contracttest.HookFireInput, wantLeg map[string]Leg, declared, builds map[string]string, rep *FireReport, wants *[]Want, perLeg map[string]*LegFire) ([]ProbeFire, error) {
	probes := OwnProbes()
	cps := make([]contracttest.Probe, len(probes))
	for i, p := range probes {
		cps[i] = contracttest.Probe{ID: p.ID, Scenario: p.Scenario, Check: p.Check}
	}
	results, err := contracttest.FireProbes(in, cps)
	if err != nil {
		return nil, err
	}
	var out []ProbeFire
	for i, res := range results {
		p := probes[i]
		pf := ProbeFire{ID: p.ID, Leg: p.Leg, Silent: p.Silent, OK: res.Err == nil}
		leg := perLeg[p.Leg]
		if leg == nil {
			leg = &LegFire{Leg: p.Leg}
			perLeg[p.Leg] = leg
		}
		if res.Err != nil {
			pf.Problem = res.Err.Error()
			leg.Failed++
		} else {
			leg.Matched++
			if !p.Silent {
				leg.Positive++
			}
		}
		out = append(out, pf)
		if len(res.Observed.Steps) > 0 && p.Scenario.Steps[0].Hook != "" {
			step := p.Scenario.Steps[0]
			subject := SubjectOf(step.Stdin, "")
			observed := res.Observed.Steps[0]
			*wants = append(*wants, Want{Fixture: "probe:" + p.ID, Step: 0, Leg: p.Leg, Event: wantLeg[p.Leg].Event, Subject: subject})
			rep.Receipts = append(rep.Receipts, Receipt{
				Run: rep.Run, Plugin: rep.Plugin, Binary: builds[p.Leg], Fixture: "probe:" + p.ID, Step: 0, Leg: p.Leg, Event: wantLeg[p.Leg].Event,
				Command: declared[p.Leg], Session: subject.Session, Turn: subject.Turn, ToolUse: subject.ToolUse, ToolName: subject.ToolName,
				Exit: observed.Exit, StdoutSHA256: StdoutDigest(observed.StdoutBytes()),
			})
		}
	}
	return out, nil
}

// stdinJSON is a hook payload as a step's stdin.
func stdinJSON(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}
