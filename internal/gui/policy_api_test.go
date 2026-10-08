package gui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/policystore"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
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
	// CRW_CONFIG would otherwise name the developer's real management configuration, which the
	// relay reader consults for the socket and the state directory.
	t.Setenv("CRW_CONFIG", filepath.Join(root, "crw-config.json"))
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
	payload := "{\"expectedDigest\":\"" + digestOf(policyText) + "\",\"change\":{\"kind\":\"setRolePairs\",\"role\":\"child\",\"pairs\":[{\"model\":\"nobody/nothing\",\"reasoningEffort\":\"xhigh\"}]}}"
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
	payload := "{\"expectedDigest\":\"" + digestOf(policyText) + "\",\"change\":{\"kind\":\"setAllowed\",\"model\":\"gpt-6.1-sol\",\"efforts\":[\"max\"]}}"
	code, body := checkResponse(t, policyServer(t), payload)
	if code != http.StatusOK || body["valid"] != false {
		t.Fatalf("max was accepted where the file approves xhigh: %d %v", code, body)
	}
}

// TestPolicyCheckRejectsASupervisorModelWrite is C3.
func TestPolicyCheckRejectsASupervisorModelWrite(t *testing.T) {
	policyHost(t, policyText, true)
	payload := "{\"expectedDigest\":\"" + digestOf(policyText) + "\",\"change\":{\"kind\":\"setRolePairs\",\"role\":\"supervisor\",\"pairs\":[{\"model\":\"anthropic/opus\",\"reasoningEffort\":\"xhigh\"}]}}"
	code, body := checkResponse(t, policyServer(t), payload)
	if code != http.StatusOK || body["valid"] != false {
		t.Fatalf("a supervisor model write was accepted: %d %v", code, body)
	}
}

// TestPolicyCheckAcceptsAValidChange is C3's positive half.
func TestPolicyCheckAcceptsAValidChange(t *testing.T) {
	policyHost(t, policyText, true)
	payload := "{\"expectedDigest\":\"" + digestOf(policyText) + "\",\"change\":{\"kind\":\"removeException\",\"id\":\"legacy\"}}"
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
	payload := "{\"expectedDigest\":\"0000\",\"change\":{\"kind\":\"removeException\",\"id\":\"legacy\"}}"
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
		"{\"kind\":\"setRolePairs\",\"role\":\"child\",\"pairs\":[{\"model\":\"anthropic/opus\",\"reasoningEffort\":\"xhigh\"}]}",
		"{\"kind\":\"removeException\",\"id\":\"legacy\"}",
		"{\"kind\":\"setAllowed\",\"model\":\"openai/gpt-5\",\"efforts\":[\"high\"]}",
		"{\"kind\":\"setException\",\"id\":\"extra\",\"role\":\"child\",\"model\":\"anthropic/opus\",\"reasoningEffort\":\"xhigh\",\"cwd\":[\"/tmp/project\"]}",
		"{\"kind\":\"removeAllowed\",\"model\":\"openai/gpt-5\"}",
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
	payload := "{\"expectedDigest\":\"x\",\"change\":{\"kind\":\"removeException\",\"id\":\"legacy\"}}"
	recorder := request(policyServer(t), http.MethodPost, "/api/policy/check", guardHost, payload, map[string]string{"Content-Type": "application/json"})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("a check without the token was answered %d", recorder.Code)
	}
}

// catalogHost isolates the catalog tests: the reader resolves the model catalog below the home and
// runs the OCX probe, so HOME and CRW_HOME are pointed at a temporary directory before either runs.
func catalogHost(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("CRW_HOME", filepath.Join(root, "crw"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	// The catalog reader probes the OCX binary on PATH and reads the Codex home's model catalog,
	// so both are pointed at an empty directory: the test is about this route, not the host.
	t.Setenv("PATH", filepath.Join(root, "bin"))
	return root
}

// TestCatalogIsRegistered is C5: the catalog route answers and keeps the reader's status apart.
func TestCatalogIsRegistered(t *testing.T) {
	catalogHost(t)
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
	catalogHost(t)
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

// policyWriteResponse drives POST /api/policy and decodes the body whatever the status.
func policyWriteResponse(t *testing.T, server *Server, payload string) (int, map[string]any) {
	t.Helper()
	recorder := request(server, http.MethodPost, "/api/policy", guardHost, payload, writeHeaders())
	var body map[string]any
	if recorder.Body.Len() > 0 {
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("write body %q: %v", recorder.Body.String(), err)
		}
	}
	return recorder.Code, body
}

// policyWriteSeamsFor installs the seams the route's write runs with: a registration the test
// chooses and a running digest that cannot be read, so no installer runs and no real relay is
// touched. The previous seams are restored when the test ends.
func policyWriteSeamsFor(t *testing.T, register policystore.RegisterFunc, running func(context.Context, policystore.LookupEnv) policystore.Running) {
	t.Helper()
	if register == nil {
		// A test that does not expect the registration step must not fall through to the real
		// installer.
		register = func(context.Context, string) policystore.RegisterAnswer {
			t.Fatal("the registration step ran where the test did not expect it")
			return policystore.RegisterAnswer{}
		}
	}
	if running == nil {
		running = unavailablePolicyRunning
	}
	previous := policyWriteSeams
	policyWriteSeams = policystore.WriteOptions{Register: register, Running: running}
	t.Cleanup(func() { policyWriteSeams = previous })
}

// rewritePolicyRecord writes the wiring record naming file with digest, the durable effect a
// successful re-registration leaves.
func rewritePolicyRecord(t *testing.T, file, digest string) {
	t.Helper()
	record := map[string]any{
		"recordVersion": 2, "owner": "plugin", "serverName": "codex-thread-bridge",
		"bridgeExecutable": "/usr/local/bin/codex-thread-bridge", "args": []string{},
		"executionPolicy": map[string]any{"path": file, "digest": digest},
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "crw-bridge-mcp.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// updatingPolicyRegister is a registration that rewrites the record to the file's current digest
// and answers record_updated, as crw install register-mcp --re-register-policy does.
func updatingPolicyRegister(t *testing.T) policystore.RegisterFunc {
	return func(_ context.Context, path string) policystore.RegisterAnswer {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		rewritePolicyRecord(t, path, digestOf(string(raw)))
		return policystore.RegisterAnswer{ExitCode: 0, Stdout: []byte("{\"outcome\": \"record_updated\"}")}
	}
}

// unavailablePolicyRunning is a running digest that could not be read.
func unavailablePolicyRunning(context.Context, policystore.LookupEnv) policystore.Running {
	return policystore.Running{State: policystore.RunningUnavailable, Reason: "worker_policy_unreadable"}
}

// TestPolicyWriteStoresAndReportsSeparateFields is C4 at the route: stored, registered and applied
// come back as three separate answers, and a relay that holds the old bytes is needs_user_action.
func TestPolicyWriteStoresAndReportsSeparateFields(t *testing.T) {
	file := policyHost(t, policyWritableText, true)
	policyWriteSeamsFor(t, updatingPolicyRegister(t), func(context.Context, policystore.LookupEnv) policystore.Running {
		return policystore.Running{State: policystore.RunningObserved, Digest: digestOf(policyWritableText)}
	})
	payload := "{\"expectedDigest\":\"" + digestOf(policyWritableText) + "\",\"change\":{\"kind\":\"removeException\",\"id\":\"legacy\"}}"
	code, body := policyWriteResponse(t, policyServer(t), payload)
	if code != http.StatusOK {
		t.Fatalf("POST /api/policy: %d %v", code, body)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	newDigest := digestOf(string(raw))
	stored, _ := body["stored"].(map[string]any)
	registered, _ := body["registered"].(map[string]any)
	if stored["digest"] != newDigest || registered["digest"] != newDigest {
		t.Fatalf("stored = %v registered = %v, want %q", body["stored"], body["registered"], newDigest)
	}
	if newDigest == digestOf(policyWritableText) {
		t.Fatal("the policy file was not replaced")
	}
	if body["applied"] != policystore.AppliedNeedsAction {
		t.Fatalf("applied = %v, want %q", body["applied"], policystore.AppliedNeedsAction)
	}
	actions, _ := body["actions"].([]any)
	if len(actions) != 1 || actions[0] != policystore.AppliedActionRestart {
		t.Fatalf("actions = %v, want the restart", body["actions"])
	}
}

// TestPolicyWriteRefusesAStaleDigest is C1 at the route: the digest on disk comes back with the
// refusal and the file is untouched.
func TestPolicyWriteRefusesAStaleDigest(t *testing.T) {
	file := policyHost(t, policyWritableText, true)
	policyWriteSeamsFor(t, nil, unavailablePolicyRunning)
	payload := "{\"expectedDigest\":\"0000\",\"change\":{\"kind\":\"removeException\",\"id\":\"legacy\"}}"
	code, body := policyWriteResponse(t, policyServer(t), payload)
	if code != http.StatusConflict || body["error"] != "stale_digest" {
		t.Fatalf("stale write: %d %v", code, body)
	}
	if body["currentDigest"] != digestOf(policyWritableText) {
		t.Fatalf("currentDigest = %v", body["currentDigest"])
	}
	raw, _ := os.ReadFile(file)
	if string(raw) != policyWritableText {
		t.Fatal("a refused write changed the policy file")
	}
}

// TestPolicyWriteRefusesAnInvalidChange is C2 at the route: the check's own reasons come back.
func TestPolicyWriteRefusesAnInvalidChange(t *testing.T) {
	policyHost(t, policyWritableText, true)
	policyWriteSeamsFor(t, nil, unavailablePolicyRunning)
	payload := "{\"expectedDigest\":\"" + digestOf(policyWritableText) + "\",\"change\":{\"kind\":\"setAllowed\",\"model\":\"gpt-6.1-sol\",\"efforts\":[\"max\"]}}"
	code, body := policyWriteResponse(t, policyServer(t), payload)
	if code != http.StatusUnprocessableEntity || body["error"] != "invalid_policy" {
		t.Fatalf("invalid write: %d %v", code, body)
	}
	errors, _ := body["errors"].([]any)
	if len(errors) == 0 {
		t.Fatalf("an invalid change carries no reason: %v", body)
	}
}

// TestPolicyWriteRestoresWhenTheRegistrationRefuses is C3's (b) at the route: the refusal is a 502
// with restored, and the file is back to the bytes it had.
func TestPolicyWriteRestoresWhenTheRegistrationRefuses(t *testing.T) {
	file := policyHost(t, policyWritableText, true)
	policyWriteSeamsFor(t, func(context.Context, string) policystore.RegisterAnswer {
		return policystore.RegisterAnswer{ExitCode: 1, Stdout: []byte("{\"outcome\": \"record_absent\"}")}
	}, unavailablePolicyRunning)
	payload := "{\"expectedDigest\":\"" + digestOf(policyWritableText) + "\",\"change\":{\"kind\":\"removeException\",\"id\":\"legacy\"}}"
	code, body := policyWriteResponse(t, policyServer(t), payload)
	if code != http.StatusBadGateway || body["error"] != "register_failed" || body["restored"] != true {
		t.Fatalf("failed registration: %d %v", code, body)
	}
	raw, _ := os.ReadFile(file)
	if string(raw) != policyWritableText {
		t.Fatal("the original bytes were not restored")
	}
}

// TestPolicyWriteReportsRecoveryNeeded is C3's (c) at the route: both digests and the recovery
// command come back, and the response is not a success.
func TestPolicyWriteReportsRecoveryNeeded(t *testing.T) {
	file := policyHost(t, policyWritableText, true)
	third := digestOf("a document neither the file nor the record holds\n")
	policyWriteSeamsFor(t, func(_ context.Context, path string) policystore.RegisterAnswer {
		rewritePolicyRecord(t, path, third)
		return policystore.RegisterAnswer{Err: errors.New("the registration response was lost")}
	}, unavailablePolicyRunning)
	payload := "{\"expectedDigest\":\"" + digestOf(policyWritableText) + "\",\"change\":{\"kind\":\"removeException\",\"id\":\"legacy\"}}"
	code, body := policyWriteResponse(t, policyServer(t), payload)
	if code != http.StatusInternalServerError || body["error"] != "recovery_needed" {
		t.Fatalf("recovery: %d %v", code, body)
	}
	for _, field := range []string{"fileDigest", "registeredDigest", "backup", "recovery"} {
		if value, _ := body[field].(string); value == "" {
			t.Fatalf("the recovery answer does not carry %s: %v", field, body)
		}
	}
	if body["registeredDigest"] != third {
		t.Fatalf("registeredDigest = %v, want %q", body["registeredDigest"], third)
	}
	raw, _ := os.ReadFile(file)
	if string(raw) == policyWritableText {
		t.Fatal("the file was put back where the write could not reconcile it")
	}
}

// TestPolicyWriteRejectsAMalformedBody is the route's own input check.
func TestPolicyWriteRejectsAMalformedBody(t *testing.T) {
	policyHost(t, policyWritableText, true)
	policyWriteSeamsFor(t, nil, unavailablePolicyRunning)
	code, body := policyWriteResponse(t, policyServer(t), "{not json")
	if code != http.StatusBadRequest || body["error"] != "bad_request" {
		t.Fatalf("malformed write: %d %v", code, body)
	}
}

// TestPolicyWriteIsGuarded is the write rules at the route: the route is registered, and a write
// that presents no token is refused before it can reach the handler.
func TestPolicyWriteIsGuarded(t *testing.T) {
	policyHost(t, policyWritableText, true)
	policyWriteSeamsFor(t, nil, unavailablePolicyRunning)
	server := policyServer(t)
	payload := "{\"expectedDigest\":\"0000\",\"change\":{\"kind\":\"removeException\",\"id\":\"legacy\"}}"
	// The route exists: the guard lets a token-bearing write through to it, and the handler's own
	// refusal is a JSON error rather than the 404 an unregistered path answers.
	if code, body := policyWriteResponse(t, server, payload); code != http.StatusConflict || body["error"] != "stale_digest" {
		t.Fatalf("the route is not registered or is not reached: %d %v", code, body)
	}
	// No token: the guard refuses it.
	recorder := request(server, http.MethodPost, "/api/policy", guardHost, payload, map[string]string{"Content-Type": "application/json"})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("a write without the token: %d %s", recorder.Code, recorder.Body.String())
	}
}

// TestPolicyWriteReportsRecoveryWhenTheRecordCannotBeReadBack is the route-level half of the (c)
// decision: a registration that reported success and a record that then cannot be read back is a
// state where the file and the record may no longer describe one document, so the route answers
// recovery_needed with the file's digest, the backup and the repair rather than a 200 that presents
// the unestablished record as applied.
func TestPolicyWriteReportsRecoveryWhenTheRecordCannotBeReadBack(t *testing.T) {
	policyHost(t, policyWritableText, true)
	policyWriteSeamsFor(t, func(_ context.Context, path string) policystore.RegisterAnswer {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		rewritePolicyRecord(t, path, digestOf(string(raw)))
		// The record becomes unreadable after the registration reported success.
		if err := os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "crw-bridge-mcp.json"), []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		return policystore.RegisterAnswer{ExitCode: 0, Stdout: []byte("{\"outcome\": \"record_updated\"}")}
	}, unavailablePolicyRunning)
	payload := "{\"expectedDigest\":\"" + digestOf(policyWritableText) + "\",\"change\":{\"kind\":\"removeException\",\"id\":\"legacy\"}}"
	code, body := policyWriteResponse(t, policyServer(t), payload)
	if code != http.StatusInternalServerError || body["error"] != "recovery_needed" {
		t.Fatalf("POST /api/policy: %d %v, want a 500 recovery_needed", code, body)
	}
	for _, field := range []string{"fileDigest", "backup", "recovery"} {
		if value, _ := body[field].(string); value == "" {
			t.Fatalf("the recovery answer does not carry %s: %v", field, body)
		}
	}
}

// --- CRW-867c: the registration answer's own facts reach the route ---

// TestPolicyWriteReturnsTheRecordBackupAndRestartAdvice is R4 at the route: the installer's
// record_updated envelope carries the wiring record's backup and the restart advice, and both must
// come back on the registered object, separately from the policy file's own backup.
func TestPolicyWriteReturnsTheRecordBackupAndRestartAdvice(t *testing.T) {
	policyHost(t, policyWritableText, true)
	const recordBackup = "/tmp/does-not-need-to-exist/crw-bridge-mcp.json.crw-20261007T000000Z.bak"
	const restart = "the record now names the execution policy this run read; restart the relay service before the new policy takes effect"
	policyWriteSeamsFor(t, func(_ context.Context, path string) policystore.RegisterAnswer {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		rewritePolicyRecord(t, path, digestOf(string(raw)))
		return policystore.RegisterAnswer{ExitCode: 0, Stdout: []byte(
			"{\"outcome\": \"record_updated\", \"backup\": \"" + recordBackup + "\", \"restartRequired\": \"" + restart + "\"}")}
	}, unavailablePolicyRunning)
	payload := "{\"expectedDigest\":\"" + digestOf(policyWritableText) + "\",\"change\":{\"kind\":\"removeException\",\"id\":\"legacy\"}}"
	code, body := policyWriteResponse(t, policyServer(t), payload)
	if code != http.StatusOK {
		t.Fatalf("POST /api/policy: %d %v", code, body)
	}
	registered, _ := body["registered"].(map[string]any)
	if registered == nil {
		t.Fatalf("the body carries no registered object: %v", body)
	}
	if registered["recordBackup"] != recordBackup {
		t.Fatalf("registered.recordBackup = %v, want %q", registered["recordBackup"], recordBackup)
	}
	if registered["restartRequired"] != restart {
		t.Fatalf("registered.restartRequired = %v, want the installer's advice", registered["restartRequired"])
	}
	// The top-level backup is the policy file's own backup, a different artifact from the record's.
	policyBackup, _ := body["backup"].(string)
	if policyBackup == "" {
		t.Fatalf("the policy file's own backup is not reported: %v", body)
	}
	if policyBackup == recordBackup {
		t.Fatal("the policy file backup and the wiring record backup are reported as one artifact")
	}
	if _, err := os.Stat(policyBackup); err != nil {
		t.Fatalf("the reported policy backup does not exist: %v", err)
	}
}

// TestPolicyWriteReportsTheRestoreWarning is the route half of R2: a restore whose directory could
// not be synced answers register_failed with restored and the power-loss warning, and the caller
// sees it. The seam fails only the SECOND publication (the restore); the first one really writes.
// This test proves the body mapping (restored, warnings, backup reaching the caller); the durability
// of the restore itself is pinned by TestARestoreWhoseDirectorySyncFailedIsStillARestore, which wraps
// the real publishPolicy.
func TestPolicyWriteReportsTheRestoreWarning(t *testing.T) {
	policyHost(t, policyWritableText, true)
	policyWriteSeamsFor(t, func(context.Context, string) policystore.RegisterAnswer {
		return policystore.RegisterAnswer{ExitCode: 1, Stdout: []byte("{\"outcome\": \"record_absent\"}")}
	}, unavailablePolicyRunning)
	previous := policyWriteSeams
	calls := 0
	policyWriteSeams.Swap = func(_ context.Context, path string, expected, next []byte, mode os.FileMode) ([]byte, string, error) {
		calls++
		// The GUI package cannot reach policystore's unexported swapPolicy, so the seam exchanges the
		// bytes itself: the first call publishes the candidate, the second restores the original.
		displaced, err := os.ReadFile(path)
		if err != nil {
			return nil, "", err
		}
		if err := os.WriteFile(path, next, mode.Perm()); err != nil {
			return nil, "", err
		}
		if calls == 2 {
			return displaced, "", errors.New("the directory could not be synced")
		}
		return displaced, "", nil
	}
	t.Cleanup(func() { policyWriteSeams = previous })
	payload := "{\"expectedDigest\":\"" + digestOf(policyWritableText) + "\",\"change\":{\"kind\":\"removeException\",\"id\":\"legacy\"}}"
	code, body := policyWriteResponse(t, policyServer(t), payload)
	if code != http.StatusBadGateway || body["error"] != "register_failed" || body["restored"] != true {
		t.Fatalf("a restore whose directory sync failed: %d %v", code, body)
	}
	warnings, _ := body["warnings"].([]any)
	if len(warnings) == 0 {
		t.Fatalf("the power-loss warning does not reach the caller: %v", body)
	}
	joined := ""
	for _, w := range warnings {
		text, _ := w.(string)
		joined += text + " "
	}
	if !strings.Contains(joined, "power") {
		t.Fatalf("the warning does not name the power-loss exposure: %q", joined)
	}
	if body["backup"] == nil || body["backup"] == "" {
		t.Fatalf("the restore does not name the backup: %v", body)
	}
}

// TestPolicyWriteNamesAnUnspellableBackupWithItsBytes is the pre-merge evaluation's finding: a policy
// whose filename is not UTF-8 has a backup whose name holds the same byte. The installer spells such
// a byte as its surrogate escape, and the response must spell it the same way; the standard JSON
// encoder would write U+FFFD, naming a file that does not exist, so a caller following the answer
// could not recover.
func TestPolicyWriteNamesAnUnspellableBackupWithItsBytes(t *testing.T) {
	root := t.TempDir()
	codexHome := filepath.Join(root, ".codex")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("CRW_CONFIG", filepath.Join(root, "crw-config.json"))
	raw := append([]byte("policy"), 0x80)
	raw = append(raw, []byte(".json")...)
	real := filepath.Join(root, string(raw))
	if err := os.WriteFile(real, []byte(policyWritableText), 0o644); err != nil {
		t.Skipf("this filesystem refuses a non-UTF-8 name: %v", err)
	}
	surrogate := string([]byte{0xED, 0xB2, 0x80})
	escaped := filepath.Join(root, "policy"+surrogate+".json")
	policySurrogateRecord(t, codexHome, escaped, digestOf(policyWritableText))
	policyWriteSeamsFor(t, func(_ context.Context, path string) policystore.RegisterAnswer {
		encoded, ok := pyvalue.FSEncode(path)
		if !ok {
			t.Fatalf("the path cannot be encoded to bytes: %q", path)
		}
		body, err := os.ReadFile(encoded)
		if err != nil {
			t.Fatal(err)
		}
		// The registration is handed the kernel spelling, so the record's surrogate spelling is built
		// from the bytes it names rather than from the path as it arrived.
		policySurrogateRecord(t, codexHome, pyvalue.FSDecode(path), digestOf(string(body)))
		return policystore.RegisterAnswer{ExitCode: 0, Stdout: []byte("{\"outcome\": \"record_updated\"}")}
	}, unavailablePolicyRunning)
	payload := "{\"expectedDigest\":\"" + digestOf(policyWritableText) + "\",\"change\":{\"kind\":\"removeException\",\"id\":\"legacy\"}}"
	// The raw body is read rather than decoded through a map: Go's decoder turns a \\udcXX escape
	// into U+FFFD itself, which would hide the very corruption this test pins.
	recorder := request(policyServer(t), http.MethodPost, "/api/policy", guardHost, payload, writeHeaders())
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST /api/policy: %d %s", recorder.Code, recorder.Body.String())
	}
	bodyText := recorder.Body.String()
	if strings.ContainsRune(bodyText, 0xfffd) {
		t.Fatalf("the answer spells the path as a replacement character: %s", bodyText)
	}
	if !strings.Contains(bodyText, `\udc80`) {
		t.Fatalf("the answer does not spell the byte as its surrogate escape: %s", bodyText)
	}
	// The escape is read from the raw body: Go's map decoder turns \udc80 into U+FFFD itself, which
	// would name a file that does not exist and hide the corruption this test pins.
	const prefix = `"backup":"`
	start := strings.Index(bodyText, prefix)
	if start < 0 {
		t.Fatalf("the answer names no backup: %s", bodyText)
	}
	rest := bodyText[start+len(prefix):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatalf("the backup path is not terminated: %s", bodyText)
	}
	backup := rest[:end]
	if !strings.Contains(backup, `\udc80`) {
		t.Fatalf("the backup path is not spelled as its surrogate escape: %q", backup)
	}
	// Following the answer must reach the file the write really made: the escape stands for 0x80.
	encoded := strings.Replace(backup, `\udc80`, string([]byte{0x80}), 1)
	original, err := os.ReadFile(encoded)
	if err != nil {
		t.Fatalf("the answer's backup path names no readable file: %v", err)
	}
	if string(original) != policyWritableText {
		t.Fatal("the answer's backup does not hold the original bytes")
	}
}

// policySurrogateRecord writes the wiring record naming a surrogate-escaped policy path, the way a
// Python writer spells a byte that is not UTF-8, so the GUI tests can drive a policy whose filename
// is not UTF-8.
func policySurrogateRecord(t *testing.T, codexHome, escaped, digest string) {
	t.Helper()
	document := "{\n" +
		"  \"recordVersion\": 2,\n" +
		"  \"owner\": \"plugin\",\n" +
		"  \"serverName\": \"codex-thread-bridge\",\n" +
		"  \"bridgeExecutable\": \"/usr/local/bin/codex-thread-bridge\",\n" +
		"  \"args\": [],\n" +
		"  \"executionPolicy\": {\"path\": \"" + surrogateJSONEscape(escaped) + "\", \"digest\": \"" + digest + "\"}\n" +
		"}\n"
	if err := os.WriteFile(filepath.Join(codexHome, "crw-bridge-mcp.json"), []byte(document), 0o644); err != nil {
		t.Fatal(err)
	}
}

// surrogateJSONEscape spells a WTF-8 surrogate (ED B2 80) as the JSON escape a Python writer emits.
func surrogateJSONEscape(value string) string {
	out := make([]byte, 0, len(value))
	for i := 0; i < len(value); i++ {
		if value[i] == 0xED && i+2 < len(value) && value[i+1] == 0xB2 && value[i+2] == 0x80 {
			out = append(out, []byte("\\udc80")...)
			i += 2
			continue
		}
		out = append(out, value[i])
	}
	return string(out)
}
