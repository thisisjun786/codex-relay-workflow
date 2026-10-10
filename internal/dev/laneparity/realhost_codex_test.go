//go:build dev && realhost

package laneparity

import (
	"path/filepath"
	"testing"
)

// The real-host cells against the real Codex binary on PATH: heavy (a few seconds a cell, a real host
// process each), so it is behind the realhost build tag: go test -tags dev,realhost ./internal/dev/laneparity.
// Without a Codex binary the test skips with the reason the report gives.
func TestRealHost_theCellsRunOnTheRealCodex(t *testing.T) {
	crw, err := crwUnderTest()
	if err != nil {
		t.Fatal(err)
	}
	root := repoRoot(t)
	opts := RealHostOptions{Root: root, CRW: crw, Plugin: filepath.Join(root, "plugins", "crw"), Scratch: t.TempDir()}
	rep, err := RealHost(opts)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Skipped != "" {
		t.Skip(rep.Skipped)
	}
	if rep.Bypass || rep.Codex.Version == "" || rep.Codex.SHA256 == "" {
		t.Errorf("%+v", rep)
	}
	for _, a := range rep.Args {
		if a == "--dangerously-bypass-hook-trust" || a == "--dangerously-bypass-approvals-and-sandbox" {
			t.Errorf("flag %s", a)
		}
	}
	byName := map[string]HostCell{}
	for _, c := range rep.Cells {
		byName[c.Name] = c
		if c.Name != CellPermission && (!c.OK || len(c.Problems) != 0) {
			t.Errorf("%s: %q", c.Name, c.Problems)
		}
	}
	for _, name := range hostCellNames {
		if _, ok := byName[name]; !ok {
			t.Errorf("no cell %s", name)
		}
	}
	on := byName[CellTurnCRW]
	if on.Answered == 0 || on.Answered != on.Reached || on.Records == 0 || len(on.Firings) != 21 {
		t.Errorf("turn/crw: %d answered, %d reached the model, %d records, %d starts", on.Answered, on.Reached, on.Records, len(on.Firings))
	}
	for _, name := range []string{CellTurnOff, CellTurnCXC} {
		if c := byName[name]; c.Answered != 0 || c.Records != 0 || len(c.Firings) != 21 {
			t.Errorf("%s: %d answered, %d records, %d starts", name, c.Answered, c.Records, len(c.Firings))
		}
	}
	if c := byName[CellUntrusted]; len(c.Firings) != 0 || c.ModelRequest == 0 {
		t.Errorf("untrusted: %d starts, %d model requests", len(c.Firings), c.ModelRequest)
	}
	if c := byName[CellCompaction]; c.Events["PostCompact"] == 0 || c.Events["SessionStart"] != 18 {
		t.Errorf("compaction: %v", c.Events)
	}
	if c := byName[CellSpawn]; c.Events["SubagentStop"] != 2 {
		t.Errorf("spawn: %v", c.Events)
	}
	if c := byName[CellPermission]; c.Unverified == "" {
		t.Errorf("permission: %+v", c)
	}
	if len(rep.Unverified) != 1 || rep.Unverified[0].Cell != "real host: "+CellPermission {
		t.Errorf("unverified %+v", rep.Unverified)
	}
}

// A recorder that runs the build for no hook, or hands the host nothing of its answer, must turn the
// cell that needs answers red: a host whose hooks do nothing cannot pass.
func TestRealHost_aHookThatDoesNothingOrIsNotHeardFails(t *testing.T) {
	crw, err := crwUnderTest()
	if err != nil {
		t.Fatal(err)
	}
	root := repoRoot(t)
	for _, fault := range []string{FaultNoop, FaultDropStdout} {
		rep, err := RealHost(RealHostOptions{Root: root, CRW: crw, Plugin: filepath.Join(root, "plugins", "crw"), Scratch: t.TempDir(), Fault: fault, Only: regexpOf(`^turn/crw$`)})
		if err != nil {
			t.Fatal(err)
		}
		if rep.Skipped != "" {
			t.Skip(rep.Skipped)
		}
		if rep.OK || len(rep.Cells) != 1 || rep.Cells[0].OK {
			t.Errorf("fault %s was not caught: %+v", fault, rep.Cells)
		}
	}
}
