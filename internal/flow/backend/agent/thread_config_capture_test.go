package agent_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// TestCaptureThreadConfig is an evidence probe (CRW-1570), skipped unless CRW_CAPTURE_THREAD_CONFIG names the file it writes: the
// shell keys (shell_environment_policy.set.*) of the config the backend sends on the thread/start of a call of a run whose start
// recorded TMPDIR. The run is the one whose run.json CRW_CAPTURE_RUN_SPEC names (its env.TMPDIR), else the directory
// CRW_CAPTURE_TMPDIR names. The file is the JSON object of those keys, the map a real App Server's thread/start and thread/fork are
// then given (the lane's real-shell check), so what the real shell prints is what this backend sends.
func TestCaptureThreadConfig(t *testing.T) {
	out := os.Getenv("CRW_CAPTURE_THREAD_CONFIG")
	if out == "" {
		t.Skip("CRW_CAPTURE_THREAD_CONFIG is not set: the evidence probe writes nothing")
	}
	a := os.Getenv("CRW_CAPTURE_TMPDIR")
	if file := os.Getenv("CRW_CAPTURE_RUN_SPEC"); file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var spec struct {
			Env map[string]string `json:"env"`
		}
		if err := json.Unmarshal(raw, &spec); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		a = spec.Env["TMPDIR"]
	}
	if a == "" || !filepath.IsAbs(a) {
		t.Fatalf("no recorded TMPDIR to capture (%q): set CRW_CAPTURE_RUN_SPEC to a run.json with env.TMPDIR or CRW_CAPTURE_TMPDIR", a)
	}
	e := newEnv(t)
	serverPolicy(e)
	spec := e.policy.Roles["explorer"]
	if _, err := e.backend.StartThread(context.Background(), engine.ThreadSpec{CallID: "c_capture", Role: "explorer", Pair: spec.Pair, Sandbox: spec.Sandbox,
		Cwd: filepath.Join(e.root, "capture"), Tools: runTMPDIRTools(a)}); err != nil {
		t.Fatal(err)
	}
	starts := e.requests("thread/start")
	if len(starts) != 1 {
		t.Fatalf("%d thread/start requests", len(starts))
	}
	config, _ := starts[0].Params["config"].(map[string]any)
	keys := map[string]any{}
	for k, v := range config {
		if strings.HasPrefix(k, "shell_environment_policy.set.") {
			keys[k] = v
		}
	}
	if keys["shell_environment_policy.set.TMPDIR"] != a {
		t.Fatalf("the thread/start carries shell keys %v, want TMPDIR = %s", keys, a)
	}
	raw, err := json.MarshalIndent(keys, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s: %s", out, raw)
}
