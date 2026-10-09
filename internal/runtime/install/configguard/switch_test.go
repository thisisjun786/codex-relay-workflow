package configguard

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/hookswitch"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

const switchStampA = "2026-10-10T01:00:00.000Z"

func switchDeps(home string) SwitchDeps {
	return SwitchDeps{CodexHome: home, Now: func() string { return switchStampA }}
}

func switchCXCRole(name string) string {
	body := "# codexclaw subagent role: " + name + "\nname = \"" + name + "\"\n"
	return fmt.Sprintf("# codexclaw-managed: %x\n%s", sha256.Sum256([]byte(body)), body)
}

// switchHost is a Codex home with config.toml and CXC-owned role files.
func switchHost(t *testing.T, config string) string {
	t.Helper()
	home := t.TempDir()
	if config != "" {
		activationWrite(t, filepath.Join(home, "config.toml"), config)
	}
	if err := os.MkdirAll(filepath.Join(home, "agents"), 0o777); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"executor", "architect"} {
		activationWrite(t, filepath.Join(home, "agents", name+".toml"), switchCXCRole(name))
	}
	return home
}

type switchSnapshot map[string]string

// switchTree reads every file under home (relative path to content) except the lock sidecar and backups.
func switchTree(t *testing.T, home string) switchSnapshot {
	t.Helper()
	tree := switchSnapshot{}
	err := filepath.WalkDir(home, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(home, p)
		if strings.Contains(rel, ".crw-") && strings.HasSuffix(rel, ".bak") || strings.HasSuffix(rel, ".crw-lock") {
			return nil
		}
		b, err := os.ReadFile(p)
		tree[rel] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func switchDiff(a, b switchSnapshot) string {
	var out []string
	for k, v := range a {
		if w, ok := b[k]; !ok {
			out = append(out, "only before: "+k)
		} else if v != w {
			out = append(out, fmt.Sprintf("changed: %s\n  before %q\n  after  %q", k, v, w))
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			out = append(out, "only after: "+k)
		}
	}
	return strings.Join(out, "\n")
}

var switchConfigs = map[string]string{
	"standard":           "model = \"x\"\n\n[plugins.\"crw@crw\"]\nenabled = true\n\n[plugins.\"codexclaw@codexclaw\"]\nenabled = true\n\n[features]\ngoals = true\n",
	"tight spacing":      "[plugins.\"codexclaw@codexclaw\"]\nenabled=true\n[plugins.\"crw@crw\"]\nenabled=true\n",
	"tabs and comment":   "[plugins.\"codexclaw@codexclaw\"]\n\tenabled\t=\ttrue   # keep me\n",
	"crlf":               "[plugins.\"crw@crw\"]\r\nenabled = true\r\n[plugins.\"codexclaw@codexclaw\"]\r\nenabled=true\r\nother = 1\r\n",
	"mixed endings":      "[plugins.\"codexclaw@codexclaw\"]\r\nenabled=true\nother = 1\r\n\n",
	"no final newline":   "[plugins.\"codexclaw@codexclaw\"]\nenabled=true",
	"key absent":         "[plugins.\"codexclaw@codexclaw\"]\nmarketplace = \"x\"\n\n[features]\ngoals = true\n",
	"key absent at eof":  "[plugins.\"codexclaw@codexclaw\"]\nmarketplace = \"x\"",
	"already disabled":   "[plugins.\"codexclaw@codexclaw\"]\nenabled=false\n",
	"table is missing":   "[plugins.\"crw@crw\"]\nenabled = true\n",
	"header with spaces": "[plugins.\"codexclaw@codexclaw\"]   # cxc\n  enabled   =   true\n",
}

func TestSwitchRoundTripGivesTheConfigBackToTheByte(t *testing.T) {
	for name, config := range switchConfigs {
		t.Run(name, func(t *testing.T) {
			home := switchHost(t, config)
			before := switchTree(t, home)
			r, err := RunSwitch(switchDeps(home), "crw")
			if err != nil {
				t.Fatal(err)
			}
			mid := activationRead(t, filepath.Join(home, "config.toml"))
			if strings.Contains(config, `[plugins."codexclaw@codexclaw"]`) {
				if st := ReadTableKeyLine(mid, `plugins."codexclaw@codexclaw"`, "enabled"); !st.Found || st.Value != "false" {
					t.Fatalf("codexclaw enabled after switch crw = %+v\n%q", st, mid)
				}
				if !r.ConfigChanged && !strings.Contains(name, "already") {
					t.Fatalf("config not reported changed: %+v", r)
				}
			} else if mid != config {
				t.Fatalf("a config without the CXC table was edited: %q", mid)
			}
			if got := strings.Count(mid, `[plugins."crw@crw"]`); got != strings.Count(config, `[plugins."crw@crw"]`) {
				t.Fatalf("crw@crw table changed: %q", mid)
			}
			if crw := ReadTableKeyLine(mid, `plugins."crw@crw"`, "enabled"); strings.Contains(config, `"crw@crw"`) && (!crw.Found || crw.Value != "true") {
				t.Fatalf("crw@crw enabled was touched: %+v", crw)
			}
			if _, err := RunSwitch(switchDeps(home), "cxc"); err != nil {
				t.Fatal(err)
			}
			after := switchTree(t, home)
			delete(after, switchFileRel(t))
			delete(after, InstallManifestName)
			delete(before, InstallManifestName)
			if d := switchDiff(before, after); d != "" {
				t.Fatalf("host differs after cxc -> crw -> cxc:\n%s", d)
			}
		})
	}
}

func TestSwitchWritesStateAndTheSwitchSectionApart(t *testing.T) {
	home := switchHost(t, switchConfigs["standard"])
	if _, err := RunSwitch(switchDeps(home), "crw"); err != nil {
		t.Fatal(err)
	}
	st, err := hookswitch.Load(home)
	if err != nil || st == nil || st.Active != hookswitch.CRW || st.ChangedAt != switchStampA || st.By != SwitchBy {
		t.Fatalf("switch.json = %+v, %v", st, err)
	}
	m, err := ReadInstallManifest(home)
	if err != nil || m == nil || m.Switch == nil {
		t.Fatalf("manifest = %+v, %v", m, err)
	}
	if len(m.TableKeys) != 0 || len(m.Flags) != 0 {
		t.Fatalf("the switch wrote ordinary manifest entries: %+v %+v", m.TableKeys, m.Flags)
	}
	sw := m.Switch
	if sw.Active != "crw" || sw.Pending || sw.ConfigBackup == nil || len(sw.Keys) != 1 || len(sw.Roles) != 2 {
		t.Fatalf("switch section = %+v", sw)
	}
	if k := sw.Keys[0]; k.Table != `plugins."codexclaw@codexclaw"` || k.Key != "enabled" || k.PriorLine == nil || *k.PriorLine != "enabled = true" || k.AppliedValue != "false" {
		t.Fatalf("key record = %+v", k)
	}
	if b, err := os.ReadFile(*sw.ConfigBackup); err != nil || string(b) != switchConfigs["standard"] {
		t.Fatalf("config backup = %q, %v", b, err)
	}
	for _, r := range sw.Roles {
		if r.PriorOwner != "codexclaw" || r.BackupPath == nil || r.AppliedDigest == "" {
			t.Fatalf("role record = %+v", r)
		}
		if b, err := os.ReadFile(*r.BackupPath); err != nil || string(b) != switchCXCRole(r.Role) {
			t.Fatalf("role backup %s = %q, %v", r.Role, b, err)
		}
	}
	if _, err := RunSwitch(switchDeps(home), "cxc"); err != nil {
		t.Fatal(err)
	}
	m, _ = ReadInstallManifest(home)
	if m.Switch == nil || m.Switch.Active != "cxc" || len(m.Switch.Keys) != 0 || len(m.Switch.Roles) != 0 {
		t.Fatalf("switch section after cxc = %+v", m.Switch)
	}
	if st, _ := hookswitch.Load(home); st.Active != hookswitch.CXC {
		t.Fatalf("switch.json after cxc = %+v", st)
	}
}

func TestSwitchRoleFiles(t *testing.T) {
	t.Run("a CXC role is backed up and replaced, then restored", func(t *testing.T) {
		home := switchHost(t, switchConfigs["standard"])
		r, err := RunSwitch(switchDeps(home), "crw")
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range r.Roles {
			if c.Action != "replaced-cxc" || c.BackupPath == nil {
				t.Fatalf("role change = %+v", c)
			}
			b, _ := os.ReadFile(c.Path)
			if role.OwnerOf(b) != role.OwnerCRW {
				t.Fatalf("%s is not CRW-owned after switch crw", c.Path)
			}
		}
		r, err = RunSwitch(switchDeps(home), "cxc")
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range r.Roles {
			if c.Action != "restored-cxc" {
				t.Fatalf("role change = %+v", c)
			}
			if got := activationRead(t, c.Path); got != switchCXCRole(c.Role) {
				t.Fatalf("%s not restored byte for byte: %q", c.Path, got)
			}
		}
	})
	t.Run("an absent role is installed and removed again", func(t *testing.T) {
		home := switchHost(t, switchConfigs["standard"])
		if err := os.Remove(filepath.Join(home, "agents", "architect.toml")); err != nil {
			t.Fatal(err)
		}
		if _, err := RunSwitch(switchDeps(home), "crw"); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(home, "agents", "architect.toml"))
		if err != nil || role.OwnerOf(b) != role.OwnerCRW {
			t.Fatalf("architect.toml after crw: %q, %v", b, err)
		}
		if _, err := RunSwitch(switchDeps(home), "cxc"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(home, "agents", "architect.toml")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("architect.toml remains: %v", err)
		}
	})
	t.Run("a user's own role file is never replaced", func(t *testing.T) {
		home := switchHost(t, switchConfigs["standard"])
		mine := "# my own executor\nname = \"executor\"\n"
		activationWrite(t, filepath.Join(home, "agents", "executor.toml"), mine)
		r, err := RunSwitch(switchDeps(home), "crw")
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range r.Roles {
			if c.Role == "executor" && c.Action != "kept-other" {
				t.Fatalf("executor change = %+v", c)
			}
		}
		if got := activationRead(t, filepath.Join(home, "agents", "executor.toml")); got != mine {
			t.Fatalf("user's role file changed: %q", got)
		}
		if _, err := RunSwitch(switchDeps(home), "cxc"); err != nil {
			t.Fatal(err)
		}
		if got := activationRead(t, filepath.Join(home, "agents", "executor.toml")); got != mine {
			t.Fatalf("user's role file changed by the way back: %q", got)
		}
	})
	t.Run("a CRW role file the user edited is left on the way back", func(t *testing.T) {
		home := switchHost(t, switchConfigs["standard"])
		if _, err := RunSwitch(switchDeps(home), "crw"); err != nil {
			t.Fatal(err)
		}
		edited := activationRead(t, filepath.Join(home, "agents", "executor.toml")) + "# mine\n"
		activationWrite(t, filepath.Join(home, "agents", "executor.toml"), edited)
		r, err := RunSwitch(switchDeps(home), "cxc")
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range r.Roles {
			if c.Role == "executor" && c.Action != "left-modified" {
				t.Fatalf("executor change = %+v", c)
			}
		}
		if got := activationRead(t, filepath.Join(home, "agents", "executor.toml")); got != edited {
			t.Fatalf("edited role file changed: %q", got)
		}
	})
}

func TestSwitchTwiceKeepsTheFirstValues(t *testing.T) {
	home := switchHost(t, switchConfigs["tight spacing"])
	before := switchTree(t, home)
	for _, target := range []string{"crw", "crw", "cxc", "cxc"} {
		if _, err := RunSwitch(switchDeps(home), target); err != nil {
			t.Fatalf("switch %s: %v", target, err)
		}
	}
	after := switchTree(t, home)
	for _, k := range []string{InstallManifestName, switchFileRel(t)} {
		delete(before, k)
		delete(after, k)
	}
	if d := switchDiff(before, after); d != "" {
		t.Fatalf("host differs:\n%s", d)
	}
}

func TestSwitchFailureUndoesEveryStepToTheByte(t *testing.T) {
	cases := map[string][]string{
		"crw": {"manifest", "state", "config", "role:architect", "role-replace:architect", "role:executor", "role-replace:executor", "manifest-final"},
		"cxc": {"manifest", "state", "config", "role:architect", "role:executor", "manifest-final"},
	}
	for target, steps := range cases {
		for _, step := range steps {
			t.Run(target+" fails at "+step, func(t *testing.T) {
				home := switchHost(t, switchConfigs["tight spacing"])
				if target == "cxc" {
					if _, err := RunSwitch(switchDeps(home), "crw"); err != nil {
						t.Fatal(err)
					}
				}
				before := switchTree(t, home)
				deps := switchDeps(home)
				boom := errors.New("injected failure")
				deps.Fail = func(s string) error {
					if s == step {
						return boom
					}
					return nil
				}
				if _, err := RunSwitch(deps, target); !errors.Is(err, boom) {
					t.Fatalf("err = %v, want the injected failure", err)
				}
				after := switchTree(t, home)
				if d := switchDiff(before, after); d != "" {
					t.Fatalf("the failed switch left the host changed:\n%s", d)
				}
			})
		}
	}
}

func TestSwitchRefusesWhatItCannotRead(t *testing.T) {
	t.Run("damaged manifest", func(t *testing.T) {
		home := switchHost(t, switchConfigs["standard"])
		activationWrite(t, filepath.Join(home, InstallManifestName), "{not json")
		before := switchTree(t, home)
		if _, err := RunSwitch(switchDeps(home), "crw"); err == nil || !strings.Contains(err.Error(), "not a readable install manifest") {
			t.Fatalf("err = %v", err)
		}
		if d := switchDiff(before, switchTree(t, home)); d != "" {
			t.Fatalf("a refused switch wrote:\n%s", d)
		}
	})
	t.Run("a value form crw will not rewrite", func(t *testing.T) {
		home := switchHost(t, "[plugins.\"codexclaw@codexclaw\"]\nenabled = [true]\n")
		before := switchTree(t, home)
		if _, err := RunSwitch(switchDeps(home), "crw"); err == nil || !strings.Contains(err.Error(), "will not rewrite") {
			t.Fatalf("err = %v", err)
		}
		if d := switchDiff(before, switchTree(t, home)); d != "" {
			t.Fatalf("a refused switch wrote:\n%s", d)
		}
	})
	t.Run("a damaged switch section", func(t *testing.T) {
		home := switchHost(t, switchConfigs["standard"])
		activationWrite(t, filepath.Join(home, InstallManifestName), `{"version":2,"configPath":"x","flags":{},"tableKeys":{},"switch":{"active":"both"}}`)
		if _, err := RunSwitch(switchDeps(home), "crw"); err == nil {
			t.Fatal("a damaged switch section was accepted")
		}
	})
	t.Run("an unknown target", func(t *testing.T) {
		if _, err := RunSwitch(switchDeps(t.TempDir()), "both"); err == nil {
			t.Fatal("target both accepted")
		}
	})
}

func TestSwitchSectionSurvivesTheOtherManifestWriters(t *testing.T) {
	home := switchHost(t, switchConfigs["standard"])
	if _, err := RunSwitch(switchDeps(home), "crw"); err != nil {
		t.Fatal(err)
	}
	want := activationRead(t, filepath.Join(home, InstallManifestName))
	section := func() string {
		m, err := ReadInstallManifest(home)
		if err != nil || m == nil || m.Switch == nil {
			t.Fatalf("manifest = %+v, %v", m, err)
		}
		b, _ := manifestBytes(&InstallManifest{Version: 2, ConfigPath: "x", Flags: map[string]FlagRecord{}, TableKeys: map[string]TableKeyRecord{}, Switch: m.Switch})
		return string(b)
	}
	base := section()

	var calls [][]string
	state := map[string]bool{}
	if _, err := Activate(activationDeps(t, home, state, &calls)); err != nil {
		t.Fatal(err)
	}
	if got := section(); got != base {
		t.Fatalf("Activate changed the switch section:\n%s\nwant\n%s", got, base)
	}
	if _, err := ApplyManagedKey(ConfigSetDeps{CodexHome: home}, "memories.dedicated_tools", ptrBool(false)); err != nil {
		t.Fatal(err)
	}
	if got := section(); got != base {
		t.Fatalf("ApplyManagedKey changed the switch section")
	}
	before := activationRead(t, filepath.Join(home, InstallManifestName))
	if _, err := Deactivate(DeactivateDeps{CodexHome: home, Run: activationRun(t, home, state, &calls)}); err != nil {
		t.Fatal(err)
	}
	if got := activationRead(t, filepath.Join(home, InstallManifestName)); got != before {
		t.Fatalf("Deactivate rewrote the manifest")
	}
	_ = want
	cfg := activationRead(t, filepath.Join(home, "config.toml"))
	if st := ReadTableKeyLine(cfg, `plugins."codexclaw@codexclaw"`, "enabled"); !st.Found || st.Value != "false" {
		t.Fatalf("features disable undid the switch: %+v", st)
	}
}

func ptrBool(b bool) *bool { return &b }

// switchCrashed stands for a process that died: the panic skips every rollback.
type switchCrashed struct{}

// switchRunCrashing runs a switch that dies just before its at-th step (1-based) and reports whether it did.
func switchRunCrashing(t *testing.T, deps SwitchDeps, target string, at int) (crashed bool) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(switchCrashed); !ok {
				panic(r)
			}
			crashed = true
		}
	}()
	n := 0
	deps.Fail = func(string) error {
		if n++; n == at {
			panic(switchCrashed{})
		}
		return nil
	}
	if _, err := RunSwitch(deps, target); err != nil {
		t.Fatalf("switch %s without a crash: %v", target, err)
	}
	return false
}

func switchTreeWithoutSwitchFiles(t *testing.T, home string) switchSnapshot {
	tree := switchTree(t, home)
	delete(tree, switchFileRel(t))
	delete(tree, InstallManifestName)
	return tree
}

// A switch that dies at any write boundary is finished by running it again, and the way back to CXC
// from there gives the host back to the byte (verification finding 1 and 5 of CRW-201 round 1).
func TestSwitchDiedAtEveryBoundaryIsFinishedAndReturnsToCXC(t *testing.T) {
	for _, cfgName := range []string{"standard", "tight spacing", "key absent"} {
		for _, first := range []string{"crw", "cxc"} {
			for _, how := range []string{"again", "straight back"} {
				for at := 1; ; at++ {
					name := fmt.Sprintf("%s/%s dies at step %d then %s", cfgName, first, at, how)
					home := switchHost(t, switchConfigs[cfgName])
					orig := switchTreeWithoutSwitchFiles(t, home)
					if first == "cxc" {
						if _, err := RunSwitch(switchDeps(home), "crw"); err != nil {
							t.Fatal(err)
						}
					}
					if !switchRunCrashing(t, switchDeps(home), first, at) {
						break
					}
					if how == "again" {
						if _, err := RunSwitch(switchDeps(home), first); err != nil {
							t.Fatalf("%s: running it again: %v", name, err)
						}
					}
					if _, err := RunSwitch(switchDeps(home), "cxc"); err != nil {
						t.Fatalf("%s: the way back: %v", name, err)
					}
					if d := switchDiff(orig, switchTreeWithoutSwitchFiles(t, home)); d != "" {
						t.Fatalf("%s: host differs from before the first switch:\n%s", name, d)
					}
					m, err := ReadInstallManifest(home)
					if err != nil || (m != nil && m.Switch != nil && (m.Switch.Pending || m.Switch.Active != "cxc" || len(m.Switch.Roles) != 0)) {
						t.Fatalf("%s: manifest after the way back = %+v, %v", name, m, err)
					}
				}
			}
		}
	}
}

// The way back is recorded as pending before it changes anything (finding 6).
func TestSwitchBackToCXCLeavesAPendingSectionWhenItDies(t *testing.T) {
	home := switchHost(t, switchConfigs["standard"])
	if _, err := RunSwitch(switchDeps(home), "crw"); err != nil {
		t.Fatal(err)
	}
	// Dies before the config step: the state file already says cxc.
	if !switchRunCrashing(t, switchDeps(home), "cxc", 3) {
		t.Fatal("no crash")
	}
	m, err := ReadInstallManifest(home)
	if err != nil || m == nil || m.Switch == nil {
		t.Fatalf("manifest = %+v, %v", m, err)
	}
	if !m.Switch.Pending || m.Switch.Active != "cxc" || len(m.Switch.Roles) != 2 || len(m.Switch.Keys) != 1 {
		t.Fatalf("switch section after a died return = %+v", m.Switch)
	}
}

// A CXC plugin table that appears after the first switch is switched off by the next one, and the
// way back gives its line back (finding 2).
func TestSwitchTakesUpACXCTableThatAppearedAfterTheFirstSwitch(t *testing.T) {
	home := switchHost(t, switchConfigs["table is missing"])
	if _, err := RunSwitch(switchDeps(home), "crw"); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(home, "config.toml")
	added := activationRead(t, cfgPath) + "\n[plugins.\"codexclaw@codexclaw\"]\nenabled=true\n"
	activationWrite(t, cfgPath, added)
	r, err := RunSwitch(switchDeps(home), "crw")
	if err != nil {
		t.Fatal(err)
	}
	if st := ReadTableKeyLine(activationRead(t, cfgPath), `plugins."codexclaw@codexclaw"`, "enabled"); !st.Found || st.Value != "false" {
		t.Fatalf("the new CXC table was left on: %+v (report %+v)", st, r)
	}
	if _, err := RunSwitch(switchDeps(home), "cxc"); err != nil {
		t.Fatal(err)
	}
	if got := activationRead(t, cfgPath); got != added {
		t.Fatalf("config after the way back:\n%q\nwant\n%q", got, added)
	}
}

// The activation's drift hash moves with the switch only when the file was still the one the
// activation left (finding 3).
func TestSwitchKeepsTheActivationDriftHonest(t *testing.T) {
	hashOf := func(t *testing.T, home string) string {
		sum := sha256.Sum256([]byte(activationRead(t, filepath.Join(home, "config.toml"))))
		return fmt.Sprintf("%x", sum)
	}
	activate := func(t *testing.T) string {
		home := switchHost(t, switchConfigs["standard"])
		var calls [][]string
		if _, err := Activate(activationDeps(t, home, map[string]bool{}, &calls)); err != nil {
			t.Fatal(err)
		}
		return home
	}
	t.Run("an untouched config follows the switch", func(t *testing.T) {
		home := activate(t)
		if _, err := RunSwitch(switchDeps(home), "crw"); err != nil {
			t.Fatal(err)
		}
		m, _ := ReadInstallManifest(home)
		if m.PostActivateHash == nil || *m.PostActivateHash != hashOf(t, home) {
			t.Fatalf("PostActivateHash = %v, want the hash of the switched file", m.PostActivateHash)
		}
	})
	t.Run("a drifted config stays drifted", func(t *testing.T) {
		home := activate(t)
		m0, _ := ReadInstallManifest(home)
		cfgPath := filepath.Join(home, "config.toml")
		activationWrite(t, cfgPath, activationRead(t, cfgPath)+"# edited by hand\n")
		for _, target := range []string{"crw", "cxc"} {
			if _, err := RunSwitch(switchDeps(home), target); err != nil {
				t.Fatal(err)
			}
			m, _ := ReadInstallManifest(home)
			if m.PostActivateHash == nil || *m.PostActivateHash != *m0.PostActivateHash {
				t.Fatalf("switch %s moved the activation hash %v -> %v", target, *m0.PostActivateHash, m.PostActivateHash)
			}
		}
	})
}

// A CRW role file that differs from this build's template is not rewritten without a way back
// (finding 4): the switch leaves it, and the way back leaves it too.
func TestSwitchKeepsAnExistingCRWRoleFile(t *testing.T) {
	home := switchHost(t, switchConfigs["standard"])
	want, err := role.NativeRoleContent(role.Architect)
	if err != nil {
		t.Fatal(err)
	}
	body := "# an older crw architect\nname = \"architect\"\n"
	old := fmt.Sprintf("# crw-managed: %x\n%s", sha256.Sum256([]byte(body)), body)
	if role.OwnerOf([]byte(old)) != role.OwnerCRW || old == string(want) {
		t.Fatalf("the fixture is not an older CRW role file: %q", old)
	}
	path := filepath.Join(home, "agents", "architect.toml")
	activationWrite(t, path, old)
	r, err := RunSwitch(switchDeps(home), "crw")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range r.Roles {
		if c.Role == "architect" && (c.Action != "kept-crw" || c.BackupPath != nil) {
			t.Fatalf("architect change = %+v", c)
		}
	}
	if got := activationRead(t, path); got != old {
		t.Fatalf("an existing CRW role file was rewritten: %q", got)
	}
	if _, err := RunSwitch(switchDeps(home), "cxc"); err != nil {
		t.Fatal(err)
	}
	if got := activationRead(t, path); got != old {
		t.Fatalf("the way back changed an existing CRW role file: %q", got)
	}
}

// A switch that dies after writing config.toml but before its manifest still leaves the activation's
// drift hash right once it is run again.
func TestSwitchDiedAtEveryBoundaryKeepsTheActivationHash(t *testing.T) {
	for at := 1; ; at++ {
		home := switchHost(t, switchConfigs["standard"])
		var calls [][]string
		if _, err := Activate(activationDeps(t, home, map[string]bool{}, &calls)); err != nil {
			t.Fatal(err)
		}
		if !switchRunCrashing(t, switchDeps(home), "crw", at) {
			break
		}
		if _, err := RunSwitch(switchDeps(home), "crw"); err != nil {
			t.Fatalf("step %d: %v", at, err)
		}
		m, _ := ReadInstallManifest(home)
		sum := sha256.Sum256([]byte(activationRead(t, filepath.Join(home, "config.toml"))))
		if m.PostActivateHash == nil || *m.PostActivateHash != fmt.Sprintf("%x", sum) || m.Switch.ConfigHash != nil || m.Switch.Pending {
			t.Fatalf("step %d: hash %v, switch %+v", at, m.PostActivateHash, m.Switch)
		}
	}
}

// switchFileRel is switch.json relative to the Codex home, as switchTree keys it.
func switchFileRel(t *testing.T) string {
	t.Helper()
	rel, err := filepath.Rel("/h", hookswitch.Path("/h"))
	if err != nil {
		t.Fatal(err)
	}
	return rel
}
