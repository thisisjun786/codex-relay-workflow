//go:build unix

package hookswitch

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteReadRoundTripAndExactBytes(t *testing.T) {
	home := t.TempDir()
	if s, err := Load(home); s != nil || err != nil {
		t.Fatalf("Load of an absent file = %v, %v; want nil, nil", s, err)
	}
	want := State{Active: CRW, ChangedAt: "2026-10-10T01:02:03.000Z", By: "crw install switch"}
	if err := Write(home, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(home)
	if err != nil || got == nil || *got != want {
		t.Fatalf("Load = %v, %v; want %v", got, err, want)
	}
	raw, _ := os.ReadFile(Path(home))
	const text = "{\n  \"active\": \"crw\",\n  \"changedAt\": \"2026-10-10T01:02:03.000Z\",\n  \"by\": \"crw install switch\"\n}\n"
	if string(raw) != text {
		t.Fatalf("bytes = %q, want %q", raw, text)
	}
	if entries, _ := os.ReadDir(filepath.Dir(Path(home))); len(entries) != 1 {
		t.Fatalf("a temporary file was left behind: %v", entries)
	}
	// What Write publishes is what a hook reads.
	if r := readWithin(t, home); !r.On || r.Problem != "" {
		t.Fatalf("a hook reads %+v, want on", r)
	}
}

func TestParseRefusesWhatItCannotRead(t *testing.T) {
	for name, in := range map[string]string{
		"unknown active": `{"active":"both","changedAt":"x","by":"y"}`,
		"empty active":   `{"changedAt":"x","by":"y"}`,
		"not json":       `active: crw`,
		"trailing":       `{"active":"crw","changedAt":"x","by":"y"} {}`,
	} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: Parse accepted %q", name, in)
		}
	}
	if _, err := Marshal(State{Active: "both"}); err == nil {
		t.Error("Marshal accepted an unknown active value")
	}
}

func TestWriteReplacesWholeAndRawRestoresBytes(t *testing.T) {
	home := t.TempDir()
	if err := Write(home, State{Active: CXC, ChangedAt: "a", By: "b"}); err != nil {
		t.Fatal(err)
	}
	prev, err := ReadRaw(home)
	if err != nil || prev == nil {
		t.Fatalf("ReadRaw = %q, %v", prev, err)
	}
	if err := Write(home, State{Active: CRW, ChangedAt: "c", By: "d"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteRaw(home, prev); err != nil {
		t.Fatal(err)
	}
	if again, _ := ReadRaw(home); string(again) != string(prev) {
		t.Fatalf("restored %q, want %q", again, prev)
	}
}

func TestReadRawRefusesAnEntryThatIsNotAFile(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(Path(home), 0o700); err != nil {
		t.Fatal(err)
	}
	if b, err := ReadRaw(home); err == nil {
		t.Fatalf("ReadRaw of a directory = %q, nil", b)
	}
}

func TestRemoveToleratesAnAbsentFile(t *testing.T) {
	home := t.TempDir()
	if err := Remove(home); err != nil {
		t.Fatal(err)
	}
	if err := Write(home, State{Active: CXC}); err != nil {
		t.Fatal(err)
	}
	if err := Remove(home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(Path(home)); !os.IsNotExist(err) {
		t.Fatalf("file remains: %v", err)
	}
}
