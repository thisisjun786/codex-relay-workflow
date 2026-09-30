package contracttest

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(sqlitePeerEnv); mode != "" {
		os.Exit(sqlitePeer(mode, os.Getenv(sqlitePeerPath)))
	}
	dir, err := os.MkdirTemp("", "crw-contracttest-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	buildDir = dir
	// Build both binaries before HOME isolation, inheriting the invoking toolchain caches: a build
	// after it starts from an empty GOCACHE and would spend a scenario's deadline compiling.
	for _, binary := range []func() (string, error){crwBinary, crwDevBinary} {
		if _, err := binary(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			if cleanupErr := testsupport.RemoveTempTree(dir); cleanupErr != nil {
				fmt.Fprintln(os.Stderr, cleanupErr)
			}
			os.Exit(1)
		}
	}
	for _, key := range []string{"HOME", "XDG_STATE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "CODEX_HOME", "CODEX_SESSION_RELAY_STATE", "CODEX_SESSION_RELAY_SCOPE_DIR"} {
		value := dir + "/" + key
		if err := os.MkdirAll(value, 0700); err != nil {
			panic(err)
		}
		if err := os.Setenv(key, value); err != nil {
			panic(err)
		}
	}
	code := m.Run()
	if err := errors.Join(testsupport.RemoveTempTree(dir), testsupport.RemoveCRW()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

// TestDomain replays contract/fixtures/<domain>/*.json. `-run 'Domain/<name>'` selects one
// domain. Every fixture of every domain runs: a fixture whose kind has no runner, or that asks for
// something its runner cannot do, is a corpus defect and fails, never a skip.
func TestDomain(t *testing.T) {
	domains, err := Domains()
	if err != nil {
		t.Fatal(err)
	}
	for _, domain := range domains {
		scenarios, err := Load(domain)
		if err != nil {
			t.Fatal(err)
		}
		for _, scenario := range scenarios {
			if runners[scenario.Kind] == nil {
				t.Fatalf("%s: %s: no runner for run kind %q", scenario.ID, scenario.Path, scenario.Kind)
			}
		}
		t.Run(domain, func(t *testing.T) {
			if domain == "sqlite-ddl" {
				checkSQLiteContract(t)
			}
			for _, scenario := range scenarios {
				t.Run(scenario.ID, func(t *testing.T) {
					replay(t, scenario)
				})
			}
		})
	}
}

// replay runs one scenario and asserts its expectations.
func replay(t *testing.T, scenario Scenario) {
	t.Helper()
	actual, err := runners[scenario.Kind](t, scenario)
	if err != nil {
		t.Fatalf("%s: %v", scenario.ID, err)
	}
	if err := Assert(scenario, actual); err != nil {
		t.Fatal(err)
	}
}
