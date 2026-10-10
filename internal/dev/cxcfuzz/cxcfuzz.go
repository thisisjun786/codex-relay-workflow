//go:build dev

// Package cxcfuzz is the dev-only differential-fuzz harness. It feeds one generated input to a
// CXC v0.2.40 oracle worker and to the equivalent Go function, compares the two answers in one
// canonical form, shrinks a divergence to a minimal input, and pins it as a case the tests replay
// without Node. It is built only with -tags dev and never ships in a release archive.
package cxcfuzz

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/homeguard"
)

// DefaultOutRoot is where a campaign writes when --out is not given.
const DefaultOutRoot = "/scratch/crw/fuzz"

// DefaultWorkers and DefaultTimeout are the issue's defaults.
const (
	DefaultWorkers = 4
	DefaultTimeout = 5 * time.Second
	// DefaultStartupTimeout bounds how long a freshly started oracle worker may take to boot and
	// answer its handshake, separately from the per-case DefaultTimeout. Starting a worker costs an
	// interpreter boot and its top-level imports (measured 19 ms idle, 3 s under a 1% CPU quota),
	// which is a property of the worker program rather than of a case, so a slow runner cannot turn
	// it into a timeout case. It is paid once per worker start.
	DefaultStartupTimeout = 60 * time.Second
)

// Config is one campaign.
type Config struct {
	Target  Target
	Cases   int
	Seconds int
	Seed    int64
	// SeedSet says Seed was given, so a Seed of 0 is the seed the campaign runs, not a request for the clock.
	SeedSet bool
	Workers int
	Out     string
	Timeout time.Duration
	// StartupTimeout bounds one worker's start-up (see DefaultStartupTimeout). Zero takes the
	// default; the per-case Timeout is not affected.
	StartupTimeout time.Duration
	Env            []string
	Now            time.Time
	DevSHA         string
}

// Summary is <out>/summary.json: what one campaign did.
type Summary struct {
	Target   string  `json:"target"`
	Seed     int64   `json:"seed"`
	DevSHA   string  `json:"devSha"`
	Seconds  float64 `json:"seconds"`
	Cases    int     `json:"cases"`
	Same     int     `json:"same"`
	Miss     int     `json:"miss"`
	Extra    int     `json:"extra"`
	Differ   int     `json:"differ"`
	Timeouts int     `json:"timeouts"`
	// DeadWorkers, NoAnswers and Errors count the cases the harness could not answer for a reason other than a
	// timeout: a worker that ended before it replied, a reply that answers nothing, and a failure of the case's own
	// machinery (CRW-978 c7).
	DeadWorkers int `json:"deadWorkers"`
	NoAnswers   int `json:"noAnswers"`
	Errors      int `json:"errors"`
	// Refused counts the cases whose fs scenario the harness declined to build; it says nothing about the gates. The three
	// fields after it count the cases whose command the shared reader could not read, the ones the Go side refused and the ones it
	// did not; the run fails when the last is not zero.
	Refused              int `json:"refused"`
	UnreadableCases      int `json:"unreadableCases"`
	UnreadableRefused    int `json:"unreadableRefused"`
	UnreadableNotRefused int `json:"unreadableNotRefused"`
	// Failures names the case records written under the output directory, one per distinct failing input.
	Failures  []string `json:"failures,omitempty"`
	PerSecond float64  `json:"casesPerSecond"`
}

// Divergence is one shrunk difference, written to <out>/divergences/<kind>-<sha12>.json.
type Divergence struct {
	Kind    Kind    `json:"kind"`
	Input   string  `json:"input"`
	Go      string  `json:"go"`
	Oracle  string  `json:"oracle"`
	Verdict Verdict `json:"verdict"`
	Seed    int64   `json:"seed"`
	DevSHA  string  `json:"devSha"`
}

// DivergenceDir is the subdirectory of --out the divergence files go in.
const DivergenceDir = "divergences"

// ShrinkAttempts bounds one divergence's shrinking.
const ShrinkAttempts = 500

// divergenceName is <kind>-<the first 12 hex characters of the input hash>.json, so the same input
// is one file and a case is named by what it holds.
func divergenceName(kind Kind, input string) string {
	sum := sha256.Sum256([]byte(input))
	return fmt.Sprintf("%s-%s.json", kind, hex.EncodeToString(sum[:])[:12])
}

// Run is `crw-dev fuzz <target> [--seconds N] [--cases N] [--seed S] [--workers N] [--out DIR]`.
// It exits 2 when the target is unknown or its oracle command is missing, 1 when the campaign
// found a divergence, and 0 when every case agreed.
func Run(args []string, stdout, stderr io.Writer) int {
	const usage = "usage: crw-dev fuzz <target> [--seconds N] [--cases N] [--seed S] [--workers N] [--out DIR] [--adopt FILE --tag TAG --record TEXT --name NAME]"
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(stderr, usage)
		fmt.Fprintln(stderr, "crw-dev fuzz: error: the following arguments are required: target")
		return 2
	}
	targetName := args[0]
	set := flag.NewFlagSet("crw-dev fuzz", flag.ContinueOnError)
	set.SetOutput(stderr)
	seconds := set.Int("seconds", 0, "stop after this many seconds")
	cases := set.Int("cases", 0, "stop after this many cases")
	seed := set.Int64("seed", 0, "the generator seed (default: the clock, written to summary.json)")
	workers := set.Int("workers", DefaultWorkers, "long-lived oracle workers")
	out := set.String("out", "", "the output directory (default "+DefaultOutRoot+"/<target>/<UTC time>)")
	adopt := set.String("adopt", "", "a divergence file to pin as a case")
	tag := set.String("tag", "", "the adopted case's tag: identical, intentionally-changed or open")
	record := set.String("record", "", "the adopted case's known-defects or follow-up pointer")
	name := set.String("name", "", "the adopted case's name")
	if err := set.Parse(args[1:]); err != nil {
		return 2
	}
	if extra := set.Args(); len(extra) > 0 {
		fmt.Fprintf(stderr, "crw-dev fuzz: error: unrecognized arguments: %s\n", strings.Join(extra, " "))
		return 2
	}
	seedGiven := false
	set.Visit(func(f *flag.Flag) {
		if f.Name == "seed" {
			seedGiven = true
		}
	})
	target, ok := Lookup(targetName)
	if !ok {
		fmt.Fprintf(stderr, "crw-dev fuzz: error: unknown target %q (registered: %s)\n", targetName, strings.Join(Names(), ", "))
		return 2
	}
	if *adopt != "" {
		if err := Adopt(target, *adopt, *tag, *record, *name); err != nil {
			fmt.Fprintf(stderr, "crw-dev fuzz: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "crw-dev fuzz: pinned %s from %s\n", *name, *adopt)
		return 0
	}
	if *seconds <= 0 && *cases <= 0 {
		fmt.Fprintln(stderr, "crw-dev fuzz: error: one of --seconds or --cases is required")
		return 2
	}
	cfg := Config{Target: target, Cases: *cases, Seconds: *seconds, Seed: *seed, SeedSet: seedGiven, Workers: *workers, Timeout: DefaultTimeout, DevSHA: devSHA()}
	if *out != "" {
		cfg.Out = *out
	} else {
		cfg.Out = filepath.Join(DefaultOutRoot, target.Name, time.Now().UTC().Format("20060102T150405Z"))
	}
	if err := homeguard.Refuse(cfg.Out); err != nil {
		fmt.Fprintf(stderr, "crw-dev fuzz: the output directory: %v\n", err)
		return 1
	}
	// A campaign refuses a directory that already holds results: a second run there would overwrite
	// summary.json and leave the first run's divergence files beside it.
	if entries, err := os.ReadDir(cfg.Out); err == nil && len(entries) > 0 {
		fmt.Fprintf(stderr, "crw-dev fuzz: error: the output directory %s is not empty\n", cfg.Out)
		return 2
	}
	if err := os.MkdirAll(cfg.Out, 0o755); err != nil {
		fmt.Fprintf(stderr, "crw-dev fuzz: %v\n", err)
		return 1
	}
	summary, err := Campaign(cfg)
	if err != nil {
		var missing NoCommand
		if errors.As(err, &missing) {
			fmt.Fprintf(stderr, "crw-dev fuzz: %v\n", err)
			return 2
		}
		fmt.Fprintf(stderr, "crw-dev fuzz: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "crw-dev fuzz: %s seed %d %d cases in %.1fs: same %d differ %d miss %d extra %d timeout %d refused %d; unreadable %d refused %d not refused %d\n",
		summary.Target, summary.Seed, summary.Cases, summary.Seconds, summary.Same, summary.Differ, summary.Miss, summary.Extra, summary.Timeouts, summary.Refused,
		summary.UnreadableCases, summary.UnreadableRefused, summary.UnreadableNotRefused)
	if campaignFailed(summary) {
		return 1
	}
	return 0
}

// campaignFailed is whether a campaign's summary is a failure. A case that timed out, whose worker died or did not answer, whose own
// machinery failed (CRW-978 c7), or whose fs scenario was refused, compared nothing, so it is not an agreement either: a campaign
// that compared nothing does not report success. A case whose command the
// reader could not read and the Go side did not refuse is a failure of its own, whatever the oracle answered.
func campaignFailed(summary Summary) bool {
	return summary.Differ+summary.Miss+summary.Extra+summary.Timeouts+summary.DeadWorkers+summary.NoAnswers+summary.Errors+summary.Refused+summary.UnreadableNotRefused > 0
}

// Campaign runs one target: it generates inputs, evaluates each on both sides, shrinks every
// divergence, and writes the divergence files and summary.json under cfg.Out.
func Campaign(cfg Config) (summary Summary, err error) {
	if cfg.Out != "" {
		if err := homeguard.Refuse(cfg.Out); err != nil {
			return Summary{}, fmt.Errorf("the output directory: %w", err)
		}
	}
	seed := cfg.Seed
	if seed == 0 && !cfg.SeedSet {
		now := cfg.Now
		if now.IsZero() {
			now = time.Now()
		}
		seed = now.UnixNano()
	}
	env := cfg.Env
	if env == nil {
		env = os.Environ()
	}
	pool, err := NewPool(cfg.Target.Oracle, cfg.Workers, cfg.Timeout, cfg.StartupTimeout, env)
	if err != nil {
		return Summary{}, err
	}
	defer func() { err = joinCleanup(err, pool.Close()) }()
	run := &campaign{cfg: cfg, pool: pool, rng: rand.New(rand.NewSource(seed))}
	start := time.Now()
	deadline := time.Time{}
	if cfg.Seconds > 0 {
		deadline = start.Add(time.Duration(cfg.Seconds) * time.Second)
	}
	summary = Summary{Target: cfg.Target.Name, Seed: seed, DevSHA: cfg.DevSHA}
	written := map[string]bool{}
	failed := map[string]bool{}
	for n := 0; cfg.Cases <= 0 || n < cfg.Cases; n++ {
		if !deadline.IsZero() && time.Now().After(deadline) {
			break
		}
		summary.Cases++
		run.reading = readingResult{}
		verdict, goOut, oracleOut, input, err := run.one()
		reading := run.reading
		switch classifyCaseError(err) {
		case caseRemovalFailed:
			// A root that survived its removal is reported, never counted: the caller must see which root
			// was left and why, and the run must not continue as if the case had merely been refused.
			return summary, err
		case caseFailed:
			if err := recordFailure(cfg, &summary, seed, summary.Cases, input, err, failed); err != nil {
				return summary, err
			}
			continue
		case caseRefused:
			summary.Refused++
			continue
		}
		if reading.unreadable {
			summary.UnreadableCases++
			if reading.refused {
				summary.UnreadableRefused++
			} else {
				summary.UnreadableNotRefused++
			}
		}
		switch verdict.Kind {
		case Same:
			summary.Same++
			continue
		case Miss:
			summary.Miss++
		case Extra:
			summary.Extra++
		case Differ:
			summary.Differ++
		default:
			return summary, fmt.Errorf("the target's Compare answered an unknown verdict kind %q", verdict.Kind)
		}
		shrinker := &shrinkRun{campaign: run, verdict: verdict, goOut: goOut, oracleOut: oracleOut}
		shrunk, _ := Shrink(input, ShrinkAttempts, shrinker.keep)
		if shrinker.removal != nil {
			// A removal failure during shrinking is the same fact as one during the run: a root outlived
			// the work that owned it, so it is reported rather than folded into the kept candidate.
			return summary, shrinker.removal
		}
		if err := writeDivergence(cfg.Out, Divergence{
			Kind:    verdict.Kind,
			Input:   canonical(shrunk),
			Go:      shrinker.goOut,
			Oracle:  shrinker.oracleOut,
			Verdict: shrinker.verdict,
			Seed:    seed,
			DevSHA:  cfg.DevSHA,
		}, written); err != nil {
			return summary, err
		}
	}
	summary.Seconds = time.Since(start).Seconds()
	if summary.Seconds > 0 {
		summary.PerSecond = float64(summary.Cases) / summary.Seconds
	}
	if err := writeSummary(cfg.Out, summary); err != nil {
		return summary, err
	}
	return summary, nil
}

// errRefused is a case whose fs scenario the harness refused to build, so it was not run.
var errRefused = errors.New("the fs scenario was refused")

// caseOutcome is what one case's error means to a campaign.
type caseOutcome int

const (
	// caseOK is no error: the case ran and its verdict decides.
	caseOK caseOutcome = iota
	// caseRefused is a scenario the harness declined, which the run counts and carries on from.
	caseRefused
	// caseFailed is a case the harness could not answer: a timeout, a worker that died, a reply that answers
	// nothing, or a failure of the case's own machinery. The run records it under its cause and carries on.
	caseFailed
	// caseRemovalFailed is a root that outlived its run, which is reported and never counted.
	caseRemovalFailed
)

// classifyCaseError says what one case's error means. A removal failure is checked before the refusal,
// because a refused build whose root survived is answered as the RemovalError itself: reading it as a
// refused case would let the run carry on with a root still on the host.
func classifyCaseError(err error) caseOutcome {
	if err == nil {
		return caseOK
	}
	if errors.As(err, new(RemovalError)) {
		return caseRemovalFailed
	}
	if errors.Is(err, errRefused) {
		return caseRefused
	}
	return caseFailed
}

// refusedOutcome turns a scenario failure into the error the caller sees. A refusal is the harness
// declining the case; a RemovalError is not - the root survived, so it is reported as itself and never
// as a refused case a target may run on.
func refusedOutcome(err error) error {
	var removal RemovalError
	if errors.As(err, &removal) {
		return err
	}
	return errRefused
}

// joinCleanup folds a cleanup failure into the error a function is already returning. The first
// failure is kept, because it is the one that explains the run, and a cleanup failure is never
// discarded: it says a root outlived the run that owned it.
func joinCleanup(err, cleanup error) error {
	if cleanup == nil {
		return err
	}
	if err == nil {
		return cleanup
	}
	return errors.Join(err, cleanup)
}

type campaign struct {
	cfg  Config
	pool *Pool
	rng  *rand.Rand
	// reading is what the target's Reading said about the last case evaluate ran (the zero value when the target has none).
	reading readingResult
}

// readingResult is one case's reading: whether the shared reader could not read it, and whether the Go answer refused it.
type readingResult struct{ unreadable, refused bool }

// one generates one input and evaluates it on both sides, each in its own fresh root. The roots
// are removed before it returns, and each side's own root is replaced by ${ROOT} in the answers.
func (c *campaign) one() (Verdict, string, string, any, error) {
	input := c.cfg.Target.Generate(c.rng, c.rng.Int())
	verdict, goOut, oracleOut, err := c.evaluate(input)
	return verdict, goOut, oracleOut, input, err
}

func (c *campaign) evaluate(input any) (verdict Verdict, goText, oracleText string, err error) {
	text := canonical(input)
	goRoot, err := MkdirTempRoot("cxcfuzz-go-")
	if err != nil {
		return Verdict{}, "", "", CaseFailure{Cause: CauseTempRoot, Err: err}
	}
	defer func() { err = joinCleanup(err, CleanupCaseRoot(goRoot)) }()
	oracleRoot, err := MkdirTempRoot("cxcfuzz-oracle-")
	if err != nil {
		return Verdict{}, "", "", CaseFailure{Cause: CauseTempRoot, Err: err}
	}
	defer func() { err = joinCleanup(err, CleanupCaseRoot(oracleRoot)) }()
	for _, root := range []string{goRoot, oracleRoot} {
		if err := PrepareRoot(root); err != nil {
			return Verdict{}, "", "", CaseFailure{Cause: CauseTempRoot, Err: err}
		}
	}
	// A refused scenario is the harness declining the case, and a removal that still failed is its own
	// error: the caller must not read either as an agreement, and must not read the removal failure as
	// a refused case a target may run on.
	if _, err := Scenarios(goRoot, input); err != nil {
		return Verdict{}, "", "", refusedOutcome(err)
	}
	if _, err := Scenarios(oracleRoot, input); err != nil {
		return Verdict{}, "", "", refusedOutcome(err)
	}
	value, err := decode(text)
	if err != nil {
		return Verdict{}, "", "", CaseFailure{Cause: CauseDecode, Err: err}
	}
	goValue, goErr := c.cfg.Target.Go(value, RootEnv(goRoot))
	goOut := any(goValue)
	if goErr != nil {
		goOut = errorValue(goErr)
	}
	if c.cfg.Target.Reading != nil {
		c.reading.unreadable, c.reading.refused = c.cfg.Target.Reading(value, RootEnv(goRoot), goOut)
	}
	oracleAnswer, err := c.pool.Call(text, oracleRoot)
	if err != nil {
		return Verdict{}, "", "", err
	}
	oracleValue, err := decode(oracleAnswer)
	if err != nil {
		return Verdict{}, "", "", CaseFailure{Cause: CauseDecode, Err: err}
	}
	goStripped := stripRoot(goOut, goRoot)
	oracleStripped := stripRoot(oracleValue, oracleRoot)
	verdict = c.cfg.Target.Compare(goStripped, oracleStripped)
	return verdict, canonical(goStripped), canonical(oracleStripped), nil
}

// shrinkRun is the shrinker's keep test. A candidate is kept only while it still produces the
// same verdict kind and detail, so a divergence keeps its class and a read difference never shrinks into a
// write difference of the same kind (CRW-978 c1, review d1). The verdict and the two answers of the last candidate kept are what the
// divergence file ends up holding: the shrunk input, the answers and the verdict must describe
// one run, or a case adopted from the file could never replay and its detail could contradict it.
type shrinkRun struct {
	campaign  *campaign
	verdict   Verdict
	goOut     string
	oracleOut string
	// removal is the first removal failure a candidate hit. The shrinker asks keep many times, and a
	// root that survived is a fact about the host, not a property of the candidate, so it is reported
	// rather than folded into the keep decision.
	removal error
}

func (s *shrinkRun) keep(candidate any) bool {
	verdict, goOut, oracleOut, err := s.campaign.evaluate(candidate)
	var removal RemovalError
	if errors.As(err, &removal) && s.removal == nil {
		s.removal = err
	}
	if err != nil || verdict.Kind != s.verdict.Kind || verdict.Detail != s.verdict.Detail {
		return false
	}
	s.verdict, s.goOut, s.oracleOut = verdict, goOut, oracleOut
	return true
}

// writeDivergence writes one divergence file, once per input hash.
func writeDivergence(out string, d Divergence, written map[string]bool) error {
	name := divergenceName(d.Kind, d.Input)
	if written[name] {
		return nil
	}
	written[name] = true
	dir := filepath.Join(out, DivergenceDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), append(raw, '\n'), 0o644)
}

// writeSummary writes <out>/summary.json.
func writeSummary(out string, summary Summary) error {
	raw, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "summary.json"), append(raw, '\n'), 0o644)
}

// repositoryRoot is the checkout holding the working directory: the first parent with a go.mod.
func repositoryRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.mod above the working directory")
		}
		dir = parent
	}
}

// devSHA is the commit the campaign ran on, as summary.json records it.
func devSHA() string {
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}
