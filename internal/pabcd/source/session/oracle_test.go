package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The oracle's answers for the scenarios of testdata/scenarios.json were recorded once with testdata/record-oracle.mjs
// (CXC v0.2.40 under Node 24); no Node runs here. Each scenario builds a world of repositories and worktrees with shell
// scripts, runs its steps against the session functions and compares every answer with the recorded one. A step's text
// is read with the CRW names: ".codexclaw" is ".crw", and the gate's commands are the CRW commands of the CLI table.
// An operating-system error is compared by its class (ENOENT), as Node and Go word it differently. A tree hash covers the path
// names of its entries, so a capture that includes the state directory ("noHash") is compared without the hash value.

type scenarioFile struct {
	World     string `json:"world"`
	Scenarios []struct {
		ID    string            `json:"id"`
		Setup string            `json:"setup"`
		Steps []json.RawMessage `json:"steps"`
	} `json:"scenarios"`
}

type step struct {
	Do        string            `json:"do"`
	Cwd       string            `json:"cwd"`
	Session   string            `json:"session"`
	Target    string            `json:"target"`
	RawTarget string            `json:"rawTarget"`
	Script    string            `json:"script"`
	Path      string            `json:"path"`
	Env       map[string]string `json:"env"`
	NoHash    bool              `json:"noHash"`
	JSON      map[string]any    `json:"json"`
	Options   struct {
		Exclude        *bool    `json:"excludeCodexclawArtifacts"`
		GeneratedPaths []string `json:"generatedPaths"`
	} `json:"options"`
}

var cliNames = strings.NewReplacer("cxc session source", "crw relay session source", "`cxc loop init", "`crw pabcd loop init", "`cxc receipt test", "`crw pabcd receipt test")

func readJSON(t *testing.T, name string, into any, replace *strings.Replacer) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	must(t, err)
	if replace != nil {
		raw = []byte(replace.Replace(string(raw)))
	}
	must(t, json.Unmarshal(raw, into))
}

func shell(t *testing.T, dir, script string) {
	t.Helper()
	cmd := exec.Command("sh", "-ec", strings.ReplaceAll(script, ".codexclaw", ".crw"))
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s\n%s", err, script, out)
	}
}

// answer is the recorded form of a result: {"ok": value}, or {"err": message} for a message the oracle throws, or {"errno": class}.
func answer(value any, err error) map[string]any {
	var refused refusal
	switch {
	case err == nil:
		return map[string]any{"ok": value}
	case errors.As(err, &refused):
		return map[string]any{"err": string(refused)}
	case errors.Is(err, fs.ErrNotExist):
		return map[string]any{"errno": "ENOENT"}
	}
	return map[string]any{"err": "unexpected: " + err.Error()}
}

func orNil[T comparable](v T) any {
	var zero T
	if v == zero {
		return nil
	}
	return v
}

func TestOracleParity(t *testing.T) {
	var file scenarioFile
	var golden map[string][]any
	readJSON(t, "scenarios.json", &file, nil)
	readJSON(t, "oracle.json", &golden, cliNames)
	if len(golden) != len(file.Scenarios) {
		t.Fatalf("%d scenarios, %d recorded answers", len(file.Scenarios), len(golden))
	}
	base := hermetic(t)
	for _, sc := range file.Scenarios {
		t.Run(sc.ID, func(t *testing.T) {
			root := filepath.Join(base, sc.ID)
			must(t, os.Mkdir(root, 0o755))
			shell(t, root, file.World)
			shell(t, root, sc.Setup)
			var got []any
			noHash := map[int]bool{}
			for _, raw := range sc.Steps {
				var st step
				text := strings.ReplaceAll(strings.ReplaceAll(string(raw), "$R", root), ".codexclaw", ".crw")
				must(t, json.Unmarshal([]byte(text), &st))
				for name, value := range st.Env {
					old, had := os.LookupEnv(name)
					must(t, os.Setenv(name, value))
					defer func() {
						if had {
							_ = os.Setenv(name, old)
						} else {
							_ = os.Unsetenv(name)
						}
					}()
				}
				cwd := filepath.Join(root, st.Cwd)
				switch st.Do {
				case "bind":
					target := st.RawTarget
					if target == "" {
						target = filepath.Join(root, st.Target)
					}
					got = append(got, answer(Bind(cwd, st.Session, target)))
				case "resolve":
					got = append(got, answer(Resolve(cwd, st.Session)))
				case "capture":
					noHash[len(got)] = st.NoHash
					id, err := Capture(cwd, st.Session, CaptureOptions{ExcludeStateArtifacts: st.Options.Exclude, GeneratedPaths: st.Options.GeneratedPaths})
					got = append(got, answer(map[string]any{"kind": id.Kind, "commitSha": id.CommitSha, "dirty": id.Dirty, "treeHash": orNil(id.TreeHash), "sourceRoot": orNil(derefString(id.SourceRoot))}, err))
				case "gate":
					res := CheckBound(cwd, st.Session)
					verdict := map[string]any{"ok": res.OK}
					if res.Reason != "" {
						verdict["reason"] = res.Reason
					}
					got = append(got, answer(verdict, nil))
				case "state":
					path := filepath.Join(cwd, ".crw", "sessions", st.Session+".json")
					data, err := json.Marshal(st.JSON)
					must(t, err)
					must(t, os.MkdirAll(filepath.Dir(path), 0o755))
					must(t, os.WriteFile(path, data, 0o644))
				case "sh":
					shell(t, root, st.Script)
				case "read":
					info, err := os.Lstat(filepath.Join(root, st.Path))
					must(t, err)
					content, err := os.ReadFile(filepath.Join(root, st.Path))
					must(t, err)
					got = append(got, answer(map[string]any{"mode": fmt.Sprintf("%o", info.Mode().Perm()), "content": string(content)}, nil))
				case "ls":
					entries, err := os.ReadDir(filepath.Join(root, st.Path))
					must(t, err)
					names := []any{}
					for _, e := range entries {
						names = append(names, e.Name())
					}
					got = append(got, answer(names, nil))
				}
			}
			// Both sides in the recorded form: the case root is "$R" and the numbers are JSON.
			encoded, err := json.Marshal(got)
			must(t, err)
			var normalized []any
			must(t, json.Unmarshal([]byte(strings.ReplaceAll(string(encoded), root, "$R")), &normalized))
			want := golden[sc.ID]
			for i, masked := range noHash {
				if masked && len(want) > i {
					maskHash(normalized[i])
					maskHash(want[i])
				}
			}
			if !reflect.DeepEqual(normalized, want) {
				t.Fatalf("answers differ from the oracle's\n got %v\nwant %v", normalized, want)
			}
		})
	}
}

func maskHash(answer any) {
	if m, ok := answer.(map[string]any)["ok"].(map[string]any); ok && m["treeHash"] != nil {
		m["treeHash"] = "<hash>"
	}
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
