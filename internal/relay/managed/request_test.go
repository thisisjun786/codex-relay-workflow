package managed

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func settingsFixture(t *testing.T) map[string]any {
	t.Helper()
	root := t.TempDir()
	return map[string]any{"sandbox": map[string]any{"type": "workspaceWrite"}, "approvalPolicy": "never", "cwd": root, "runtimeWorkspaceRoots": []string{root}, "model": "gpt-5", "reasoningEffort": "medium", "environments": []any{}}
}
func requestFixture(t *testing.T) []byte {
	t.Helper()
	r := map[string]any{"schema": Schema, "requestId": "managed-1", "issueKey": "REL-MANAGED", "parent": map[string]any{"taskId": "parent", "hostId": "host", "settings": settingsFixture(t)}, "child": map[string]any{"hostId": "host", "title": "Verify", "settings": settingsFixture(t)}, "artifactRoots": []string{t.TempDir()}, "allowedRecipients": []string{"parent"}, "criteria": []map[string]any{{"id": "c1", "title": " preserve replay identity ", "required": true}}, "criteriaSource": "issue:REL-MANAGED", "baselineRevision": "baseline", "scopeRef": "issue:REL-MANAGED", "prompt": "business-secret"}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func Test27_MST_5_InputSnapshotAndStableBoundedOperationIDs(t *testing.T) {
	input := requestFixture(t)
	r, err := ParseRequest(input)
	if err != nil {
		t.Fatal(err)
	}
	for i := range input {
		input[i] = 'x'
	}
	if r["requestId"] != "managed-1" {
		t.Fatal("request not snapshotted")
	}
	create, business := OperationIDs("managed-1")
	if create == business || len(create) > 128 || len(business) > 128 || create != "managed-create-72a8e50ded92d4aa42328b9181bd188470f5be13faf25bdb8cf3ffa221c92020" {
		t.Fatalf("operation ids: %q %q", create, business)
	}
}
func Test27_MST_9_UnknownInputBeforeRPC(t *testing.T) {
	var r map[string]any
	if err := json.Unmarshal(requestFixture(t), &r); err != nil {
		t.Fatal(err)
	}
	r["overridePermissions"] = true
	b, _ := json.Marshal(r)
	_, err := ParseRequest(b)
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown key accepted: %v", err)
	}
}

func Test27_MST_10_ManagedShowAbsentDoesNotCreateStore(t *testing.T) {
	root := t.TempDir()
	_, file, _, _ := runtime.Caller(0)
	repo := filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
	bin := filepath.Join(root, "crw")
	build := exec.Command("go", "build", "-o", bin, "./cmd/crw")
	build.Dir = repo
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %s: %v", output, err)
	}
	state := filepath.Join(root, "absent")
	command := exec.Command(bin, "relay", "--state", state, "managed-show", "--request-id", "missing")
	command.Env = append(os.Environ(), "HOME="+root, "XDG_STATE_HOME="+root, "XDG_CONFIG_HOME="+root, "XDG_DATA_HOME="+root, "CODEX_HOME="+root)
	got, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("{\n  \"request\": null,\n  \"lastObservation\": null,\n  \"readable\": false,\n  \"detail\": \"FileNotFoundError: [Errno 2] No such file or directory: '%s/relay.sqlite3'\"\n}\n", state)
	if !bytes.Equal(got, []byte(want)) {
		t.Fatalf("managed-show differs from Python:\nGo: %s\nPython: %s", got, want)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("managed-show created state: %v", err)
	}
}
