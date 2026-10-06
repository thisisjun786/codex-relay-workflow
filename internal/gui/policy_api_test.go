package gui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

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

// digestOf is the SHA-256 of the bytes.
func digestOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// policyHost is an isolated Codex home with a policy file and, when withRecord is true, a
// version-2 wiring record naming it. It sets HOME, CODEX_HOME and XDG_STATE_HOME for the test
// process, so the handlers read the isolated host and never the operator's.
func policyHost(t *testing.T, text string, withRecord bool) string {
	t.Helper()
	root := t.TempDir()
	codexHome := filepath.Join(root, ".codex")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
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
	return file
}

// policyServer builds a Server over the package registry so the real routes are exercised.
func policyServer(t *testing.T) *Server {
	t.Helper()
	server, err := New(Options{Port: guardPort, Token: guardToken, Version: "test-version"})
	if err != nil {
		t.Fatalf("New over the package registry: %v", err)
	}
	return server
}

// decodePolicy drives GET /api/policy and decodes the body.
func decodePolicy(t *testing.T, server *Server) map[string]any {
	t.Helper()
	recorder := request(server, http.MethodGet, "/api/policy", guardHost, "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/policy: %d %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("policy body %q: %v", recorder.Body.String(), err)
	}
	return body
}

// checkResponse drives POST /api/policy/check and decodes the body.
func checkResponse(t *testing.T, server *Server, payload string) (int, map[string]any) {
	t.Helper()
	recorder := request(server, http.MethodPost, "/api/policy/check", guardHost, payload, writeHeaders())
	var body map[string]any
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("check body %q: %v", recorder.Body.String(), err)
		}
	}
	return recorder.Code, body
}

// TestPolicyReadsTheRegisteredFile is C1: the declared values and the file's digests come back.
func TestPolicyReadsTheRegisteredFile(t *testing.T) {
	file := policyHost(t, policyText, true)
	body := decodePolicy(t, policyServer(t))
	if body["state"] != "registered" {
		t.Fatalf("state = %v (%v)", body["state"], body["reason"])
	}
	if body["path"] != file || body["digest"] != digestOf(policyText) || body["registeredDigest"] != digestOf(policyText) {
		t.Fatalf("digests = %v", body)
	}
	if body["mode"] != "allowlist" {
		t.Fatalf("mode = %v", body["mode"])
	}
	roles, ok := body["roles"].([]any)
	if !ok || len(roles) != 3 {
		t.Fatalf("roles = %v", body["roles"])
	}
	allowed, _ := body["allowed"].([]any)
	if len(allowed) != 2 {
		t.Fatalf("allowed = %v", body["allowed"])
	}
	exceptions, _ := body["exceptions"].([]any)
	if len(exceptions) != 1 {
		t.Fatalf("exceptions = %v", body["exceptions"])
	}
	// The running digest could not be read on this isolated host, so applied is unverifiable with
	// the reason attached, never a silent success.
	if body["applied"] != "unverifiable" {
		t.Fatalf("applied = %v", body["applied"])
	}
	if reason, _ := body["runningReason"].(string); reason == "" {
		t.Fatalf("an unreadable running digest carries no reason: %v", body)
	}
	if body["runningDigest"] != nil {
		t.Fatalf("runningDigest = %v, want null", body["runningDigest"])
	}
}

// TestPolicyWithoutARecordIsNotRegistered is C2.
func TestPolicyWithoutARecordIsNotRegistered(t *testing.T) {
	policyHost(t, policyText, false)
	body := decodePolicy(t, policyServer(t))
	if body["state"] != "not_registered" {
		t.Fatalf("state = %v", body["state"])
	}
	if body["applied"] != "unverifiable" {
		t.Fatalf("applied = %v", body["applied"])
	}
}

// TestPolicyWithAnUnreadableRecordIsUnreadable is C2.
func TestPolicyWithAnUnreadableRecordIsUnreadable(t *testing.T) {
	policyHost(t, policyText, true)
	codexHome := os.Getenv("CODEX_HOME")
	if err := os.WriteFile(filepath.Join(codexHome, "crw-bridge-mcp.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := decodePolicy(t, policyServer(t))
	if body["state"] != "unreadable" {
		t.Fatalf("state = %v", body["state"])
	}
	if reason, _ := body["reason"].(string); reason == "" {
		t.Fatal("an unreadable answer carries no reason")
	}
}

// TestPolicyWithAnUnreadableFileIsUnreadable is C2.
func TestPolicyWithAnUnreadableFileIsUnreadable(t *testing.T) {
	file := policyHost(t, policyText, true)
	if err := os.WriteFile(file, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := decodePolicy(t, policyServer(t))
	if body["state"] != "unreadable" {
		t.Fatalf("state = %v", body["state"])
	}
}

// TestPolicyCheckRejectsAnInvalidPair is C3.
func TestPolicyCheckRejectsAnInvalidPair(t *testing.T) {
	policyHost(t, policyText, true)
	payload := "{\"expectedDigest\":\"" + digestOf(policyText) + "\",\"change\":{\"Kind\":\"setRolePairs\",\"Role\":\"child\",\"Pairs\":[{\"Model\":\"nobody/nothing\",\"Effort\":\"xhigh\"}]}}"
	code, body := checkResponse(t, policyServer(t), payload)
	if code != http.StatusOK || body["valid"] != false {
		t.Fatalf("an invalid pair was accepted: %d %v", code, body)
	}
	errors, _ := body["errors"].([]any)
	if len(errors) == 0 {
		t.Fatalf("an invalid check carries no error: %v", body)
	}
}

// TestPolicyCheckDoesNotSwapMaxAndXhigh is C3.
func TestPolicyCheckDoesNotSwapMaxAndXhigh(t *testing.T) {
	policyHost(t, policyText, true)
	payload := "{\"expectedDigest\":\"" + digestOf(policyText) + "\",\"change\":{\"Kind\":\"setAllowed\",\"Model\":\"gpt-6.1-sol\",\"Efforts\":[\"max\"]}}"
	code, body := checkResponse(t, policyServer(t), payload)
	if code != http.StatusOK || body["valid"] != false {
		t.Fatalf("max was accepted where the file approves xhigh: %d %v", code, body)
	}
}

// TestPolicyCheckRejectsASupervisorModelWrite is C3.
func TestPolicyCheckRejectsASupervisorModelWrite(t *testing.T) {
	policyHost(t, policyText, true)
	payload := "{\"expectedDigest\":\"" + digestOf(policyText) + "\",\"change\":{\"Kind\":\"setRolePairs\",\"Role\":\"supervisor\",\"Pairs\":[{\"Model\":\"anthropic/opus\",\"Effort\":\"xhigh\"}]}}"
	code, body := checkResponse(t, policyServer(t), payload)
	if code != http.StatusOK || body["valid"] != false {
		t.Fatalf("a supervisor model write was accepted: %d %v", code, body)
	}
}

// TestPolicyCheckAcceptsAValidChange is C3's positive half.
func TestPolicyCheckAcceptsAValidChange(t *testing.T) {
	policyHost(t, policyText, true)
	payload := "{\"expectedDigest\":\"" + digestOf(policyText) + "\",\"change\":{\"Kind\":\"removeException\",\"ID\":\"legacy\"}}"
	code, body := checkResponse(t, policyServer(t), payload)
	if code != http.StatusOK || body["valid"] != true {
		t.Fatalf("a valid change was refused: %d %v", code, body)
	}
	diff, _ := body["diff"].([]any)
	if len(diff) != 1 || diff[0] != "exceptions.legacy" {
		t.Fatalf("diff = %v", body["diff"])
	}
}

// TestPolicyCheckReportsAStaleDigest is C3: the caller's digest is compared and returned.
func TestPolicyCheckReportsAStaleDigest(t *testing.T) {
	policyHost(t, policyText, true)
	payload := "{\"expectedDigest\":\"0000\",\"change\":{\"Kind\":\"removeException\",\"ID\":\"legacy\"}}"
	code, body := checkResponse(t, policyServer(t), payload)
	if code != http.StatusOK || body["stale"] != true || body["currentDigest"] != digestOf(policyText) {
		t.Fatalf("stale check: %d %v", code, body)
	}
}

// TestPolicyCheckWritesNothing is C4: both files stay byte-identical and no new file appears.
// policyWritableText is a policy with an allowlist entry no role uses, so every one of the five
// change kinds is a legal candidate against it.
const policyWritableText = "{\n" +
	"  \"roles\": {\n" +
	"    \"child\": {\"model\": \"anthropic/opus\", \"reasoningEffort\": \"xhigh\"},\n" +
	"    \"parent\": {\"pairs\": [{\"model\": \"anthropic/opus\", \"reasoningEffort\": \"xhigh\"}, {\"model\": \"gpt-6.1-sol\", \"reasoningEffort\": \"xhigh\"}]},\n" +
	"    \"supervisor\": {\"expectation\": \"record\"}\n" +
	"  },\n" +
	"  \"allowed\": [\n" +
	"    {\"model\": \"anthropic/opus\", \"efforts\": [\"xhigh\", \"max\"]},\n" +
	"    {\"model\": \"gpt-6.1-sol\", \"efforts\": [\"xhigh\"]},\n" +
	"    {\"model\": \"openai/gpt-5\", \"efforts\": [\"high\"]}\n" +
	"  ],\n" +
	"  \"exceptions\": {\n" +
	"    \"legacy\": {\"role\": \"parent\", \"model\": \"devin/swe-2\", \"reasoningEffort\": \"max\", \"cwd\": [\"/tmp/project\"]}\n" +
	"  }\n" +
	"}\n"

func TestPolicyCheckWritesNothing(t *testing.T) {
	file := policyHost(t, policyWritableText, true)
	codexHome := os.Getenv("CODEX_HOME")
	record := filepath.Join(codexHome, "crw-bridge-mcp.json")
	before := policyListing(t, filepath.Dir(file))
	recordBefore := policyListing(t, codexHome)
	fileBytes, _ := os.ReadFile(file)
	recordBytes, _ := os.ReadFile(record)

	server := policyServer(t)
	for _, change := range []string{
		"{\"Kind\":\"setRolePairs\",\"Role\":\"child\",\"Pairs\":[{\"Model\":\"anthropic/opus\",\"Effort\":\"xhigh\"}]}",
		"{\"Kind\":\"removeException\",\"ID\":\"legacy\"}",
		"{\"Kind\":\"setAllowed\",\"Model\":\"openai/gpt-5\",\"Efforts\":[\"high\"]}",
		"{\"Kind\":\"setException\",\"ID\":\"extra\",\"Role\":\"child\",\"Model\":\"anthropic/opus\",\"Effort\":\"xhigh\",\"CWD\":[\"/tmp/project\"]}",
		"{\"Kind\":\"removeAllowed\",\"Model\":\"openai/gpt-5\"}",
	} {
		payload := "{\"expectedDigest\":\"" + digestOf(policyWritableText) + "\",\"change\":" + change + "}"
		code, body := checkResponse(t, server, payload)
		if code != http.StatusOK {
			t.Fatalf("check: %d %v", code, body)
		}
		// A check that answered 200 with valid=false would leave the write-nothing claim vacuous, so
		// every one of the five kinds must actually be a legal candidate.
		if body["valid"] != true {
			t.Fatalf("change %s was refused, so this test proves nothing: %v", change, body)
		}
	}

	if got := policyListing(t, filepath.Dir(file)); !sameList(before, got) {
		t.Fatalf("the policy directory changed: %v -> %v", before, got)
	}
	if got := policyListing(t, codexHome); !sameList(recordBefore, got) {
		t.Fatalf("the Codex home changed: %v -> %v", recordBefore, got)
	}
	if after, _ := os.ReadFile(file); string(after) != string(fileBytes) {
		t.Fatal("the policy file changed")
	}
	if after, _ := os.ReadFile(record); string(after) != string(recordBytes) {
		t.Fatal("the wiring record changed")
	}
}

// TestPolicyCheckNeedsTheGuard is a security check: the write route is not reachable without the
// token the guard requires.
func TestPolicyCheckNeedsTheGuard(t *testing.T) {
	policyHost(t, policyText, true)
	payload := "{\"expectedDigest\":\"x\",\"change\":{\"Kind\":\"removeException\",\"ID\":\"legacy\"}}"
	recorder := request(policyServer(t), http.MethodPost, "/api/policy/check", guardHost, payload, map[string]string{"Content-Type": "application/json"})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("a check without the token was answered %d", recorder.Code)
	}
}

// TestCatalogIsRegistered is C5: the catalog route answers and keeps the reader's status apart.
func TestCatalogIsRegistered(t *testing.T) {
	found := false
	for _, route := range Routes() {
		if route.Method == http.MethodGet && route.Path == "/api/catalog" {
			found = true
		}
	}
	if !found {
		t.Fatal("GET /api/catalog is not registered")
	}
	recorder := request(policyServer(t), http.MethodGet, "/api/catalog", guardHost, "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/catalog: %d %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Status string `json:"status"`
		State  string `json:"state"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("catalog body %q: %v", recorder.Body.String(), err)
	}
	if body.Status != "unavailable" && body.Status != "fresh" && body.Status != "stale" {
		t.Fatalf("status = %q", body.Status)
	}
	// A catalog that could not be read is not reported as an unsupported one.
	if body.Status == "unavailable" && body.State == "unsupported-ocx-catalog" {
		t.Fatalf("an unreadable catalog was reported unsupported: %s", recorder.Body.String())
	}
}

// TestCatalogRefreshIsAccepted is C5: ?refresh=1 is a legal request.
func TestCatalogRefreshIsAccepted(t *testing.T) {
	recorder := request(policyServer(t), http.MethodGet, "/api/catalog?refresh=1", guardHost, "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/catalog?refresh=1: %d %s", recorder.Code, recorder.Body.String())
	}
}

func policyListing(t *testing.T, dir string) []string {
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

func sameList(one, other []string) bool {
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
