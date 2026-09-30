package managed

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A managed request's missing and unknown fields are named as Python's f"{sorted(keys)}" names
// them: repr() of each str, so a key holding a quote, a backslash or a character str.isprintable()
// refuses (U+00A0, U+2028, U+200B) reads as the same bytes in both runtimes. A key escaped as a
// lone surrogate is left out: Python refuses the whole request when it encodes it to measure its
// size, and ParseRequest reads U+FFFD, a difference docs/port/known-defects.md records ("two JSON
// readers").
func TestAnUnknownRequestFieldIsNamedAsPythonReprsIt(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	var documents []string
	for _, key := range []string{"it's", `a\b`, "x\u00a0y", "x\u2028y", "x\u200by", `say "it's"`} {
		raw, err := json.Marshal(map[string]any{key: 1, "b'": 2})
		if err != nil {
			t.Fatal(err)
		}
		documents = append(documents, string(raw))
	}
	raw, err := json.Marshal(documents)
	if err != nil {
		t.Fatal(err)
	}
	script := `import json, sys
from codex_session_relay.managed import parse_request
out = []
for document in json.loads(sys.argv[1]):
    try:
        parse_request(json.loads(document))
        out.append(None)
    except ValueError as error:
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
		_, err := ParseRequest([]byte(document))
		if want[i] == nil || err == nil || err.Error() != *want[i] {
			t.Errorf("%s:\n go     %v\n python %v", document, err, strings.TrimSpace(*want[i]))
		}
	}
}
