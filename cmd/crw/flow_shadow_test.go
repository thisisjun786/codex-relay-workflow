package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/trace/tracetest"
)

// crw flow trace export, shadow and measure are reached through the mode table, `crw flow help` lists them beside the engine's
// commands, and the commands the engine and lint serve are reached as before.
func TestFlowRoutesTraceShadowAndMeasure(t *testing.T) {
	ctx := context.Background()
	var out, errOut strings.Builder
	reset := func() { out.Reset(); errOut.Reset() }

	if code := run(ctx, "crw", []string{"flow", "help"}, &out, &errOut); code != 0 ||
		!strings.Contains(out.String(), "crw flow run") || !strings.Contains(out.String(), "crw flow trace export") ||
		!strings.Contains(out.String(), "crw flow shadow") || !strings.Contains(out.String(), "crw flow measure") {
		t.Errorf("crw flow help: %d %q", code, out.String())
	}
	for _, name := range []string{"shadow", "measure"} {
		reset()
		if code := run(ctx, "crw", []string{"flow", name}, &out, &errOut); code != parserExit || !strings.Contains(errOut.String(), "crw flow "+name+": error: ") {
			t.Errorf("crw flow %s: %d %q", name, code, errOut.String())
		}
	}
	reset()
	if code := run(ctx, "crw", []string{"flow", "trace", "export"}, &out, &errOut); code != parserExit || !strings.Contains(errOut.String(), "crw flow trace export: error: ") {
		t.Errorf("crw flow trace export: %d %q", code, errOut.String())
	}
	reset()
	if code := run(ctx, "crw", []string{"flow", "nope"}, &out, &errOut); code != parserExit || !strings.Contains(errOut.String(), "invalid choice") {
		t.Errorf("crw flow nope: %d %q", code, errOut.String())
	}
	reset()
	if code := run(ctx, "crw", []string{"flow", "roles"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "executor") {
		t.Errorf("crw flow roles: %d %q %q", code, out.String(), errOut.String())
	}

	// the three commands in a row over a synthetic store copy
	s := tracetest.NewScenario(tracetest.New(t))
	copyPath := s.Copy()
	dir := t.TempDir()
	tracePath, measurePath := filepath.Join(dir, "trace.json"), filepath.Join(dir, "measure.json")
	reset()
	if code := run(ctx, "crw", []string{"flow", "trace", "export", "--store-copy", copyPath, "--out", tracePath}, &out, &errOut); code != 0 {
		t.Fatalf("crw flow trace export: %d %q", code, errOut.String())
	}
	reset()
	script := filepath.Join("..", "..", "testdata", "flow", "shadow", "recorded.js")
	if code := run(ctx, "crw", []string{"flow", "shadow", "--trace", tracePath, "--script", script}, &out, &errOut); code != 0 || !strings.Contains(out.String(), `"same": 7`) {
		t.Errorf("crw flow shadow: %d %q %q", code, out.String(), errOut.String())
	}
	reset()
	if code := run(ctx, "crw", []string{"flow", "measure", "--trace", tracePath, "--out", measurePath}, &out, &errOut); code != 0 {
		t.Fatalf("crw flow measure: %d %q", code, errOut.String())
	}
	if data, err := os.ReadFile(measurePath); err != nil || !strings.Contains(string(data), `"tokens_per_accepted_issue"`) {
		t.Errorf("measure table: %v %q", err, data)
	}
}
