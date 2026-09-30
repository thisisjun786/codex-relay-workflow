package skill

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type skillShapeCase struct {
	name, family string
	args         []string
	stdin        any
	files        map[string]any
}

// shapeRawFile is a case file written byte for byte, for JSON that Go's encoder
// cannot spell: NaN, Infinity, 1.0, or several documents in one file.
type shapeRawFile string

// One process-level matrix owns all JSON-shape surfaces, with Python as the oracle.
func TestSkillJSONShapeLivePython(t *testing.T) {
	t.Setenv("PYTHONDONTWRITEBYTECODE", "1")
	t.Setenv("TZ", "Pacific/Honolulu")
	root := repositoryRoot()
	binary := buildHookProbeCLI(t)
	cases := append(hookShapeCases(t), titleShapeCases(t)...)
	cases = append(cases, hostShapeCases()...)
	cases = append(cases, observeShapeCases(t)...)
	t.Logf("JSON shape matrix: %d cases", len(cases))
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			// Given one generated shape mutation in an otherwise usable request.
			dir := t.TempDir()
			for name, value := range test.files {
				path := filepath.Join(dir, name)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				raw, err := []byte(nil), error(nil)
				if text, ok := value.(shapeRawFile); ok {
					raw = []byte(text)
				} else if raw, err = json.MarshalIndent(value, "", "  "); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			args := make([]string, len(test.args))
			for i, arg := range test.args {
				args[i] = strings.ReplaceAll(arg, "$TMP", dir)
			}
			input, err := json.Marshal(test.stdin)
			if err != nil {
				t.Fatal(err)
			}
			script := strings.ReplaceAll(test.family, "-", "_") + ".py"
			python := exec.Command(filepath.Join(root, ".venv/bin/python"), append([]string{filepath.Join(root, "plugins/crw/skills/crw-run/scripts", script)}, args...)...)
			python.Stdin = strings.NewReader(string(input))
			// When the real Python and Go commands read exactly the same bytes.
			want := captureSkillProcess(t, python)
			command := exec.Command(binary, append([]string{"skill", test.family}, args...)...)
			command.Stdin = strings.NewReader(string(input))
			got := captureSkillProcess(t, command)
			// Then compare all observable bytes, including refusals and exceptions;
			// only Python's traceback frames are excluded (decision 29a).
			if !skillProcessParity(want, got) {
				t.Errorf("live Python mismatch\nPython exit=%d stdout=%q stderr=%q\nGo exit=%d stdout=%q stderr=%q", want.exit, want.stdout, want.stderr, got.exit, got.stdout, got.stderr)
			}
		})
	}
}

type shapeVariant struct {
	name   string
	value  any
	absent bool
}

func shapeVariants() []shapeVariant {
	return []shapeVariant{
		{name: "absent", absent: true}, {name: "null"},
		{name: "string", value: "bad"}, {name: "number", value: 7},
		{name: "true", value: true}, {name: "false", value: false},
		{name: "array", value: []any{"bad"}}, {name: "object", value: map[string]any{"bad": true}},
		{name: "empty-array", value: []any{}}, {name: "empty-object", value: map[string]any{}},
		{name: "empty-string", value: ""}, {name: "zero", value: 0},
	}
}

func shapeClone(t *testing.T, value any) any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var copy any
	if err := json.Unmarshal(raw, &copy); err != nil {
		t.Fatal(err)
	}
	return copy
}

func shapeSet(value any, path []string, variant shapeVariant) any {
	if len(path) == 0 {
		return variant.value
	}
	if list, ok := value.([]any); ok {
		if len(list) == 0 {
			panic("empty shape seed at " + strings.Join(path, "."))
		}
		list[0] = shapeSet(list[0], path[1:], variant)
		return list
	}
	object, ok := value.(map[string]any)
	if !ok {
		panic(fmt.Sprintf("shape seed at %v is %T", path, value))
	}
	if len(path) == 1 {
		if variant.absent {
			delete(object, path[0])
		} else {
			object[path[0]] = variant.value
		}
	} else {
		object[path[0]] = shapeSet(object[path[0]], path[1:], variant)
	}
	return object
}
