package skill

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestHookHostMutationsLivePython(t *testing.T) {
	crw := filepath.Join(t.TempDir(), "crw")
	build := exec.Command("go", "build", "-o", crw, "./cmd/crw")
	build.Dir = repositoryRoot()
	if result := captureSkillProcess(t, build); result.exit != 0 {
		t.Fatalf("build failed: %+v", result)
	}
	host := diskSkillPath(defaultFixture("host"))
	contractPath := diskSkillPath(defaultContract("hook-contract.md"))
	for _, name := range []string{"missing capability", "wrong pair", "wrong version", "missing required fields", "delivered fields", "missing types", "type fields", "unknown type", "missing rows", "extra row", "row question", "row status", "row observed", "row evidence", "unresolved row", "duplicate packet", "unreadable packet", "invalid observation"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "host")
			if err := os.CopyFS(dir, os.DirFS(host)); err != nil {
				t.Fatal(err)
			}
			observationPath := filepath.Join(dir, "host-observation-codex-0.154.0.json")
			capabilityPath := filepath.Join(dir, "host-capability-codex-0.154.0.json")
			raw, err := os.ReadFile(observationPath)
			if err != nil {
				t.Fatal(err)
			}
			var record map[string]any
			if err := json.Unmarshal(raw, &record); err != nil {
				t.Fatal(err)
			}
			rows := record["observations"].(map[string]any)
			row := rows["H1"].(map[string]any)
			stop := record["stopInput"].(map[string]any)
			contract := contractPath
			switch name {
			case "missing capability":
				if err := os.Remove(capabilityPath); err != nil {
					t.Fatal(err)
				}
			case "wrong pair":
				record["capabilityRecord"] = "host-capability-codex-9.0.json"
			case "wrong version":
				record["version"] = "codex-cli 10.154.0"
			case "missing required fields":
				if err := os.WriteFile(capabilityPath, []byte(`{"events":{"stop":{"input":{"required":[]}}}}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "delivered fields":
				stop["fields"] = []any{"unexpected"}
			case "missing types":
				delete(stop, "types")
			case "type fields":
				stop["types"] = map[string]any{"unexpected": "str"}
			case "unknown type":
				stop["types"].(map[string]any)["cwd"] = "path"
			case "missing rows":
				delete(rows, "H1")
				delete(rows, "H3")
			case "extra row":
				rows["H8"] = map[string]any{}
			case "row question":
				row["question"] = ""
			case "row status":
				row["status"] = "maybe"
			case "row observed":
				row["observed"] = ""
			case "row evidence":
				row["evidence"] = ""
			case "unresolved row":
				row["status"] = "unresolved"
			case "duplicate packet", "unreadable packet":
				data, err := os.ReadFile(contractPath)
				if err != nil {
					t.Fatal(err)
				}
				data = append(data, []byte("\n| H1 | question | Resolved |\n")...)
				if name == "unreadable packet" {
					data = append(data, []byte("| H8 | question | maybe |\n")...)
				}
				contract = filepath.Join(t.TempDir(), "contract.md")
				if err := os.WriteFile(contract, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			data, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(observationPath, data, 0600); err != nil {
				t.Fatal(err)
			}
			if name == "invalid observation" {
				if err := os.WriteFile(observationPath, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"replay", "--host-fixtures", dir, "--contract", contract}
			python := runHookProbePython(t, args...)
			if python.exit != 1 || !strings.Contains(python.stdout, "HOST OBSERVATION:") {
				t.Fatalf("Python mutation was not rejected: %+v", python)
			}
			program := "import importlib.util,json,sys; s=importlib.util.spec_from_file_location('probe',sys.argv[1]); m=importlib.util.module_from_spec(s); s.loader.exec_module(m); print(json.dumps(m.check_host_observations(sys.argv[2],sys.argv[3])))"
			command := exec.Command(filepath.Join(repositoryRoot(), ".venv/bin/python"), "-c", program, filepath.Join(repositoryRoot(), "plugins/crw/skills/crw-run/scripts/hook_probe.py"), dir, contract)
			command.Env = oracleEnv("PYTHONDONTWRITEBYTECODE=1")
			result := captureSkillProcess(t, command)
			if result.exit != 0 {
				t.Fatalf("host oracle failed: %+v", result)
			}
			var expected []json.RawMessage
			if err := json.Unmarshal([]byte(result.stdout), &expected); err != nil {
				t.Fatal(err)
			}
			var count int
			var problems []string
			if err := json.Unmarshal(expected[0], &count); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(expected[1], &problems); err != nil {
				t.Fatal(err)
			}
			contractFS, relative := explicitSkillFS(contract)
			gotCount, gotProblems, hostErr := replayHostObservations(os.DirFS(dir), ".", contractFS, relative)
			if hostErr != nil {
				t.Fatal(hostErr)
			}
			if count != gotCount || !reflect.DeepEqual(problems, gotProblems) {
				t.Fatalf("host facts differ: Python %d %q; Go %d %q", count, problems, gotCount, gotProblems)
			}
			actual := captureSkillProcess(t, exec.Command(crw, append([]string{"skill", "hook-probe"}, args...)...))
			requireHookProbeParity(t, python, hookProbeResult(actual))
		})
	}
}
