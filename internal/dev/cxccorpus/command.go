//go:build dev

package cxccorpus

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

const usage = "usage: crw-dev cxc {declarations,constants,record,check,lint} ..."

// Run is `crw-dev cxc`.
//
//	declarations --oracle DIR        regenerate hook-declarations.json from the oracle
//	constants --oracle DIR           regenerate injected-text.json (K5) from the oracle
//	record --oracle DIR [--only RE]  record each spec twice, require the two normalised
//	                                 recordings to be identical, write the fixtures and sync
//	                                 the coverage index
//	check --oracle DIR [--only RE]   record again and compare with the committed fixtures
//	lint                             check the corpus without an oracle (CI)
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		fmt.Fprintln(stderr, "crw-dev cxc: error: the following arguments are required: command")
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, usage)
		return 0
	case "lint":
		return runLint(args[1:], stdout, stderr)
	case "declarations", "constants", "record", "check":
		return runOracle(args[0], args[1:], stdout, stderr)
	}
	fmt.Fprintln(stderr, usage)
	fmt.Fprintf(stderr, "crw-dev cxc: error: invalid command %q\n", args[0])
	return 2
}

// RepositoryRoot is the checkout holding the corpus: the working directory's git top level.
func RepositoryRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("not inside the repository: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func runLint(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("crw-dev cxc lint", flag.ContinueOnError)
	set.SetOutput(stderr)
	rootFlag := set.String("root", "", "repository root (default: the git top level)")
	if err := set.Parse(args); err != nil {
		return 2
	}
	root := *rootFlag
	if root == "" {
		var err error
		if root, err = RepositoryRoot(); err != nil {
			fmt.Fprintln(stderr, "crw-dev cxc lint:", err)
			return 1
		}
	}
	return LintReport(root, stdout, stderr)
}

// LintReport prints the lint result and returns its exit status.
func LintReport(root string, stdout, stderr io.Writer) int {
	problems := Lint(root)
	for _, p := range problems {
		fmt.Fprintln(stderr, "FAIL cxc corpus: "+p)
	}
	if len(problems) > 0 {
		fmt.Fprintf(stderr, "%d problem(s) in the CXC corpus (%s, %s).\n", len(problems), FixtureDir, SchemaDir)
		return 1
	}
	specs, _ := LoadSpecs(root)
	cov, _ := LoadCoverage(root)
	count := map[string]int{}
	for _, item := range cov.Items {
		count[item.Status]++
	}
	scenarios := 0
	for _, s := range specs {
		scenarios += len(s.Scenarios)
	}
	fmt.Fprintf(stdout, "cxc corpus: %d fixtures recorded from CXC %s agree with their specs; coverage %d recorded, %d extracted, %d pending, %d not recordable\n",
		scenarios, OracleTag, count["recorded"], count["extracted"], count["pending"], count["not-recordable"])
	return 0
}

func runOracle(command string, args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("crw-dev cxc "+command, flag.ContinueOnError)
	set.SetOutput(stderr)
	oracle := set.String("oracle", "", "the extracted CXC v0.2.40 tree (git archive v0.2.40)")
	node := set.String("node", "", "node executable (default: node on PATH)")
	git := set.String("git", "", "git executable (default: git on PATH)")
	scratch := set.String("scratch", "/var/tmp", "parent directory of every case root")
	only := set.String("only", "", "record only scenarios whose id matches this regular expression")
	keep := set.Bool("keep", false, "keep case roots for inspection")
	once := set.Bool("once", false, "record each scenario once (skips the determinism proof)")
	noSync := set.Bool("no-sync", false, "leave coverage.json alone (parallel recordings of disjoint scenario sets)")
	timeout := set.Duration("timeout", 60*time.Second, "per-step deadline")
	if err := set.Parse(args); err != nil {
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "crw-dev cxc %s: %v\n", command, err)
		return 1
	}
	if *oracle == "" {
		return fail(errors.New("--oracle is required"))
	}
	root, err := RepositoryRoot()
	if err != nil {
		return fail(err)
	}
	if command == "declarations" {
		decls, err := ExtractDeclarations(*oracle)
		if err != nil {
			return fail(err)
		}
		raw, err := Marshal(decls)
		if err != nil {
			return fail(err)
		}
		if err := os.WriteFile(filepath.Join(root, Declarations), raw, 0o644); err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "wrote %s: %d legs\n", Declarations, len(decls.Legs))
		return 0
	}
	r, err := newRecorder(root, *oracle, *node, *git, *scratch)
	if err != nil {
		return fail(err)
	}
	if command == "constants" {
		file, err := r.ExtractConstants()
		if err != nil {
			return fail(err)
		}
		raw, err := Marshal(file)
		if err != nil {
			return fail(err)
		}
		if err := os.WriteFile(filepath.Join(root, Constants), raw, 0o644); err != nil {
			return fail(err)
		}
		count := 0
		for _, m := range file.Modules {
			count += len(m)
		}
		fmt.Fprintf(stdout, "wrote %s: %d constants from %d modules\n", Constants, count, len(file.Modules))
		return 0
	}
	r.Keep, r.Timeout = *keep, *timeout
	var filter *regexp.Regexp
	if *only != "" {
		if filter, err = regexp.Compile(*only); err != nil {
			return fail(err)
		}
	}
	specs, err := LoadSpecs(root)
	if err != nil {
		return fail(err)
	}
	syscall.Umask(0o022)
	var failures []string
	recorded := 0
	for _, spec := range specs {
		for _, s := range spec.Scenarios {
			if filter != nil && !filter.MatchString(s.ID) {
				continue
			}
			path := filepath.Join(root, FixtureDir, s.ID+".json")
			first, err := r.Record(s)
			if err != nil {
				failures = append(failures, err.Error())
				continue
			}
			raw, err := Marshal(first)
			if err != nil {
				return fail(err)
			}
			if command == "record" && !*once {
				second, err := r.Record(s)
				if err != nil {
					failures = append(failures, err.Error())
					continue
				}
				again, _ := Marshal(second)
				if !bytes.Equal(raw, again) {
					failures = append(failures, fmt.Sprintf("%s: two recordings differ after normalisation:\n%s", s.ID, firstDiff(raw, again)))
					continue
				}
			}
			switch command {
			case "record":
				if err := os.WriteFile(path, raw, 0o644); err != nil {
					return fail(err)
				}
			case "check":
				committed, err := os.ReadFile(path)
				if err != nil {
					failures = append(failures, fmt.Sprintf("%s: %v", s.ID, err))
				} else if !bytes.Equal(committed, raw) {
					failures = append(failures, fmt.Sprintf("%s: the oracle now answers differently from the fixture:\n%s", s.ID, firstDiff(committed, raw)))
				}
			}
			recorded++
		}
	}
	if command == "record" && filter == nil {
		if err := pruneFixtures(root, specs, stdout); err != nil {
			return fail(err)
		}
	}
	if command == "record" && !*noSync {
		if err := syncCoverageFile(root); err != nil {
			return fail(err)
		}
	}
	sort.Strings(failures)
	for _, f := range failures {
		fmt.Fprintln(stderr, "FAIL "+f)
	}
	verb := map[string]string{"record": "recorded twice and identical", "check": "re-recorded and identical to the fixtures"}[command]
	if *once && command == "record" {
		verb = "recorded once"
	}
	fmt.Fprintf(stdout, "%d scenario(s) %s; %d failure(s)\n", recorded, verb, len(failures))
	if len(failures) > 0 {
		return 1
	}
	return 0
}

func newRecorder(root, oracle, node, git, scratch string) (*Recorder, error) {
	var err error
	if node == "" {
		if node, err = exec.LookPath("node"); err != nil {
			return nil, err
		}
	}
	if git == "" {
		if git, err = exec.LookPath("git"); err != nil {
			return nil, err
		}
	}
	for _, p := range []*string{&oracle, &node, &git, &scratch} {
		if *p, err = filepath.Abs(*p); err != nil {
			return nil, err
		}
	}
	rules, err := LoadRules(root)
	if err != nil {
		return nil, err
	}
	committed, byLeg, err := LoadDeclarations(root)
	if err != nil {
		return nil, err
	}
	fresh, err := ExtractDeclarations(oracle)
	if err != nil {
		return nil, err
	}
	if !sameJSON(committed, fresh) {
		return nil, fmt.Errorf("%s differs from the oracle's hook files: run `crw-dev cxc declarations`", Declarations)
	}
	r := &Recorder{Oracle: oracle, Node: node, Git: git, Scratch: scratch, Decls: byLeg, Rules: rules}
	return r, r.Check()
}

func pruneFixtures(root string, specs []SpecFile, stdout io.Writer) error {
	want := map[string]bool{}
	for _, spec := range specs {
		for _, s := range spec.Scenarios {
			want[s.ID] = true
		}
	}
	paths, err := filepath.Glob(filepath.Join(root, FixtureDir, "*.json"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		if id := strings.TrimSuffix(filepath.Base(path), ".json"); !want[id] {
			if err := os.Remove(path); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "removed %s (no spec)\n", id)
		}
	}
	return nil
}

func syncCoverageFile(root string) error {
	cov, err := LoadCoverage(root)
	if err != nil {
		return err
	}
	covers, err := FixtureCovers(root)
	if err != nil {
		return err
	}
	SyncCoverage(&cov, covers)
	raw, err := Marshal(cov)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, Coverage), raw, 0o644)
}

// firstDiff shows the first differing line of two recordings.
func firstDiff(a, b []byte) string {
	x, y := strings.Split(string(a), "\n"), strings.Split(string(b), "\n")
	for i := 0; i < len(x) || i < len(y); i++ {
		var l, r string
		if i < len(x) {
			l = x[i]
		}
		if i < len(y) {
			r = y[i]
		}
		if l != r {
			return fmt.Sprintf("  line %d:\n  - %s\n  + %s", i+1, l, r)
		}
	}
	return "  (no line differs)"
}
