package gui

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestPolicyResponseSpellings pins the wire contract the next screen consumes: the document
// spellings, not Go field names, and no zero-valued effort beside a reasoningEffort.
func TestPolicyResponseSpellings(t *testing.T) {
	policyHost(t, policyText, true)
	recorder := request(policyServer(t), http.MethodGet, "/api/policy", guardHost, "", nil)
	body := recorder.Body.String()
	for _, want := range []string{`"registeredDigest"`, `"runningDigest"`, `"reasoningEffort"`, `"expectation"`, `"efforts"`, `"cwd"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("the response does not carry %s: %s", want, body)
		}
	}
	for _, unwanted := range []string{`"Name"`, `"Expectation"`, `"Efforts"`, `"CWD"`, `"effort":""`} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("the response carries the Go spelling %s: %s", unwanted, body)
		}
	}
}

// TestPolicyCheckAcceptsTheDocumentSpellings is the contract end to end: the documented lower-case
// keys drive the route, not encoding/json case-insensitive fallback.
func TestPolicyCheckAcceptsTheDocumentSpellings(t *testing.T) {
	policyHost(t, policyText, true)
	payload := "{\"expectedDigest\":\"" + digestOf(policyText) + "\",\"change\":{\"kind\":\"removeException\",\"id\":\"legacy\"}}"
	code, body := checkResponse(t, policyServer(t), payload)
	if code != http.StatusOK || body["valid"] != true {
		t.Fatalf("the documented spellings were not accepted: %d %v", code, body)
	}
	diff, _ := body["diff"].([]any)
	if len(diff) != 1 || diff[0] != "exceptions.legacy" {
		t.Fatalf("diff = %v", body["diff"])
	}
}

// TestPolicyCheckRejectsAGoSpelledField is the same contract from the other side: a request that
// carries a field of another kind is refused, whatever its spelling.
func TestPolicyCheckRejectsAGoSpelledField(t *testing.T) {
	policyHost(t, policyText, true)
	payload := "{\"expectedDigest\":\"" + digestOf(policyText) + "\",\"change\":{\"kind\":\"removeException\",\"id\":\"legacy\",\"efforts\":[\"xhigh\"]}}"
	code, body := checkResponse(t, policyServer(t), payload)
	if code != http.StatusOK || body["valid"] != false {
		t.Fatalf("a request carrying a stray field was accepted: %d %v", code, body)
	}
	_ = json.Valid
}
