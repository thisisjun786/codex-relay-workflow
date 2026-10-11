package sandboxroot

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Case is one row of testdata/cases.json (testdata/README.md describes the schema). The coordinator's
// Python guard runs the same table.
type Case struct {
	Name        string            `json:"name"`
	Note        string            `json:"note"`
	AccountHome string            `json:"account_home"`
	HomeEnv     string            `json:"home_env"`
	Dirs        []string          `json:"dirs"`
	GitDirs     []string          `json:"git_dirs"`
	GitFiles    []string          `json:"git_files"`
	Symlinks    map[string]string `json:"symlinks"`
	CrwConfig   map[string]any    `json:"crw_config"`
	CodexTOML   *string           `json:"codex_toml"`
	Kind        string            `json:"kind"`
	Candidate   string            `json:"candidate"`
	Sandbox     string            `json:"sandbox"`
	Expect      struct {
		Verdict  string `json:"verdict"`
		Rule     string `json:"rule"`
		Project  string `json:"project"`
		Resolved string `json:"resolved"`
	} `json:"expect"`
}

type caseTable struct {
	Schema string `json:"schema"`
	Cases  []Case `json:"cases"`
}

func loadCases(t *testing.T) []Case {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var table caseTable
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatal(err)
	}
	if table.Schema != "crw-sandbox-root-cases/1" || len(table.Cases) == 0 {
		t.Fatalf("case table schema %q with %d cases", table.Schema, len(table.Cases))
	}
	return table.Cases
}

// world is a materialized case: its real root and the substitution of {root}.
type world struct {
	root string
	sub  func(string) string
}

// materialize builds the case's fake tree, configuration files and account home in a temporary
// directory and points the rule at them. Nothing of the real host is read.
func materialize(t *testing.T, c Case) world {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sub := func(s string) string { return strings.ReplaceAll(s, "{root}", root) }
	for _, dir := range c.Dirs {
		if err := os.MkdirAll(sub(dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range c.GitDirs {
		if err := os.MkdirAll(filepath.Join(sub(dir), ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range c.GitFiles {
		if err := os.WriteFile(filepath.Join(sub(dir), ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range c.Symlinks {
		if err := os.MkdirAll(filepath.Dir(sub(link)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(sub(target), sub(link)); err != nil {
			t.Fatal(err)
		}
	}
	codexHome := filepath.Join(root, "codex-home")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if c.CodexTOML != nil {
		if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(sub(*c.CodexTOML)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(root, "crw-config.json")
	if c.CrwConfig != nil {
		data, err := json.Marshal(c.CrwConfig)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(configPath, []byte(sub(string(data))), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	home := c.HomeEnv
	if home == "" {
		home = c.AccountHome
	}
	env := map[string]string{"HOME": sub(home), "CODEX_HOME": codexHome, "CRW_CONFIG": configPath}
	t.Cleanup(Inject(func(name string) string { return env[name] }, sub(c.AccountHome)))
	return world{root: root, sub: sub}
}

// judge runs a case through the entry point its kind names.
func judge(c Case, w world) error {
	candidate := w.sub(c.Candidate)
	switch c.Kind {
	case "destination":
		return CheckDestination(candidate, c.Sandbox)
	case "writable":
		return CheckWritable(candidate, c.Sandbox)
	default:
		return Check(candidate, c.Sandbox)
	}
}

func runCase(t *testing.T, c Case) {
	t.Helper()
	w := materialize(t, c)
	err := judge(c, w)
	if c.Expect.Verdict == "allow" {
		if err != nil {
			t.Fatalf("%s: want allowed, got %v", c.Name, err)
		}
		return
	}
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("%s: want a refusal by %s, got %v", c.Name, c.Expect.Rule, err)
	}
	if refusal.Rule != c.Expect.Rule {
		t.Fatalf("%s: rule %s (%v), want %s", c.Name, refusal.Rule, err, c.Expect.Rule)
	}
	if got, want := refusal.Project, w.sub(c.Expect.Project); got != want {
		t.Fatalf("%s: project %q, want %q", c.Name, got, want)
	}
	if c.Expect.Resolved != "" && refusal.Resolved != w.sub(c.Expect.Resolved) {
		t.Fatalf("%s: resolved %q, want %q", c.Name, refusal.Resolved, w.sub(c.Expect.Resolved))
	}
	if refusal.Path != w.sub(c.Candidate) {
		t.Fatalf("%s: path %q is not the candidate as received %q", c.Name, refusal.Path, w.sub(c.Candidate))
	}
	if refusal.Code() != "sandbox_root_refused" || !strings.Contains(err.Error(), c.Expect.Rule) {
		t.Fatalf("%s: refusal text %q", c.Name, err)
	}
}

// runNamed runs the table rows with these names, each as a subtest, and fails when a name is not a row.
func runNamed(t *testing.T, names ...string) {
	t.Helper()
	byName := map[string]Case{}
	for _, c := range loadCases(t) {
		byName[c.Name] = c
	}
	for _, name := range names {
		c, ok := byName[name]
		if !ok {
			t.Fatalf("the case table has no row %q", name)
		}
		t.Run(name, func(t *testing.T) { runCase(t, c) })
	}
}

// runPrefixed runs every row whose name starts with one of the prefixes; it fails when none matches.
func runPrefixed(t *testing.T, prefixes ...string) {
	t.Helper()
	ran := 0
	for _, c := range loadCases(t) {
		for _, prefix := range prefixes {
			if strings.HasPrefix(c.Name, prefix) {
				ran++
				t.Run(c.Name, func(t *testing.T) { runCase(t, c) })
				break
			}
		}
	}
	if ran == 0 {
		t.Fatalf("no case row starts with %v", prefixes)
	}
}

func TestCaseTableMatchesCheck(t *testing.T) {
	cases := loadCases(t)
	seen := map[string]bool{}
	for _, c := range cases {
		if seen[c.Name] {
			t.Fatalf("duplicate case %q", c.Name)
		}
		seen[c.Name] = true
		t.Run(c.Name, func(t *testing.T) { runCase(t, c) })
	}
}
