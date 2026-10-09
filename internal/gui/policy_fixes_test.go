package gui

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// TestPolicyReportsAnUnreadableManageConfig is C1 over the route: a management configuration that is
// there but cannot be read must not be answered with the default relay's digest. The route reports
// the running digest as unavailable with the reason and never as applied.
func TestPolicyReportsAnUnreadableManageConfig(t *testing.T) {
	policyHost(t, policyText, true)
	// The management configuration file is present and is not JSON, so it cannot be read.
	config := os.Getenv("CRW_CONFIG")
	if err := os.WriteFile(config, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := decodePolicy(t, policyServer(t))
	if body["applied"] == "applied" {
		t.Fatalf("an unreadable management configuration was reported applied: %v", body)
	}
	if body["runningDigest"] != nil {
		t.Fatalf("runningDigest = %v, want null", body["runningDigest"])
	}
	reason, _ := body["runningReason"].(string)
	if reason == "" {
		t.Fatalf("an unreadable management configuration carries no reason: %v", body)
	}
	// The reason must be the configuration failure itself. Any other reason means the reader
	// answered about a relay it should not have looked at, which is the defect this test pins.
	if !strings.Contains(strings.ToLower(reason), "configuration") {
		t.Fatalf("the reason does not name the configuration failure: %q", reason)
	}
}

// TestPolicyCheckAcceptsTheContractEffortField is C2 over the route: the contract spelling is a
// valid request, and the answer is a check result rather than a decode failure.
func TestPolicyCheckAcceptsTheContractEffortField(t *testing.T) {
	policyHost(t, policyWritableText, true)
	payload := "{\"expectedDigest\":\"" + digestOf(policyWritableText) + "\",\"change\":{\"kind\":\"setException\",\"id\":\"extra\",\"role\":\"child\",\"model\":\"anthropic/opus\",\"effort\":\"xhigh\",\"cwd\":[\"/tmp/project\"]}}"
	code, body := checkResponse(t, policyServer(t), payload)
	if code != http.StatusOK || body["valid"] != true {
		t.Fatalf("the contract-shaped setException was refused: %d %v", code, body)
	}
}

// TestPolicyCheckRefusesDisagreeingEffortSpellings is C2 over the route: a request naming both
// spellings with different values is invalid, and the answer is still a check result. A decode
// failure would be answered as a bad request, which would hide which field disagreed.
func TestPolicyCheckRefusesDisagreeingEffortSpellings(t *testing.T) {
	policyHost(t, policyWritableText, true)
	payload := "{\"expectedDigest\":\"" + digestOf(policyWritableText) + "\",\"change\":{\"kind\":\"setException\",\"id\":\"extra\",\"role\":\"child\",\"model\":\"anthropic/opus\",\"reasoningEffort\":\"xhigh\",\"effort\":\"max\",\"cwd\":[\"/tmp/project\"]}}"
	code, body := checkResponse(t, policyServer(t), payload)
	if code != http.StatusOK {
		t.Fatalf("a disagreeing alias was answered %d, want a check result", code)
	}
	if body["valid"] != false {
		t.Fatalf("a request naming two disagreeing efforts was accepted: %v", body)
	}
	errors, _ := body["errors"].([]any)
	if len(errors) == 0 {
		t.Fatalf("a refused change carries no error: %v", body)
	}
}

// TestPolicyCheckRefusesRemovingTheLastAllowedModel is C3 over the route, the CRW-134 evaluation
// reproduction.
func TestPolicyCheckRefusesRemovingTheLastAllowedModel(t *testing.T) {
	one := "{\"roles\": {\"child\": {\"model\": \"m\", \"reasoningEffort\": \"high\"}}, \"allowed\": [{\"model\": \"m\", \"efforts\": [\"high\"]}]}\n"
	policyHost(t, one, true)
	payload := "{\"expectedDigest\":\"" + digestOf(one) + "\",\"change\":{\"kind\":\"removeAllowed\",\"model\":\"m\"}}"
	code, body := checkResponse(t, policyServer(t), payload)
	if code != http.StatusOK || body["valid"] != false {
		t.Fatalf("removing the last allowed model was approved: %d %v", code, body)
	}
}

// TestCatalogTellsUnsupportedFromAFailure is C4 over the route: a host whose OCX rejects the
// live-catalog command is answered with the unsupported state, which is not the state a read
// failure produces.
func TestCatalogTellsUnsupportedFromAFailure(t *testing.T) {
	root := catalogHost(t)
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	// The stub OCX refuses the command line: nothing on stdout, a usage block on stderr, exit 1.
	stub := "#!/bin/sh\necho 'Unexpected argument(s): live' >&2\necho 'Usage: ocx models [--provider <name>] [--json]' >&2\nexit 1\n"
	if err := testsupport.WriteProgram(filepath.Join(bin, "ocx"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	state, status := catalogAnswer(t)
	if state != "unsupported-ocx-catalog" {
		t.Fatalf("state = %q, want unsupported-ocx-catalog (status %q)", state, status)
	}
	// The same host with an OCX that runs and fails is a read failure, not unsupported.
	failing := "#!/bin/sh\necho 'Error: Proxy is not running. Start the intended proxy with: ocx start.' >&2\nexit 1\n"
	if err := testsupport.WriteProgram(filepath.Join(bin, "ocx"), []byte(failing), 0o700); err != nil {
		t.Fatal(err)
	}
	state, status = catalogAnswer(t)
	if state == "unsupported-ocx-catalog" {
		t.Fatalf("a read failure was answered unsupported (status %q)", status)
	}
	if state != "unavailable" {
		t.Fatalf("state = %q, want unavailable (status %q)", state, status)
	}
}

// catalogAnswer drives GET /api/catalog and returns the state and status of the answer.
func catalogAnswer(t *testing.T) (string, string) {
	t.Helper()
	recorder := request(policyServer(t), http.MethodGet, "/api/catalog?refresh=1", guardHost, "", nil)
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
	return body.State, body.Status
}
