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

// shapeDirectory is a case path made a directory, which read_text refuses with
// IsADirectoryError.
type shapeDirectory struct{}

// shapeUnreadableFile is a shapeRawFile whose mode is then 000, which
// read_text refuses with PermissionError.
type shapeUnreadableFile string

// One process-level matrix owns all JSON-shape surfaces, each answer held to the golden (first
// taken as the Python script's answer).
func TestSkillJSONShapeLivePython(t *testing.T) {
	goldenRoot(t)
	t.Setenv("TZ", "Pacific/Honolulu")
	binary := recordedCRW(t)
	cases := append(hookShapeCases(t), titleShapeCases(t)...)
	cases = append(cases, hostShapeCases()...)
	cases = append(cases, observeShapeCases(t)...)
	cases = append(cases, osErrorShapeCases()...)
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
				if _, ok := value.(shapeDirectory); ok {
					if err := os.MkdirAll(path, 0700); err != nil {
						t.Fatal(err)
					}
					continue
				}
				raw, err := []byte(nil), error(nil)
				switch text := value.(type) {
				case shapeRawFile:
					raw = []byte(text)
				case shapeUnreadableFile:
					raw = []byte(text)
				default:
					if raw, err = json.MarshalIndent(value, "", "  "); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				if _, ok := value.(shapeUnreadableFile); ok {
					if err := os.Chmod(path, 0); err != nil {
						t.Fatal(err)
					}
					if _, err := os.ReadFile(path); !os.IsPermission(err) {
						t.Fatalf("mode 000 did not refuse reads of %q: %v", name, err)
					}
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
			// When the Go command reads those bytes.
			command := exec.Command(binary, append([]string{"skill", test.family}, args...)...)
			command.Stdin = strings.NewReader(string(input))
			got := captureSkillProcess(t, command)
			// Then all observable bytes, refusals and exceptions included, are the golden's.
			checkSkillAnswer(t, "", "", append([]string{"skill", test.family}, args...), normalizedAnswer(got))
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
