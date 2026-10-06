//go:build dev

package cxcfuzz

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The goalplan target is registered and its oracle is a Node worker.
func TestGoalplanTargetIsRegistered(t *testing.T) {
	if !slices.Contains(Names(), "goalplan") {
		t.Fatalf("goalplan is not registered: %v", Names())
	}
	target, ok := Lookup("goalplan")
	if !ok {
		t.Fatal("goalplan is not registered")
	}
	if target.Oracle.Command != "node" {
		t.Fatalf("oracle %+v is not a Node worker", target.Oracle)
	}
}

// goalplanGo reads a plan, rewrites it under the write lock, and reports the rewritten bytes with the
// write timestamp masked, all with no Node and no python3.
func TestGoalplanGoReadsAndRewrites(t *testing.T) {
	root := t.TempDir()
	writeGoalplanFile(t, root, `.crw/goalplans/rec-plan/goalplan.json`, `{"objective": "o", "slug": "rec-plan", "workPhases": [], "criteria": [], "host": {"armed": false, "armedAt": null, "source": "none"}}`)
	answer, err := goalplanGo(nil, RootEnv(root))
	if err != nil {
		t.Fatal(err)
	}
	if kind, _ := field(answer, "kind"); kind != "ok" {
		t.Fatalf("a valid plan read as %v", kind)
	}
	written, _ := field(answer, "written")
	text, _ := written.(string)
	if !strings.Contains(text, `"objective": "o"`) || !strings.Contains(text, "@TS@") {
		t.Fatalf("the rewritten bytes %q are not the masked rewrite", written)
	}
}

// A plan whose rewrite would drop a key is refused by the write lock rather than published.
func TestGoalplanGoRefusesALossyRewrite(t *testing.T) {
	root := t.TempDir()
	writeGoalplanFile(t, root, `.crw/goalplans/rec-plan/goalplan.json`, `{"objective": "o", "slug": "rec-plan", "workPhases": [], "criteria": [], "host": {"armed": false, "armedAt": null, "source": "none"}, "unknownKey": 1}`)
	answer, err := goalplanGo(nil, RootEnv(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, found := field(answer, "written"); found {
		t.Fatal("a lossy rewrite was published")
	}
	writeError, _ := field(answer, "writeError")
	if text, _ := writeError.(string); !strings.Contains(text, "refusing to rewrite") {
		t.Fatalf("the refusal %q does not name the loss", writeError)
	}
}

// writeGoalplanFile puts one case file under a root.
func writeGoalplanFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
