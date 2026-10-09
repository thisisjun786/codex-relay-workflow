package switchstate_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install/switchstate"
)

func TestPathIsUnderTheCodexHome(t *testing.T) {
	if got, want := switchstate.Path("/h"), filepath.Join("/h", "crw", "switch.json"); got != want {
		t.Fatalf("Path = %q, want %q", got, want)
	}
}

func TestWriteReadRoundTripAndExactBytes(t *testing.T) {
	home := t.TempDir()
	if s, err := switchstate.Read(home); s != nil || err != nil {
		t.Fatalf("Read of an absent file = %v, %v; want nil, nil", s, err)
	}
	want := switchstate.State{Active: switchstate.CRW, ChangedAt: "2026-10-10T01:02:03.000Z", By: "crw install switch"}
	if err := switchstate.Write(home, want); err != nil {
		t.Fatal(err)
	}
	got, err := switchstate.Read(home)
	if err != nil || got == nil || *got != want {
		t.Fatalf("Read = %v, %v; want %v", got, err, want)
	}
	raw, _ := os.ReadFile(switchstate.Path(home))
	const text = "{\n  \"active\": \"crw\",\n  \"changedAt\": \"2026-10-10T01:02:03.000Z\",\n  \"by\": \"crw install switch\"\n}\n"
	if string(raw) != text {
		t.Fatalf("bytes = %q, want %q", raw, text)
	}
	if entries, _ := os.ReadDir(filepath.Dir(switchstate.Path(home))); len(entries) != 1 {
		t.Fatalf("a temporary file was left behind: %v", entries)
	}
}

func TestParseRefusesWhatItCannotRead(t *testing.T) {
	for name, in := range map[string]string{
		"unknown active": `{"active":"both","changedAt":"x","by":"y"}`,
		"empty active":   `{"changedAt":"x","by":"y"}`,
		"unknown field":  `{"active":"crw","changedAt":"x","by":"y","extra":1}`,
		"not json":       `active: crw`,
		"trailing":       `{"active":"crw","changedAt":"x","by":"y"} {}`,
	} {
		if _, err := switchstate.Parse([]byte(in)); err == nil {
			t.Errorf("%s: Parse accepted %q", name, in)
		}
	}
	if _, err := switchstate.Marshal(switchstate.State{Active: "both"}); err == nil {
		t.Error("Marshal accepted an unknown active value")
	}
}

func TestRemoveToleratesAnAbsentFile(t *testing.T) {
	home := t.TempDir()
	if err := switchstate.Remove(home); err != nil {
		t.Fatal(err)
	}
	if err := switchstate.Write(home, switchstate.State{Active: switchstate.CXC}); err != nil {
		t.Fatal(err)
	}
	if err := switchstate.Remove(home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(switchstate.Path(home)); !os.IsNotExist(err) {
		t.Fatalf("file remains: %v", err)
	}
}
