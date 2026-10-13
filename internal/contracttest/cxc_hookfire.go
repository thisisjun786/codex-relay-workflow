package contracttest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// RecDirEnv names the records directory of a replay case. A process that has it set and is not the
// harness itself is one of the case's stub programs or its git wrapper: the program hands itself to
// RunStubHelper before anything else (cmd/crw-dev does, as TestMain does for the tests).
const RecDirEnv = cxcRecDir

// RunStubHelper plays a stub program or the git wrapper of a replay case and returns its exit status.
func RunStubHelper(dir string) int { return cxcHelper(dir) }

// HookFireInput is one firing of the corpus's hook fixtures through the commands a plugin root
// declares (crw-dev parity, CRW-203). Everything else is the replay of TestDomain/cxc: the same
// fixtures, name substitution, normalisation, status files and comparison, so a fixture that passes
// here is the fixture the replay passes, reached the way the host reaches it.
type HookFireInput struct {
	Root     string            // the repository holding contract/
	CRW      string            // the crw build the declared commands start
	Plugin   string            // the plugin root: PLUGIN_ROOT of every step
	Declared map[string]string // leg to the command line the plugin root declares for it
	Scratch  string            // parent of the case roots of fixtures that do not depend on path length
	Only     *regexp.Regexp    // fixture ids to fire (nil: every hook fixture)
	// Light readies the cases for timing: the real git and silent stubs instead of helper processes
	// that log calls. The outcome is not compared with the same strictness (no call is logged).
	Light bool
	// Mutate may change a fixture's observed outcome before it is compared, and Steps the steps a
	// fixture runs (a fault injected between the process and the comparison, or into the delivery).
	Mutate func(id string, got *cxccorpus.Expect)
	Steps  func(id string, steps []cxccorpus.Step) []cxccorpus.Step
	// Seed puts the harness's own files into every case root it fires in (cxccorpus.RunOptions.Seed),
	// and takes them out before the tree is observed.
	Seed func(c *cxccorpus.Case, env []string) (undo func() error, err error)
}

// FiredStep is one step of a fixture as delivered: the leg it fires and the payload it sends.
type FiredStep struct {
	Leg     string // empty for a step that is not a hook
	Payload json.RawMessage
}

// HookFireResult is the outcome of one hook fixture.
type HookFireResult struct {
	ID    string
	Leg   string   // the leg the fixture is named for (hook__<leg>__<case>)
	Legs  []string // the legs its steps fire, in order
	State string   // identical, intentionally-changed, pending or "" (no status file claims it)
	Run   bool     // the declared command ran and the outcome was compared
	// Err is why the fixture does not match, or could not run; nil when it matches.
	Err error
	// Observed is the normalised outcome (nil when the fixture did not run); Expected is the
	// recorded expectation in crw's names, before any claim's patch.
	Observed *cxccorpus.Expect
	Expected cxccorpus.Expect
	Steps    []FiredStep
	// Silent is whether the fixture expects no output and exit 0 on every step: a command that does
	// nothing passes it too, so it is not evidence that the handler ran.
	Silent bool
	// Scripted is whether the fixture scripts the answers of stub programs: a light (timing) case
	// answers none of them, so its timing is not that of the recorded run.
	Scripted bool
}

// FireHooks fires every claimed hook fixture of the corpus through in.Declared and compares each
// outcome with the fixture's expectation. A pending fixture is reported, not run.
func FireHooks(in HookFireInput) ([]HookFireResult, error) {
	r, err := newCXCReplayer(in.Root, in.CRW)
	if err != nil {
		return nil, err
	}
	r.plugin, r.declared, r.mutate, r.light, r.seed = in.Plugin, in.Declared, in.Mutate, in.Light, in.Seed
	removeConfig, err := r.writeSandboxConfig(in.Scratch)
	if err != nil {
		return nil, err
	}
	defer removeConfig()
	ids, fixtures, err := loadCXCFixtures(in.Root)
	if err != nil {
		return nil, err
	}
	claims, err := loadCXCNotes(in.Root+"/contract/notes/cxc", ids)
	if err != nil {
		return nil, err
	}
	syscall.Umask(0o022) // the modes of the recording; the declared command carries no umask of its own
	observed := map[string]cxccorpus.Expect{}
	r.observe = func(id string, got cxccorpus.Expect) { observed[id] = got }
	var results []HookFireResult
	for _, id := range ids {
		if !strings.HasPrefix(id, "hook__") || (in.Only != nil && !in.Only.MatchString(id)) {
			continue
		}
		fix, claim := fixtures[id], claims[id]
		res := HookFireResult{ID: id, State: claim.State, Silent: true, Scripted: len(fix.Given.Stubs) > 0}
		for _, step := range fix.Run.Steps {
			res.Steps = append(res.Steps, FiredStep{Leg: step.Hook, Payload: step.Stdin})
			if step.Hook != "" {
				res.Legs = append(res.Legs, step.Hook)
			}
		}
		if expected, mapErr := mapStrings(fix.Expect, r.rename); mapErr == nil {
			res.Expected = expected
		}
		if in.Steps != nil {
			fix.Run.Steps = in.Steps(id, append([]cxccorpus.Step(nil), fix.Run.Steps...))
		}
		res.Leg = strings.SplitN(strings.TrimPrefix(id, "hook__"), "__", 2)[0] // the leg the fixture is about
		for _, step := range fix.Expect.Steps {
			res.Silent = res.Silent && step.Exit == 0 && step.StdoutForm == "empty"
		}
		switch claim.State {
		case "":
			res.Err = fmt.Errorf("%s is registered in no status file under contract/notes/cxc", id)
		case cxcPending:
		default:
			res.Run = true
			var made []string
			tmp := func() string {
				dir, mkErr := os.MkdirTemp(in.Scratch, "fire-")
				if mkErr != nil {
					panic(mkErr)
				}
				made = append(made, dir)
				return dir
			}
			res.Err = r.check(id, fix, claim, tmp)
			for _, dir := range made {
				_ = os.RemoveAll(dir)
			}
			if got, ok := observed[id]; ok {
				res.Observed = &got
				delete(observed, id)
			}
		}
		results = append(results, res)
	}
	return results, nil
}

// writeSandboxConfig writes the crw configuration every case of a firing reads as CRW_CONFIG (CRW-1265): its
// sandbox_root is the scratch directory the case roots lie in (TMPDIR when scratch is ""), and the system
// temporary locations the long cases use are its project roots (testsupport.SandboxConfigDocument). A firing
// runs in crw-dev, which isolates nothing, as well as in a test, so it does not rely on the configuration of
// the process: without it a case read the rule's default sandbox base, and a scratch directory outside that
// base was judged outside the rule in the managed-dispatch fixtures. The file lies in a directory of its own in scratch, outside every
// case root; the returned function removes it.
func (r *cxcReplayer) writeSandboxConfig(scratch string) (remove func(), err error) {
	base := scratch
	if base == "" {
		base = os.TempDir()
	}
	dir, err := os.MkdirTemp(base, "crw-config-")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "config.json")
	if err := testsupport.WriteSandboxConfig(path, base); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("the crw configuration of the firing: %w", err)
	}
	r.sandboxConfig = path
	return func() { _ = os.RemoveAll(dir) }, nil
}

// Probe is a scenario of CRW's own, for a registration the corpus has no fixture for. It is written
// in crw's names, so it goes through no name substitution, and is judged by Check, not by a
// recorded expectation.
type Probe struct {
	ID       string
	Scenario cxccorpus.Scenario
	Check    func(cxccorpus.Expect) error
}

// ProbeResult is the outcome of one probe.
type ProbeResult struct {
	ID       string
	Err      error
	Observed cxccorpus.Expect
}

// FireProbes runs each probe once through in.Declared in a fresh case root.
func FireProbes(in HookFireInput, probes []Probe) ([]ProbeResult, error) {
	r, err := newCXCReplayer(in.Root, in.CRW)
	if err != nil {
		return nil, err
	}
	r.plugin, r.declared, r.light, r.seed = in.Plugin, in.Declared, in.Light, in.Seed
	removeConfig, err := r.writeSandboxConfig(in.Scratch)
	if err != nil {
		return nil, err
	}
	defer removeConfig()
	syscall.Umask(0o022)
	var out []ProbeResult
	for _, p := range probes {
		s := p.Scenario
		s.ID = p.ID
		got, err := cxccorpus.RunScenario(r, cxccorpus.RunOptions{Scratch: in.Scratch, HomeVar: "CRW_HOME", Rules: r.rules, Seed: r.seed}, s)
		res := ProbeResult{ID: p.ID, Observed: got}
		if err != nil {
			res.Err = err
		} else {
			if in.Mutate != nil {
				in.Mutate(p.ID, &got)
			}
			if p.Check != nil {
				res.Err = p.Check(got)
			}
		}
		out = append(out, res)
	}
	return out, nil
}
