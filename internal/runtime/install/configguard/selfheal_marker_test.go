package configguard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSelfHealMarkerRecordedAnswers(t *testing.T) {
	var data struct {
		Parses []struct{ Input, Expected string }
		Merges []struct{ Input, Opted, Cleared string }
	}
	b, err := os.ReadFile("testdata/oracle-deactivate-marker.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &data); err != nil {
		t.Fatal(err)
	}
	for _, row := range data.Parses {
		m := ParseSelfHealMarker(row.Input)
		if row.Expected == "null\n" {
			if m != nil {
				t.Errorf("invalid marker accepted: %q", row.Input)
			}
			continue
		}
		if m == nil {
			t.Fatalf("valid marker rejected: %q", row.Input)
		}
		home := activationHome(t)
		if err := WriteSelfHealMarkerFile(home, m); err != nil {
			t.Fatal(err)
		}
		if got := activationRead(t, SelfHealMarkerPath(home)); got != row.Expected {
			t.Errorf("parse %q\ngot %s\nwant %s", row.Input, got, row.Expected)
		}
	}
	for _, row := range data.Merges {
		home := activationHome(t)
		path := SelfHealMarkerPath(home)
		activationWrite(t, path, row.Input)
		if err := MarkSelfHealOptedOut(home, "fixed"); err != nil {
			t.Fatal(err)
		}
		if got := activationRead(t, path); got != row.Opted {
			t.Errorf("optout got%q want%q", got, row.Opted)
		}
		if err := ClearSelfHealOptOut(home); err != nil {
			t.Fatal(err)
		}
		if got := activationRead(t, path); got != row.Cleared {
			t.Errorf("clear got%q want%q", got, row.Cleared)
		}
	}
}

func TestSelfHealMarkerAbsentMalformedAndNewHome(t *testing.T) {
	home := filepath.Join(activationHome(t), "not-created")
	if m, err := ReadSelfHealMarkerFile(home); m != nil || err != nil {
		t.Fatalf("absent=%+v %v", m, err)
	}
	if err := ClearSelfHealOptOut(home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatal("clear created home")
	}
	if err := MarkSelfHealOptedOut(home, "fixed"); err != nil {
		t.Fatal(err)
	}
	if SelfHealMarkerName != "crw-self-heal.json" || SelfHealMarkerPath(home) != filepath.Join(home, SelfHealMarkerName) {
		t.Fatal("name table mismatch")
	}
	path := SelfHealMarkerPath(home)
	for _, malformed := range []string{"bad{", "[]", "null", "true"} {
		activationWrite(t, path, malformed)
		if m, err := ReadSelfHealMarkerFile(home); m != nil || err != nil {
			t.Fatalf("malformed=%+v %v", m, err)
		}
		if err := ClearSelfHealOptOut(home); err != nil || activationRead(t, path) != malformed {
			t.Fatal("clear replaced malformed marker")
		}
	}
}

func TestSelfHealMarkerUnreadablePreservesConsent(t *testing.T) {
	home := activationHome(t)
	path := SelfHealMarkerPath(home)
	original := deactivationLossOriginal(t, "unreadable_marker")
	activationWrite(t, path, original)
	if err := os.Chmod(path, 0200); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0600) })
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("effective privileges allow reading mode0200")
	}
	if _, err := ReadSelfHealMarkerFile(home); err == nil {
		t.Fatal("read hid refusal")
	}
	for _, err := range []error{MarkSelfHealOptedOut(home, "fixed"), ClearSelfHealOptOut(home), WriteSelfHealMarkerFile(home, &SelfHealMarker{})} {
		if err == nil {
			t.Fatal("unreadable consent record replaced")
		}
	}
	// The optimization remains non-gating on the no-manifest uninstall path.
	r, err := Deactivate(deactivationDeps(home, func([]string) CodexRunResult { t.Fatal("CLI called"); return CodexRunResult{} }))
	if err != nil || !r.NoManifest {
		t.Fatalf("marker gated uninstall: %+v %v", r, err)
	}
	_ = os.Chmod(path, 0600)
	if activationRead(t, path) != original {
		t.Fatal("consent fields lost")
	}
}

func TestSelfHealMarkerAtomicPublication(t *testing.T) {
	home := activationHome(t)
	path := SelfHealMarkerPath(home)
	original := "{\"healedKeys\":[\"goals\"]}\n"
	activationWrite(t, path, original)
	activationWrite(t, path+".tmp", "another writer's staging bytes")
	if err := os.Chmod(home, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(home, 0700) })
	probe, err := os.CreateTemp(home, ".probe-")
	if err == nil {
		_ = probe.Close()
		_ = os.Remove(probe.Name())
		t.Skip("effective privileges allow creating in mode0500")
	}
	if err := MarkSelfHealOptedOut(home, "fixed"); err == nil || activationRead(t, path) != original {
		t.Fatal("failed publication lost old marker")
	}
	_ = os.Chmod(home, 0700)
	if err := MarkSelfHealOptedOut(home, "fixed"); err != nil {
		t.Fatal(err)
	}
	if activationRead(t, path+".tmp") != "another writer's staging bytes" {
		t.Fatal("fixed-name temporary file overwritten")
	}
	entries, _ := os.ReadDir(home)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "."+SelfHealMarkerName+".") {
			t.Fatal("temporary publication file retained")
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("marker permissions changed: %v %v", info, err)
	}
}
