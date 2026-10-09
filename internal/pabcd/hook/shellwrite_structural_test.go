package hook

import (
	"strings"
	"testing"
)

// TestMemoryGateUnattributableWriteSpellings: a Node or Python program that holds a file API and writes through a name the
// reader cannot attribute to a literal destination has an unknown destination, so the memory gate asks for a grant.
func TestMemoryGateUnattributableWriteSpellings(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, cmd := range []string{
		"node -e 'const {writeFileSync:w}=require(\"fs\"); w(\"" + root + "/a\",\"x\")'",
		"node -e 'require(\"fs\")[\"writeFileSync\"](\"" + root + "/a\",\"x\")'",
		"node -e 'const f=require(\"fs\").writeFileSync; f(\"" + root + "/a\",\"x\")'",
		"python3 -c 'import shutil; getattr(shutil,\"co\"+\"py\")(\"/tmp/x\",\"" + root + "/a\")'",
		"python3 -c 'import shutil; c=shutil.copy; c(\"/tmp/x\",\"" + root + "/a\")'",
	} {
		got := memoryGateClassify("Bash", map[string]any{"command": cmd}, cwd, env)
		if got.Surface != "shell" || got.Target != "(a destination the gate cannot read)" {
			t.Errorf("%q: %+v, want an unknown shell destination", cmd, got)
		}
	}
}

// TestMemoryGateLiteralWriteKeepsDestination: a write called as a dot call with a literal destination keeps its exact
// destination, and a program with no write is no attempt.
func TestMemoryGateLiteralWriteKeepsDestination(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, cmd := range []string{
		"node -e 'require(\"fs\").writeFileSync(\"" + root + "/a\",\"x\")'",
		"python3 -c 'import shutil; shutil.copy(\"/tmp/x\",\"" + root + "/a\")'",
		"python3 -c 'from shutil import copy; copy(\"/tmp/x\",\"" + root + "/a\")'",
	} {
		got := memoryGateClassify("Bash", map[string]any{"command": cmd}, cwd, env)
		if got.Surface != "shell" || !strings.Contains(got.Target, root) {
			t.Errorf("%q: %+v, want the literal destination under the memory root", cmd, got)
		}
	}
	if got := memoryGateClassify("Bash", map[string]any{"command": "python3 -c 'import json; print(json.dumps({\"a\": 1}))'"}, cwd, env); got.Surface != "" {
		t.Errorf("a benign program: %+v, want no attempt", got)
	}
}
