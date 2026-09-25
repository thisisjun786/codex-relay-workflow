package contracttest

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "crw-contracttest-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	buildDir = dir
	code := m.Run()
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

// strict reports CRW_CONTRACT_STRICT=1: every skip is a failure (used from wave 6 on).
func strict() bool { return os.Getenv("CRW_CONTRACT_STRICT") == "1" }

// notPorted records the skip in *skips, then skips, or fails under strict mode.
func notPorted(t *testing.T, reason string, skips *int) {
	t.Helper()
	*skips++ // subtests run sequentially, so the counter needs no lock
	if strict() {
		t.Fatalf("not ported: %s (CRW_CONTRACT_STRICT=1 forbids skips)", reason)
	}
	t.Skip("not ported: " + reason)
}

// TestDomain replays contract/fixtures/<domain>/*.json. `-run 'Domain/<name>'` selects one
// domain. Every fixture of every domain is parsed, ported or not, so a broken fixture fails.
func TestDomain(t *testing.T) {
	domains, err := Domains()
	if err != nil {
		t.Fatal(err)
	}
	var skipped []string
	for _, domain := range domains {
		scenarios, err := Load(domain)
		if err != nil {
			t.Fatal(err)
		}
		for _, scenario := range scenarios {
			if !Kinds[scenario.Kind] {
				t.Fatalf("%s: %s: unknown run kind %q", scenario.ID, scenario.Path, scenario.Kind)
			}
		}
		skips := 0
		t.Run(domain, func(t *testing.T) {
			if !ported[domain] {
				for _, kind := range kindsOf(scenarios) {
					t.Run(string(kind), func(t *testing.T) {
						notPorted(t, domain+"/"+string(kind), &skips)
					})
				}
				return
			}
			for _, scenario := range scenarios {
				t.Run(scenario.ID, func(t *testing.T) {
					replay(t, scenario, &skips)
				})
			}
		})
		if skips > 0 {
			skipped = append(skipped, domain)
		}
	}
	fmt.Printf("skipped: %d domains not ported: %s\n", len(skipped), strings.Join(skipped, ", "))
	if strict() && len(skipped) > 0 {
		t.Errorf("CRW_CONTRACT_STRICT=1: domains not ported: %s", strings.Join(skipped, ", "))
	}
}

// replay runs one scenario and asserts its expectations.
func replay(t *testing.T, scenario Scenario, skips *int) {
	t.Helper()
	runner, registered := runners[scenario.Kind]
	if !registered {
		notPorted(t, scenario.Domain+"/"+string(scenario.Kind), skips)
	}
	actual, err := runner(t, scenario)
	if errors.Is(err, ErrNotPorted) {
		notPorted(t, err.Error(), skips)
	}
	if err != nil {
		t.Fatalf("%s: %v", scenario.ID, err)
	}
	if err := Assert(scenario, actual); err != nil {
		t.Fatal(err)
	}
}

func kindsOf(scenarios []Scenario) []RunKind {
	var kinds []RunKind
	for _, scenario := range scenarios {
		if !slices.Contains(kinds, scenario.Kind) {
			kinds = append(kinds, scenario.Kind)
		}
	}
	slices.Sort(kinds)
	return kinds
}
