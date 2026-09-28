package contracttest

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
)

// Test28_PortableDomainFixtures applies strictness per fixture. A fixture that
// requires another todo is inventoried separately, not run as a false adapter failure.
func Test28_PortableDomainFixtures(t *testing.T) {
	scenarios, err := Load("cli-shape")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range scenarios {
		steps, err := cliSteps(scenario.Run)
		if err != nil {
			t.Fatal(err)
		}
		owned := true
		for _, step := range steps {
			argv, ok := step["argv"].([]any)
			if !ok || len(argv) == 0 {
				t.Fatalf("fixture %s has no command", scenario.ID)
			}
			name, ok := argv[0].(string)
			if !ok {
				t.Fatalf("fixture %s has a nonliteral command", scenario.ID)
			}
			if !cli.Registered(name) {
				owned = false
				break
			}
		}
		if !owned {
			continue
		}
		t.Run(scenario.ID, func(t *testing.T) {
			actual, err := runCLI(t, scenario)
			if err != nil {
				t.Fatal(err)
			}
			if err := Assert(scenario, actual); err != nil {
				t.Fatal(err)
			}
		})
	}
}
