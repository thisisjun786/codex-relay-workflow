//go:build dev

package laneparity

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
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
	GoP50  time.Duration `json:"goP50"`
	GoP95  time.Duration `json:"goP95"`
	TSP50  time.Duration `json:"tsP50"`
	TSP95  time.Duration `json:"tsP95"`
	Oracle bool          `json:"oracle"`
	// TimeoutMs is the timeout the plugin root declares for the leg, the one the verdict holds it to.
	TimeoutMs int `json:"timeoutMs"`
	// Attempts is how many measurements it took (1 when the first passed).
	Attempts int  `json:"attempts"`
	OK       bool `json:"ok"`
	// Inconclusive is a leg that failed its comparison while the host's load average was above its
	// CPU count: the numbers are the host's as much as the hook's, so the cell neither passes nor
	// fails it (OK is true) and the report lists it as not verified. Load is the one-minute load
	// average when the leg was last measured.
	Inconclusive bool    `json:"inconclusive,omitempty"`
	Load         float64 `json:"load,omitempty"`
	// Broken is a leg whose command did not do its work while it was timed (it failed, was killed,
	// timed out, answered something else, or starts another build): its time is not a latency, and
	// host load does not excuse it.
	Broken bool `json:"broken,omitempty"`
	// Skipped is a leg no claimed fixture exercises yet (its port is pending): not timed, and not
	// a failure, but listed as unverified in the report.
	Skipped bool   `json:"skipped,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// Judge decides a leg: the Go p95 must not exceed the TS p95 (when the leg has an oracle) and must
// be at most half the timeout the root declares for it, so a loaded host still answers inside it.
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
	// Attempts is how many times a leg that fails is measured again (fresh samples each time) before
	// it is reported as failing: a shared host's load puts outliers into a p95 on either side, and a
	// leg that is really slower fails every attempt. Zero means one attempt.
	Attempts int
	// Strict makes a failing leg a failure whatever the host's load.
	Strict bool
	Only   *regexp.Regexp // legs
}

// hostLoad is the one-minute load average, where the host says it.
var hostLoad = func() (float64, bool) {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, false
	}
	first, _, _ := strings.Cut(string(raw), " ")
	v, err := strconv.ParseFloat(first, 64)
	return v, err == nil
}

// Settle turns a failing verdict into an inconclusive one when the host was overloaded (load
// average above cpus) and the run is not strict. A passing verdict is returned as it is.
func Settle(v Latency, load float64, loadKnown bool, cpus int, strict bool) Latency {
	if loadKnown {
		v.Load = load
	}
	if v.OK || v.Broken || strict || !loadKnown || load <= float64(cpus) {
		return v
	}
	v.Inconclusive, v.OK = true, true
	v.Reason = fmt.Sprintf("inconclusive, host load %.1f is above its %d CPUs: %s", load, cpus, v.Reason)
	return v
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
	entries := DeclaredRegistrations(registered, want)
	declared := Declared(registered, want)
	scratch := o.Scratch
	if scratch == "" {
		if scratch, err = os.MkdirTemp("", "crw-parity-lat-"); err != nil {
			return nil, err
		}
		defer os.RemoveAll(scratch)
	}
	// Every case's CODEX_HOME holds the hook switch at crw, so the ported legs do their work (CRW-392).
	in := contracttest.HookFireInput{Root: o.Root, CRW: o.CRW, Plugin: o.Plugin, Declared: declared, Scratch: scratch, Light: true, Seed: seedSwitch}
	// The fixtures that pick a leg's timed payload are those named for the legs timed (hook__<leg>__).
	choose := in
	if o.Only != nil {
		var names []string
		for _, l := range want {
			if !l.Own && o.Only.MatchString(l.Leg) {
				names = append(names, regexp.QuoteMeta(l.Leg))
			}
		}
		choose.Only = regexp.MustCompile(`^hook__(?:` + strings.Join(names, "|") + `)__`)
	}
	all, err := contracttest.FireHooks(choose)
	if err != nil {
		return nil, err
	}
	pick := map[string]contracttest.HookFireResult{}
	unfit := map[string]string{} // leg -> why its fixtures cannot be timed: the command did not do its work
	for _, res := range all {
		if !res.Run || res.Scripted || len(res.Legs) == 0 || res.Steps[0].Leg != res.Leg {
			continue
		}
		if err := timedStepProblem(res); err != "" {
			if unfit[res.Leg] == "" {
				unfit[res.Leg] = res.ID + ": " + err
			}
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
		// The leg is held to the timeout the root declares for it (zero when it declares none): a
		// registration may pass its own cell with another timeout than the table's (CRW's own legs need
		// only a positive one), and the host gives the command what the root declares.
		timeout := entries[l.Leg].Timeout
		var verdict Latency
		fixture := "probe"
		var res contracttest.HookFireResult
		var scenario cxccorpus.Scenario
		var wantExit int
		if !l.Own {
			var ok bool
			if res, ok = pick[l.Leg]; !ok && unfit[l.Leg] != "" {
				out = append(out, Latency{Leg: l.Leg, TimeoutMs: timeout * 1000, Broken: true, Reason: "the declared command does not do its work: " + unfit[l.Leg]})
				continue
			} else if !ok {
				out = append(out, Latency{Leg: l.Leg, TimeoutMs: timeout * 1000, OK: true, Skipped: true, Reason: "no claimed fixture to time (its port is pending)"})
				continue
			}
			fixture = res.ID
			if rec != nil {
				if scenario, wantExit, err = oracleScenario(o.Root, res.ID); err != nil {
					return nil, err
				}
			}
		}
		if problem := timedBuildProblem(declared[l.Leg], o.CRW); problem != "" {
			out = append(out, Latency{Leg: l.Leg, Fixture: fixture, TimeoutMs: timeout * 1000, Broken: true, Attempts: 1, Reason: problem})
			continue
		}
		for attempt := 1; attempt <= max(o.Attempts, 1); attempt++ {
			var goSamples, tsSamples []time.Duration
			var broken string
			if l.Own {
				if goSamples, broken, err = timeProbe(in, l.Leg, o.Runs); err != nil {
					return nil, err
				}
			} else {
				only := regexp.MustCompile("^" + regexp.QuoteMeta(res.ID) + "$")
				// The two sides alternate, so a load that comes and goes on a shared host weighs on both.
				for i := 0; i < o.Runs && broken == ""; i++ {
					in := in
					in.Only = only
					rs, err := contracttest.FireHooks(in)
					if err != nil {
						return nil, err
					}
					if len(rs) != 1 {
						return nil, fmt.Errorf("%s: %d fixtures fired, expected 1", res.ID, len(rs))
					}
					if broken = timedStepProblem(rs[0]); broken != "" {
						break
					}
					goSamples = append(goSamples, rs[0].Observed.Steps[0].Elapsed)
					if rec != nil {
						elapsed, err := timeOracle(rec, scenario, wantExit)
						if err != nil {
							if errors.Is(err, errOracleStep) {
								broken = err.Error()
								break
							}
							return nil, err
						}
						tsSamples = append(tsSamples, elapsed)
					}
				}
			}
			if broken != "" {
				verdict = Latency{Leg: l.Leg, Fixture: fixture, Runs: len(goSamples), TimeoutMs: timeout * 1000, Broken: true, Attempts: attempt,
					Reason: "the command did not do its work while it was timed: " + broken}
				break
			}
			verdict = Judge(l.Leg, fixture, timeout, goSamples, tsSamples)
			verdict.Attempts = attempt
			if verdict.OK {
				break
			}
		}
		load, known := hostLoad()
		out = append(out, Settle(verdict, load, known, runtime.NumCPU(), o.Strict))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Leg < out[j].Leg })
	return out, nil
}

// timeProbe times a registration of CRW's own with its probe; broken is non-empty when a run of the
// probe did not do its work (the probe's check failed).
func timeProbe(in contracttest.HookFireInput, leg string, runs int) (samples []time.Duration, broken string, err error) {
	var probe *OwnProbe
	for _, p := range OwnProbes() {
		if p.Leg == leg && (probe == nil || !p.Silent) {
			p := p
			probe = &p
		}
	}
	if probe == nil {
		return nil, "", fmt.Errorf("no probe for %s", leg)
	}
	for i := 0; i < runs; i++ {
		rs, err := contracttest.FireProbes(in, []contracttest.Probe{{ID: probe.ID, Scenario: probe.Scenario, Check: probe.Check}})
		if err != nil {
			return nil, "", err
		}
		if rs[0].Err != nil {
			return samples, probe.ID + ": " + rs[0].Err.Error(), nil
		}
		if len(rs[0].Observed.Steps) == 0 {
			return samples, probe.ID + ": no step ran", nil
		}
		if problem := stepProblem(rs[0].Observed.Steps[0]); problem != "" {
			return samples, probe.ID + ": " + problem, nil
		}
		samples = append(samples, rs[0].Observed.Steps[0].Elapsed)
	}
	return samples, "", nil
}

// stepProblem is why a step's time is not a latency: it was killed, timed out or never ran.
func stepProblem(s cxccorpus.StepResult) string {
	switch {
	case s.Timeout:
		return "the command timed out"
	case s.Signal != "":
		return "the command was killed by signal " + s.Signal
	}
	return ""
}

// timedStepProblem is why a fired fixture's first step is not a measurement of the hook working: it
// differs from the fixture's expectation (a failing exit, another answer, a missing command), did
// not run, was killed or timed out. Empty when the time is a latency.
func timedStepProblem(res contracttest.HookFireResult) string {
	switch {
	case res.Err != nil:
		return firstLines(res.Err.Error(), 3)
	case res.Observed == nil || len(res.Observed.Steps) == 0:
		return "no step ran"
	}
	return stepProblem(res.Observed.Steps[0])
}

// timedBuildProblem is why the declared command is not the build under test: the latency of another
// executable is not the latency of crw.
func timedBuildProblem(command, crw string) string {
	started, problem := CommandBuild(command, crw)
	if problem != "" {
		return problem
	}
	want, err := FileDigest(crw)
	if err != nil {
		return err.Error()
	}
	if started != want {
		return fmt.Sprintf("the declared command %q starts another executable than the crw build under test (%s, not %s)", command, short(started), short(want))
	}
	return ""
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
	rec := &cxccorpus.Recorder{Oracle: oracle, Node: node, Git: git, Scratch: scr, Decls: byLeg, Rules: rules, Timeout: time.Minute, Light: true}
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

// oracleScenario is a fixture's original scenario, in the oracle's own names, and the exit status
// its first step recorded.
func oracleScenario(root, id string) (cxccorpus.Scenario, int, error) {
	fix, err := cxccorpus.LoadFixture(filepath.Join(root, cxccorpus.FixtureDir, id+".json"))
	if err != nil {
		return cxccorpus.Scenario{}, 0, err
	}
	exit := 0
	if len(fix.Expect.Steps) > 0 {
		exit = fix.Expect.Steps[0].Exit
	}
	return cxccorpus.Scenario{ID: id, Covers: fix.Covers, Note: fix.Note, Given: fix.Given, Steps: fix.Run.Steps, Observe: fix.Run.Observe}, exit, nil
}

// errOracleStep marks an oracle run whose command did not do what its recording says.
var errOracleStep = errors.New("the oracle command did not run as recorded")

// timeOracle runs the scenario once against the CXC oracle and returns the first step's time. The
// step must have exited as recorded (wantExit), not been killed and not timed out: a command that
// failed at once is quicker than one that works.
func timeOracle(rec *cxccorpus.Recorder, s cxccorpus.Scenario, wantExit int) (time.Duration, error) {
	got, err := rec.Record(s)
	if err != nil {
		return 0, fmt.Errorf("oracle %s: %w", s.ID, err)
	}
	if len(got.Expect.Steps) == 0 {
		return 0, fmt.Errorf("oracle %s: no step", s.ID)
	}
	step := got.Expect.Steps[0]
	if problem := stepProblem(step); problem != "" {
		return 0, fmt.Errorf("%w: %s: %s", errOracleStep, s.ID, problem)
	}
	if step.Exit != wantExit {
		return 0, fmt.Errorf("%w: %s exited %d, recorded %d", errOracleStep, s.ID, step.Exit, wantExit)
	}
	return step.Elapsed, nil
}
