//go:build dev

package cxcfuzz

import (
	"path/filepath"
	"testing"
)

// TestCases replays every registered target's pinned cases through the Go side only, with no Node
// and no worker: an identical case must agree with the oracle's recorded answer, an
// intentionally-changed case must agree with the Go answer its record names, and an open case must
// still produce the Go answer it was pinned with, so a change in the Go output fails this test.
func TestCases(t *testing.T) {
	for _, target := range registry() {
		dir := filepath.Join("testdata", target.Name)
		cases, err := LoadCases(dir)
		if err != nil {
			t.Fatalf("%s: %v", target.Name, err)
		}
		if len(cases) == 0 {
			t.Fatalf("%s: no cases in %s", target.Name, dir)
		}
		for _, c := range cases {
			if problem := CheckCase(target, c); problem != "" {
				t.Errorf("%s/%s: %s", target.Name, c.Name, problem)
			}
		}
	}
}

// An open case is a divergence kept visible: its Go output is what the port prints today, and a
// change to it fails until the case is pinned again.
func TestOpenCaseFailsWhenTheGoOutputChanges(t *testing.T) {
	target := echoTarget()
	c := Case{Name: "open", Input: `{"text": "hello"}`, Oracle: `{"text": "hello"}`, Go: `{"text": "hello"}`, Tag: TagOpen}
	if problem := CheckCase(target, c); problem != "" {
		t.Fatalf("an open case pinned at the Go output: %s", problem)
	}
	c.Go = `{"text": "changed"}`
	if problem := CheckCase(target, c); problem == "" {
		t.Fatal("an open case whose Go output moved passed")
	}
}

// Each tag is judged against the answer it names.
func TestCaseTags(t *testing.T) {
	target := echoTarget()
	c := Case{Name: "identical", Input: `{"text": "hello"}`, Oracle: `{"text": "hello"}`, Tag: TagIdentical}
	if problem := CheckCase(target, c); problem != "" {
		t.Fatalf("identical: %s", problem)
	}
	c.Oracle = `{"text": "other"}`
	if problem := CheckCase(target, c); problem == "" {
		t.Fatal("an identical case whose answers disagree passed")
	}
	c = Case{Name: "changed", Input: `{"text": "hello"}`, Oracle: `{"text": "oracle"}`, Go: `{"text": "hello"}`, Tag: TagIntentionallyChanged, Record: "CRW-999"}
	if problem := CheckCase(target, c); problem != "" {
		t.Fatalf("intentionally-changed: %s", problem)
	}
	c.Record = ""
	if problem := CheckCase(target, c); problem == "" {
		t.Fatal("an intentionally-changed case without a record passed")
	}
	c = Case{Name: "bad", Input: `{"text": "hello"}`, Go: `{"text": "hello"}`, Tag: "nope"}
	if problem := CheckCase(target, c); problem == "" {
		t.Fatal("an unknown tag passed")
	}
}

// A case file survives a round trip.
func TestCasesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := []Case{{Name: "a", Input: `1`, Oracle: `1`, Go: `1`, Tag: TagIdentical}}
	if err := SaveCases(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCases(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "a" || got[0].Tag != TagIdentical {
		t.Fatalf("loaded %+v", got)
	}
}
