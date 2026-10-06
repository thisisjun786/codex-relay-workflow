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
)

// DefaultOutRoot is where a campaign writes when --out is not given.
const DefaultOutRoot = "/scratch/crw/fuzz"

// DefaultWorkers and DefaultTimeout are the issue's defaults.
const (
	DefaultWorkers = 4
	DefaultTimeout = 5 * time.Second
)

// Config is one campaign.
type Config struct {
	Target  Target
	Cases   int
	Seconds int
	Seed    int64
	Workers int
	Out     string
	Timeout time.Duration
	Env     []string
	Now     time.Time
	DevSHA  string
}

// Summary is <out>/summary.json: what one campaign did.
type Summary struct {
	Target    string  `json:"target"`
	Seed      int64   `json:"seed"`
	DevSHA    string  `json:"devSha"`
	Seconds   float64 `json:"seconds"`
	Cases     int     `json:"cases"`
	Same      int     `json:"same"`
	Miss      int     `json:"miss"`
	Extra     int     `json:"extra"`
	Differ    int     `json:"differ"`
	Timeouts  int     `json:"timeouts"`
	PerSecond float64 `json:"casesPerSecond"`
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
	const usage = "usage: crw-dev fuzz <target> [--seconds N] [--cases N] [--seed S] [--workers N] [--out DIR]"
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
	if err := set.Parse(args[1:]); err != nil {
		return 2
	}
	if extra := set.Args(); len(extra) > 0 {
		fmt.Fprintf(stderr, "crw-dev fuzz: error: unrecognized arguments: %s\n", strings.Join(extra, " "))
		return 2
	}
	target, ok := Lookup(targetName)
	if !ok {
		fmt.Fprintf(stderr, "crw-dev fuzz: error: unknown target %q (registered: %s)\n", targetName, strings.Join(Names(), ", "))
		return 2
	}
	if *seconds <= 0 && *cases <= 0 {
		fmt.Fprintln(stderr, "crw-dev fuzz: error: one of --seconds or --cases is required")
		return 2
	}
	cfg := Config{Target: target, Cases: *cases, Seconds: *seconds, Seed: *seed, Workers: *workers, Timeout: DefaultTimeout, DevSHA: devSHA()}
	if *out != "" {
		cfg.Out = *out
	} else {
		cfg.Out = filepath.Join(DefaultOutRoot, target.Name, time.Now().UTC().Format("20060102T150405Z"))
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
	fmt.Fprintf(stdout, "crw-dev fuzz: %s seed %d %d cases in %.1fs: same %d differ %d miss %d extra %d timeout %d\n",
		summary.Target, summary.Seed, summary.Cases, summary.Seconds, summary.Same, summary.Differ, summary.Miss, summary.Extra, summary.Timeouts)
	if summary.Differ+summary.Miss+summary.Extra > 0 {
		return 1
	}
	return 0
}

// Campaign runs one target: it generates inputs, evaluates each on both sides, shrinks every
// divergence, and writes the divergence files and summary.json under cfg.Out.
func Campaign(cfg Config) (Summary, error) {
	seed := cfg.Seed
	if seed == 0 {
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
	pool, err := NewPool(cfg.Target.Oracle, cfg.Workers, cfg.Timeout, env)
	if err != nil {
		return Summary{}, err
	}
	defer func() { _ = pool.Close() }()
	run := &campaign{cfg: cfg, pool: pool, rng: rand.New(rand.NewSource(seed))}
	start := time.Now()
	deadline := time.Time{}
	if cfg.Seconds > 0 {
		deadline = start.Add(time.Duration(cfg.Seconds) * time.Second)
	}
	summary := Summary{Target: cfg.Target.Name, Seed: seed, DevSHA: cfg.DevSHA}
	written := map[string]bool{}
	for n := 0; cfg.Cases <= 0 || n < cfg.Cases; n++ {
		if !deadline.IsZero() && time.Now().After(deadline) {
			break
		}
		summary.Cases++
		verdict, goOut, oracleOut, input, err := run.one()
		switch {
		case errors.Is(err, Timeout{}):
			summary.Timeouts++
			continue
		case err != nil:
			return summary, err
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
		}
		if err := writeDivergence(cfg.Out, Divergence{
			Kind:    verdict.Kind,
			Input:   canonical(input),
			Go:      goOut,
			Oracle:  oracleOut,
			Verdict: verdict,
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

type campaign struct {
	cfg  Config
	pool *Pool
	rng  *rand.Rand
}

// one generates one input and evaluates it on both sides, each in its own fresh root. The roots
// are removed before it returns, and each side's own root is replaced by ${ROOT} in the answers.
func (c *campaign) one() (Verdict, string, string, any, error) {
	input := c.cfg.Target.Generate(c.rng, c.rng.Int())
	verdict, goOut, oracleOut, err := c.evaluate(input)
	return verdict, goOut, oracleOut, input, err
}

func (c *campaign) evaluate(input any) (Verdict, string, string, error) {
	text := canonical(input)
	goRoot, err := os.MkdirTemp("", "cxcfuzz-go-")
	if err != nil {
		return Verdict{}, "", "", err
	}
	defer func() { _ = os.RemoveAll(goRoot) }()
	oracleRoot, err := os.MkdirTemp("", "cxcfuzz-oracle-")
	if err != nil {
		return Verdict{}, "", "", err
	}
	defer func() { _ = os.RemoveAll(oracleRoot) }()
	value, err := decode(text)
	if err != nil {
		return Verdict{}, "", "", err
	}
	goValue, goErr := c.cfg.Target.Go(value, RootEnv(goRoot))
	goOut := any(goValue)
	if goErr != nil {
		goOut = errorValue(goErr)
	}
	oracleText, err := c.pool.Call(text, oracleRoot)
	if err != nil {
		return Verdict{}, "", "", err
	}
	oracleValue, err := decode(oracleText)
	if err != nil {
		return Verdict{}, "", "", err
	}
	goStripped := stripRoot(goOut, goRoot)
	oracleStripped := stripRoot(oracleValue, oracleRoot)
	verdict := c.cfg.Target.Compare(goStripped, oracleStripped)
	return verdict, canonical(goStripped), canonical(oracleStripped), nil
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
