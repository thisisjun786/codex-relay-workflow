package configguard

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const configSetKey = "memories.dedicated_tools"
const configSetOriginal = "[memories]\ngenerate_memories = true\n"

func configSetHome(t *testing.T, content string, withManifest bool) (string, string) {
	t.Helper()
	home := t.TempDir()
	for _, key := range []string{"HOME", "CODEX_HOME", "CRW_HOME"} {
		t.Setenv(key, home)
	}
	path := filepath.Join(home, "config.toml")
	activationWrite(t, path, content)
	if withManifest {
		m := &InstallManifest{Version: 1, ActivatedAt: "2026-08-29T00:00:00.000Z", ConfigPath: path, Flags: map[string]FlagRecord{}, TableKeys: map[string]TableKeyRecord{}}
		b, err := manifestBytes(m)
		if err != nil {
			t.Fatal(err)
		}
		activationWrite(t, manifestPath(home), string(b))
	}
	return home, path
}

func configSetApply(t *testing.T, home, path string, value *bool) ConfigSetOutcome {
	t.Helper()
	r, err := ApplyManagedKey(ConfigSetDeps{CodexHome: home, ConfigPath: path, Now: func() string { return "2026-08-29T01:02:03.456Z" }}, configSetKey, value)
	if err != nil || !r.OK {
		t.Fatalf("outcome %+v error %v", r, err)
	}
	return r
}

func configSetManifest(t *testing.T, home string) *InstallManifest {
	t.Helper()
	m := parseInstallManifest(activationRead(t, manifestPath(home)))
	if m == nil {
		t.Fatal("invalid saved manifest")
	}
	return m
}

// CXC config-set.test.ts:37-60: the injected disable consumer actually restores the key.
func TestConfigSetRecordedAndDeactivationReverts(t *testing.T) {
	home, path := configSetHome(t, configSetOriginal, true)
	value := true
	r := configSetApply(t, home, path, &value)
	if !r.Changed || r.PriorValue != nil || r.AppliedValue != "true" || ManagedKeyID(r.Entry) != configSetKey {
		t.Fatalf("%+v", r)
	}
	m := configSetManifest(t, home)
	rec := m.TableKeys[configSetKey]
	if m.Version != 2 || rec.PriorValue != nil || rec.AppliedValue != "true" || !rec.SetByCodexclaw {
		t.Fatalf("%+v", m)
	}
	run := func([]string) CodexRunResult { return CodexRunResult{} }
	d, err := Deactivate(DeactivateDeps{Run: run, CodexHome: home, ConfigPath: path})
	if err != nil || !reflect.DeepEqual(d.RestoredKeys, []string{configSetKey}) || activationRead(t, path) != configSetOriginal {
		t.Fatalf("%+v %v", d, err)
	}
}

// CXC config-set.test.ts:63-71,93-111: backup, original prior, unset and record removal.
func TestConfigSetBackupRepeatAndUnset(t *testing.T) {
	original := "[memories]\ndedicated_tools = false # keep\nforeign = true\n"
	home, path := configSetHome(t, original, true)
	value := true
	r := configSetApply(t, home, path, &value)
	wantBackup := path + ".crw-2026-08-29T01-02-03-456Z.bak"
	if r.BackupPath == nil || *r.BackupPath != wantBackup || activationRead(t, wantBackup) != original {
		t.Fatalf("backup %+v", r)
	}
	second := configSetApply(t, home, path, &value)
	if second.Changed || second.BackupPath != nil || second.PriorValue == nil || *second.PriorValue != "true" {
		t.Fatalf("repeat %+v", second)
	}
	rec := configSetManifest(t, home).TableKeys[configSetKey]
	if rec.PriorValue == nil || *rec.PriorValue != "false" || !rec.SetByCodexclaw {
		t.Fatalf("history %+v", rec)
	}
	r = configSetApply(t, home, path, nil)
	if !r.Changed || r.PriorValue == nil || *r.PriorValue != "false" || r.AppliedValue != "false" || activationRead(t, path) != original {
		t.Fatalf("unset %+v", r)
	}
	if _, ok := configSetManifest(t, home).TableKeys[configSetKey]; ok {
		t.Fatal("unset retained record")
	}
}

// CXC config-set.test.ts:73-91,113-118: refusals never mutate config or manifest.
func TestConfigSetRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, id, content, reason string
		manifest                  bool
		value                     *bool
	}{
		{name: "no manifest", id: configSetKey, content: configSetOriginal, reason: "run 'crw install features enable' first"},
		{name: "foreign key", id: "tools.dangerous", content: configSetOriginal, reason: "not a crw-managed key", manifest: true},
		{name: "unrecorded", id: configSetKey, content: configSetOriginal, reason: "not recorded", manifest: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, path := configSetHome(t, tc.content, tc.manifest)
			before, _ := os.ReadFile(manifestPath(home))
			r, err := ApplyManagedKey(ConfigSetDeps{CodexHome: home}, tc.id, tc.value)
			after, _ := os.ReadFile(manifestPath(home))
			if err != nil || r.OK || !strings.Contains(r.Reason, tc.reason) || activationRead(t, path) != tc.content || string(before) != string(after) {
				t.Fatalf("%+v %v", r, err)
			}
		})
	}
	entry, reason := ResolveManagedKey("tools.dangerous")
	if entry != nil || !strings.Contains(reason, "crw install config list") {
		t.Fatalf("%+v %q", entry, reason)
	}
	entry, reason = ResolveManagedKey(" \ufeff" + configSetKey + "\t")
	if entry == nil || ManagedKeyID(*entry) != configSetKey || reason != "" {
		t.Fatalf("%+v %q", entry, reason)
	}
}

// CXC config-set.test.ts:120-127: live whitelist values, including missing config.
func TestConfigSetReadManagedState(t *testing.T) {
	home, path := configSetHome(t, configSetOriginal, true)
	states, err := ReadManagedState(path)
	if err != nil || len(states) != 1 || states[0].Value != nil {
		t.Fatalf("%+v %v", states, err)
	}
	activationWrite(t, path, "[memories]\ndedicated_tools = true\n")
	states, err = ReadManagedState(path)
	if err != nil || states[0].Value == nil || *states[0].Value != "true" {
		t.Fatalf("%+v %v", states, err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	states, err = ReadManagedState(path)
	if err != nil || states[0].Value != nil {
		t.Fatalf("%+v %v", states, err)
	}
	value := false
	r := configSetApply(t, home, path, &value)
	if !r.Changed || r.BackupPath != nil || r.PriorValue != nil || activationRead(t, path) != "[memories]\ndedicated_tools = false\n" {
		t.Fatalf("%+v", r)
	}
	r = configSetApply(t, home, path, nil)
	if r.AppliedValue != "(absent)" || strings.Contains(activationRead(t, path), "dedicated_tools") {
		t.Fatalf("%+v", r)
	}
}

func TestConfigSetNoopOwnershipAndUnsupportedValues(t *testing.T) {
	home, path := configSetHome(t, "[memories]\ndedicated_tools = true\n", true)
	value := true
	r := configSetApply(t, home, path, &value)
	rec := configSetManifest(t, home).TableKeys[configSetKey]
	if r.Changed || rec.SetByCodexclaw || rec.PriorValue == nil || *rec.PriorValue != "true" {
		t.Fatalf("%+v %+v", r, rec)
	}
	r = configSetApply(t, home, path, nil)
	if r.Changed || r.AppliedValue != "true" {
		t.Fatalf("%+v", r)
	}
	// CRW-1141: an unterminated string is a config.toml that does not decode, refused as such rather than as a value form.
	for raw, reason := range map[string]string{"[true]": "will not rewrite", `"oops`: "is not valid TOML"} {
		content := "[memories]\ndedicated_tools = " + raw + "\n"
		activationWrite(t, path, content)
		before := activationRead(t, manifestPath(home))
		r, err := ApplyManagedKey(ConfigSetDeps{CodexHome: home}, configSetKey, &value)
		if err != nil || r.OK || !strings.Contains(r.Reason, reason) || activationRead(t, path) != content || activationRead(t, manifestPath(home)) != before {
			t.Fatalf("%+v %v", r, err)
		}
	}
	activationWrite(t, path, "[memories]\ndedicated_tools = false\n")
	configSetApply(t, home, path, &value)
	activationWrite(t, path, "[memories]\ndedicated_tools = [true]\n")
	r, err := ApplyManagedKey(ConfigSetDeps{CodexHome: home}, configSetKey, nil)
	if err != nil || r.OK || !strings.Contains(r.Reason, "will not rewrite") {
		t.Fatalf("%+v %v", r, err)
	}
}

// Intentionally-changed: prevent data loss from the oracle's in-place writes by
// publishing whole files through the existing fsync/rename owner.
func TestConfigSetAtomicSettingsManifestBackupAndSymlink(t *testing.T) {
	home, path := configSetHome(t, configSetOriginal, true)
	manifest := manifestPath(home)
	backup := path + ".crw-2026-08-29T01-02-03-456Z.bak"
	activationWrite(t, backup, "old backup")
	originalManifest := activationRead(t, manifest)
	for _, file := range []string{path, manifest, backup} {
		if err := os.Link(file, file+".old-inode"); err != nil {
			t.Fatal(err)
		}
	}
	target := path + ".target"
	if err := os.Rename(path, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	value := true
	configSetApply(t, home, path, &value)
	if link, err := os.Readlink(path); err != nil || link != target {
		t.Fatalf("link %q %v", link, err)
	}
	for file, want := range map[string]string{path: configSetOriginal, manifest: originalManifest, backup: "old backup"} {
		if got := activationRead(t, file+".old-inode"); got != want {
			t.Fatalf("%s truncated/reused: %q", file, got)
		}
	}
	if activationRead(t, backup) != configSetOriginal {
		t.Fatal("backup content")
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatal("staging leaked")
		}
	}
}

// Intentionally-changed: the protected writer refuses unreadable/dangling
// destinations rather than treating them as absent and replacing them.
func TestConfigSetUnreadableDestinationsAreNotOverwritten(t *testing.T) {
	for _, name := range []string{"config.toml", InstallManifestName, "config.toml.crw-2026-08-29T01-02-03-456Z.bak"} {
		t.Run(name, func(t *testing.T) {
			home, path := configSetHome(t, configSetOriginal, true)
			file := filepath.Join(home, name)
			before := activationRead(t, path)
			if name != filepath.Base(file) {
				t.Fatal("fixture path")
			}
			if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if err := os.Symlink("absent-target", file); err != nil {
				t.Fatal(err)
			}
			value := true
			r, err := ApplyManagedKey(ConfigSetDeps{CodexHome: home, Now: func() string { return "2026-08-29T01:02:03.456Z" }}, configSetKey, &value)
			if r.OK || err == nil && !strings.Contains(r.Reason, "no readable install manifest") {
				t.Fatalf("%+v %v", r, err)
			}
			if target, e := os.Readlink(file); e != nil || target != "absent-target" {
				t.Fatalf("link %q %v", target, e)
			}
			if name != "config.toml" && activationRead(t, path) != before {
				t.Fatal("settings changed before refusal")
			}
		})
	}
	home, path := configSetHome(t, configSetOriginal, true)
	activationWrite(t, manifestPath(home), "{malformed")
	value := true
	r, err := ApplyManagedKey(ConfigSetDeps{CodexHome: home}, configSetKey, &value)
	if err != nil || r.OK || activationRead(t, path) != configSetOriginal || activationRead(t, manifestPath(home)) != "{malformed" {
		t.Fatalf("%+v %v", r, err)
	}
}
