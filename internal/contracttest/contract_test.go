package contracttest

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func TestMain(m *testing.M) {
	if dir := os.Getenv(cxcRecDir); dir != "" {
		os.Exit(cxcHelper(dir))
	}
	if mode := os.Getenv(sqlitePeerEnv); mode != "" {
		os.Exit(sqlitePeer(mode, os.Getenv(sqlitePeerPath)))
	}
	testsupport.Main(m, func(string) (func() error, error) {
		// Both binaries are built before the first scenario, so none spends its deadline linking.
		for _, binary := range []func() (string, error){crwBinary, crwDevBinary} {
			if _, err := binary(); err != nil {
				return nil, err
			}
		}
		// The scenarios start in homes whose directories exist.
		for _, key := range []string{"HOME", "XDG_STATE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "CODEX_HOME", "CODEX_SESSION_RELAY_STATE", "CODEX_SESSION_RELAY_SCOPE_DIR"} {
			if err := os.MkdirAll(os.Getenv(key), 0o700); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
}

// pendingDomains are fixture domains recorded for an implementation the Go build does not have
// yet. Each is skipped by name with its reason, never silently; every other domain runs in full.
// None is pending now: the cxc domain (the CXC v0.2.40 corpus) is replayed by replayCXC, which
// decides each fixture by the status files under contract/notes/cxc.
var pendingDomains = map[string]string{}

// TestDomain replays contract/fixtures/<domain>/*.json. `-run 'Domain/<name>'` selects one
// domain. Every fixture of every domain runs, apart from the pendingDomains, which are skipped by
// name: a fixture whose kind has no runner, or that asks for something its runner cannot do, is a
// corpus defect and fails, never a skip.
func TestDomain(t *testing.T) {
	domains, err := Domains()
	if err != nil {
		t.Fatal(err)
	}
	for _, domain := range domains {
		if reason, pending := pendingDomains[domain]; pending {
			t.Run(domain, func(t *testing.T) { t.Skip(reason) })
			continue
		}
		if domain == "cxc" {
			t.Run(domain, func(t *testing.T) {
				root, err := Root()
				if err != nil {
					t.Fatal(err)
				}
				crw, err := crwBinary()
				if err != nil {
					t.Fatal(err)
				}
				replayCXC(t, root, crw)
			})
			continue
		}
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

// A pending domain names a fixture directory that exists, so the skip list cannot outlive the
// corpus it excuses.
func TestPendingDomains_are_fixture_directories(t *testing.T) {
	domains, err := Domains()
	if err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{}
	for _, domain := range domains {
		present[domain] = true
	}
	for domain := range pendingDomains {
		if !present[domain] {
			t.Errorf("pending domain %q has no contract/fixtures/%s directory", domain, domain)
		}
	}
}

// requiredDomains are the fixture domains the contract corpus holds. TestDomain replays whatever
// directories exist, so a deleted domain would leave it green over a smaller corpus; naming them
// here makes dropping one an explicit edit of this list. Adding a domain needs no edit. This is the
// live part of the port test map's check (decision 75): the domains the map named exist.
var requiredDomains = []string{"appserver", "cli-shape", "cxc", "git", "hook", "ledger-fingerprint", "mcp-tools", "records", "sqlite-ddl"}

func TestCorpusKeepsItsDomains(t *testing.T) {
	domains, err := Domains()
	if err != nil {
		t.Fatal(err)
	}
	root, err := Root()
	if err != nil {
		t.Fatal(err)
	}
	for _, domain := range requiredDomains {
		if !slices.Contains(domains, domain) {
			t.Errorf("contract/fixtures/%s is missing", domain)
			continue
		}
		entries, err := os.ReadDir(filepath.Join(root, "contract", "fixtures", domain))
		if err != nil {
			t.Errorf("contract/fixtures/%s: %v", domain, err)
			continue
		}
		if !slices.ContainsFunc(entries, func(e os.DirEntry) bool { return e.Type().IsRegular() }) {
			t.Errorf("contract/fixtures/%s holds no fixture file", domain)
		}
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
