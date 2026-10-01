package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The same eight Domain/cli-shape inputs go through the console script, not a module entry
// point. No fixture runner behavior is changed. The console script is Go's: the Python console
// script these frozen fixtures were first checked against left with the Python runtime (todo 44),
// and Go answers each fixture's exit and checks as it did.
func Test29ServiceDomainFixtureOracle(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(testRoot, "contract/fixtures/cli-shape/test_dispositions__test_cli_service_uses_isolated_scope__*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 8 {
		t.Fatalf("service actions: %d", len(paths))
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var fixture struct {
				Given  map[string]any
				Run    struct{ Argv []string }
				Expect struct {
					Exit   int
					Checks []struct {
						Kind  string
						Path  []string
						Value any
					}
				}
			}
			if err = json.Unmarshal(raw, &fixture); err != nil {
				t.Fatal(err)
			}
			home := t.TempDir()
			args := fixture.Run.Argv
			if _, host := fixture.Given["host"]; host {
				args = append([]string{"--socket", home + "/socket"}, args...)
			}
			actual := invoke(t, home, args...)
			if actual.Code != fixture.Expect.Exit {
				t.Fatal(actual)
			}
			var answer map[string]any
			if err = json.Unmarshal([]byte(actual.Out), &answer); err != nil {
				t.Fatal(err)
			}
			for _, check := range fixture.Expect.Checks {
				var value any = map[string]any{"stdout_json": answer}
				for _, key := range check.Path {
					value = value.(map[string]any)[key]
				}
				if value != check.Value {
					t.Fatalf("%v: %v != %v", check.Path, value, check.Value)
				}
			}
		})
	}
}
