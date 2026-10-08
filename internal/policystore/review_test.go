package policystore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// roleWithMCP is a policy whose child role declares MCP profiles beside its pair.
const roleWithMCP = "{\n" +
	"  \"roles\": {\n" +
	"    \"child\": {\"model\": \"anthropic/opus\", \"reasoningEffort\": \"xhigh\", \"mcp\": {\"default\": \"minimal\", \"profiles\": {\"minimal\": {\"servers\": [\"alpha\"]}}}},\n" +
	"    \"parent\": {\"model\": \"anthropic/opus\", \"reasoningEffort\": \"xhigh\"}\n" +
	"  }\n" +
	"}\n"

// exceptionWithReason is a policy whose exception records why it exists.
const exceptionWithReason = "{\n" +
	"  \"roles\": {\"child\": {\"model\": \"anthropic/opus\", \"reasoningEffort\": \"xhigh\"}},\n" +
	"  \"exceptions\": {\n" +
	"    \"legacy\": {\"role\": \"child\", \"model\": \"devin/swe-2\", \"reasoningEffort\": \"max\", \"cwd\": [\"/tmp/project\"], \"reason\": \"temporary vendor migration\"}\n" +
	"  }\n" +
	"}\n"

// TestCheckPreservesARolesMCPProfiles is a review finding: a pair change must not drop the other
// keys a role declares.
func TestCheckPreservesARolesMCPProfiles(t *testing.T) {
	change := Change{Kind: KindSetRolePairs, Role: "child", Pairs: []Pair{{Model: "anthropic/opus", Effort: "max"}}}
	result := Check([]byte(roleWithMCP), digestOf(roleWithMCP), change)
	if !result.Valid {
		t.Fatalf("the change was refused: %+v", result)
	}
	candidate, err := candidateOf(t, roleWithMCP, change)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(candidate, "\"mcp\"") || !strings.Contains(candidate, "minimal") {
		t.Fatalf("the candidate dropped the role mcp profiles: %s", candidate)
	}
}

// TestCheckPreservesAnExceptionsReason is a review finding: replacing an exception must not drop
// the reason it records.
func TestCheckPreservesAnExceptionsReason(t *testing.T) {
	change := Change{Kind: KindSetException, ID: "legacy", Role: "child", Model: "devin/swe-2", Effort: "high", CWD: []string{"/tmp/project"}}
	result := Check([]byte(exceptionWithReason), digestOf(exceptionWithReason), change)
	if !result.Valid {
		t.Fatalf("the change was refused: %+v", result)
	}
	candidate, err := candidateOf(t, exceptionWithReason, change)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(candidate, "temporary vendor migration") {
		t.Fatalf("the candidate dropped the exception reason: %s", candidate)
	}
}

// TestCheckRefusesARequestThatSetsTwoKinds is a review finding: fields outside the named kind must
// not be silently ignored.
func TestCheckRefusesARequestThatSetsTwoKinds(t *testing.T) {
	change := Change{Kind: KindSetRolePairs, Role: "child", Pairs: []Pair{{Model: "anthropic/opus", Effort: "xhigh"}}, Model: "gpt-x", Efforts: []string{"max"}}
	result := Check([]byte(policyText), digestOf(policyText), change)
	if result.Valid {
		t.Fatalf("a request carrying fields of two kinds was accepted: %+v", result)
	}
}

// TestCheckRemovingTheLastAllowedModelKeepsTheKey is the CRW-134 evaluation finding: dropping the
// allowed key when its last model is removed turns the document into a presence_only one, which
// widens what the host allows, so the candidate must keep the key and be refused by the parser.
func TestCheckRemovingTheLastAllowedModelKeepsTheKey(t *testing.T) {
	one := "{\"roles\": {\"child\": {\"model\": \"anthropic/opus\", \"reasoningEffort\": \"xhigh\"}}, \"allowed\": [{\"model\": \"anthropic/opus\", \"efforts\": [\"xhigh\"]}]}\n"
	change := Change{Kind: KindRemoveAllowed, Model: "anthropic/opus"}
	result := Check([]byte(one), digestOf(one), change)
	if result.Valid {
		t.Fatalf("removing the last allowed model was approved, which widens the policy: %+v", result)
	}
	candidate, err := candidateOf(t, one, change)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(candidate, "\"allowed\"") {
		t.Fatalf("the candidate dropped the allowed key, which is presence_only: %s", candidate)
	}
}

// TestLocateRefusesARecordTheLauncherWouldRefuse is a review finding: a record whose owner or
// version the launcher rejects must not read as registered.
func TestLocateRefusesARecordTheLauncherWouldRefuse(t *testing.T) {
	for _, record := range []map[string]any{
		{"recordVersion": 2, "owner": "config", "serverName": "codex-thread-bridge", "bridgeExecutable": "/usr/local/bin/codex-thread-bridge", "args": []string{}},
		{"recordVersion": 1, "owner": "plugin", "serverName": "codex-thread-bridge", "bridgeExecutable": "/usr/local/bin/codex-thread-bridge", "args": []string{}},
		{"recordVersion": 2, "owner": "plugin", "serverName": "another-server", "bridgeExecutable": "/usr/local/bin/codex-thread-bridge", "args": []string{}},
	} {
		root := t.TempDir()
		codexHome := filepath.Join(root, ".codex")
		if err := os.MkdirAll(codexHome, 0o755); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(root, "policy.json")
		if err := os.WriteFile(file, []byte(policyText), 0o644); err != nil {
			t.Fatal(err)
		}
		if record["recordVersion"] == 2 {
			record["executionPolicy"] = map[string]any{"path": file, "digest": digestOf(policyText)}
		}
		raw, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(codexHome, "crw-bridge-mcp.json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
		located := Locate(envOf(map[string]string{"HOME": root, "CODEX_HOME": codexHome}))
		if located.State == Registered {
			t.Fatalf("a record the launcher refuses read as registered: %+v (record %v)", located, record)
		}
	}
}

// candidateOf is the candidate document a check would write, re-read from its own encoder.
func candidateOf(t *testing.T, text string, change Change) (string, error) {
	t.Helper()
	document, err := decode([]byte(text))
	if err != nil {
		return "", err
	}
	updated, _, err := apply(document, change)
	if err != nil {
		return "", err
	}
	return encode(updated), nil
}
