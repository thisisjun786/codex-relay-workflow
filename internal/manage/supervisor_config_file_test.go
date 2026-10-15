package manage

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1486/S1: the supervisor section is read from the real configuration file (CRW_CONFIG) with the
// configuration seam untouched, every refusal for a missing value names the key and the file it was
// looked for in, and show needs the task only.

// supervisorConfigFileEnv is supervisorEnv whose configuration memo and relay-state memo are dropped
// when the test ends, the way Run drops them when an invocation ends.
func supervisorConfigFileEnv(t *testing.T, exe string) (e *Env, out, errOut *strings.Builder) {
	t.Helper()
	e, out, errOut = supervisorEnv(exe)
	t.Cleanup(func() {
		coreForgetConfig(e)
		relayHelperForget(e)
	})
	return e, out, errOut
}

// supervisorConfigFile writes a configuration document whose manage object is manage and points
// CRW_CONFIG at it. The relay block names a state directory and a socket, so the helper never asks
// doctor. Every value is synthetic.
func supervisorConfigFile(t *testing.T, supervisor any) string {
	t.Helper()
	coreTempHome(t)
	manage := map[string]any{"relay": map[string]any{"state": "/state", "socket": "/socket"}}
	if supervisor != nil {
		manage["supervisor"] = supervisor
	}
	path := coreConfigWrite(t, filepath.Join(t.TempDir(), "config.json"), coreConfigDocument(t, manage))
	t.Setenv("CRW_CONFIG", path)
	return path
}

// show reads the task only: settings-show and linkage-up both take --task and nothing else, so a run
// with no host anywhere, or no section at all, still reads the calling thread or the named one.
func TestSupervisorShowNeedsOnlyTheTask(t *testing.T) {
	answers := map[string]string{
		"settings-show": `{"task":"t","settings":{},"usable":true}`,
		"linkage-up":    supervisorUnregistered,
	}
	for _, test := range []struct {
		name string
		env  string
		args []string
		want string
	}{
		{"the calling thread with no section", "thread-env", nil, "thread-env"},
		{"--task with no section", "", []string{"--task", "thread-named"}, "thread-named"},
		{"--task=THREAD with no section", "", []string{"--task=thread-named"}, "thread-named"},
		{"--task beats the calling thread", "thread-env", []string{"--task", "thread-named"}, "thread-named"},
	} {
		t.Run(test.name, func(t *testing.T) {
			exe, record := supervisorScript(t, answers)
			t.Setenv("CODEX_THREAD_ID", test.env)
			supervisorUseConfig(t, supervisorTestConfig(nil))
			e, out, errOut := supervisorEnv(exe)
			if code := supervisorShow(context.Background(), e, test.args); code != 0 {
				t.Fatalf("show %q: exit %d, stderr %q", test.args, code, errOut.String())
			}
			supervisorCheckCalls(t, record, [][]string{
				{"relay", "--state", "/state", "--socket", "/socket", "settings-show", "--task", test.want},
				{"relay", "--state", "/state", "--socket", "/socket", "linkage-up", "--task", test.want},
			})
			var report supervisorReportOut
			if err := json.Unmarshal([]byte(out.String()), &report); err != nil {
				t.Fatalf("the output is not one JSON object: %v\n%s", err, out.String())
			}
			if !report.OK || report.TaskID != test.want {
				t.Errorf("report %+v, want ok with taskId %q", report, test.want)
			}
		})
	}
	t.Run("a section with a task and no host", func(t *testing.T) {
		exe, record := supervisorScript(t, answers)
		section := supervisorTestSection()
		section.HostID, section.SettingsFile = "", ""
		supervisorUseConfig(t, supervisorTestConfig(&section))
		e, _, errOut := supervisorEnv(exe)
		if code := supervisorShow(context.Background(), e, nil); code != 0 {
			t.Fatalf("show: exit %d, stderr %q", code, errOut.String())
		}
		if calls := supervisorCalls(t, record); len(calls) != 2 {
			t.Errorf("the relay saw %q, want settings-show and linkage-up", calls)
		}
	})
}

// A refusal for a missing value names the key path and the file the run read, or says that no file
// was read and where one is looked for, so an operator knows what to write where.
func TestSupervisorRegisterRefusalNamesWhereTheValueComesFrom(t *testing.T) {
	type check struct {
		name      string
		configure func(t *testing.T) string // returns the path the stderr must name
		args      []string
		named     []string
		notNamed  []string
	}
	for _, test := range []check{
		{
			name:      "a file with no supervisor section",
			configure: func(t *testing.T) string { return supervisorConfigFile(t, nil) },
			args:      []string{"--task", "thread-named"},
			named:     []string{"manage.supervisor.host_id", "manage.supervisor.settings_file"},
			notNamed:  []string{"manage.supervisor.task_id"},
		},
		{
			name: "a section with no settings_file",
			configure: func(t *testing.T) string {
				return supervisorConfigFile(t, map[string]any{"task_id": "task-file", "host_id": "host-file"})
			},
			named:    []string{"manage.supervisor.settings_file"},
			notNamed: []string{"manage.supervisor.host_id", "manage.supervisor.task_id"},
		},
		{
			name: "a section with no host_id and no settings_file",
			configure: func(t *testing.T) string {
				return supervisorConfigFile(t, map[string]any{"task_id": "task-file"})
			},
			named:    []string{"manage.supervisor.host_id", "manage.supervisor.settings_file"},
			notNamed: []string{"manage.supervisor.task_id"},
		},
		{
			name: "no task anywhere",
			configure: func(t *testing.T) string {
				return supervisorConfigFile(t, map[string]any{"host_id": "host-file", "settings_file": "/work/s.json"})
			},
			named:    []string{"manage.supervisor.task_id", "--task", "CODEX_THREAD_ID"},
			notNamed: []string{"manage.supervisor.host_id", "manage.supervisor.settings_file"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := test.configure(t)
			record := supervisorFakeProcess(t, 0)
			code, _, errOut := supervisorRunLine(t, append([]string{"supervisor", "register"}, test.args...)...)
			if code != usageExit {
				t.Fatalf("exit %d, want %d (stderr %q)", code, usageExit, errOut)
			}
			for _, want := range append([]string{supervisorUnconfigured, path}, test.named...) {
				if !strings.Contains(errOut, want) {
					t.Errorf("stderr %q does not name %q", errOut, want)
				}
			}
			for _, unwanted := range test.notNamed {
				if strings.Contains(errOut, unwanted) {
					t.Errorf("stderr %q names %q, which the configuration carries", errOut, unwanted)
				}
			}
			if strings.Contains(errOut, "no configuration file was read") {
				t.Errorf("stderr %q says no file was read, but %s was", errOut, path)
			}
			if calls := supervisorRecordedCalls(t, record); len(calls) != 0 {
				t.Errorf("the relay was called %q", calls)
			}
		})
	}
	t.Run("no file at the configuration home", func(t *testing.T) {
		coreTempHome(t)
		home := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", home)
		t.Setenv("CRW_CONFIG", "")
		record := supervisorFakeProcess(t, 0)
		code, _, errOut := supervisorRunLine(t, "supervisor", "register", "--task", "thread-named")
		if code != usageExit {
			t.Fatalf("exit %d, want %d (stderr %q)", code, usageExit, errOut)
		}
		for _, want := range []string{supervisorUnconfigured, "manage.supervisor.host_id", "manage.supervisor.settings_file",
			"no configuration file was read", filepath.Join(home, "crw", "config.json"), "CRW_CONFIG"} {
			if !strings.Contains(errOut, want) {
				t.Errorf("stderr %q does not name %q", errOut, want)
			}
		}
		if calls := supervisorRecordedCalls(t, record); len(calls) != 0 {
			t.Errorf("the relay was called %q", calls)
		}
	})
	t.Run("CRW_CONFIG names a file that is not there", func(t *testing.T) {
		coreTempHome(t)
		missing := filepath.Join(t.TempDir(), "absent.json")
		t.Setenv("CRW_CONFIG", missing)
		record := supervisorFakeProcess(t, 0)
		code, _, errOut := supervisorRunLine(t, "supervisor", "register", "--task", "thread-named")
		if code != usageExit {
			t.Fatalf("exit %d, want %d (stderr %q)", code, usageExit, errOut)
		}
		for _, want := range []string{"manage.supervisor.host_id", "no configuration file was read", missing, "CRW_CONFIG"} {
			if !strings.Contains(errOut, want) {
				t.Errorf("stderr %q does not name %q", errOut, want)
			}
		}
		if calls := supervisorRecordedCalls(t, record); len(calls) != 0 {
			t.Errorf("the relay was called %q", calls)
		}
	})
	t.Run("show with no task anywhere", func(t *testing.T) {
		path := supervisorConfigFile(t, map[string]any{"host_id": "host-file"})
		record := supervisorFakeProcess(t, 0)
		code, _, errOut := supervisorRunLine(t, "supervisor", "show")
		if code != usageExit {
			t.Fatalf("exit %d, want %d (stderr %q)", code, usageExit, errOut)
		}
		for _, want := range []string{supervisorUnconfigured, "manage.supervisor.task_id", "--task", "CODEX_THREAD_ID", path} {
			if !strings.Contains(errOut, want) {
				t.Errorf("stderr %q does not name %q", errOut, want)
			}
		}
		for _, unwanted := range []string{"manage.supervisor.host_id", "manage.supervisor.settings_file"} {
			if strings.Contains(errOut, unwanted) {
				t.Errorf("stderr %q names %q, which show does not need", errOut, unwanted)
			}
		}
		if calls := supervisorRecordedCalls(t, record); len(calls) != 0 {
			t.Errorf("the relay was called %q", calls)
		}
	})
}

// Pin: the section is read from the real file through the package's own configuration, not the test
// seam. register binds and records with the file's values, and show reads the file's task back.
func TestSupervisorRegisterReadsManageSupervisorFromTheFile(t *testing.T) {
	supervisorConfigFile(t, map[string]any{
		"task_id":       "task-file",
		"host_id":       "host-file",
		"cwd":           "/work/file",
		"settings_file": "/work/file/settings.json",
	})
	exe, record := supervisorScript(t, map[string]string{
		"linkage-bind":    `{"bindingId":"bnd-file","cwd":"/work/file"}`,
		"settings-record": `{"taskId":"task-file","settings":{}}`,
		"settings-show":   `{"task":"task-file","settings":{},"usable":true}`,
		"linkage-up":      supervisorUnregistered,
	})
	e, out, errOut := supervisorConfigFileEnv(t, exe)
	if code := supervisorRegister(context.Background(), e, nil); code != 0 {
		t.Fatalf("register: exit %d, stderr %q", code, errOut.String())
	}
	var registered supervisorRecord
	if err := json.Unmarshal([]byte(out.String()), &registered); err != nil || !registered.OK || registered.TaskID != "task-file" {
		t.Fatalf("register reported %q (%v)", out.String(), err)
	}
	out.Reset()
	if code := supervisorShow(context.Background(), e, nil); code != 0 {
		t.Fatalf("show: exit %d, stderr %q", code, errOut.String())
	}
	var shown supervisorReportOut
	if err := json.Unmarshal([]byte(out.String()), &shown); err != nil || !shown.OK || shown.TaskID != "task-file" {
		t.Fatalf("show reported %q (%v)", out.String(), err)
	}
	supervisorCheckCalls(t, record, [][]string{
		{"relay", "--state", "/state", "--socket", "/socket", "linkage-bind", "--role", "supervisor",
			"--scope-kind", "management", "--scope", "task-file", "--task", "task-file", "--host", "host-file",
			"--cwd", "/work/file"},
		{"relay", "--state", "/state", "--socket", "/socket", "settings-record", "--task", "task-file",
			"--role", "supervisor", "--settings", "@/work/file/settings.json", "--source", "crw manage supervisor register"},
		{"relay", "--state", "/state", "--socket", "/socket", "settings-show", "--task", "task-file"},
		{"relay", "--state", "/state", "--socket", "/socket", "linkage-up", "--task", "task-file"},
	})
}

// show's help request prints its usage, which names --task, and calls no relay.
func TestSupervisorShowHelpNamesTheTask(t *testing.T) {
	for _, flag := range []string{"-h", "--help"} {
		t.Run(flag, func(t *testing.T) {
			record := supervisorFakeProcess(t, 0)
			supervisorUseConfig(t, supervisorTestConfig(nil))
			code, out, errOut := supervisorRunLine(t, "supervisor", "show", "--task", "thread-named", flag)
			if code != 0 || out != "usage: crw manage supervisor show [--task THREAD]\n" {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
			}
			if calls := supervisorRecordedCalls(t, record); len(calls) != 0 {
				t.Errorf("a help request called the relay %q", calls)
			}
		})
	}
}
