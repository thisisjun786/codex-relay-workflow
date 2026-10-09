//go:build dev

package laneparity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contracttest"
	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
)

const usage = `usage: crw-dev parity {plugin-root,registration,fire,latency,all} [flags]

  plugin-root  --crw PATH --out DIR     write a plugin root declaring the 32 K1 legs and CRW's two own
                                        registrations, every command starting the crw build PATH
  registration --plugin DIR             compare what the root declares with what it must declare
  fire         --crw PATH --plugin DIR  fire the corpus's hook fixtures through the declared commands
               [--only RE] [--inject FAULT]
  latency      --crw PATH --plugin DIR --oracle DIR [--node PATH] [--runs N] [--attempts N] [--strict] [--legs RE]
                                        p50 and p95 of the Go command against the CXC v0.2.40 command
  all          --crw PATH [--plugin DIR] [--oracle DIR] [--runs N] [--attempts N]
                                        every cell; a generated root when --plugin is not given

common flags: --repo DIR (default: the git top level) --json FILE (the report) --scratch DIR (parent of the run's case roots)
              --reuse FILE (a passing report of the same build, plugin root, criteria and options stands in for a run)

The crw build for the fire and latency cells is built the way TestDomain/cxc builds it (go build -trimpath
-ldflags "-X main.recallTestClock=1767225600000 -X github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor.retrustTestClock=1767225600000" ./cmd/crw).
Nothing here touches the real Codex home: every case is a temporary root, and the plugin root is only read.`

// Report is the whole result of a run.
type Report struct {
	CRW    ReportCRW    `json:"crw"`
	Plugin ReportPlugin `json:"plugin"`
	// Spec is the revision of the registration spec the cells judged against: the oracle and the sha256
	// of contract K1 (hook-declarations.json).
	Spec string `json:"spec"`
	// Scope is what a pass means.
	Scope        string              `json:"scope"`
	Registration *RegistrationReport `json:"registration,omitempty"`
	Fire         *FireReport         `json:"fire,omitempty"`
	Latency      []Latency           `json:"latency,omitempty"`
	NotVerified  []NotVerified       `json:"notVerified"`
	// Key identifies the artifact, plugin root, criteria and options a run judged: a report with the
	// same key already holds the evidence (--reuse).
	Key  string      `json:"key"`
	Test ReportOwner `json:"test"`
	OK   bool        `json:"ok"`
}

// ReportOwner records who ran the cells and where their state lived: the owner and process, the
// storage paths, and how they were cleaned up. No shared database and no live session is used.
type ReportOwner struct {
	User    string `json:"user"`
	PID     int    `json:"pid"`
	Scratch string `json:"scratch"`
	// LeftBehind is the number of entries still in the scratch directory when the cells had run
	// (case roots are removed by the engine as each case ends: zero is the expected value).
	LeftBehind int    `json:"leftBehind"`
	Cleanup    string `json:"cleanup"`
	// CPUs and the one-minute load average at the start and the end of the run: a latency cell on a
	// host whose load is above its CPU count is measuring the host as much as the hook.
	CPUs      int    `json:"cpus"`
	LoadStart string `json:"loadStart,omitempty"`
	LoadEnd   string `json:"loadEnd,omitempty"`
}

// load1 is the one-minute load average, or empty where the host does not say.
func load1() string {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return ""
	}
	first, _, _ := strings.Cut(string(raw), " ")
	return first
}

// ReportCRW names the build under test.
type ReportCRW struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// ReportPlugin names the plugin root fired.
type ReportPlugin struct {
	Root      string `json:"root"`
	Digest    string `json:"digest"`
	Generated bool   `json:"generated"`
}

// NotVerified is a cell this harness does not cover, with what would.
type NotVerified struct {
	Cell     string `json:"cell"`
	Reason   string `json:"reason"`
	Followup string `json:"followup"`
}

const realHost = "real-host cells with a stub model provider (decision 3 of the 10-10 coordinator comment); a follow-up issue carries them"

// NotVerifiedCells are the cells every run leaves unverified, so a green run is not read as more.
func NotVerifiedCells() []NotVerified {
	return []NotVerified{
		{"real Codex binary fires the declared hook from a real turn (trust, thread, turn, socket receipts)", "needs the host started in an isolated home with a stub model provider", realHost},
		{"hook trust: a declared hook does not run until trusted", "host behaviour; measured once in docs/plugin-packaging.md, not driven here", realHost},
		{"pause, cancel and permission refusal at the host; forced exit and restart of the host mid-turn; stall", "need a live turn; the permission-request leg's payload handling is fired, the host's refusal is not", realHost},
		{"context recovery after a real compaction", "post-compact and recall legs are fired with corpus payloads; the host's compaction is not", realHost},
		{"native spawn surface: skill selection, delivery and behaviour of a spawned agent", "the spawn attach leg is fired with corpus payloads; the spawned agent is not", realHost},
		{"real-model behaviour of the injected directives", "no model runs", realHost},
		{"the CRW-392 switch (<CODEX_HOME>/crw/switch.json) and a normal installation", "not on dev; installation parity belongs to CRW-201 and CRW-204, and a pass here is never an installation pass", "CRW-201, CRW-204"},
		{"completion Stop effect", "the declared command is fired and released in silence; its guard daemon is not started here (see internal/runtime/integration)", "CRW-204"},
	}
}

// Run is `crw-dev parity`.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintln(stdout, usage)
		return 0
	}
	switch args[0] {
	case "plugin-root", "registration", "fire", "latency", "all":
		return runCommand(args[0], args[1:], stdout, stderr)
	}
	fmt.Fprintln(stderr, usage)
	fmt.Fprintf(stderr, "crw-dev parity: error: invalid command %q\n", args[0])
	return 2
}

func runCommand(command string, args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("crw-dev parity "+command, flag.ContinueOnError)
	set.SetOutput(stderr)
	repo := set.String("repo", "", "repository root (default: the git top level)")
	crw := set.String("crw", "", "the crw build under test (absolute path)")
	plugin := set.String("plugin", "", "the plugin root to read and fire")
	out := set.String("out", "", "plugin-root: where to write the generated root")
	only := set.String("only", "", "fire: only fixtures whose id matches this regular expression")
	legs := set.String("legs", "", "latency: only legs matching this regular expression")
	inject := set.String("inject", "", "fire: inject a fault ("+strings.Join(Faults, ", ")+"); the run must fail")
	oracle := set.String("oracle", "", "latency: the extracted CXC v0.2.40 tree")
	node := set.String("node", "", "latency: node executable (default: node on PATH)")
	runs := set.Int("runs", 30, "latency: runs per leg and side")
	strict := set.Bool("strict", false, "latency: a failing leg fails the run even when the host's load average is above its CPU count (otherwise it is inconclusive)")
	attempts := set.Int("attempts", 5, "latency: measurements of a leg that fails before it is reported failing (a shared host's load puts outliers in a p95)")
	jsonOut := set.String("json", "", "write the report here")
	scratch := set.String("scratch", "", "parent of the run's directory of case roots (default: $TMPDIR)")
	reuse := set.String("reuse", "", "a report: when it judged the same build, plugin root, criteria and options and passed, its evidence stands and nothing is run")
	if err := set.Parse(args); err != nil {
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "crw-dev parity %s: %v\n", command, err)
		return 1
	}
	root := *repo
	if root == "" {
		var err error
		if root, err = cxccorpus.RepositoryRoot(); err != nil {
			return fail(err)
		}
	}
	var onlyRE, legsRE *regexp.Regexp
	var err error
	if *only != "" {
		if onlyRE, err = regexp.Compile(*only); err != nil {
			return fail(err)
		}
	}
	if *legs != "" {
		if legsRE, err = regexp.Compile(*legs); err != nil {
			return fail(err)
		}
	}
	expected, err := ExpectedLegs(root)
	if err != nil {
		return fail(err)
	}
	needCRW := func() (string, error) {
		if *crw == "" || !filepath.IsAbs(*crw) {
			return "", fmt.Errorf("--crw must be the absolute path of the crw build under test")
		}
		return *crw, nil
	}
	switch command {
	case "plugin-root":
		bin, err := needCRW()
		if err != nil {
			return fail(err)
		}
		if *out == "" {
			return fail(fmt.Errorf("--out is required"))
		}
		if err := GeneratePluginRoot(*out, filepath.Join(root, "plugins", "crw"), bin, expected); err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "wrote a plugin root declaring %d legs under %s\n", len(expected), *out)
		return 0
	case "registration":
		if *plugin == "" {
			return fail(fmt.Errorf("--plugin is required"))
		}
		rep, err := registration(*plugin, expected)
		if err != nil {
			return fail(err)
		}
		printRegistration(stdout, rep)
		return exitCode(rep.OK)
	}
	// fire, latency and all fire or time a build.
	bin, err := needCRW()
	if err != nil {
		return fail(err)
	}
	report := Report{NotVerified: NotVerifiedCells(), OK: true,
		Scope: "isolated roots only: the declared commands of the named plugin root, fired with corpus payloads; not a real host turn, not a normal installation (CRW-201, CRW-204)"}
	report.CRW.Path = bin
	if report.CRW.SHA256, err = FileDigest(bin); err != nil {
		return fail(err)
	}
	if file, _, derr := cxccorpus.LoadDeclarations(root); derr == nil {
		report.Spec = file.Oracle + ", " + cxccorpus.Declarations
		if digest, derr := FileDigest(filepath.Join(root, cxccorpus.Declarations)); derr == nil {
			report.Spec += " sha256 " + digest
		}
	}
	// Every case root and a generated plugin root live in one directory of this run, removed at the end.
	run, err := os.MkdirTemp(*scratch, "crw-parity-run-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(run)
	cases := filepath.Join(run, "cases")
	if err := os.Mkdir(cases, 0o700); err != nil {
		return fail(err)
	}
	report.Test = ReportOwner{User: os.Getenv("USER"), PID: os.Getpid(), Scratch: run, CPUs: runtime.NumCPU(), LoadStart: load1(),
		Cleanup: "each case root is removed as its case ends and the run directory when the run ends; nothing outside the run directory is written, and no shared database, live session, Codex home or runtime is used"}
	pluginRoot := *plugin
	if pluginRoot == "" {
		if command != "all" {
			return fail(fmt.Errorf("--plugin is required"))
		}
		pluginRoot = filepath.Join(run, "plugin", "crw")
		if err := GeneratePluginRoot(pluginRoot, filepath.Join(root, "plugins", "crw"), bin, expected); err != nil {
			return fail(err)
		}
		report.Plugin.Generated = true
	}
	report.Plugin.Root = pluginRoot
	if report.Plugin.Digest, err = PluginDigest(pluginRoot); err != nil {
		return fail(err)
	}
	if report.Key, err = ReportKey(root, report.CRW.SHA256, report.Plugin.Digest, command, *only, *legs, *inject, *oracle, *runs, *attempts); err != nil {
		return fail(err)
	}
	if *reuse != "" {
		if prior, ok := reusable(*reuse, report.Key); ok {
			fmt.Fprintf(stdout, "reused: %s judged this build, plugin root, criteria and options (key %.16s) and passed\n", *reuse, report.Key)
			if *jsonOut != "" && *jsonOut != *reuse {
				raw, _ := json.MarshalIndent(prior, "", "  ")
				if err := os.WriteFile(*jsonOut, append(raw, '\n'), 0o644); err != nil {
					return fail(err)
				}
			}
			return 0
		}
	}
	if command == "all" {
		rep, err := registration(pluginRoot, expected)
		if err != nil {
			return fail(err)
		}
		report.Registration = &rep
		printRegistration(stdout, rep)
		report.OK = report.OK && rep.OK
	}
	if command == "fire" || command == "all" {
		rep, err := Fire(FireOptions{Root: root, CRW: bin, Plugin: pluginRoot, Scratch: cases, Only: onlyRE, Fault: *inject})
		if err != nil {
			return fail(err)
		}
		report.Fire = &rep
		printFire(stdout, rep)
		report.OK = report.OK && rep.OK
	}
	if command == "latency" || (command == "all" && *oracle != "") {
		lat, err := MeasureLatency(LatencyOptions{Root: root, CRW: bin, Plugin: pluginRoot, Scratch: cases, Oracle: *oracle, Node: *node, Runs: *runs, Attempts: *attempts, Strict: *strict, Only: legsRE})
		if err != nil {
			return fail(err)
		}
		report.Latency = lat
		printLatency(stdout, lat)
		for _, l := range lat {
			report.OK = report.OK && l.OK
			if l.Skipped {
				report.NotVerified = append(report.NotVerified, NotVerified{"latency of " + l.Leg, l.Reason, "the issue that ports the leg"})
			}
			if l.Inconclusive {
				report.NotVerified = append(report.NotVerified, NotVerified{"latency of " + l.Leg, l.Reason, "run again on a host whose load is below its CPU count, or with --strict to fail"})
			}
		}
	}
	report.Test.LoadEnd = load1()
	if left, err := os.ReadDir(cases); err == nil {
		report.Test.LeftBehind = len(left)
		if len(left) > 0 {
			fmt.Fprintf(stdout, "FAIL cleanup: %d entr(ies) left in %s\n", len(left), cases)
			report.OK = false
		}
	}
	for _, n := range report.NotVerified {
		fmt.Fprintf(stdout, "NOT VERIFIED: %s -- %s\n", n.Cell, n.Reason)
	}
	fmt.Fprintln(stdout, "SCOPE: "+report.Scope)
	if *jsonOut != "" {
		raw, _ := json.MarshalIndent(report, "", "  ")
		if err := os.WriteFile(*jsonOut, append(raw, '\n'), 0o644); err != nil {
			return fail(err)
		}
	}
	if *inject != "" {
		// A fault run proves the harness catches it: it succeeds exactly when the cells failed.
		if report.OK {
			fmt.Fprintf(stderr, "FAULT %q WAS NOT CAUGHT: every cell passed\n", *inject)
			return 1
		}
		fmt.Fprintf(stdout, "fault %q caught\n", *inject)
		return 0
	}
	return exitCode(report.OK)
}

// ReportKey identifies what a run judged: the build, the plugin root's declarations, the corpus
// files the cells read (K1, the rename and normalisation tables, the status files and the hook
// fixtures) and the command and options.
func ReportKey(root, crw, plugin string, parts ...any) (string, error) {
	sum := sha256.New()
	fmt.Fprintf(sum, "crw=%s\nplugin=%s\nopts=%v\n", crw, plugin, parts)
	var paths []string
	for _, glob := range []string{
		filepath.Join(root, cxccorpus.SchemaDir, "*.json"),
		filepath.Join(root, "contract", "notes", "cxc", "*.json"),
		filepath.Join(root, cxccorpus.FixtureDir, "hook__*.json"),
	} {
		hits, err := filepath.Glob(glob)
		if err != nil {
			return "", err
		}
		paths = append(paths, hits...)
	}
	sort.Strings(paths)
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		rel, _ := filepath.Rel(root, path)
		fmt.Fprintf(sum, "%s\x00%d\x00", rel, len(raw))
		sum.Write(raw)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// reusable reads a report and returns it when it passed and carries the key.
func reusable(path, key string) (Report, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Report{}, false
	}
	var prior Report
	if json.Unmarshal(raw, &prior) != nil {
		return Report{}, false
	}
	return prior, prior.OK && prior.Key == key && key != ""
}

func exitCode(ok bool) int {
	if ok {
		return 0
	}
	return 1
}

func registration(plugin string, expected []Leg) (RegistrationReport, error) {
	manifest, registered, err := ReadRegistered(plugin)
	if err != nil {
		return RegistrationReport{}, err
	}
	return CheckRegistration(manifest, registered, expected), nil
}

func printRegistration(w io.Writer, rep RegistrationReport) {
	good := 0
	for _, l := range rep.Legs {
		if l.OK {
			good++
			continue
		}
		fmt.Fprintf(w, "FAIL registration %s: %s\n", l.Leg, strings.Join(l.Problems, "; "))
	}
	for _, p := range rep.Problems {
		fmt.Fprintf(w, "FAIL registration: %s\n", p)
	}
	for _, e := range rep.Extra {
		fmt.Fprintf(w, "FAIL registration: extra %s\n", e)
	}
	fmt.Fprintf(w, "registration: %d/%d legs declared as required, %d extra\n", good, len(rep.Legs), len(rep.Extra))
}

func printFire(w io.Writer, rep FireReport) {
	matched, failed, pending := 0, 0, 0
	for _, l := range rep.Legs {
		matched, failed, pending = matched+l.Matched, failed+l.Failed, pending+l.Pending
		if !l.OK {
			fmt.Fprintf(w, "FAIL fire %s: %s\n", l.Leg, l.Note)
		}
	}
	for _, f := range rep.Fixtures {
		if f.Problem != "" {
			fmt.Fprintf(w, "FAIL effect %s: %s\n", f.ID, firstLines(f.Problem, 4))
		}
	}
	for _, p := range rep.Probes {
		if p.Problem != "" {
			fmt.Fprintf(w, "FAIL probe %s: %s\n", p.ID, p.Problem)
		}
	}
	for _, p := range rep.ReceiptProblems {
		fmt.Fprintf(w, "FAIL receipt: %s\n", p)
	}
	for _, u := range rep.Unverified {
		fmt.Fprintf(w, "UNVERIFIED %s\n", u)
	}
	fmt.Fprintf(w, "fire (run %s): %d fixture(s) fired and equal their expectation, %d differ, %d pending; %d receipt(s), %d receipt problem(s)\n",
		rep.Run, matched, failed, pending, len(rep.Receipts), len(rep.ReceiptProblems))
}

func attemptNote(l Latency) string {
	if l.Attempts > 1 {
		return fmt.Sprintf("[attempt %d] ", l.Attempts)
	}
	return ""
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = append(lines[:n], fmt.Sprintf("... and %d more line(s)", len(lines)-n))
	}
	return strings.Join(lines, " | ")
}

func printLatency(w io.Writer, lat []Latency) {
	good := 0
	for _, l := range lat {
		mark := "ok  "
		switch {
		case l.Skipped:
			mark = "SKIP"
		case l.Inconclusive:
			mark = "INCL"
		case l.OK:
			good++
		default:
			mark = "FAIL"
		}
		ts := "no oracle"
		if l.Oracle {
			ts = fmt.Sprintf("ts p50 %s p95 %s", l.TSP50.Round(time.Microsecond), l.TSP95.Round(time.Microsecond))
		}
		fmt.Fprintf(w, "%s latency %-62s runs %d go p50 %s p95 %s; %s; timeout %dms %s\n", mark, l.Leg, l.Runs,
			l.GoP50.Round(time.Microsecond), l.GoP95.Round(time.Microsecond), ts, l.TimeoutMs, attemptNote(l)+l.Reason)
	}
	skipped, inconclusive := 0, 0
	for _, l := range lat {
		if l.Skipped {
			skipped++
		}
		if l.Inconclusive {
			inconclusive++
		}
	}
	fmt.Fprintf(w, "latency: %d/%d legs pass (Go p95 <= TS p95 and <= half the timeout), %d inconclusive under host load, %d not timed\n", good, len(lat)-skipped, inconclusive, skipped)
}

// HelperEnv is the variable that marks a process as a stub program or git wrapper of a replay case.
const HelperEnv = contracttest.RecDirEnv
