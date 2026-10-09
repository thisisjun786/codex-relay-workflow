//go:build dev

package laneparity

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contracttest"
	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
)

// Percentile is the nearest-rank percentile (0 < p <= 100) of the samples; zero for none.
func Percentile(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	s := slices.Clone(samples)
	slices.Sort(s)
	rank := int(math.Ceil(p / 100 * float64(len(s))))
	return s[min(max(rank, 1), len(s))-1]
}

// Latency is the latency cell of one leg.
type Latency struct {
	Leg     string `json:"leg"`
	Fixture string `json:"fixture"`
	Runs    int    `json:"runs"`
	// GoP50 and GoP95 are the declared Go command's; TSP50 and TSP95 the CXC v0.2.40 command's for
	// the same payload (zero, and Oracle false, for a registration CRW holds that CXC does not).
	GoP50     time.Duration `json:"goP50"`
	GoP95     time.Duration `json:"goP95"`
	TSP50     time.Duration `json:"tsP50"`
	TSP95     time.Duration `json:"tsP95"`
	Oracle    bool          `json:"oracle"`
	TimeoutMs int           `json:"timeoutMs"`
	OK        bool          `json:"ok"`
	Reason    string        `json:"reason,omitempty"`
}

// Judge decides a leg: the Go p95 must not exceed the TS p95 (when the leg has an oracle) and must
// be at most half the declared timeout, so a loaded host still answers inside it.
func Judge(leg, fixture string, timeoutSec int, goSamples, tsSamples []time.Duration) Latency {
	l := Latency{
		Leg: leg, Fixture: fixture, Runs: len(goSamples), Oracle: len(tsSamples) > 0, TimeoutMs: timeoutSec * 1000,
		GoP50: Percentile(goSamples, 50), GoP95: Percentile(goSamples, 95),
		TSP50: Percentile(tsSamples, 50), TSP95: Percentile(tsSamples, 95),
	}
	switch {
	case len(goSamples) == 0:
		l.Reason = "no Go sample"
	case l.Oracle && l.GoP95 > l.TSP95:
		l.Reason = fmt.Sprintf("Go p95 %s is above the TS p95 %s", l.GoP95, l.TSP95)
	case timeoutSec <= 0:
		l.Reason = "no declared timeout"
	case l.GoP95 > time.Duration(timeoutSec)*time.Second/2:
		l.Reason = fmt.Sprintf("Go p95 %s is above half of the %ds timeout", l.GoP95, timeoutSec)
	default:
		l.OK = true
	}
	return l
}

// LatencyOptions is one latency measurement.
type LatencyOptions struct {
	Root    string // the repository holding contract/
	CRW     string
	Plugin  string
	Scratch string
	Oracle  string // the extracted CXC v0.2.40 tree; empty measures the Go side only
	Node    string
	Runs    int
	Only    *regexp.Regexp // legs
}

// MeasureLatency times each leg's declared command and the oracle's command on the same payload,
// Runs fresh case roots each, and judges them. The leg's fixture is the first claimed one that
// expects an answer (else the first claimed one); a registration CRW holds beyond K1 is timed with
// its own probe and judged against its timeout alone.
func MeasureLatency(o LatencyOptions) ([]Latency, error) {
	if o.Runs < 1 {
		return nil, fmt.Errorf("runs must be at least 1")
	}
	want, err := ExpectedLegs(o.Root)
	if err != nil {
		return nil, err
	}
	_, registered, err := ReadRegistered(o.Plugin)
	if err != nil {
		return nil, err
	}
	declared := Declared(registered, want)
	scratch := o.Scratch
	if scratch == "" {
		if scratch, err = os.MkdirTemp("", "crw-parity-lat-"); err != nil {
			return nil, err
		}
		defer os.RemoveAll(scratch)
	}
	in := contracttest.HookFireInput{Root: o.Root, CRW: o.CRW, Plugin: o.Plugin, Declared: declared, Scratch: scratch}
	all, err := contracttest.FireHooks(in)
	if err != nil {
		return nil, err
	}
	pick := map[string]contracttest.HookFireResult{}
	for _, res := range all {
		if !res.Run || res.Leg == "" || len(res.Legs) == 0 || res.Steps[0].Leg != res.Leg {
			continue
		}
		prior, have := pick[res.Leg]
		if !have || (prior.Silent && !res.Silent) {
			pick[res.Leg] = res
		}
	}
	var rec *cxccorpus.Recorder
	if o.Oracle != "" {
		if rec, err = oracleRecorder(o, scratch); err != nil {
			return nil, err
		}
	}
	var out []Latency
	for _, l := range want {
		if o.Only != nil && !o.Only.MatchString(l.Leg) {
			continue
		}
		var goSamples, tsSamples []time.Duration
		fixture := "probe"
		if l.Own {
			if goSamples, err = timeProbe(in, l.Leg, o.Runs); err != nil {
				return nil, err
			}
		} else {
			res, ok := pick[l.Leg]
			if !ok {
				out = append(out, Latency{Leg: l.Leg, TimeoutMs: l.Timeout * 1000, Reason: "no claimed fixture to time"})
				continue
			}
			fixture = res.ID
			only := regexp.MustCompile("^" + regexp.QuoteMeta(res.ID) + "$")
			for i := 0; i < o.Runs; i++ {
				in := in
				in.Only = only
				rs, err := contracttest.FireHooks(in)
				if err != nil {
					return nil, err
				}
				if len(rs) != 1 || rs[0].Observed == nil || len(rs[0].Observed.Steps) == 0 {
					return nil, fmt.Errorf("%s: no timed step", res.ID)
				}
				goSamples = append(goSamples, rs[0].Observed.Steps[0].Elapsed)
			}
			if rec != nil {
				if tsSamples, err = timeOracle(rec, o.Root, res.ID, o.Runs); err != nil {
					return nil, err
				}
			}
		}
		out = append(out, Judge(l.Leg, fixture, l.Timeout, goSamples, tsSamples))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Leg < out[j].Leg })
	return out, nil
}

func timeProbe(in contracttest.HookFireInput, leg string, runs int) ([]time.Duration, error) {
	var probe *OwnProbe
	for _, p := range OwnProbes() {
		if p.Leg == leg && (probe == nil || !p.Silent) {
			p := p
			probe = &p
		}
	}
	if probe == nil {
		return nil, fmt.Errorf("no probe for %s", leg)
	}
	var out []time.Duration
	for i := 0; i < runs; i++ {
		rs, err := contracttest.FireProbes(in, []contracttest.Probe{{ID: probe.ID, Scenario: probe.Scenario}})
		if err != nil {
			return nil, err
		}
		if rs[0].Err != nil || len(rs[0].Observed.Steps) == 0 {
			return nil, fmt.Errorf("%s: %v", probe.ID, rs[0].Err)
		}
		out = append(out, rs[0].Observed.Steps[0].Elapsed)
	}
	return out, nil
}

func oracleRecorder(o LatencyOptions, scratch string) (*cxccorpus.Recorder, error) {
	node, git := o.Node, ""
	var err error
	if node == "" {
		if node, err = exec.LookPath("node"); err != nil {
			return nil, err
		}
	}
	if git, err = exec.LookPath("git"); err != nil {
		return nil, err
	}
	abs := func(p *string) error {
		var e error
		*p, e = filepath.Abs(*p)
		return e
	}
	oracle, scr := o.Oracle, scratch
	if err := errorsJoin(abs(&oracle), abs(&node), abs(&git), abs(&scr)); err != nil {
		return nil, err
	}
	rules, err := cxccorpus.LoadRules(o.Root)
	if err != nil {
		return nil, err
	}
	_, byLeg, err := cxccorpus.LoadDeclarations(o.Root)
	if err != nil {
		return nil, err
	}
	rec := &cxccorpus.Recorder{Oracle: oracle, Node: node, Git: git, Scratch: scr, Decls: byLeg, Rules: rules, Timeout: time.Minute}
	return rec, rec.Check()
}

func errorsJoin(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// timeOracle runs the fixture's original scenario against the CXC oracle runs times.
func timeOracle(rec *cxccorpus.Recorder, root, id string, runs int) ([]time.Duration, error) {
	fix, err := cxccorpus.LoadFixture(filepath.Join(root, cxccorpus.FixtureDir, id+".json"))
	if err != nil {
		return nil, err
	}
	s := cxccorpus.Scenario{ID: id, Covers: fix.Covers, Note: fix.Note, Given: fix.Given, Steps: fix.Run.Steps, Observe: fix.Run.Observe}
	var out []time.Duration
	for i := 0; i < runs; i++ {
		got, err := rec.Record(s)
		if err != nil {
			return nil, fmt.Errorf("oracle %s: %w", id, err)
		}
		if len(got.Expect.Steps) == 0 {
			return nil, fmt.Errorf("oracle %s: no step", id)
		}
		out = append(out, got.Expect.Steps[0].Elapsed)
	}
	return out, nil
}
