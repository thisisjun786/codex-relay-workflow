package supervisor

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func supervisorCLI(t *testing.T, binary, state string, args ...string) (int, map[string]any) {
	t.Helper()
	cmd := exec.Command(binary, append([]string{"relay", "--state", state}, args...)...)
	home := filepath.Dir(state)
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "xdg"), "CODEX_HOME="+filepath.Join(home, "codex"))
	out, err := cmd.Output()
	code := 0
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	var payload map[string]any
	if err = json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	return code, payload
}
func Test24_SR_20_RealBinaryUsageExitFour(t *testing.T) {
	t.Parallel()
	binary := testsupport.CRW(t)
	state := filepath.Join(t.TempDir(), "state")
	reading := filepath.Join(filepath.Dir(state), "reported.json")
	if err := os.WriteFile(reading, []byte(`{"schema":"reporting-observation/1","reportingState":"reported","relationshipId":"rel-1","selectors":{"turn":"turn-7"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		word string
	}{
		{"ordinary-event", []string{"supervisor-report-recorded", "--event", "no-event"}, "raises no obligation"},
		{"settled-observation", []string{"supervisor-report-recorded", "--observation", reading}, "state unreported"},
		{"unreadable-observation", []string{"supervisor-standing", "--project", "CRW", "--observation", filepath.Join(filepath.Dir(state), "missing.json")}, "could not be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, payload := supervisorCLI(t, binary, state, tc.args...)
			if code != 4 || payload["error"] != "usage" || !strings.Contains(payload["detail"].(string), tc.word) {
				t.Fatalf("exit=%d payload=%s", code, jsonText(payload))
			}
		})
	}
}
func Test24_SR_19_RealBinaryOmissionConverges(t *testing.T) {
	t.Parallel()
	binary := testsupport.CRW(t)
	state := filepath.Join(t.TempDir(), "state")
	reading := filepath.Join(filepath.Dir(state), "unreported.json")
	data := `{"schema":"reporting-observation/1","reportingState":"unreported","reason":"terminal_without_report","relationshipId":"rel-0123456789abcdef","executionGeneration":1,"selectors":{"turn":"turn-7"}}`
	if err := os.WriteFile(reading, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	firstCode, first := supervisorCLI(t, binary, state, "supervisor-report-recorded", "--observation", reading, "--message", "m-1")
	nextCode, next := supervisorCLI(t, binary, state, "supervisor-report-recorded", "--observation", reading, "--message", "m-1")
	if firstCode != 0 || nextCode != 0 || first["recorded"] != true || next["recorded"] != false || first["seq"] != next["seq"] || !reflect.DeepEqual(first["obligation"], next["obligation"]) {
		t.Fatalf("first=%d %s next=%d %s", firstCode, jsonText(first), nextCode, jsonText(next))
	}
}
func Test24_SR_21_DoctorActorReachability(t *testing.T) {
	t.Parallel()
	binary := testsupport.CRW(t)
	state := filepath.Join(t.TempDir(), "absent-state")
	code, payload := supervisorCLI(t, binary, state, "doctor")
	if code != 0 {
		t.Fatalf("doctor exit=%d %s", code, jsonText(payload))
	}
	reach, ok := payload["actorReachability"].(map[string]any)
	if !ok {
		t.Fatal(payload)
	}
	offline, ok := reach["offlineCommands"].([]any)
	if !ok {
		t.Fatal(reach)
	}
	host, ok := reach["hostRequiredCommands"].([]any)
	if !ok {
		t.Fatal(reach)
	}
	for _, name := range []string{"supervisor-select", "supervisor-standing", "supervisor-report-recorded"} {
		present := false
		for _, v := range offline {
			if v == name {
				present = true
			}
		}
		if !present {
			t.Errorf("%s absent from offlineCommands", name)
		}
		for _, v := range host {
			if v == name {
				t.Errorf("%s is hostRequired", name)
			}
		}
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("doctor created store: %v", err)
	}
}
