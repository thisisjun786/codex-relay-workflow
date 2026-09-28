package hook

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func Test33StatusKeepsRegistrationSettingsDistinct(t *testing.T) {
	home := t.TempDir()
	entry := filepath.Join(home, entryPointName)
	if err := os.WriteFile(entry, []byte("# adapter"), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(home, ConfigName)
	writeStatusJSON(t, settings, map[string]any{"configVersion": 1, "event": "Stop", "relayExecutable": filepath.Join(home, "relay"), "markerRoot": filepath.Join(home, "marker"), "dbPath": nil, "mode": "observe", "timeoutSeconds": 5, "journalRoot": filepath.Join(home, "journal"), "journalPolicy": "every_invocation", "installedBy": "CRW-37", "isolationAssertedBy": nil})
	writeStatusJSON(t, filepath.Join(home, "hooks.json"), map[string]any{"hooks": map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "python3 " + entry + " relative-settings.json", "timeout": 10}, map[string]any{"type": "command", "command": "python3 " + entry + " " + settings, "timeout": 10}}}}}})

	got := Status(context.Background(), home, map[string]string{"CODEX_HOME": home}, "Stop")
	configuration := got["configuration"].(map[string]any)
	if configuration["value"] != registrationAmbiguous {
		t.Fatalf("configuration=%v", configuration)
	}
	absence := got["firingRecordAbsence"].(map[string]any)
	candidates := absence["candidates"].([]any)
	if candidates[0].(map[string]any)["cause"] != "record_path_unidentified" || candidates[1].(map[string]any)["cause"] != "nothing_recorded" {
		t.Fatalf("absence=%v", absence)
	}
}

func Test33StatusNamesRelativeRegistrationSettings(t *testing.T) {
	home := t.TempDir()
	entry := filepath.Join(home, entryPointName)
	if err := os.WriteFile(entry, []byte("# adapter"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeStatusJSON(t, filepath.Join(home, "hooks.json"), map[string]any{"hooks": map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "python3 " + entry + " relative-settings.json", "timeout": 10}}}}}})
	got := Status(context.Background(), home, nil, "Stop")
	if value := got["configuration"].(map[string]any)["value"]; value != registrationRelative {
		t.Fatalf("configuration value=%v", value)
	}
}

func Test33StatusProbesRuntimeCapabilityNotPresenceAlone(t *testing.T) {
	home := t.TempDir()
	runtime := filepath.Join(home, "codex-session-relay")
	if err := os.WriteFile(runtime, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeStatusJSON(t, filepath.Join(home, ConfigName), map[string]any{"configVersion": 1, "event": "Stop", "relayExecutable": runtime, "markerRoot": filepath.Join(home, "marker"), "dbPath": nil, "mode": "observe", "timeoutSeconds": 5, "journalRoot": filepath.Join(home, "journal"), "journalPolicy": "every_invocation", "installedBy": "CRW-37", "isolationAssertedBy": nil})
	got := Status(context.Background(), home, nil, "Stop")
	if got["relayExecutable"].(map[string]any)["value"] != present || got["guardEvaluateOffered"].(map[string]any)["value"] != "guard_rejected_the_call" {
		t.Fatalf("status=%v", got)
	}
}

func writeStatusJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}
