//go:build dev

package laneparity

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// slice is a few legs of every shape: stateless and once-per-session, answering and silent, with
// the first leg of the table (the one the registration faults aim at).
var slice = regexp.MustCompile(`^hook__(session-start-ensuring-provider-bridge|session-start-announcing-map-affordance|pre-tool-use-guarding-managed-worktree-deletion|user-prompt-submit-guiding-worktree-rename|subagent-stop-verifying-evidence)__`)

func regexpOf(expr string) *regexp.Regexp { return regexp.MustCompile(expr) }

func fireFixture(t *testing.T) FireOptions {
	t.Helper()
	crw, err := crwUnderTest()
	if err != nil {
		t.Fatal(err)
	}
	root := repoRoot(t)
	legs, err := ExpectedLegs(root)
	if err != nil {
		t.Fatal(err)
	}
	plugin := filepath.Join(t.TempDir(), "crw")
	if err := GeneratePluginRoot(plugin, filepath.Join(root, "plugins", "crw"), crw, legs); err != nil {
		t.Fatal(err)
	}
	return FireOptions{Root: root, CRW: crw, Plugin: plugin, Scratch: t.TempDir(), Only: slice}
}

func TestFire_aGeneratedRootFiresAndItsEffectsMatchTheCorpus(t *testing.T) {
	rep, err := Fire(fireFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK {
		t.Fatalf("report not ok: receipts %q fixtures %+v probes %+v", rep.ReceiptProblems, failed(rep), rep.Probes)
	}
	byLeg := map[string]LegFire{}
	for _, l := range rep.Legs {
		byLeg[l.Leg] = l
	}
	for _, leg := range []string{"session-start-ensuring-provider-bridge", "pre-tool-use-guarding-managed-worktree-deletion", "user-prompt-submit-guiding-worktree-rename", GitHubPostLeg} {
		if l := byLeg[leg]; l.Matched == 0 || !l.Witnessed {
			t.Errorf("%s: matched %d, witnessed %v", leg, l.Matched, l.Witnessed)
		}
	}
	if len(rep.Receipts) < 20 || rep.Run == "" || rep.Plugin == "" || rep.Binary == "" {
		t.Errorf("%d receipts, run %q plugin %q binary %q", len(rep.Receipts), rep.Run, rep.Plugin, rep.Binary)
	}
	for _, r := range rep.Receipts {
		if r.Run != rep.Run || r.Command == "" || !strings.Contains(r.Command, "hook ") {
			t.Fatalf("receipt %+v", r)
		}
	}
	if len(rep.Probes) != 3 {
		t.Errorf("%d probes", len(rep.Probes))
	}
	if rep.Switch.Absent || rep.Switch.Active != "crw" || rep.Switch.By != SwitchBy {
		t.Errorf("switch report %+v: every case root must hold the switch at crw", rep.Switch)
	}
}

func failed(rep FireReport) (out []FixtureFire) {
	for _, f := range rep.Fixtures {
		if f.Problem != "" {
			out = append(out, f)
		}
	}
	return out
}

// Every injected fault must turn the report red, for the reason it names: this is what stops a
// green report from meaning only that the harness ran.
func TestFire_everyInjectedFaultIsCaught(t *testing.T) {
	base := fireFixture(t)
	for _, c := range []struct{ fault, says string }{
		{FaultWrongEvent, "no declared command"},
		{FaultMissingHook, "no declared command"},
		{FaultNoop, "differs from the expectation"},
		{FaultKill, "differs from the expectation"},
		{FaultDropStdout, "differs from the expectation"},
		{FaultReverseSteps, "differs from the expectation"},
		{FaultStaleReceipt, "receipt of run 0000000000000000"},
		{FaultOtherPlugin, "another plugin root"},
		{FaultOtherBuild, "another crw build"},
		{FaultReceiptEvent, "receipt is for event Notification"},
		{FaultReceiptAgent, `receipt agent is "other-agent"`},
		{FaultReceiptSkill, "receipt skills"},
		{FaultLostReceipt, "no receipt"},
		{FaultDoubleReceipt, "2 receipts for one firing"},
	} {
		t.Run(c.fault, func(t *testing.T) {
			o := base
			o.Fault = c.fault
			rep, err := Fire(o)
			if err != nil {
				t.Fatal(err)
			}
			if rep.OK {
				t.Fatalf("fault %s was not caught", c.fault)
			}
			var all []string
			all = append(all, rep.ReceiptProblems...)
			for _, f := range rep.Fixtures {
				all = append(all, f.Problem)
			}
			for _, p := range rep.Probes {
				all = append(all, p.Problem)
			}
			if !strings.Contains(strings.Join(all, "\n"), c.says) {
				t.Errorf("no problem says %q: %.600q", c.says, strings.Join(all, "\n"))
			}
		})
	}
}

func TestFire_aNoopCommandIsNotWitnessedAsFiring(t *testing.T) {
	o := fireFixture(t)
	o.Fault = FaultNoop
	rep, err := Fire(o)
	if err != nil {
		t.Fatal(err)
	}
	// Silent fixtures pass a command that does nothing; the leg is still red because a fixture that
	// expects an answer does not.
	for _, l := range rep.Legs {
		if l.Leg == "session-start-announcing-map-affordance" && (l.OK || l.Failed == 0) {
			t.Errorf("%+v", l)
		}
	}
}

func TestFire_refusesAnUnknownFault(t *testing.T) {
	o := fireFixture(t)
	o.Fault = "teleport"
	if _, err := Fire(o); err == nil || !strings.Contains(err.Error(), "unknown fault") {
		t.Fatalf("err = %v", err)
	}
}

func TestRun_exitCodes(t *testing.T) {
	o := fireFixture(t)
	args := func(extra ...string) []string {
		return append([]string{"fire", "--crw", o.CRW, "--plugin", o.Plugin, "--scratch", o.Scratch, "--only", slice.String()}, extra...)
	}
	var out, errs bytes.Buffer
	if code := Run(args(), &out, &errs); code != 0 {
		t.Fatalf("a good run exits %d: %s %s", code, out.String(), errs.String())
	}
	out.Reset()
	errs.Reset()
	if code := Run(args("--inject", FaultDropStdout), &out, &errs); code != 0 || !strings.Contains(out.String(), `fault "drop-stdout" caught`) {
		t.Fatalf("a caught fault exits %d: %s %s", code, out.String(), errs.String())
	}
	if code := Run(args("--inject", "teleport"), &out, &errs); code != 1 {
		t.Errorf("an unknown fault exits %d", code)
	}
	if code := Run([]string{"fire", "--plugin", o.Plugin}, &out, &errs); code != 1 {
		t.Errorf("no --crw exits %d", code)
	}
	if code := Run([]string{"nonsense"}, &out, &errs); code != 2 {
		t.Errorf("an unknown command exits %d", code)
	}
}

func TestRun_registrationOfATamperedRootFails(t *testing.T) {
	o := fireFixture(t)
	var out, errs bytes.Buffer
	if code := Run([]string{"registration", "--plugin", o.Plugin}, &out, &errs); code != 0 {
		t.Fatalf("a generated root exits %d: %s", code, out.String())
	}
	out.Reset()
	// The shipped plugin declares the 34 registrations itself since CRW-392.
	if code := Run([]string{"registration", "--plugin", filepath.Join(repoRoot(t), "plugins", "crw")}, &out, &errs); code != 0 {
		t.Fatalf("the shipped plugin declares every leg and must pass, exit %d: %s", code, out.String())
	}
	out.Reset()
	// The leg's file stays (the manifest lists it) but declares nothing.
	if err := os.WriteFile(filepath.Join(o.Plugin, "wiring", "hooks", "session-start-bootstrapping-pabcd-state.json"), []byte(`{"hooks":{}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{"registration", "--plugin", o.Plugin}, &out, &errs); code != 1 {
		t.Fatalf("a root missing one declaration must fail, exit %d: %s %s", code, out.String(), errs.String())
	}
	if !strings.Contains(out.String(), "no registration starts this leg") {
		t.Errorf("output %.400q", out.String())
	}
}

// A declaration whose hook syntax sits behind a comment registers nothing: the shell runs /bin/true.
func TestRun_registrationOfACommentedOutHookFails(t *testing.T) {
	plugin, _ := generated(t)
	editDeclaration(t, plugin, `"command": "`, `"command": "/bin/true # `)
	var out, errs bytes.Buffer
	if code := Run([]string{"registration", "--plugin", plugin}, &out, &errs); code != 1 {
		t.Fatalf("a root whose hooks are commented out exits %d: %.600s", code, out.String())
	}
	if !strings.Contains(out.String(), "no registration starts this leg") {
		t.Errorf("output %.400q", out.String())
	}
}

// A passing report is evidence of the executables the declared commands start, by content: the same
// path holding another file is another artifact, and the report is not reused for it.
func TestRun_reuseIsRefusedWhenTheStartedExecutableChanges(t *testing.T) {
	real, err := crwUnderTest()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	started := filepath.Join(dir, "bin", "crw")
	if err := testsupport.CopyBinary(real, started); err != nil {
		t.Fatal(err)
	}
	root := repoRoot(t)
	legs, err := ExpectedLegs(root)
	if err != nil {
		t.Fatal(err)
	}
	plugin := filepath.Join(dir, "plugin", "crw")
	if err := GeneratePluginRoot(plugin, filepath.Join(root, "plugins", "crw"), started, legs); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(dir, "report.json")
	args := func(extra ...string) []string {
		return append([]string{"fire", "--crw", real, "--plugin", plugin, "--scratch", t.TempDir(),
			"--only", `^hook__session-start-announcing-map-affordance__`, "--json", report}, extra...)
	}
	var out, errs bytes.Buffer
	if code := Run(args(), &out, &errs); code != 0 {
		t.Fatalf("the first run exits %d: %.800s %s", code, out.String(), errs.String())
	}
	other, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no true on PATH")
	}
	if err := os.Remove(started); err != nil {
		t.Fatal(err)
	}
	if err := testsupport.CopyBinary(other, started); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	code := Run(args("--reuse", report), &out, &errs)
	if strings.Contains(out.String(), "reused") || code == 0 {
		t.Fatalf("a report was reused for another executable at the same path, exit %d: %.800s", code, out.String())
	}
}

// The oracle's files are part of what a latency cell judged: another file at the same path is
// another oracle.
func TestOracleIdentity_namesTheOracleByContent(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "components", "x", "dist", "cli.js")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := oracleIdentity("fire", dir); got != "" {
		t.Errorf("a cell that runs no oracle has no oracle identity: %q", got)
	}
	a := oracleIdentity("latency", dir)
	if a == "" || a != oracleIdentity("all", dir) {
		t.Fatalf("identity %q", a)
	}
	if err := os.WriteFile(file, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b := oracleIdentity("latency", dir); b == a {
		t.Error("another file at the same path keeps the oracle's identity")
	}
	if c := oracleIdentity("latency", filepath.Join(dir, "missing")); c == a || c == "" {
		t.Errorf("a missing oracle: %q", c)
	}
}

// The recorder follows links in the oracle: a symlinked root, directory or file is the code that
// runs, so its bytes are the identity, not the link text.
func TestOracleIdentity_followsLinksToTheCodeThatRuns(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	shared := filepath.Join(dir, "shared")
	for _, d := range []string{filepath.Join(real, "components", "x", "dist"), shared} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	entry := filepath.Join(real, "components", "x", "dist", "cli.js")
	lib := filepath.Join(shared, "lib.js")
	single := filepath.Join(dir, "single.js")
	for _, f := range []string{entry, lib, single} {
		if err := os.WriteFile(f, []byte("one"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(shared, filepath.Join(real, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(single, filepath.Join(real, "single.js")); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "oracle")
	if err := os.Symlink(real, root); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{oracleIdentity("latency", root): true}
	for name, file := range map[string]string{"the entry script under a linked root": entry, "a file in a linked directory": lib, "a linked file": single} {
		if err := os.WriteFile(file, []byte("changed "+name), 0o644); err != nil {
			t.Fatal(err)
		}
		id := oracleIdentity("latency", root)
		if seen[id] {
			t.Errorf("%s changed and the oracle keeps its identity", name)
		}
		seen[id] = true
	}
}

// Without an oracle, `all` has measured no latency: the report says the cell is not verified rather
// than leaving it out of a green report; `latency` alone refuses to run.
func TestRun_withoutAnOracleTheLatencyCellIsNotVerified(t *testing.T) {
	o := fireFixture(t)
	report := filepath.Join(t.TempDir(), "report.json")
	var out, errs bytes.Buffer
	code := Run([]string{"all", "--crw", o.CRW, "--plugin", o.Plugin, "--scratch", o.Scratch,
		"--only", `^hook__session-start-announcing-map-affordance__`, "--json", report}, &out, &errs)
	if code != 0 {
		t.Fatalf("all exits %d: %.800s %s", code, out.String(), errs.String())
	}
	raw, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	var rep Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range rep.NotVerified {
		if strings.HasPrefix(n.Cell, "latency") && strings.Contains(n.Reason, "--oracle") {
			found = true
		}
	}
	if rep.Latency != nil || !found {
		t.Errorf("latency %v, notVerified %+v", rep.Latency, rep.NotVerified)
	}
	if !strings.Contains(out.String(), "NOT VERIFIED: latency") {
		t.Errorf("output does not say the latency cell is not verified: %.600s", out.String())
	}
	out.Reset()
	errs.Reset()
	if code := Run([]string{"latency", "--crw", o.CRW, "--plugin", o.Plugin, "--scratch", o.Scratch}, &out, &errs); code == 0 || !strings.Contains(errs.String(), "--oracle") {
		t.Errorf("latency without --oracle exits %d: %s %s", code, out.String(), errs.String())
	}
}

// rootWithPendingLeg is a view of the repository whose status files leave every fixture of leg
// pending, whoever claims them in the checkout: the corpus the tests run on does not depend on
// which ports the tree has merged. Everything else is linked from the real root.
func rootWithPendingLeg(t *testing.T, leg string) string {
	t.Helper()
	real := repoRoot(t)
	root := t.TempDir()
	link := func(from, to string) {
		t.Helper()
		if err := os.Symlink(from, to); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(real)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "contract" {
			link(filepath.Join(real, e.Name()), filepath.Join(root, e.Name()))
		}
	}
	contract, err := os.ReadDir(filepath.Join(real, "contract"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "contract", "notes", "cxc"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, e := range contract {
		if e.Name() != "notes" {
			link(filepath.Join(real, "contract", e.Name()), filepath.Join(root, "contract", e.Name()))
		}
	}
	notes, err := os.ReadDir(filepath.Join(real, "contract", "notes"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range notes {
		if e.Name() != "cxc" {
			link(filepath.Join(real, "contract", "notes", e.Name()), filepath.Join(root, "contract", "notes", e.Name()))
		}
	}
	prefix := "hook__" + leg + "__"
	var released []string
	files, err := filepath.Glob(filepath.Join(real, "contract", "notes", "cxc", "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Ext(path) == ".json" {
			var note map[string]json.RawMessage
			if err := json.Unmarshal(raw, &note); err != nil {
				t.Fatal(err)
			}
			var identical []string
			if note["identical"] != nil {
				if err := json.Unmarshal(note["identical"], &identical); err != nil {
					t.Fatal(err)
				}
			}
			var kept []string
			for _, id := range identical {
				if strings.HasPrefix(id, prefix) {
					released = append(released, id)
				} else {
					kept = append(kept, id)
				}
			}
			var changed []map[string]json.RawMessage
			if note["intentionally-changed"] != nil {
				if err := json.Unmarshal(note["intentionally-changed"], &changed); err != nil {
					t.Fatal(err)
				}
			}
			var keptChanged []map[string]json.RawMessage
			for _, c := range changed {
				var id string
				_ = json.Unmarshal(c["id"], &id)
				if strings.HasPrefix(id, prefix) {
					released = append(released, id)
				} else {
					keptChanged = append(keptChanged, c)
				}
			}
			if note["identical"] != nil {
				note["identical"], _ = json.Marshal(kept)
			}
			if note["intentionally-changed"] != nil {
				note["intentionally-changed"], _ = json.Marshal(keptChanged)
			}
			if raw, err = json.Marshal(note); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(root, "contract", "notes", "cxc", filepath.Base(path)), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pending, _ := json.Marshal(map[string]any{"issue": "TEST", "pending": released})
	if err := os.WriteFile(filepath.Join(root, "contract", "notes", "cxc", "TEST.json"), pending, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestMeasureLatency_timesTheGoSideAndJudgesItAgainstTheTimeout(t *testing.T) {
	o := fireFixture(t)
	const pendingLeg = "subagent-stop-observing-review"
	lat, err := MeasureLatency(LatencyOptions{
		Root: o.Root, CRW: o.CRW, Plugin: o.Plugin, Scratch: o.Scratch, Runs: 3, Attempts: 2,
		Only: regexp.MustCompile(`^(session-start-announcing-map-affordance|pre-tool-use-guarding-github-post)$`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(lat) != 2 {
		t.Fatalf("%d legs timed: %+v", len(lat), lat)
	}
	for _, l := range lat {
		if l.Skipped || l.Runs != 3 || l.Oracle || l.GoP95 <= 0 || l.TimeoutMs != 10000 || l.Attempts < 1 {
			t.Errorf("%s: %+v", l.Leg, l)
		}
	}
	// A leg whose port is still pending has no claimed fixture: skipped, not failed. A tree that has
	// merged its port claims the fixtures, and then they are timed like any other leg's.
	lat, err = MeasureLatency(LatencyOptions{
		Root: rootWithPendingLeg(t, pendingLeg), CRW: o.CRW, Plugin: o.Plugin, Scratch: o.Scratch, Runs: 3, Attempts: 2,
		Only: regexp.MustCompile(`^` + pendingLeg + `$`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(lat) != 1 || !lat[0].Skipped || !lat[0].OK || lat[0].Runs != 0 {
		t.Errorf("a leg no claimed fixture exercises must be skipped, not failed: %+v", lat)
	}
}

// CRW-564 is on dev: its two fixtures of subagent-stop-observing-review are claimed, so the latency cell
// times that leg like any other instead of skipping it (CRW-1082, item 3).
func TestMeasureLatency_timesSubagentStopObservingReviewNowThatItsPortIsClaimed(t *testing.T) {
	o := fireFixture(t)
	const leg = "subagent-stop-observing-review"
	lat, err := MeasureLatency(LatencyOptions{Root: o.Root, CRW: o.CRW, Plugin: o.Plugin, Scratch: o.Scratch, Runs: 3, Attempts: 2, Only: regexp.MustCompile(`^` + leg + `$`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(lat) != 1 || lat[0].Leg != leg || lat[0].Skipped || lat[0].Runs != 3 || lat[0].GoP95 <= 0 || lat[0].Broken {
		t.Fatalf("the leg is skipped or broken although its port is claimed: %+v", lat)
	}
	rep, err := Fire(FireOptions{Root: o.Root, CRW: o.CRW, Plugin: o.Plugin, Scratch: o.Scratch, Only: regexp.MustCompile(`^hook__` + leg + `__`)})
	if err != nil {
		t.Fatal(err)
	}
	var found *LegFire
	for i := range rep.Legs {
		if rep.Legs[i].Leg == leg {
			found = &rep.Legs[i]
		}
	}
	if !rep.OK || found == nil || found.Matched != 2 || found.Pending != 0 || found.Failed != 0 {
		t.Errorf("ok %v, legs %+v", rep.OK, rep.Legs)
	}
}

// With an oracle checkout (CXC_PARITY_ORACLE names the extracted v0.2.40 tree) the TS side is timed too.
func TestMeasureLatency_againstTheOracle(t *testing.T) {
	oracle := os.Getenv("CXC_PARITY_ORACLE")
	if oracle == "" {
		t.Skip("CXC_PARITY_ORACLE names the extracted CXC v0.2.40 tree; unset")
	}
	o := fireFixture(t)
	lat, err := MeasureLatency(LatencyOptions{
		Root: o.Root, CRW: o.CRW, Plugin: o.Plugin, Scratch: o.Scratch, Oracle: oracle, Runs: 5, Attempts: 3,
		Only: regexp.MustCompile(`^session-start-announcing-map-affordance$`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(lat) != 1 || !lat[0].Oracle || lat[0].TSP95 <= 0 || !lat[0].OK {
		t.Fatalf("%+v", lat)
	}
}

func TestRun_aPassingReportOfTheSameKeyStandsInForARun(t *testing.T) {
	o := fireFixture(t)
	report := filepath.Join(t.TempDir(), "report.json")
	args := func(extra ...string) []string {
		return append([]string{"fire", "--crw", o.CRW, "--plugin", o.Plugin, "--scratch", o.Scratch, "--only", slice.String(), "--json", report}, extra...)
	}
	var out, errs bytes.Buffer
	if code := Run(args(), &out, &errs); code != 0 || strings.Contains(out.String(), "reused") {
		t.Fatalf("first run exits %d: %s %s", code, out.String(), errs.String())
	}
	raw, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	var rep Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	if !rep.OK || rep.Key == "" || rep.Test.PID == 0 || rep.Test.Scratch == "" || rep.Test.LeftBehind != 0 || rep.Fire == nil || len(rep.NotVerified) == 0 {
		t.Fatalf("report %+v", rep.Test)
	}
	if _, err := os.Stat(rep.Test.Scratch); err == nil {
		t.Errorf("the run directory %s was not removed", rep.Test.Scratch)
	}
	out.Reset()
	if code := Run(args("--reuse", report), &out, &errs); code != 0 || !strings.Contains(out.String(), "reused") {
		t.Errorf("the same key must be reused, exit %d: %s", code, out.String())
	}
	out.Reset()
	strict := args("--reuse", report, "--strict") // a stricter verdict is another criterion
	if code := Run(strict, &out, &errs); strings.Contains(out.String(), "reused") {
		t.Errorf("--strict must not reuse a report judged without it, exit %d: %s", code, out.String())
	}
	out.Reset()
	other := append(args("--reuse", report), "--runs", "7") // an option the key holds
	if code := Run(other, &out, &errs); code != 0 || strings.Contains(out.String(), "reused") {
		t.Errorf("another option must not reuse, exit %d: %s", code, out.String())
	}
}

// The node that runs the oracle is part of what a latency cell judged.
func TestNodeIdentity_namesTheExecutableThatRunsTheOracle(t *testing.T) {
	if got := nodeIdentity("fire", "/oracle", "/bin/true"); got != "" {
		t.Errorf("a cell that runs no oracle has no node: %q", got)
	}
	if got := nodeIdentity("latency", "", "/bin/true"); got != "" {
		t.Errorf("no oracle, no node: %q", got)
	}
	a, b, c := nodeIdentity("latency", "/oracle", "/bin/true"), nodeIdentity("all", "/oracle", "/bin/false"), nodeIdentity("latency", "/oracle", "/does/not/exist")
	if a == "" || b == "" || c == "" || a == b || a == c || b == c {
		t.Errorf("identities must differ: %q %q %q", a, b, c)
	}
	if a != nodeIdentity("latency", "/oracle", "/bin/true") {
		t.Error("the same node has two identities")
	}
	k1, _ := ReportKey(repoRoot(t), "crw", "plugin", "latency", a)
	k2, _ := ReportKey(repoRoot(t), "crw", "plugin", "latency", c)
	if k1 == k2 {
		t.Error("another node shares a key")
	}
}
