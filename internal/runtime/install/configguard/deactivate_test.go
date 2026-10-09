package configguard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const deactivationConfig = "[memories]\ngenerate_memories = true\ndedicated_tools = true\n"

func deactivationKey(prior *string) TableKeyRecord {
	return TableKeyRecord{"memories", "dedicated_tools", prior, "true", true}
}
func deactivationManifest(t *testing.T, home string, keys map[string]TableKeyRecord, flags map[string]FlagRecord) *InstallManifest {
	t.Helper()
	path := filepath.Join(home, "config.toml")
	hash, err := hashOrNull(path)
	if err != nil {
		t.Fatal(err)
	}
	m := &InstallManifest{Version: 2, ConfigPath: path, PostActivateHash: hash, Flags: flags, TableKeys: keys}
	deactivationSaveManifest(t, home, m)
	return m
}
func deactivationSaveManifest(t *testing.T, home string, m *InstallManifest) {
	t.Helper()
	b, err := manifestBytes(m)
	if err != nil {
		t.Fatal(err)
	}
	activationWrite(t, manifestPath(home), string(b))
}
func deactivationRun(t *testing.T, path string, state map[string]bool, calls *[][]string) CodexRunner {
	return func(a []string) CodexRunResult {
		*calls = append(*calls, slices.Clone(a))
		if a[1] == "list" {
			var rows []string
			for _, k := range DeclaredFeatures() {
				rows = append(rows, string(k)+" stable "+map[bool]string{true: "true", false: "false"}[state[string(k)]])
			}
			return CodexRunResult{Stdout: strings.Join(rows, "\n")}
		}
		state[a[2]] = a[1] == "enable"
		content := activationRead(t, path)
		activationWrite(t, path, SetTableKey(content, "features", a[2], state[a[2]]).Content)
		return CodexRunResult{}
	}
}
func deactivationDeps(home string, run CodexRunner) DeactivateDeps {
	return DeactivateDeps{Run: run, CodexHome: home, Now: func() string { return "fixed" }}
}

func deactivationLossOriginal(t *testing.T, id string) string {
	t.Helper()
	var rows []struct{ ID, Classification, Reason, Original string }
	b, err := os.ReadFile("testdata/deactivation-changes.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == id && row.Classification == "intentionally-changed" && row.Reason != "" {
			return row.Original
		}
	}
	t.Fatalf("unclassified loss recording %q", id)
	return ""
}

func TestDeactivateRecordedDecisionTable(t *testing.T) {
	var data struct {
		Decisions []struct {
			Owned, Drift, BackupKnown bool
			Prior, Live, Backup       *string
			Expected                  struct{ Action, Reason string }
		}
	}
	b, err := os.ReadFile("testdata/oracle-deactivate-marker.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Decisions) != 144 {
		t.Fatal("decision recording incomplete")
	}
	for i, row := range data.Decisions {
		rec := deactivationKey(row.Prior)
		rec.SetByCodexclaw = row.Owned
		restore, reason := DecideKeyRestore(rec, row.Live, row.Drift, row.BackupKnown, row.Backup)
		if restore != (row.Expected.Action == "restore") || string(reason) != row.Expected.Reason {
			t.Errorf("recording %d: restore=%v reason=%q want=%+v", i, restore, reason, row.Expected)
		}
	}
}

func TestDeactivateDriftScenarios(t *testing.T) {
	prior := "false"
	for _, c := range []struct {
		name, config, edit, backup, reason string
		prior                              *string
		unowned, restore                   bool
	}{
		{name: "unrelated_without_backup", config: deactivationConfig, edit: "# user comment\n" + deactivationConfig, reason: "unverifiable"},
		{name: "unrelated_with_absence_backup", config: deactivationConfig, edit: "# user comment\n" + deactivationConfig, backup: "[memories]\ngenerate_memories = true\n", restore: true},
		{name: "changed", config: "[memories]\ndedicated_tools = false\n", prior: &prior, reason: "changed"},
		{name: "deleted", config: "[memories]\ngenerate_memories = true\n", reason: "missing"},
		{name: "delete_only_owned_line", config: deactivationConfig, restore: true},
		{name: "restore_false", config: deactivationConfig, prior: &prior, restore: true},
		{name: "never_owned", config: deactivationConfig, unowned: true, reason: "changed"},
		{name: "backup_preexisting_key", config: deactivationConfig, edit: "# edit\n" + deactivationConfig, backup: "[memories]\ndedicated_tools = false\n", reason: "unverifiable"},
		{name: "nondestructive_drift", config: deactivationConfig, edit: "# edit\n" + deactivationConfig, prior: &prior, restore: true},
		// CRW-1141: a value the editor does not rewrite is not an absent key; it is no longer the value CRW applied.
		{name: "unsupported_reads_as_changed", config: "[memories]\ndedicated_tools = [true]\n", reason: "changed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := activationHome(t)
			path := filepath.Join(home, "config.toml")
			activationWrite(t, path, c.config)
			key := deactivationKey(c.prior)
			key.SetByCodexclaw = !c.unowned
			m := deactivationManifest(t, home, map[string]TableKeyRecord{"memories.dedicated_tools": key}, nil)
			if c.backup != "" {
				backup := path + ".bak"
				activationWrite(t, backup, c.backup)
				m.BackupPath = &backup
				deactivationSaveManifest(t, home, m)
			}
			before := c.config
			if c.edit != "" {
				before = c.edit
				activationWrite(t, path, before)
			}
			var calls [][]string
			r, err := Deactivate(deactivationDeps(home, deactivationRun(t, path, nil, &calls)))
			if err != nil || r.NoManifest || r.FileDrifted != (c.edit != "") {
				t.Fatalf("result=%+v error=%v", r, err)
			}
			if c.restore {
				want := strings.Replace(before, "dedicated_tools = true\n", "", 1)
				if c.prior != nil {
					want = strings.Replace(before, "dedicated_tools = true", "dedicated_tools = "+*c.prior, 1)
				}
				if !reflect.DeepEqual(r.RestoredKeys, []string{"memories.dedicated_tools"}) || activationRead(t, path) != want {
					t.Fatalf("result=%+v config=%q want=%q", r, activationRead(t, path), want)
				}
			} else if !reflect.DeepEqual(r.SkippedExternal, []SkippedExternal{{"memories.dedicated_tools", SkipReason(c.reason)}}) || activationRead(t, path) != before {
				t.Fatalf("result=%+v config=%q", r, activationRead(t, path))
			}
		})
	}
}

func TestDeactivateActivationRoundTrip(t *testing.T) {
	for _, drift := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_drift", true: "unrelated_edit"}[drift], func(t *testing.T) {
			home := activationHome(t)
			path := filepath.Join(home, "config.toml")
			activationWrite(t, path, "# original\n[features]\nmulti_agent = true\n")
			state := map[string]bool{"multi_agent": true}
			var calls [][]string
			run := deactivationRun(t, path, state, &calls)
			if _, err := Activate(ActivateDeps{Run: run, CodexHome: home, Now: func() string { return "fixed" }}); err != nil {
				t.Fatal(err)
			}
			if drift {
				activationWrite(t, path, activationRead(t, path)+"# user edit\n")
			}
			beforeManifest := activationRead(t, manifestPath(home))
			r, err := Deactivate(deactivationDeps(home, run))
			if err != nil || r.FileDrifted != drift || !reflect.DeepEqual(r.Disabled, []string{"goals", "hooks", "default_mode_request_user_input"}) || !reflect.DeepEqual(r.SkippedPreExisting, []string{"multi_agent"}) {
				t.Fatalf("result=%+v error=%v", r, err)
			}
			// The records stay as evidence; a completed deactivation adds only its release (CRW-1145).
			before, after := parseInstallManifest(beforeManifest), parseInstallManifest(activationRead(t, manifestPath(home)))
			if before == nil || after == nil || after.ReleasedAt == nil || !reflect.DeepEqual(before.Flags, after.Flags) || !reflect.DeepEqual(before.TableKeys, after.TableKeys) || before.BackupPath == nil || after.BackupPath == nil || *before.BackupPath != *after.BackupPath {
				t.Fatalf("the deactivation changed the ownership records: %+v -> %+v", before, after)
			}
			if !state["multi_agent"] || state["goals"] || state["hooks"] || strings.Contains(activationRead(t, path), "dedicated_tools") {
				t.Fatal("roundtrip changed ownership or retained an owned setting")
			}
			if drift && !strings.Contains(activationRead(t, path), "# user edit\n") {
				t.Fatal("foreign comment lost")
			}
		})
	}
}

func TestDeactivateFlagPathsAndOrdering(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "list_unavailable"}[broken], func(t *testing.T) {
			home := activationHome(t)
			path := filepath.Join(home, "config.toml")
			activationWrite(t, path, deactivationConfig)
			m := deactivationManifest(t, home, map[string]TableKeyRecord{"memories.dedicated_tools": deactivationKey(nil)}, map[string]FlagRecord{
				"multi_agent": {PriorEnabled: true, EnabledByCodexclaw: true}, "goals": {EnabledByCodexclaw: true}, "hooks": {EnabledByCodexclaw: true},
				"default_mode_request_user_input": {EnableFailed: true}, "unknown": {EnabledByCodexclaw: true}, "failed": {EnabledByCodexclaw: true},
			})
			m.flagOrder = []string{"multi_agent", "goals", "hooks", "default_mode_request_user_input", "unknown", "failed"}
			deactivationSaveManifest(t, home, m)
			var calls [][]string
			goals := true
			run := func(a []string) CodexRunResult {
				calls = append(calls, slices.Clone(a))
				if strings.Contains(activationRead(t, path), "dedicated_tools") {
					t.Fatal("table restoration must precede every CLI call")
				}
				if a[1] == "list" {
					if broken {
						return CodexRunResult{ExitCode: 127}
					}
					return CodexRunResult{Stdout: fmt.Sprintf("goals stable %t\nhooks stable false\n", goals)}
				}
				if a[2] == "failed" {
					return CodexRunResult{ExitCode: 9}
				}
				if a[2] == "goals" {
					goals = false
				}
				return CodexRunResult{}
			}
			r, err := Deactivate(deactivationDeps(home, run))
			want := []string{"goals", "unknown"}
			failed := []string{"failed"}
			if broken {
				// CRW-1143: with the list unreadable an exit 0 cannot be confirmed, so those flags stay crw's and are
				// reported as failures.
				want, failed = []string{}, []string{"failed", "goals", "hooks", "unknown"}
			}
			var gotFailed []string
			for _, f := range r.Failed {
				gotFailed = append(gotFailed, f.Key)
			}
			if err != nil || r.FeaturesStateUnavailable != broken || !reflect.DeepEqual(r.Disabled, want) || !reflect.DeepEqual(gotFailed, failed) || !reflect.DeepEqual(r.SkippedPreExisting, []string{"multi_agent"}) {
				t.Fatalf("result=%+v error=%v calls=%v", r, err, calls)
			}
			if !broken && !reflect.DeepEqual(r.SkippedExternal, []SkippedExternal{{"hooks", SkipMissing}}) {
				t.Fatal(r.SkippedExternal)
			}
		})
	}
}

func TestDeactivateEarlyReturnsAndV1(t *testing.T) {
	for _, kind := range []string{"absent", "malformed", "unreadable", "v1"} {
		t.Run(kind, func(t *testing.T) {
			home := activationHome(t)
			path := filepath.Join(home, "config.toml")
			activationWrite(t, path, deactivationConfig)
			if kind == "malformed" {
				activationWrite(t, manifestPath(home), "bad{")
			} else if kind == "unreadable" {
				if err := os.Mkdir(manifestPath(home), 0700); err != nil {
					t.Fatal(err)
				}
			} else if kind == "v1" {
				b, _ := json.Marshal(map[string]any{"version": 1, "configPath": path, "flags": map[string]any{}})
				activationWrite(t, manifestPath(home), string(b))
			}
			calls := 0
			r, err := Deactivate(deactivationDeps(home, func([]string) CodexRunResult { calls++; return CodexRunResult{} }))
			if err != nil || r.NoManifest != (kind != "v1") || calls != map[bool]int{false: 0, true: 1}[kind == "v1"] || activationRead(t, path) != deactivationConfig {
				t.Fatalf("result=%+v error=%v calls=%d", r, err, calls)
			}
			marker, err := ReadSelfHealMarkerFile(home)
			if err != nil || marker == nil || marker.OptedOut == nil || !*marker.OptedOut || marker.OptedOutAt == nil || *marker.OptedOutAt != "fixed" {
				t.Fatalf("optout=%+v error=%v", marker, err)
			}
			encoded, err := json.Marshal(r)
			if err != nil || strings.Contains(string(encoded), "null") {
				t.Fatalf("empty arrays must serialize as []: %s %v", encoded, err)
			}
		})
	}
}

func TestDeactivateUnreadableSettingsRefusedBeforeCLI(t *testing.T) {
	for _, withKey := range []bool{false, true} {
		t.Run(map[bool]string{false: "unreadable_config_no_table_keys", true: "unreadable_config"}[withKey], func(t *testing.T) {
			home := activationHome(t)
			path := filepath.Join(home, "config.toml")
			original := deactivationLossOriginal(t, map[bool]string{false: "unreadable_config_no_table_keys", true: "unreadable_config"}[withKey])
			activationWrite(t, path, original)
			keys := map[string]TableKeyRecord{}
			if withKey {
				keys["memories.dedicated_tools"] = deactivationKey(nil)
			}
			m := deactivationManifest(t, home, keys, map[string]FlagRecord{"goals": {EnabledByCodexclaw: true}})
			m.PostActivateHash = nil
			deactivationSaveManifest(t, home, m)
			if err := os.Chmod(path, 0200); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(path, 0600) })
			if _, err := os.ReadFile(path); err == nil {
				t.Skip("effective privileges allow reading mode0200")
			}
			calls := 0
			_, err := Deactivate(deactivationDeps(home, func([]string) CodexRunResult { calls++; activationWrite(t, path, "lost"); return CodexRunResult{} }))
			if err == nil || calls != 0 {
				t.Fatalf("unreadable settings reached CLI: error=%v calls=%d", err, calls)
			}
			_ = os.Chmod(path, 0600)
			if activationRead(t, path) != original {
				t.Fatal("settings lost")
			}
		})
	}
}

func TestDeactivateOverrideMissingConfigAndBackup(t *testing.T) {
	for _, backupKind := range []string{"absent", "unreadable", "dangling"} {
		t.Run(backupKind, func(t *testing.T) {
			home := activationHome(t)
			path := filepath.Join(home, "config.toml")
			activationWrite(t, path, deactivationConfig)
			m := deactivationManifest(t, home, map[string]TableKeyRecord{"memories.dedicated_tools": deactivationKey(nil)}, nil)
			backup := path + ".bak"
			m.BackupPath = &backup
			deactivationSaveManifest(t, home, m)
			if backupKind == "unreadable" {
				if err := os.Mkdir(backup, 0700); err != nil {
					t.Fatal(err)
				}
			} else if backupKind == "dangling" {
				if err := os.Symlink("missing-backup", backup); err != nil {
					t.Fatal(err)
				}
			}
			override := path + ".override"
			original := "# foreign edit\n" + deactivationConfig
			activationWrite(t, override, original)
			deps := deactivationDeps(home, func([]string) CodexRunResult { return CodexRunResult{} })
			deps.ConfigPath = override
			r, err := Deactivate(deps)
			if err != nil || !r.FileDrifted || !reflect.DeepEqual(r.SkippedExternal, []SkippedExternal{{"memories.dedicated_tools", SkipUnverifiable}}) || activationRead(t, override) != original || activationRead(t, path) != deactivationConfig {
				t.Fatalf("result=%+v error=%v", r, err)
			}
			// An absent config skips table restoration but still handles manifest flags.
			deps.ConfigPath = path + ".absent"
			r, err = Deactivate(deps)
			if err != nil || !r.FileDrifted || len(r.SkippedExternal) != 0 || len(r.RestoredKeys) != 0 {
				t.Fatalf("absent result=%+v error=%v", r, err)
			}
		})
	}
}

func TestDeactivateNoHashNoopAndDefaultClock(t *testing.T) {
	home := activationHome(t)
	path := filepath.Join(home, "config.toml")
	activationWrite(t, path, deactivationConfig)
	prior := "true"
	m := deactivationManifest(t, home, map[string]TableKeyRecord{"memories.dedicated_tools": deactivationKey(&prior)}, nil)
	m.PostActivateHash = nil
	deactivationSaveManifest(t, home, m)
	r, err := Deactivate(DeactivateDeps{CodexHome: home, Run: func([]string) CodexRunResult { return CodexRunResult{} }})
	if err != nil || r.FileDrifted || !reflect.DeepEqual(r.RestoredKeys, []string{"memories.dedicated_tools"}) || activationRead(t, path) != deactivationConfig {
		t.Fatalf("noop result=%+v error=%v", r, err)
	}
	marker, err := ReadSelfHealMarkerFile(home)
	if err != nil || marker == nil || marker.OptedOutAt == nil || !strings.HasSuffix(*marker.OptedOutAt, "Z") || len(*marker.OptedOutAt) != 24 {
		t.Fatalf("clock marker=%+v error=%v", marker, err)
	}
}

func TestDeactivatePublicationFailureKeepsWholeSettings(t *testing.T) {
	home := activationHome(t)
	sub := filepath.Join(home, "settings")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sub, "config.toml")
	activationWrite(t, path, deactivationConfig)
	m := deactivationManifest(t, home, map[string]TableKeyRecord{"memories.dedicated_tools": deactivationKey(nil)}, nil)
	m.ConfigPath, m.PostActivateHash = path, nil
	deactivationSaveManifest(t, home, m)
	if err := os.Chmod(sub, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0700) })
	probe, err := os.CreateTemp(sub, ".probe-")
	if err == nil {
		_ = probe.Close()
		_ = os.Remove(probe.Name())
		t.Skip("effective privileges allow creating in mode0500")
	}
	calls := 0
	_, err = Deactivate(deactivationDeps(home, func([]string) CodexRunResult { calls++; return CodexRunResult{} }))
	if err == nil || calls != 0 || activationRead(t, path) != deactivationConfig {
		t.Fatalf("publication failure lost settings: error=%v calls=%d", err, calls)
	}
}
