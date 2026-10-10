//go:build dev

package laneparity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
)

// writeScript writes an executable shell script.
func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// setManifest changes fields of the manifest of a copy of the shipped plugin.
func setManifest(t *testing.T, root string, fields map[string]any) string {
	t.Helper()
	plugin := filepath.Join(t.TempDir(), "plugin")
	if err := copyPlugin(filepath.Join(root, "plugins", "crw"), plugin); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(plugin, ".codex-plugin", "plugin.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for k, v := range fields {
		doc[k] = v
	}
	if raw, err = json.Marshal(doc); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return plugin
}

// The plugin cache destination comes from the manifest's name and version; neither may leave the
// cache directory of the run, and a manifest that tries is refused before anything is copied (a
// version of nine "../" components and "victim" put the package at <scratch>/victim and overwrote
// the files there).
func TestRealHost_refusesAManifestWhoseNameOrVersionLeavesThePluginCache(t *testing.T) {
	root := repoRoot(t)
	crw := writeScript(t, t.TempDir(), "crw", "exit 0\n")
	codex := writeScript(t, t.TempDir(), "codex", "exit 0\n")
	up := strings.Repeat("../", 7)
	for name, fields := range map[string]map[string]any{
		"version up":       {"version": up + "victim"},
		"version absolute": {"version": "/tmp"},
		"version dot":      {"version": "."},
		"version nested":   {"version": "1.0/../../../victim"},
		"name up":          {"name": "../../../../../../victim"},
		"name slash":       {"name": "a/b"},
		"name dots":        {"name": ".."},
	} {
		scratch := t.TempDir()
		victim := filepath.Join(scratch, "victim")
		if err := os.MkdirAll(victim, 0o755); err != nil {
			t.Fatal(err)
		}
		sentinel := filepath.Join(victim, "LICENSE")
		if err := os.WriteFile(sentinel, []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
		plugin := setManifest(t, root, fields)
		_, err := RealHost(RealHostOptions{Root: root, CRW: crw, Plugin: plugin, Codex: codex, Scratch: scratch, Only: regexpOf(`^untrusted/crw$`)})
		if err == nil || !strings.Contains(err.Error(), "plugin cache") {
			t.Errorf("%s: error %v, want a refusal naming the plugin cache", name, err)
		}
		if raw, _ := os.ReadFile(sentinel); string(raw) != "keep" {
			t.Errorf("%s: a file outside the run was overwritten: %q", name, raw)
		}
	}
}

// A hook that writes an invocation record while the switch is off or at cxc is not silent, whatever
// the fixture observes: this fixture observes only ws/.codexclaw, so the record is seen where the
// harness takes the seed out, not in the observed tree.
func TestSilence_aRecordTheFixtureDoesNotObserveIsFound(t *testing.T) {
	o := fireFixture(t)
	o.Plugin = filepath.Join(o.Root, "plugins", "crw")
	o.CRW = writeScript(t, t.TempDir(), "crw", "d=\"${CODEX_HOME:-$HOME/.codex}/crw/hook-observations/s/t\"\n/bin/mkdir -p \"$d\" && echo '{}' > \"$d/x.json\"\nexit 0\n")
	id := "hook__session-start-announcing-map-affordance__large_workspace_adds_map_line"
	base := FireReport{Fixtures: []FixtureFire{{ID: id, Leg: "session-start-announcing-map-affordance", Run: true, OK: true}}}
	for _, state := range SilenceStates {
		got, err := Silence(o, state, base)
		if err != nil {
			t.Fatal(err)
		}
		// the fixture's own record, and the ones the harness's own probes left (their trees are never compared)
		if got.OK || !slices.ContainsFunc(got.Recorded, func(r string) bool { return strings.HasPrefix(r, id+": ") && strings.HasSuffix(r, "x.json") }) ||
			!slices.ContainsFunc(got.Recorded, func(r string) bool { return strings.HasPrefix(r, "github-post-denies-inline-body: ") }) {
			t.Errorf("%s: a runtime that records while silent passed: %+v", state, got)
		}
	}
}

// A runtime path that already holds another build is not the build under test: the receipts and the
// latency identity would name one build while the shell started the other.
func TestSeed_refusesARuntimePathThatHoldsAnotherBuild(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	crw := writeScript(t, filepath.Join(root), "crw", "echo build\n")
	other := writeScript(t, filepath.Join(root), "other", "echo other\n")
	link := filepath.Join(home, runtimeBin)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	plan := seedPlan{Switch: SwitchOn, CRW: crw, Runtime: true}
	env := []string{"HOME=" + home, "CODEX_HOME=" + filepath.Join(home, "codex")}
	c := &cxccorpus.Case{Root: root}
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.seed(c, env); err == nil || !strings.Contains(err.Error(), "not the build under test") {
		t.Errorf("a link to another build: %v", err)
	}
	if target, _ := os.Readlink(link); target != other {
		t.Errorf("the refused seed changed the link: %q", target)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, []byte("#!/bin/sh\necho other\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.seed(c, env); err == nil {
		t.Error("a file of other content at the runtime path was taken as the build under test")
	}
	// the build itself, reached by a link, a copy or the same file, is the build
	for name, place := range map[string]func() error{
		"link": func() error { return os.Symlink(crw, link) },
		"copy": func() error { raw, _ := os.ReadFile(crw); return os.WriteFile(link, raw, 0o755) },
	} {
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := place(); err != nil {
			t.Fatal(err)
		}
		undo, err := plan.seed(c, env)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if err := undo(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(link); err != nil {
			t.Errorf("%s: the undo removed a runtime the case had", name)
		}
	}
}

// An actual PermissionRequest hook start is part of the cell's accounting, not an unexpected hook:
// a host that asks for permission starts the declared PermissionRequest hook, and the cell passes;
// a host that does not ask starts none, and one that starts it for some hooks only is a defect.
func TestJudge_aPermissionRequestHookTheHostStartedIsAccountedFor(t *testing.T) {
	_, registered, err := ReadRegistered(filepath.Join(repoRoot(t), "plugins", "crw"))
	if err != nil {
		t.Fatal(err)
	}
	var spec hostCellSpec
	for _, s := range hostCellSpecs() {
		if s.name == CellPermission {
			spec = s
		}
	}
	h := &hostEnv{registered: registered}
	run := func(extra []hostEvent) []string {
		cell := HostCell{Name: spec.name, Switch: spec.state, Trusted: true, Thread: "thread-1", Events: map[string]int{}}
		cell.Firings = firingsDue(t, registered, append(append([]hostEvent{}, spec.events...), extra...), "thread-1")
		h.judge(&cell, &hostRun{requests: []StubRequest{{ToolOutputs: []string{"approval policy is Never"}}}}, spec)
		return cell.Problems
	}
	if p := run(nil); len(p) != 0 {
		t.Errorf("a host that did not ask: %q", p)
	}
	asked := []hostEvent{{"PermissionRequest", "Bash"}}
	if p := run(asked); len(p) != 0 {
		t.Errorf("a host that asked and started the PermissionRequest hook: %q", p)
	}
	cell := HostCell{Firings: []HostFiring{{Leg: "permission-request-allowing-agent-thread", Event: "PermissionRequest"}}}
	checkPermission(&cell, &hostRun{})
	if cell.Unverified != "" || len(cell.Problems) != 0 {
		t.Errorf("%+v", cell)
	}
	// a hook of another event is still unexpected
	cell2 := HostCell{Name: spec.name, Switch: spec.state, Trusted: true, Thread: "thread-1", Events: map[string]int{}}
	cell2.Firings = firingsDue(t, registered, append(append([]hostEvent{}, spec.events...), hostEvent{"PostCompact", ""}), "thread-1")
	h.judge(&cell2, &hostRun{}, spec)
	if !strings.Contains(strings.Join(cell2.Problems, "\n"), "no declared hook is due") {
		t.Errorf("an unrelated start: %q", cell2.Problems)
	}
}

// An optional event counts as raised for the whole event: with two declared PermissionRequest hooks,
// a host that starts both, or neither, passes; one that starts only one of them does not.
func TestJudge_aPartialStartOfTheDeclaredPermissionRequestHooksFails(t *testing.T) {
	_, shipped, err := ReadRegistered(filepath.Join(repoRoot(t), "plugins", "crw"))
	if err != nil {
		t.Fatal(err)
	}
	registered := append([]Registered{}, shipped...)
	registered = append(registered,
		Registered{Event: "PermissionRequest", Matcher: "Bash", Leg: "permission-request-second-leg", Command: "second"},
		Registered{Event: "PermissionRequest", Matcher: "Bash", Leg: "permission-request-third-leg", Command: "third"},
	)
	var spec hostCellSpec
	for _, s := range hostCellSpecs() {
		if s.name == CellPermission {
			spec = s
		}
	}
	h := &hostEnv{registered: registered}
	all := firingsDue(t, registered, []hostEvent{{"PermissionRequest", "Bash"}}, "thread-1")
	if len(all) < 3 {
		t.Fatalf("want three declared PermissionRequest hooks, got %d", len(all))
	}
	base := firingsDue(t, registered, spec.events, "thread-1")
	judge := func(permission []HostFiring) []string {
		cell := HostCell{Name: spec.name, Switch: spec.state, Trusted: true, Thread: "thread-1", Events: map[string]int{}}
		cell.Firings = append(append([]HostFiring{}, base...), permission...)
		h.judge(&cell, &hostRun{requests: []StubRequest{{ToolOutputs: []string{"approval policy is Never"}}}}, spec)
		return cell.Problems
	}
	if p := judge(nil); len(p) != 0 {
		t.Errorf("none started: %q", p)
	}
	if p := judge(all); len(p) != 0 {
		t.Errorf("all started: %q", p)
	}
	for i := range all {
		one := []HostFiring{all[i]}
		if p := judge(one); len(p) == 0 {
			t.Errorf("only %s started: accepted", all[i].Leg)
		}
		rest := append(append([]HostFiring{}, all[:i]...), all[i+1:]...)
		if p := judge(rest); len(p) == 0 {
			t.Errorf("all but %s started: accepted", all[i].Leg)
		}
	}
}
