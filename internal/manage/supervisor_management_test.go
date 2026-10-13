package manage

import (
	"context"
	"encoding/json"
	"testing"
)

// CRW-1333 (design CRW-1314 §1): crw manage supervisor register binds the CALLING thread as its own
// management seat (scope kind management, keyed by the thread), so two management sessions each
// register without overwriting the other. The thread is --task, else CODEX_THREAD_ID, else the
// configuration's supervisor section; the host is --host, else the section. The settings pair is
// recorded as before.

// TestSupervisorRegisterBindsTheCallingThreadsManagementSeat: the caller's CODEX_THREAD_ID is the
// seat, ahead of the section's task.
func TestSupervisorRegisterBindsTheCallingThreadsManagementSeat(t *testing.T) {
	exe, record := supervisorScript(t, map[string]string{
		"linkage-bind":    `{"bindingId":"bnd-m","role":"supervisor","scopeKind":"management","scopeKey":"thread-mgmt","taskId":"thread-mgmt"}`,
		"settings-record": `{"taskId":"thread-mgmt","settings":{"citedRole":"supervisor"}}`,
	})
	t.Setenv("CODEX_THREAD_ID", "thread-mgmt")
	section := supervisorTestSection()
	supervisorUseConfig(t, supervisorTestConfig(&section))
	e, out, errOut := supervisorEnv(exe)
	if code := supervisorRegister(context.Background(), e, nil); code != 0 {
		t.Fatalf("register: exit %d, stderr %q", code, errOut.String())
	}
	supervisorCheckCalls(t, record, [][]string{
		{"relay", "--state", "/state", "--socket", "/socket", "linkage-bind", "--role", "supervisor",
			"--scope-kind", "management", "--scope", "thread-mgmt", "--task", "thread-mgmt", "--host", "host-supervisor",
			"--cwd", "/work/management"},
		{"relay", "--state", "/state", "--socket", "/socket", "settings-record", "--task", "thread-mgmt",
			"--role", "supervisor", "--settings", "@/work/management/settings.json", "--source", "crw manage supervisor register"},
	})
	var report supervisorRecord
	if err := json.Unmarshal([]byte(out.String()), &report); err != nil {
		t.Fatalf("the output is not one JSON object: %v\n%s", err, out.String())
	}
	if !report.OK || report.TaskID != "thread-mgmt" {
		t.Errorf("report %+v", report)
	}
}

// TestSupervisorRegisterTakesTheTaskAndHostFromItsArguments: --task and --host name the seat
// without a configured task or host, in either option form; the settings file still comes from the
// section.
func TestSupervisorRegisterTakesTheTaskAndHostFromItsArguments(t *testing.T) {
	for _, args := range [][]string{
		{"--task", "thread-arg", "--host", "host-arg"},
		{"--task=thread-arg", "--host=host-arg"},
	} {
		exe, record := supervisorScript(t, map[string]string{
			"linkage-bind":    `{"bindingId":"bnd-a"}`,
			"settings-record": `{"taskId":"thread-arg","settings":{}}`,
		})
		t.Setenv("CODEX_THREAD_ID", "thread-env")
		section := supervisorSection{SettingsFile: "/work/management/settings.json"}
		supervisorUseConfig(t, supervisorTestConfig(&section))
		e, _, errOut := supervisorEnv(exe)
		if code := supervisorRegister(context.Background(), e, args); code != 0 {
			t.Fatalf("register %q: exit %d, stderr %q", args, code, errOut.String())
		}
		supervisorCheckCalls(t, record, [][]string{
			{"relay", "--state", "/state", "--socket", "/socket", "linkage-bind", "--role", "supervisor",
				"--scope-kind", "management", "--scope", "thread-arg", "--task", "thread-arg", "--host", "host-arg"},
			{"relay", "--state", "/state", "--socket", "/socket", "settings-record", "--task", "thread-arg",
				"--role", "supervisor", "--settings", "@/work/management/settings.json", "--source", "crw manage supervisor register"},
		})
	}
}

// TestSupervisorShowReadsTheManagementSeat: show reports the calling thread's management seat the
// way it reports the store seat.
func TestSupervisorShowReadsTheManagementSeat(t *testing.T) {
	exe, _ := supervisorScript(t, map[string]string{
		"settings-show": `{"task":"thread-mgmt","settings":{},"usable":true,"deliverable":true,"missing":[],"recordFinding":null}`,
		"linkage-up": `{"state":"resolved","readable":true,"levels":[{"scopeKind":"management","scopeKey":"thread-first",` +
			`"owner":{"bindingId":"bnd-m","role":"supervisor","scopeKind":"management","scopeKey":"thread-first","taskId":"thread-mgmt"},"depth":0}],"gaps":[],"contention":[]}`,
	})
	t.Setenv("CODEX_THREAD_ID", "thread-mgmt")
	section := supervisorTestSection()
	supervisorUseConfig(t, supervisorTestConfig(&section))
	e, out, errOut := supervisorEnv(exe)
	if code := supervisorShow(context.Background(), e, nil); code != 0 {
		t.Fatalf("show: exit %d, stderr %q", code, errOut.String())
	}
	var report supervisorReportOut
	if err := json.Unmarshal([]byte(out.String()), &report); err != nil {
		t.Fatalf("the output is not one JSON object: %v\n%s", err, out.String())
	}
	if report.TaskID != "thread-mgmt" || string(report.Binding) == "null" {
		t.Fatalf("show reported %+v, want the management seat of thread-mgmt", report)
	}
}
