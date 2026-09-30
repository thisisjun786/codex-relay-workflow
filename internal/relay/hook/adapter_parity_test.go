package hook

import (
	"encoding/json"
	"os/exec"
	"testing"
)

func Test33AdapterBinaryPython(t *testing.T) {
	cmd := exec.Command(python(t), "testdata/adapter_compare.py", binary(t), t.TempDir(), testRoot)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var results []map[string]any
	if err = json.Unmarshal(out, &results); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if len(results) != 8 {
		t.Fatalf("only %d QA cases", len(results))
	}
	for _, r := range results {
		if r["equal"] != true {
			t.Fatal(r)
		}
		t.Logf("%s: stdout, exit, journal and claim/outcome files equal", r["scenario"])
	}
}
