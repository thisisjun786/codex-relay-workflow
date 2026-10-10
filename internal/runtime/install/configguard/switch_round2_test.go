package configguard

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// The tests below are the six findings of the CRW-201 round-2 verification.

const switchCXCTable = `plugins."codexclaw@codexclaw"`

func switchOtherCRWRole(name string) string {
	body := "# another crw build's " + name + "\nname = \"" + name + "\"\n"
	return fmt.Sprintf("# crw-managed: %x\n%s", sha256.Sum256([]byte(body)), body)
}

func switchMustRun(t *testing.T, home, target string) *SwitchReport {
	t.Helper()
	r, err := RunSwitch(switchDeps(home), target)
	if err != nil {
		t.Fatalf("switch %s: %v", target, err)
	}
	return r
}

func switchRoleAction(r *SwitchReport, name string) SwitchRoleChange {
	for _, c := range r.Roles {
		if c.Role == name {
			return c
		}
	}
	return SwitchRoleChange{}
}

// Finding 1: TOML reads "enabled" and 'enabled' as the key enabled; the switch edits that line and
// adds no second key.
func TestSwitchEditsAQuotedEnabledKeyInPlace(t *testing.T) {
	for name, config := range map[string]string{
		"basic quoted":   "[plugins.\"codexclaw@codexclaw\"]\n\"enabled\" = true\n",
		"literal quoted": "[plugins.\"codexclaw@codexclaw\"]\n  'enabled'=true # cxc\nother = 1\n",
	} {
		t.Run(name, func(t *testing.T) {
			home := switchHost(t, config)
			before := switchTree(t, home)
			switchMustRun(t, home, "crw")
			mid := activationRead(t, filepath.Join(home, "config.toml"))
			if n := strings.Count(mid, "enabled"); n != 1 {
				t.Fatalf("config after switch crw names enabled %d times:\n%s", n, mid)
			}
			if st := ReadTableKeyLine(mid, switchCXCTable, "enabled"); !st.Found || st.Value != "false" {
				t.Fatalf("enabled after switch crw = %+v\n%q", st, mid)
			}
			switchMustRun(t, home, "cxc")
			after := switchTreeWithoutSwitchFiles(t, home)
			delete(before, InstallManifestName)
			if d := switchDiff(before, after); d != "" {
				t.Fatalf("host differs after the round trip:\n%s", d)
			}
		})
	}
	t.Run("the key twice is refused", func(t *testing.T) {
		home := switchHost(t, "[plugins.\"codexclaw@codexclaw\"]\nenabled = true\n\"enabled\" = true\n")
		before := switchTree(t, home)
		if _, err := RunSwitch(switchDeps(home), "crw"); err == nil || !strings.Contains(err.Error(), "will not rewrite") {
			t.Fatalf("err = %v", err)
		}
		if d := switchDiff(before, switchTree(t, home)); d != "" {
			t.Fatalf("a refused switch wrote:\n%s", d)
		}
	})
}

// Finding 2: a CXC role file that appears after the first switch is backed up when the next switch
// replaces it, and the way back restores it.
func TestSwitchRestoresACXCRoleFileThatAppearedAfterTheFirstSwitch(t *testing.T) {
	home := switchHost(t, switchConfigs["standard"])
	path := filepath.Join(home, "agents", "architect.toml")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	switchMustRun(t, home, "crw")
	activationWrite(t, path, switchCXCRole("architect"))
	r := switchMustRun(t, home, "crw")
	if c := switchRoleAction(r, "architect"); c.Action != "replaced-cxc" || c.BackupPath == nil {
		t.Fatalf("architect on the second switch = %+v", c)
	}
	m, err := ReadInstallManifest(home)
	if err != nil || m == nil || m.Switch == nil {
		t.Fatalf("manifest = %+v, %v", m, err)
	}
	for _, rr := range m.Switch.Roles {
		if rr.Role == "architect" && (rr.PriorOwner != string(role.OwnerCXC) || rr.BackupPath == nil) {
			t.Fatalf("architect record = %+v", rr)
		}
	}
	r = switchMustRun(t, home, "cxc")
	if c := switchRoleAction(r, "architect"); c.Action != "restored-cxc" {
		t.Fatalf("architect on the way back = %+v", c)
	}
	if got := activationRead(t, path); got != switchCXCRole("architect") {
		t.Fatalf("architect.toml after the way back = %q", got)
	}
}

// Finding 3: a recorded key whose value became a form crw will not rewrite is refused in both
// directions, and the record stays.
func TestSwitchRefusesAnUnsupportedValueAfterTheFirstSwitch(t *testing.T) {
	for _, target := range []string{"crw", "cxc"} {
		t.Run(target, func(t *testing.T) {
			home := switchHost(t, switchConfigs["standard"])
			switchMustRun(t, home, "crw")
			cfgPath := filepath.Join(home, "config.toml")
			activationWrite(t, cfgPath, strings.Replace(activationRead(t, cfgPath), "enabled = false", "enabled = [true]", 1))
			if !strings.Contains(activationRead(t, cfgPath), "enabled = [true]") {
				t.Fatal("fixture did not apply")
			}
			before := switchTree(t, home)
			if _, err := RunSwitch(switchDeps(home), target); err == nil || !strings.Contains(err.Error(), "will not rewrite") {
				t.Fatalf("err = %v", err)
			}
			if d := switchDiff(before, switchTree(t, home)); d != "" {
				t.Fatalf("a refused switch wrote:\n%s", d)
			}
			m, _ := ReadInstallManifest(home)
			if m == nil || m.Switch == nil || len(m.Switch.Keys) != 1 || len(m.Switch.Roles) != 2 {
				t.Fatalf("the record was not kept: %+v", m)
			}
		})
	}
}

// Finding 4: the way back puts a recorded line back when the key line was deleted since, and refuses,
// keeping the record, when the table itself is gone.
func TestSwitchBackRestoresADeletedKeyLine(t *testing.T) {
	const prior = "[plugins.\"codexclaw@codexclaw\"]\nenabled = false # off\nmarketplace = \"x\"\n\n[features]\ngoals = true\n"
	t.Run("key line deleted", func(t *testing.T) {
		home := switchHost(t, prior)
		switchMustRun(t, home, "crw")
		cfgPath := filepath.Join(home, "config.toml")
		activationWrite(t, cfgPath, strings.Replace(activationRead(t, cfgPath), "enabled = false # off\n", "", 1))
		switchMustRun(t, home, "cxc")
		got := activationRead(t, cfgPath)
		if st := ReadTableKeyLine(got, switchCXCTable, "enabled"); !st.Found || st.Line != "enabled = false # off" {
			t.Fatalf("the recorded line was not put back: %+v\n%q", st, got)
		}
	})
	t.Run("table deleted", func(t *testing.T) {
		home := switchHost(t, prior)
		switchMustRun(t, home, "crw")
		cfgPath := filepath.Join(home, "config.toml")
		activationWrite(t, cfgPath, "[features]\ngoals = true\n")
		before := switchTree(t, home)
		if _, err := RunSwitch(switchDeps(home), "cxc"); err == nil {
			t.Fatal("a way back that cannot put the recorded line back succeeded")
		}
		if d := switchDiff(before, switchTree(t, home)); d != "" {
			t.Fatalf("a refused switch wrote:\n%s", d)
		}
		m, _ := ReadInstallManifest(home)
		if m == nil || m.Switch == nil || len(m.Switch.Keys) != 1 || m.Switch.Keys[0].PriorLine == nil {
			t.Fatalf("the record was not kept: %+v", m)
		}
	})
}

// Finding 5: a CRW role file that is valid but not the one the switch installed (crw register from
// another build) is kept by the way back.
func TestSwitchBackKeepsACRWRoleFileUpdatedSince(t *testing.T) {
	for _, priorOwner := range []string{"none", "codexclaw"} {
		t.Run(priorOwner, func(t *testing.T) {
			home := switchHost(t, switchConfigs["standard"])
			path := filepath.Join(home, "agents", "architect.toml")
			if priorOwner == "none" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			switchMustRun(t, home, "crw")
			updated := switchOtherCRWRole("architect")
			if role.OwnerOf([]byte(updated)) != role.OwnerCRW {
				t.Fatal("fixture is not CRW-owned")
			}
			activationWrite(t, path, updated)
			r := switchMustRun(t, home, "cxc")
			if c := switchRoleAction(r, "architect"); c.Action != "left-modified" {
				t.Fatalf("architect on the way back = %+v", c)
			}
			if got := activationRead(t, path); got != updated {
				t.Fatalf("the updated CRW role file was changed: %q", got)
			}
		})
	}
}

// Finding 6: an agents directory that is a symlink is refused as role registration refuses it, and
// nothing behind it changes.
func TestSwitchRefusesASymlinkedAgentsDirectory(t *testing.T) {
	for _, target := range []string{"crw", "cxc"} {
		t.Run(target, func(t *testing.T) {
			home := switchHost(t, switchConfigs["standard"])
			if target == "cxc" {
				switchMustRun(t, home, "crw")
			}
			outside := t.TempDir()
			agents := filepath.Join(home, "agents")
			if err := os.Rename(agents, filepath.Join(outside, "agents")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(outside, "agents"), agents); err != nil {
				t.Fatal(err)
			}
			outsideBefore := switchTree(t, outside)
			cfgBefore := activationRead(t, filepath.Join(home, "config.toml"))
			if _, err := RunSwitch(switchDeps(home), target); err == nil || !strings.Contains(err.Error(), "non-regular agents directory") {
				t.Fatalf("err = %v", err)
			}
			if d := switchDiff(outsideBefore, switchTree(t, outside)); d != "" {
				t.Fatalf("files behind the symlink changed:\n%s", d)
			}
			if got := activationRead(t, filepath.Join(home, "config.toml")); got != cfgBefore {
				t.Fatalf("config.toml changed by a refused switch")
			}
		})
	}
}
