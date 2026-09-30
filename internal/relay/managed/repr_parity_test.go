package managed

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// A managed request's missing and unknown fields are named as Python's f"{sorted(keys)}" names
// them: repr() of each str, so a key holding a quote, a backslash or a character str.isprintable()
// refuses (U+00A0, U+2028, U+200B) reads as the same bytes in both runtimes. A request holding a
// lone surrogate escape anywhere is refused before any field is judged, as parse_request's
// json.dumps(raw, ensure_ascii=False).encode("utf-8") refuses it, at the position that encode
// names in the text json.dumps writes.
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
	documents = append(documents,
		`{"s\udcff": 1, "b'": 2}`,
		`{"a": "x", "b": ["\ud800\udc00", "y\udcff\udcfe"]}`,
		`{"k\n\u00e9\"": 1e300, "v": [true, null, -0, 12345678901234567890], "w": "\u0001\udfff"}`,
		`{"schema": "managed-start/1", "requestId": "\udcff", "requestId": "r"}`)
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
	out := pyoracle.Answer(t, "parse_request", func() ([]byte, error) {
		command := exec.Command(filepath.Join(root, ".venv", "bin", "python"), "-c", script, string(raw))
		command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
		out, err := command.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("oracle: %v\n%s", err, out)
		}
		return out, nil
	})
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
