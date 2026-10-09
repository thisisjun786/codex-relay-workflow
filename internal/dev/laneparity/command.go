//go:build dev

package laneparity

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
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
  latency      --crw PATH --plugin DIR --oracle DIR [--node PATH] [--runs N] [--legs RE]
                                        p50 and p95 of the Go command against the CXC v0.2.40 command
  all          --crw PATH [--plugin DIR] [--oracle DIR] [--runs N]
                                        every cell; a generated root when --plugin is not given

common flags: --repo DIR (default: the git top level) --json FILE (the report) --scratch DIR (case roots)

The crw build for the fire and latency cells is built the way TestDomain/cxc builds it (go build -trimpath
-ldflags "-X main.recallTestClock=1767225600000 -X github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor.retrustTestClock=1767225600000" ./cmd/crw).
Nothing here touches the real Codex home: every case is a temporary root, and the plugin root is only read.`

// Report is the whole result of a run.
type Report struct {
	CRW          ReportCRW           `json:"crw"`
	Plugin       ReportPlugin        `json:"plugin"`
	Spec         string              `json:"spec"`
	Registration *RegistrationReport `json:"registration,omitempty"`
	Fire         *FireReport         `json:"fire,omitempty"`
	Latency      []Latency           `json:"latency,omitempty"`
	NotVerified  []NotVerified       `json:"notVerified"`
	OK           bool                `json:"ok"`
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
	runs := set.Int("runs", 15, "latency: runs per leg and side")
	jsonOut := set.String("json", "", "write the report here")
	scratch := set.String("scratch", "", "parent of the case roots (default: $TMPDIR)")
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
	report := Report{NotVerified: NotVerifiedCells(), OK: true}
	report.CRW.Path = bin
	if report.CRW.SHA256, err = FileDigest(bin); err != nil {
		return fail(err)
	}
	if file, _, derr := cxccorpus.LoadDeclarations(root); derr == nil {
		report.Spec = file.Oracle + ", " + cxccorpus.Declarations
	}
	pluginRoot := *plugin
	if pluginRoot == "" {
		if command != "all" {
			return fail(fmt.Errorf("--plugin is required"))
		}
		tmp, err := os.MkdirTemp(*scratch, "crw-parity-plugin-")
		if err != nil {
			return fail(err)
		}
		defer os.RemoveAll(tmp)
		pluginRoot = filepath.Join(tmp, "crw")
		if err := GeneratePluginRoot(pluginRoot, filepath.Join(root, "plugins", "crw"), bin, expected); err != nil {
			return fail(err)
		}
		report.Plugin.Generated = true
	}
	report.Plugin.Root = pluginRoot
	if report.Plugin.Digest, err = PluginDigest(pluginRoot); err != nil {
		return fail(err)
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
		rep, err := Fire(FireOptions{Root: root, CRW: bin, Plugin: pluginRoot, Scratch: *scratch, Only: onlyRE, Fault: *inject})
		if err != nil {
			return fail(err)
		}
		report.Fire = &rep
		printFire(stdout, rep)
		report.OK = report.OK && rep.OK
	}
	if command == "latency" || (command == "all" && *oracle != "") {
		lat, err := MeasureLatency(LatencyOptions{Root: root, CRW: bin, Plugin: pluginRoot, Scratch: *scratch, Oracle: *oracle, Node: *node, Runs: *runs, Only: legsRE})
		if err != nil {
			return fail(err)
		}
		report.Latency = lat
		printLatency(stdout, lat)
		for _, l := range lat {
			report.OK = report.OK && l.OK
		}
	}
	for _, n := range report.NotVerified {
		fmt.Fprintf(stdout, "NOT VERIFIED: %s -- %s\n", n.Cell, n.Reason)
	}
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
		if l.OK {
			good++
		} else {
			mark = "FAIL"
		}
		ts := "no oracle"
		if l.Oracle {
			ts = fmt.Sprintf("ts p50 %s p95 %s", l.TSP50.Round(time.Microsecond), l.TSP95.Round(time.Microsecond))
		}
		fmt.Fprintf(w, "%s latency %-62s runs %d go p50 %s p95 %s; %s; timeout %dms %s\n", mark, l.Leg, l.Runs,
			l.GoP50.Round(time.Microsecond), l.GoP95.Round(time.Microsecond), ts, l.TimeoutMs, l.Reason)
	}
	fmt.Fprintf(w, "latency: %d/%d legs pass (Go p95 <= TS p95 and <= half the timeout)\n", good, len(lat))
}

// HelperEnv is the variable that marks a process as a stub program or git wrapper of a replay case.
const HelperEnv = contracttest.RecDirEnv
