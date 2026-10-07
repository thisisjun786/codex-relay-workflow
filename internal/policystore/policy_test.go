package policystore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// envOf is the LookupEnv a test supplies instead of the host's.
func envOf(pairs map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) { value, ok := pairs[key]; return value, ok }
}

// policyText is a policy with two roles, an allowlist and one exception.
const policyText = "{\n" +
	"  \"roles\": {\n" +
	"    \"child\": {\"model\": \"anthropic/opus\", \"reasoningEffort\": \"xhigh\"},\n" +
	"    \"parent\": {\"pairs\": [{\"model\": \"anthropic/opus\", \"reasoningEffort\": \"xhigh\"}, {\"model\": \"gpt-6.1-sol\", \"reasoningEffort\": \"xhigh\"}]},\n" +
	"    \"supervisor\": {\"expectation\": \"record\"}\n" +
	"  },\n" +
	"  \"allowed\": [\n" +
	"    {\"model\": \"anthropic/opus\", \"efforts\": [\"xhigh\", \"max\"]},\n" +
	"    {\"model\": \"gpt-6.1-sol\", \"efforts\": [\"xhigh\"]}\n" +
	"  ],\n" +
	"  \"exceptions\": {\n" +
	"    \"legacy\": {\"role\": \"parent\", \"model\": \"devin/swe-2\", \"reasoningEffort\": \"max\", \"cwd\": [\"/tmp/project\"]}\n" +
	"  }\n" +
	"}\n"

// digestOf is the SHA-256 of the bytes, which is the digest every reader of the file uses.
func digestOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// host writes a policy file and, when withRecord is true, a version-2 wiring record naming it.
func host(t *testing.T, text string, withRecord bool) (map[string]string, string) {
	t.Helper()
	root := t.TempDir()
	codexHome := filepath.Join(root, ".codex")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "execution-policy.json")
	if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if withRecord {
		record := map[string]any{
			"recordVersion": 2, "owner": "plugin", "serverName": "codex-thread-bridge",
			"bridgeExecutable": "/usr/local/bin/codex-thread-bridge", "args": []string{},
			"executionPolicy": map[string]any{"path": file, "digest": digestOf(text)},
		}
		raw, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(codexHome, "crw-bridge-mcp.json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return map[string]string{"HOME": root, "CODEX_HOME": codexHome}, file
}

// TestLocateWithoutARecordIsNotRegistered is C2: a host with no record is a named state.
func TestLocateWithoutARecordIsNotRegistered(t *testing.T) {
	env, _ := host(t, policyText, false)
	located := Locate(envOf(env))
	if located.State != NotRegistered {
		t.Fatalf("state = %q, want %q", located.State, NotRegistered)
	}
	if located.Path != "" || located.RegisteredDigest != "" {
		t.Fatalf("a not_registered location carries values: %+v", located)
	}
}

// TestLocateReportsTheRegisteredPolicyAndDigest is C1: the record names the file and its digest.
func TestLocateReportsTheRegisteredPolicyAndDigest(t *testing.T) {
	env, file := host(t, policyText, true)
	located := Locate(envOf(env))
	if located.State != Registered {
		t.Fatalf("state = %q (%s), want %q", located.State, located.Reason, Registered)
	}
	if located.Path != file || located.RegisteredDigest != digestOf(policyText) {
		t.Fatalf("located = %+v", located)
	}
}

// TestLocateReportsAnUnreadableRecord is C2: a record that cannot be used is not no policy.
func TestLocateReportsAnUnreadableRecord(t *testing.T) {
	env, _ := host(t, policyText, true)
	if err := os.WriteFile(filepath.Join(env["CODEX_HOME"], "crw-bridge-mcp.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	located := Locate(envOf(env))
	if located.State != Unreadable {
		t.Fatalf("state = %q, want %q", located.State, Unreadable)
	}
	if located.Reason == "" {
		t.Fatal("an unreadable record carries no reason")
	}
}

// TestReadProjectsRolesAllowedAndExceptions is C1: the declared values come from the file.
func TestReadProjectsRolesAllowedAndExceptions(t *testing.T) {
	env, _ := host(t, policyText, true)
	reading := Read(Locate(envOf(env)))
	if reading.State != Registered {
		t.Fatalf("state = %q (%s)", reading.State, reading.Reason)
	}
	if reading.Digest != digestOf(policyText) {
		t.Fatalf("digest = %q", reading.Digest)
	}
	child, ok := roleOf(reading, "child")
	if !ok || len(child.Pairs) != 1 || child.Pairs[0] != (Pair{Model: "anthropic/opus", Effort: "xhigh"}) {
		t.Fatalf("child = %+v", child)
	}
	parent, ok := roleOf(reading, "parent")
	if !ok || len(parent.Pairs) != 2 || parent.Expectation != "pair" {
		t.Fatalf("parent = %+v", parent)
	}
	supervisor, ok := roleOf(reading, "supervisor")
	if !ok || supervisor.Expectation != "record" || len(supervisor.Pairs) != 0 {
		t.Fatalf("supervisor = %+v", supervisor)
	}
	if len(reading.Allowed) != 2 || reading.Allowed[0].Model != "anthropic/opus" ||
		strings.Join(reading.Allowed[0].Efforts, ",") != "max,xhigh" {
		t.Fatalf("allowed = %+v", reading.Allowed)
	}
	if len(reading.Exceptions) != 1 || reading.Exceptions[0].ID != "legacy" ||
		reading.Exceptions[0].Model != "devin/swe-2" || reading.Exceptions[0].Role != "parent" {
		t.Fatalf("exceptions = %+v", reading.Exceptions)
	}
}

// TestReadReportsAnUnreadablePolicyFile is C2: a file that cannot be parsed is a named state.
func TestReadReportsAnUnreadablePolicyFile(t *testing.T) {
	env, file := host(t, policyText, true)
	if err := os.WriteFile(file, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	reading := Read(Locate(envOf(env)))
	if reading.State != Unreadable || reading.Reason == "" {
		t.Fatalf("reading = %+v", reading)
	}
}

// TestCheckRejectsAnInvalidPair is C3: the candidate must pass the bridge's own checks.
func TestCheckRejectsAnInvalidPair(t *testing.T) {
	change := Change{Kind: KindSetRolePairs, Role: "child", Pairs: []Pair{{Model: "anthropic/opus", Effort: "xhigh"}, {Model: "nobody/nothing", Effort: "xhigh"}}}
	result := Check([]byte(policyText), digestOf(policyText), change)
	if result.Valid {
		t.Fatalf("an unapproved pair was accepted: %+v", result)
	}
	if len(result.Errors) == 0 {
		t.Fatal("an invalid candidate carries no error")
	}
}

// TestCheckDoesNotSwapMaxAndXhigh is C3: the two effort names are distinct values.
func TestCheckDoesNotSwapMaxAndXhigh(t *testing.T) {
	change := Change{Kind: KindSetAllowed, Model: "gpt-6.1-sol", Efforts: []string{"max"}}
	result := Check([]byte(policyText), digestOf(policyText), change)
	if result.Valid {
		t.Fatalf("max was accepted where the file approves xhigh: %+v", result)
	}
}

// TestCheckRejectsASupervisorModelWrite is C3: a supervisor carries no model.
func TestCheckRejectsASupervisorModelWrite(t *testing.T) {
	change := Change{Kind: KindSetRolePairs, Role: "supervisor", Pairs: []Pair{{Model: "anthropic/opus", Effort: "xhigh"}}}
	result := Check([]byte(policyText), digestOf(policyText), change)
	if result.Valid {
		t.Fatalf("a supervisor model write was accepted: %+v", result)
	}
}

// TestCheckRejectsAChangeOutsideTheRequestedOne is C3: only the named target may move.
func TestCheckRejectsAChangeOutsideTheRequestedOne(t *testing.T) {
	change := Change{Kind: KindRemoveException, ID: "legacy"}
	result := Check([]byte(policyText), digestOf(policyText), change)
	if !result.Valid {
		t.Fatalf("removing an exception was refused: %+v", result)
	}
	if len(result.Diff) == 0 || !containsChange(result.Diff, "exceptions.legacy") {
		t.Fatalf("diff = %v, want the removed exception named", result.Diff)
	}
	// A change whose target does not exist cannot be applied, so nothing moves and it is refused.
	missing := Change{Kind: KindRemoveException, ID: "absent"}
	if refused := Check([]byte(policyText), digestOf(policyText), missing); refused.Valid {
		t.Fatalf("removing an absent exception was accepted: %+v", refused)
	}
}

// TestCheckReportsAStaleExpectedDigest is C3: the caller's digest is compared, not assumed.
func TestCheckReportsAStaleExpectedDigest(t *testing.T) {
	result := Check([]byte(policyText), "0000", Change{Kind: KindRemoveException, ID: "legacy"})
	if !result.Stale {
		t.Fatalf("a mismatched expectedDigest was not reported stale: %+v", result)
	}
	if result.CurrentDigest != digestOf(policyText) {
		t.Fatalf("currentDigest = %q", result.CurrentDigest)
	}
}

// TestCheckWritesNothing is C4: the policy file and the record stay byte-identical and no new file
// appears in either directory.
func TestCheckWritesNothing(t *testing.T) {
	env, file := host(t, policyText, true)
	record := filepath.Join(env["CODEX_HOME"], "crw-bridge-mcp.json")
	before := listing(t, filepath.Dir(file))
	recordBefore := listing(t, env["CODEX_HOME"])
	fileBytes, _ := os.ReadFile(file)
	recordBytes, _ := os.ReadFile(record)

	for _, change := range []Change{
		{Kind: KindSetRolePairs, Role: "child", Pairs: []Pair{{Model: "anthropic/opus", Effort: "xhigh"}}},
		{Kind: KindRemoveException, ID: "legacy"},
		{Kind: KindSetAllowed, Model: "anthropic/opus", Efforts: []string{"max"}},
	} {
		Check([]byte(policyText), digestOf(policyText), change)
	}

	if got := listing(t, filepath.Dir(file)); !equalLists(before, got) {
		t.Fatalf("the policy directory changed: %v -> %v", before, got)
	}
	if got := listing(t, env["CODEX_HOME"]); !equalLists(recordBefore, got) {
		t.Fatalf("the Codex home changed: %v -> %v", recordBefore, got)
	}
	if after, _ := os.ReadFile(file); string(after) != string(fileBytes) {
		t.Fatal("the policy file changed")
	}
	if after, _ := os.ReadFile(record); string(after) != string(recordBytes) {
		t.Fatal("the wiring record changed")
	}
}

func roleOf(reading Reading, name string) (RoleView, bool) {
	for _, role := range reading.Roles {
		if role.Name == name {
			return role, true
		}
	}
	return RoleView{}, false
}

func containsChange(diff []string, want string) bool {
	for _, item := range diff {
		if item == want {
			return true
		}
	}
	return false
}

func listing(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func equalLists(one, other []string) bool {
	if len(one) != len(other) {
		return false
	}
	for i := range one {
		if one[i] != other[i] {
			return false
		}
	}
	return true
}
