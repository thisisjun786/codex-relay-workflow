//go:build dev

package cxcfuzz

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The state target is registered and its oracle is a Node worker.
func TestStateTargetIsRegistered(t *testing.T) {
	if !slices.Contains(Names(), "state") {
		t.Fatalf("state is not registered: %v", Names())
	}
	target, ok := Lookup("state")
	if !ok {
		t.Fatal("state is not registered")
	}
	if target.Oracle.Command != "node" {
		t.Fatalf("oracle %+v is not a Node worker", target.Oracle)
	}
}

// stateGo reads a session file the case wrote, rewrites it, and reports the rewritten bytes with the
// write timestamp masked, all with no Node and no python3.
func TestStateGoReadsAndRewrites(t *testing.T) {
	root := t.TempDir()
	writeStateFile(t, root, ".crw/sessions/s.json", `{"phase": "P", "slug": "my-slug"}`)
	answer, err := stateGo(nil, RootEnv(root))
	if err != nil {
		t.Fatal(err)
	}
	if unreadable, _ := field(answer, "unreadable"); unreadable != false {
		t.Fatalf("a valid state read as unreadable: %v", unreadable)
	}
	stateText, _ := field(answer, "state")
	if text, _ := stateText.(string); !strings.Contains(text, `"phase": "P"`) {
		t.Fatalf("the rebuilt state %q lost its phase", stateText)
	}
	written, _ := field(answer, "written")
	text, _ := written.(string)
	if !strings.Contains(text, `"phase": "P"`) || !strings.Contains(text, "@TS@") {
		t.Fatalf("the rewritten bytes %q are not the masked rewrite", written)
	}
}

// A file that is not a valid state reads as unreadable, so a gate that must fail closed can tell.
func TestStateGoMarksAnUnreadableFile(t *testing.T) {
	root := t.TempDir()
	writeStateFile(t, root, ".crw/sessions/s.json", `{`)
	answer, err := stateGo(nil, RootEnv(root))
	if err != nil {
		t.Fatal(err)
	}
	if unreadable, _ := field(answer, "unreadable"); unreadable != true {
		t.Fatalf("a broken file read as readable: %v", unreadable)
	}
}

// writeStateFile puts one case file under a root.
func writeStateFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
