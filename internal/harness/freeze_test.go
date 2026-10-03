package harness

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
)

// crw pabcd freeze through the verb table, with the real session read: help, a dry run that writes nothing and ends with the
// goal-activation directive for a ready interview, a run that publishes the manifest, and a failure.
func TestPabcdFreezeThroughTheVerbTable(t *testing.T) {
	ws := t.TempDir()
	put := func(rel string, v any) {
		data, ok := v.([]byte)
		if !ok {
			data, _ = json.Marshal(v)
		}
		p := filepath.Join(ws, ".crw", rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o666); err != nil {
			t.Fatal(err)
		}
	}
	score := map[string]any{"level": "max", "known": []string{"k"}, "unknown": []string{}, "confidence": 1}
	put("sessions/s1.json", map[string]any{"phase": "I", "slug": "demo", "interview": map[string]any{
		"dimensions": map[string]any{"goal": score, "constraint": score, "success": score, "ontology": score}, "contradictions": []any{},
		"assumptions": []map[string]any{{"text": "Assume X", "recorded": true}}, "scanRounds": 1}})
	put("plan/demo/000_plan.md", []byte("# plan\n"))
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := Pabcd(append([]string{"freeze"}, args...), strings.NewReader(""), &out, &errOut, Verbs())
		return code, out.String(), errOut.String()
	}
	manifest := filepath.Join(ws, ".crw", "interview", "freeze.json")

	if code, out, errOut := run("--help"); code != 0 || errOut != "" || !strings.HasPrefix(out, "crw pabcd freeze \u2014 build or preview the interview freeze manifest\n") || !strings.HasSuffix(out, "--help never writes.\n") {
		t.Errorf("help: exit %d, stderr %q:\n%s", code, errOut, out)
	}
	code, out, errOut := run("--dry-run", "--session", "s1", "--cwd", ws)
	if want := "[crw freeze --dry-run]\nmanifest: " + manifest + "\nslug: demo\nplanFiles: 1\n"; code != 0 || errOut != "" || !strings.HasPrefix(out, want) || !strings.HasSuffix(out, "interviewReady: true\nopenAssumptions: 1\nstale-check: no prior manifest\n\n"+interview.GoalActivationDirective+"\n") {
		t.Errorf("dry run: exit %d, stderr %q:\n%s", code, errOut, out)
	}
	if _, err := os.Stat(manifest); err == nil {
		t.Error("a dry run wrote the manifest")
	}
	if code, _, errOut := run("--session", "s1", "--cwd", ws); code != 0 || errOut != "" {
		t.Errorf("write: exit %d, stderr %q", code, errOut)
	}
	var written struct{ Slug, PlanHash string }
	if data, err := os.ReadFile(manifest); err != nil || json.Unmarshal(data, &written) != nil || written.Slug != "demo" || len(written.PlanHash) != 64 {
		t.Errorf("manifest %q (%v)", data, err)
	}
	if code, out, errOut := run("--cwd", filepath.Join(ws, "missing")); code != 1 || out != "" || !strings.HasPrefix(errOut, "freeze failed: ") {
		t.Errorf("a workspace that is not there: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	var usage bytes.Buffer
	if Pabcd([]string{"-h"}, nil, &usage, nil, Verbs()); usage.String() != "usage: crw pabcd [-h] {freeze} ...\n" {
		t.Errorf("usage %q", usage.String())
	}
}
