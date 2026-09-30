package execution

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A policy refusal quotes a name from the file as Python's f"{name!r}" does: its quote choice,
// a backslash doubled, and every character str.isprintable() refuses (U+00A0, U+2028, U+200B)
// escaped. Each want is ExecutionPolicy.from_bytes's ExecutionPolicyError for the same bytes. A
// name escaped as a lone surrogate is left out: this decoder reads it as U+FFFD where json.loads
// keeps it, a difference docs/port/known-defects.md records ("two JSON readers").
func TestAPolicyRefusalQuotesANameAsPythonReprsIt(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	documents := []string{
		`{"roles": {"x\u00a0y": {}}}`,
		`{"roles": {"it's": {}}}`,
		`{"roles": {"a\\b": {}}}`,
		`{"roles": {"l\u2028s\u200b": {}}}`,
		`{"allowed": {}, "a\\b\u00a0": 1, "a\\b\u00a0": 2}`,
	}
	raw, err := json.Marshal(documents)
	if err != nil {
		t.Fatal(err)
	}
	script := `import json, sys
from codex_thread_bridge.execution import ExecutionPolicy, ExecutionPolicyError
out = []
for document in json.loads(sys.argv[1]):
    try:
        ExecutionPolicy.from_bytes(document.encode(), "p")
        out.append(None)
    except ExecutionPolicyError as error:
        out.append(str(error))
print(json.dumps(out))`
	command := exec.Command(filepath.Join(root, ".venv", "bin", "python"), "-c", script, string(raw))
	command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("oracle: %v\n%s", err, out)
	}
	var want []*string
	if err = json.Unmarshal(out, &want); err != nil || len(want) != len(documents) {
		t.Fatalf("%v: %s", err, out)
	}
	for i, document := range documents {
		_, err := FromBytes([]byte(document), "p")
		if want[i] == nil || err == nil || err.Error() != *want[i] {
			t.Errorf("%s:\n go     %v\n python %v", document, err, *want[i])
		}
	}
}
