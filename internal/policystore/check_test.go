package policystore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// oneAllowed is the document the CRW-134 evaluation reproduced against: one role pair, and an
// allowlist that approves exactly that pair.
const oneAllowed = "{\"roles\": {\"child\": {\"model\": \"m\", \"reasoningEffort\": \"high\"}}, \"allowed\": [{\"model\": \"m\", \"efforts\": [\"high\"]}]}\n"

// presenceOnly is a document with roles and no allowed list: the parser reads it as presence_only.
const presenceOnly = "{\"roles\": {\"child\": {\"model\": \"m\", \"reasoningEffort\": \"high\"}}}\n"

// TestCheckAcceptsTheContractEffortSpelling is C2: the check contract names the field effort, so a
// request that uses that spelling must be read as the effort it states. Decoding it as an empty
// effort refuses a request the contract allows.
func TestCheckAcceptsTheContractEffortSpelling(t *testing.T) {
	raw := "{\"kind\":\"setException\",\"id\":\"legacy\",\"role\":\"child\",\"model\":\"m\",\"effort\":\"high\",\"cwd\":[\"/tmp/project\"]}"
	var change Change
	if err := json.Unmarshal([]byte(raw), &change); err != nil {
		t.Fatal(err)
	}
	if change.Effort != "high" {
		t.Fatalf("the contract spelling decoded to %q, want %q", change.Effort, "high")
	}
	result := Check([]byte(presenceOnly), digestOf(presenceOnly), change)
	if !result.Valid {
		t.Fatalf("a contract-shaped setException was refused: %+v", result)
	}
	candidate, err := candidateOf(t, presenceOnly, change)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(candidate, "\"reasoningEffort\": \"high\"") {
		t.Fatalf("the candidate does not carry the effort the contract named: %s", candidate)
	}
}

// TestCheckRefusesTwoEffortSpellingsThatDisagree is C2: reasoningEffort is an alias of effort, and a
// request that names both with different values is ambiguous rather than silently resolved. The
// answer is a refused change, not a decode failure the API would answer as a bad request.
func TestCheckRefusesTwoEffortSpellingsThatDisagree(t *testing.T) {
	raw := "{\"kind\":\"setException\",\"id\":\"legacy\",\"role\":\"child\",\"model\":\"m\",\"reasoningEffort\":\"high\",\"effort\":\"low\",\"cwd\":[\"/tmp/project\"]}"
	var change Change
	if err := json.Unmarshal([]byte(raw), &change); err != nil {
		t.Fatalf("a disagreeing alias must not fail the decode: %v", err)
	}
	result := Check([]byte(presenceOnly), digestOf(presenceOnly), change)
	if result.Valid {
		t.Fatalf("a change naming two disagreeing efforts was accepted: %+v", result)
	}
	if len(result.Errors) == 0 {
		t.Fatalf("a refused change carries no error: %+v", result)
	}
}

// TestCheckAcceptsTheAliasSpellingOfEffort is C2: reasoningEffort is still accepted on its own.
func TestCheckAcceptsTheAliasSpellingOfEffort(t *testing.T) {
	raw := "{\"kind\":\"setException\",\"id\":\"legacy\",\"role\":\"child\",\"model\":\"m\",\"reasoningEffort\":\"high\",\"cwd\":[\"/tmp/project\"]}"
	var change Change
	if err := json.Unmarshal([]byte(raw), &change); err != nil {
		t.Fatal(err)
	}
	if change.Effort != "high" {
		t.Fatalf("effort = %q, want %q", change.Effort, "high")
	}
	if result := Check([]byte(presenceOnly), digestOf(presenceOnly), change); !result.Valid {
		t.Fatalf("the alias spelling was refused: %+v", result)
	}
}

// TestCheckRemovingTheLastAllowedModelIsNotValid is C3, the CRW-134 evaluation reproduction:
// removing the last allowed model must not hand back a valid presence_only candidate. An absent
// allowed key widens what the host allows, so the check must not approve it.
func TestCheckRemovingTheLastAllowedModelIsNotValid(t *testing.T) {
	change := Change{Kind: KindRemoveAllowed, Model: "m"}
	result := Check([]byte(oneAllowed), digestOf(oneAllowed), change)
	if result.Valid {
		t.Fatalf("removing the last allowed model was approved: %+v", result)
	}
	if len(result.Errors) == 0 {
		t.Fatalf("a refused change carries no error: %+v", result)
	}
	candidate, err := candidateOf(t, oneAllowed, change)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(candidate, "\"allowed\"") {
		t.Fatalf("the candidate dropped the allowed key, which is presence_only: %s", candidate)
	}
}

// TestCheckRefusesAChangeThatMovesThePolicyMode is C3's mode guard: a candidate the parser can read
// must still be refused when its mode differs from the file's. This is the case the empty-list
// refusal above cannot reach, because there the candidate never parses.
func TestCheckRefusesAChangeThatMovesThePolicyMode(t *testing.T) {
	change := Change{Kind: KindSetAllowed, Model: "m", Efforts: []string{"high"}}
	result := Check([]byte(presenceOnly), digestOf(presenceOnly), change)
	if result.Valid {
		t.Fatalf("a change from presence_only to allowlist was approved: %+v", result)
	}
	joined := strings.Join(result.Errors, "; ")
	if !strings.Contains(joined, "presence_only") || !strings.Contains(joined, "allowlist") {
		t.Fatalf("the refusal does not name the mode change: %q", joined)
	}
}

// TestCheckKeepsTheModeWhenAChangeStaysInsideIt is C3's contrast: a change that keeps the mode is
// still approved, so the guard refuses a mode move rather than every allowlist edit.
func TestCheckKeepsTheModeWhenAChangeStaysInsideIt(t *testing.T) {
	change := Change{Kind: KindSetAllowed, Model: "m", Efforts: []string{"high", "low"}}
	if result := Check([]byte(oneAllowed), digestOf(oneAllowed), change); !result.Valid {
		t.Fatalf("an allowlist edit that keeps the mode was refused: %+v", result)
	}
}

// TestAppliedActionReregisterNamesTheCommandThatWorks is C5: the guidance names the re-registration
// path, and every flag it names is one install actually declares. A sentence naming a flag install
// does not know is a repair instruction that fails when an operator follows it.
func TestAppliedActionReregisterNamesTheCommandThatWorks(t *testing.T) {
	if !strings.Contains(AppliedActionReregister, "--re-register-policy") {
		t.Fatalf("the guidance does not name the re-registration flag: %q", AppliedActionReregister)
	}
	args, policy := reregisterArgs(t)
	env, _ := installHome(t)
	// The command is run as written against an isolated home with no record: the re-registration
	// path answers record_absent and refuses. A flag install does not declare is a usage error
	// instead, which is the difference this test is about.
	var stdout, stderr strings.Builder
	code := install.Main(context.Background(), args, env, &stdout, &stderr)
	if code == install.Usage {
		t.Fatalf("install answered a usage error for the flags the guidance names: %q\n%s%s", AppliedActionReregister, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "not defined") {
		t.Fatalf("the guidance names a flag install does not declare: %s", stderr.String())
	}
	// A misspelled flag in the same position is a usage error, so the assertion above is about the
	// flags rather than about install accepting anything.
	misspelled := []string{"register-mcp", "--re-register-policies", "--execution-policy", policy}
	stdout.Reset()
	stderr.Reset()
	if code := install.Main(context.Background(), misspelled, env, &stdout, &stderr); code != install.Usage {
		t.Fatalf("a misspelled flag was not a usage error: exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
}

// reregisterArgs reads the command line out of the guidance sentence: the tokens from register-mcp
// on, with the placeholder replaced by a real policy file. The placeholder is written with angle
// brackets, so it is the one token that is neither a flag nor the subcommand.
func reregisterArgs(t *testing.T) ([]string, string) {
	t.Helper()
	fields := strings.Fields(AppliedActionReregister)
	start := -1
	for i, field := range fields {
		if field == "register-mcp" {
			start = i
			break
		}
	}
	if start == -1 {
		t.Fatalf("the guidance names no register-mcp command: %q", AppliedActionReregister)
	}
	policy := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policy, []byte(presenceOnly), 0o644); err != nil {
		t.Fatal(err)
	}
	args := make([]string, 0, len(fields)-start)
	for _, field := range fields[start:] {
		if strings.HasPrefix(field, "<") && strings.HasSuffix(field, ">") {
			field = policy
		}
		args = append(args, field)
	}
	return args, policy
}

// installHome is an isolated host for install: every directory install resolves is a temporary one,
// so nothing here reads or writes the operator's Codex home, runtime or record. The environment
// starts from this process's own, minus the variables install reads, and ends with the temporary
// values.
func installHome(t *testing.T) (scope.Env, string) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	for _, dir := range []string{home, filepath.Join(root, "state"), filepath.Join(root, "config"), filepath.Join(root, "crw")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	env := scope.Env{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "HOME", "XDG_STATE_HOME", "XDG_CONFIG_HOME", "CODEX_HOME", "CRW_HOME", "CODEX_SESSION_RELAY_STATE":
			continue
		}
		env = append(env, entry)
	}
	env = append(env,
		"HOME="+home,
		"CODEX_HOME="+filepath.Join(home, ".codex"),
		"XDG_STATE_HOME="+filepath.Join(root, "state"),
		"XDG_CONFIG_HOME="+filepath.Join(root, "config"),
		"CRW_HOME="+filepath.Join(root, "crw"),
	)
	return env, home
}
