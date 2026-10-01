package faults

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func cliCall(t *testing.T, dir string, args ...string) (int, map[string]any) {
	t.Helper()
	argv := append([]string{"--state", dir}, args...)
	var out, stderr bytes.Buffer
	code := executeAsCLI(context.Background(), argv, &out, &stderr)
	var payload map[string]any
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("%s (stderr: %s): %v", out.String(), stderr.String(), err)
	}
	return code, payload
}
func Test22_FLT_33_StaticKindModules(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "relay")
	for _, module := range []string{"json", "os.path", "codex_session_relay.projects"} {
		code, reply := cliCall(t, dir, "--kind-module", module, "fault-target", "--product", "crw", "--team", "team-relay")
		if code != 0 || reply["scopeKey"] != "crw" {
			t.Fatalf("%s: %d %+v", module, code, reply)
		}
	}
	code, reply := cliCall(t, dir, "--kind-module", "no_such_module_crw205", "fault-target", "--product", "crw", "--team", "team-relay")
	if code != 4 || reply["error"] != "usage" || !strings.Contains(reply["detail"].(string), strconv.Quote("no_such_module_crw205")) {
		t.Fatalf("unregistered: %d %+v", code, reply)
	}
	for _, module := range []string{"", ".relative"} {
		code, reply = cliCall(t, dir, "--kind-module", module, "fault-target", "--product", "crw", "--team", "team-relay")
		if code != 3 || reply["error"] != "host" {
			t.Fatalf("%q: %d %+v", module, code, reply)
		}
	}
}
func Test22_FLT_14_MalformedClearIsRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "relay")
	for _, value := range []string{`{"schema":"fault-observation/1","product":"crw","faultClass":"report_omitted","severity":"broken","signature":{"relationship":"r"},"occurrenceKey":"a","cleared":"false"}`, `{"schema":"fault-observation/1","product":"crw","faultClass":"report_omitted","severity":"broken","signature":{"relationship":"r"},"occurrenceKey":"a","cleared":1}`} {
		code, reply := cliCall(t, dir, "fault-observe", "--observation", value)
		if code != 2 || reply["reason"] != "fault_observation_malformed" {
			t.Fatalf("invalid cleared value accepted: %d %+v", code, reply)
		}
	}
}
func Test22_FLT_14_MalformedScopeAndEvidenceAreRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "relay")
	for _, field := range []string{`"scope":[]`, `"scope":{"projectKey":[]}`, `"scope":{"workspace":" "}`, `"evidence":{}`} {
		input := `{"schema":"fault-observation/1","product":"crw","faultClass":"report_omitted","severity":"broken","signature":{"relationship":"r"},"occurrenceKey":"a",` + field + `}`
		code, reply := cliCall(t, dir, "fault-observe", "--observation", input)
		if code != 2 || reply["reason"] != "fault_observation_malformed" {
			t.Fatalf("%s: %d %+v", field, code, reply)
		}
	}
}
func Test22_FLT_29_FaultObserveAndMalformedRefusal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "relay")
	input := map[string]any{"schema": "fault-observation/1", "product": "crw", "faultClass": "report_omitted", "severity": "broken", "signature": map[string]any{"relationship": "rel-1", "turn": "turn-7"}, "occurrenceKey": "a", "scope": map[string]any{"projectKey": "CRW", "issueKey": "CRW-205"}, "detail": "an admitted turn settled without a report", "evidence": []any{map[string]any{"kind": "row", "ref": "events", "observed": map[string]any{"rows": 0}}}}
	raw, _ := json.Marshal(input)
	code, reply := cliCall(t, dir, "fault-observe", "--observation", string(raw))
	if code != 0 || reply["state"] != Open || reply["faultId"] != "4c4ef0d33eaf63cf1af8cb7c937577e0" {
		t.Fatalf("observe: %d %+v", code, reply)
	}
	code, reply = cliCall(t, dir, "fault-observe", "--observation", "{")
	if code != 2 || reply["reason"] != "fault_observation_malformed" {
		t.Fatalf("malformed: %d %+v", code, reply)
	}
}
