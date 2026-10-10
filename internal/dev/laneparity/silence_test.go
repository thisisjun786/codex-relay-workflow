//go:build dev

package laneparity

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func receiptOf(fixture, leg string, exit int, stdout string) Receipt {
	return Receipt{Fixture: fixture, Leg: leg, Exit: exit, StdoutSHA256: StdoutDigest(stdout)}
}

// The silence judgement: every firing of a ported leg exited 0 with no output, no invocation record
// is left, and every leg a fixture was chosen for fired. The completion Stop is not behind the switch.
func TestJudgeSilence(t *testing.T) {
	legs := []string{"a", "b"}
	quiet := FireReport{Receipts: []Receipt{receiptOf("f1", "a", 0, ""), receiptOf("f2", "b", 0, ""), receiptOf("f3", CompletionLeg, 0, "answers")}}
	if got := judgeSilence(SwitchOff, quiet, legs); !got.OK || got.Firings != 2 || got.Legs != 2 {
		t.Errorf("%+v", got)
	}
	for name, c := range map[string]struct {
		rep  FireReport
		want func(SilenceReport) bool
	}{
		"a leg that printed": {FireReport{Receipts: []Receipt{receiptOf("f1", "a", 0, "{}"), receiptOf("f2", "b", 0, "")}},
			func(s SilenceReport) bool { return len(s.Loud) == 1 && strings.Contains(s.Loud[0], "printed") }},
		"a leg that failed": {FireReport{Receipts: []Receipt{receiptOf("f1", "a", 0, ""), receiptOf("f2", "b", 1, "")}},
			func(s SilenceReport) bool { return len(s.Loud) == 1 && strings.Contains(s.Loud[0], "exit 1") }},
		"a record": {FireReport{Receipts: quiet.Receipts, Recorded: []string{"f1: tree/codex/crw/hook-observations/x.json"}},
			func(s SilenceReport) bool { return len(s.Recorded) == 1 }},
		"a leg that never fired": {FireReport{Receipts: []Receipt{receiptOf("f1", "a", 0, "")}},
			func(s SilenceReport) bool { return slices.Equal(s.Missing, []string{"b"}) }},
		"no firing at all": {FireReport{Receipts: []Receipt{receiptOf("f", CompletionLeg, 0, "")}},
			func(s SilenceReport) bool { return s.Firings == 0 && len(s.Missing) == 2 }},
	} {
		if got := judgeSilence(SwitchCXC, c.rep, legs); got.OK || !c.want(got) {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

// One fixture per ported leg: one that answers when the leg has one, else the first by id; a fixture
// that did not run or did not match is not chosen, and neither is the completion Stop.
func TestSilenceFixtures_oneAnsweringFixturePerLeg(t *testing.T) {
	base := FireReport{Fixtures: []FixtureFire{
		{ID: "hook__a__quiet", Leg: "a", Run: true, OK: true, Silent: true},
		{ID: "hook__a__loud_2", Leg: "a", Run: true, OK: true},
		{ID: "hook__a__loud_1", Leg: "a", Run: true, OK: true},
		{ID: "hook__b__quiet_2", Leg: "b", Run: true, OK: true, Silent: true},
		{ID: "hook__b__quiet_1", Leg: "b", Run: true, OK: true, Silent: true},
		{ID: "hook__c__pending", Leg: "c"},
		{ID: "hook__d__failed", Leg: "d", Run: true},
		{ID: "hook__" + CompletionLeg + "__x", Leg: CompletionLeg, Run: true, OK: true},
	}}
	ids, legs := silenceFixtures(base)
	if !slices.Equal(ids, []string{"hook__a__loud_1", "hook__b__quiet_1"}) || !slices.Equal(legs, []string{"a", "b"}) {
		t.Errorf("%q %q", ids, legs)
	}
}

// The ported legs fired with the switch off and at cxc are silent, run against real commands: one
// fixture per leg of a slice of the corpus, with the shipped plugin's own declarations.
func TestSilence_thePortedLegsAreSilentWithTheSwitchOffAndAtCxc(t *testing.T) {
	o := fireFixture(t)
	o.Plugin = filepath.Join(o.Root, "plugins", "crw")
	base, err := Fire(o)
	if err != nil || !base.OK {
		t.Fatalf("%v %q", err, base.ReceiptProblems)
	}
	for _, state := range SilenceStates {
		got, err := Silence(o, state, base)
		if err != nil {
			t.Fatal(err)
		}
		if !got.OK || got.Fixtures < 4 || got.Legs < 4 || got.Firings < got.Fixtures || got.State != state {
			t.Errorf("%s: %+v", state, got)
		}
		if state == SwitchOff && !got.Switch.Absent || state == SwitchCXC && got.Switch.Active != "cxc" {
			t.Errorf("%s: switch %+v", state, got.Switch)
		}
	}
}

// A run of fire reports the cell: the report holds the switch off and at cxc, both silent, and a
// fault run does not run them.
func TestRun_aFireRunReportsTheSwitchSilence(t *testing.T) {
	o := fireFixture(t)
	report := filepath.Join(t.TempDir(), "report.json")
	var out, errs bytes.Buffer
	if code := Run([]string{"fire", "--crw", o.CRW, "--plugin", o.Plugin, "--scratch", o.Scratch, "--only", slice.String(), "--json", report}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %.600s %s", code, out.String(), errs.String())
	}
	raw, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	var rep Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.SwitchSilence) != 2 || rep.SwitchSilence[0].State != SwitchOff || rep.SwitchSilence[1].State != SwitchCXC || !rep.SwitchSilence[0].OK || !rep.SwitchSilence[1].OK {
		t.Errorf("%+v", rep.SwitchSilence)
	}
	if !strings.Contains(out.String(), "switch off: ") || !strings.Contains(out.String(), "switch cxc: ") {
		t.Errorf("%.600s", out.String())
	}
	out.Reset()
	if code := Run([]string{"fire", "--crw", o.CRW, "--plugin", o.Plugin, "--scratch", o.Scratch, "--only", slice.String(), "--inject", FaultDropStdout, "--json", report}, &out, &errs); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(out.String(), "switch off: ") {
		t.Error("a fault run ran the silence cells")
	}
}

// The real-host command on a host with no Codex records the cells as not verified and does not fail.
func TestRun_theRealHostCommandWithoutCodexIsNotVerified(t *testing.T) {
	o := fireFixture(t)
	report := filepath.Join(t.TempDir(), "report.json")
	var out, errs bytes.Buffer
	if code := Run([]string{"realhost", "--crw", o.CRW, "--scratch", o.Scratch, "--codex", filepath.Join(t.TempDir(), "no-codex"), "--json", report}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %.600s %s", code, out.String(), errs.String())
	}
	raw, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	var rep Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.RealHost == nil || rep.RealHost.Skipped == "" || len(rep.RealHost.Cells) != 0 || !rep.OK {
		t.Fatalf("%+v", rep.RealHost)
	}
	have := map[string]bool{}
	for _, n := range rep.NotVerified {
		have[n.Cell] = true
	}
	for _, name := range hostCellNames {
		if !have["real host: "+name] {
			t.Errorf("no not-verified entry for %s", name)
		}
	}
	// the base list keeps the cells the real host would have covered
	if !have["context recovery after a real compaction"] && !slices.ContainsFunc(rep.NotVerified, func(n NotVerified) bool { return strings.HasPrefix(n.Cell, "context recovery") }) {
		t.Errorf("%+v", rep.NotVerified)
	}
	if !strings.Contains(out.String(), "NOT VERIFIED: real host: turn/crw") {
		t.Errorf("%.600s", out.String())
	}
}
