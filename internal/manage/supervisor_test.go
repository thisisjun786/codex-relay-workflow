package manage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// supervisorTestSection is a configured supervisor section. Every value is synthetic, so no real
// host, thread id or private path reaches the repository.
func supervisorTestSection() supervisorSection {
	return supervisorSection{
		TaskID:       "task-supervisor",
		HostID:       "host-supervisor",
		Cwd:          "/work/management",
		SettingsFile: "/work/management/settings.json",
	}
}

// supervisorTestConfig is a Config whose relay names a state directory and a socket, so the helper
// never asks doctor, and whose document carries the given supervisor section.
func supervisorTestConfig(section *supervisorSection) *Config {
	cfg := &Config{Relay: coreRelay{State: "/state", Socket: "/socket"}, raw: map[string]json.RawMessage{}}
	if section != nil {
		encoded, err := json.Marshal(section)
		if err != nil {
			panic(err)
		}
		cfg.raw["supervisor"] = encoded
	}
	return cfg
}

// supervisorUseConfig points the configuration seam at cfg for one test.
func supervisorUseConfig(t *testing.T, cfg *Config) {
	t.Helper()
	previous := supervisorConfig
	supervisorConfig = func(*Env) *Config { return cfg }
	t.Cleanup(func() { supervisorConfig = previous })
}

// supervisorRunLine runs crw manage supervisor through the registry, so the test proves the
// command is registered and the subcommand dispatch works, not only the handler.
func supervisorRunLine(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut strings.Builder
	code := Run(context.Background(), args, strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

// supervisorFakeProcess makes this test binary act as the fake crw for the relay helper Run
// starts: the test binary is the executable, and coreFakeMain records each call's arguments and
// exits with the status given.
func supervisorFakeProcess(t *testing.T, exit int) (record string) {
	t.Helper()
	record = filepath.Join(t.TempDir(), "argv.jsonl")
	t.Setenv(coreFakeEnv, "1")
	t.Setenv(coreFakeRecordEnv, record)
	t.Setenv(coreFakeDoctorEnv, "")
	t.Setenv(coreFakeExitEnv, strconv.Itoa(exit))
	return record
}

// supervisorRecordedCalls is coreFakeCalls for a run that may have made no call at all: an absent
// record is no calls rather than a failure.
func supervisorRecordedCalls(t *testing.T, record string) [][]string {
	t.Helper()
	if _, err := os.Stat(record); err != nil {
		return nil
	}
	return coreFakeCalls(t, record)
}

// supervisorEnv is an Env whose executable the relay helper runs, so a test can pin the answers a
// relay command gives without the test binary being the fake.
func supervisorEnv(exe string) (e *Env, out, errOut *strings.Builder) {
	out, errOut = &strings.Builder{}, &strings.Builder{}
	return &Env{Stdin: strings.NewReader(""), Stdout: out, Stderr: errOut, Getenv: os.Getenv, Now: time.Now, Executable: exe}, out, errOut
}

// supervisorScript writes a fake crw that records each call's arguments on one line and prints the
// answer for the command it was given, keyed by the command name.
func supervisorScript(t *testing.T, answers map[string]string) (exe, record string) {
	t.Helper()
	dir := t.TempDir()
	record = filepath.Join(dir, "calls.txt")
	var script strings.Builder
	script.WriteString("#!/bin/sh\n")
	script.WriteString("printf '%s\\n' \"$*\" >> " + supervisorQuote(record) + "\n")
	script.WriteString("case \"$*\" in\n")
	for _, name := range []string{"linkage-bind", "settings-record", "settings-show", "linkage-up"} {
		answer, ok := answers[name]
		if !ok {
			continue
		}
		path := filepath.Join(dir, name+".out")
		if err := os.WriteFile(path, []byte(answer), 0o600); err != nil {
			t.Fatal(err)
		}
		script.WriteString("  *" + name + "*) cat " + supervisorQuote(path) + " ;;\n")
	}
	script.WriteString("esac\nexit 0\n")
	exe = filepath.Join(dir, "crw")
	if err := os.WriteFile(exe, []byte(script.String()), 0o700); err != nil {
		t.Fatal(err)
	}
	return exe, record
}

func supervisorQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

// supervisorCalls is the recorded command lines of supervisorScript, in call order.
func supervisorCalls(t *testing.T, record string) []string {
	t.Helper()
	data, err := os.ReadFile(record)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var calls []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			calls = append(calls, line)
		}
	}
	return calls
}

// supervisorReportOut is the JSON object crw manage supervisor show writes.
type supervisorReportOut struct {
	OK       bool            `json:"ok"`
	TaskID   string          `json:"taskId"`
	Settings json.RawMessage `json:"settings"`
	Binding  json.RawMessage `json:"binding"`
}

// C1: register binds the store scope first and records the settings pair second, with exactly the
// decided argument vectors.
func TestSupervisorRegisterBindsTheStoreScopeThenRecordsSettings(t *testing.T) {
	record := supervisorFakeProcess(t, 0)
	section := supervisorTestSection()
	supervisorUseConfig(t, supervisorTestConfig(&section))
	code, out, errOut := supervisorRunLine(t, "supervisor", "register")
	if code != 0 {
		t.Fatalf("register: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	coreCheckCalls(t, supervisorRecordedCalls(t, record), [][]string{
		{"relay", "--state", "/state", "--socket", "/socket", "linkage-bind", "--role", "supervisor",
			"--scope-kind", "store", "--scope", "store", "--task", "task-supervisor", "--host", "host-supervisor",
			"--cwd", "/work/management"},
		{"relay", "--state", "/state", "--socket", "/socket", "settings-record", "--task", "task-supervisor",
			"--role", "supervisor", "--settings", "@/work/management/settings.json", "--source", "crw manage supervisor register"},
	})
}

// The command line overrides the section's cwd and settings file, and omitting cwd leaves the flag
// off the bind call entirely.
func TestSupervisorRegisterTakesCwdAndSettingsFileFromTheCommandLine(t *testing.T) {
	record := supervisorFakeProcess(t, 0)
	section := supervisorTestSection()
	section.Cwd, section.SettingsFile = "", "/other/settings.json"
	supervisorUseConfig(t, supervisorTestConfig(&section))
	if code, _, errOut := supervisorRunLine(t, "supervisor", "register", "--cwd", "/override", "--settings-file", "/override/settings.json"); code != 0 {
		t.Fatalf("register: exit %d, stderr %q", code, errOut)
	}
	coreCheckCalls(t, supervisorRecordedCalls(t, record), [][]string{
		{"relay", "--state", "/state", "--socket", "/socket", "linkage-bind", "--role", "supervisor",
			"--scope-kind", "store", "--scope", "store", "--task", "task-supervisor", "--host", "host-supervisor",
			"--cwd", "/override"},
		{"relay", "--state", "/state", "--socket", "/socket", "settings-record", "--task", "task-supervisor",
			"--role", "supervisor", "--settings", "@/override/settings.json", "--source", "crw manage supervisor register"},
	})
}

// C2: with no task_id or no host_id the relay is never called and the refusal is exit 2.
func TestSupervisorRegisterRefusesWhenUnconfigured(t *testing.T) {
	for _, test := range []struct {
		name           string
		taskID, hostID string
	}{
		{"no task", "", "host-supervisor"},
		{"no host", "task-supervisor", ""},
		{"neither", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := supervisorFakeProcess(t, 0)
			section := supervisorTestSection()
			section.TaskID, section.HostID = test.taskID, test.hostID
			supervisorUseConfig(t, supervisorTestConfig(&section))
			code, _, errOut := supervisorRunLine(t, "supervisor", "register")
			if code != usageExit {
				t.Fatalf("exit %d, want %d", code, usageExit)
			}
			if !strings.Contains(errOut, supervisorUnconfigured) {
				t.Errorf("stderr %q does not name %s", errOut, supervisorUnconfigured)
			}
			if calls := supervisorRecordedCalls(t, record); len(calls) != 0 {
				t.Errorf("the relay was called %q", calls)
			}
		})
	}
}

// C2: show is refused the same way, before any relay call.
func TestSupervisorShowRefusesWhenUnconfigured(t *testing.T) {
	record := supervisorFakeProcess(t, 0)
	section := supervisorTestSection()
	section.HostID = ""
	supervisorUseConfig(t, supervisorTestConfig(&section))
	code, _, errOut := supervisorRunLine(t, "supervisor", "show")
	if code != usageExit || !strings.Contains(errOut, supervisorUnconfigured) {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if calls := supervisorRecordedCalls(t, record); len(calls) != 0 {
		t.Errorf("the relay was called %q", calls)
	}
}

// C3: a refused bind is reported as the relay's own refusal with exit 1, and settings-record is
// never called.
func TestSupervisorRegisterDoesNotRecordSettingsWhenTheBindIsRefused(t *testing.T) {
	record := supervisorFakeProcess(t, 2)
	section := supervisorTestSection()
	supervisorUseConfig(t, supervisorTestConfig(&section))
	code, _, errOut := supervisorRunLine(t, "supervisor", "register")
	if code != supervisorExitRefused {
		t.Fatalf("exit %d, want %d (stderr %q)", code, supervisorExitRefused, errOut)
	}
	calls := supervisorRecordedCalls(t, record)
	if len(calls) != 1 || !slices.Contains(calls[0], "linkage-bind") || slices.Contains(calls[0], "settings-record") {
		t.Fatalf("the fake saw %q, want the bind call alone", calls)
	}
}

// C4: show reads the recorded settings and the recorded store binding and writes them as one JSON
// object.
func TestSupervisorShowReadsTheBindingAndSettings(t *testing.T) {
	exe, record := supervisorScript(t, map[string]string{
		"settings-show": `{"task":"task-supervisor","settings":{"cwd":"/work/management","model":"m"},"usable":true,"deliverable":true,"missing":[],"recordFinding":null}`,
		"linkage-up": `{"state":"resolved","readable":true,"levels":[{"scopeKind":"store","scopeKey":"store",` +
			`"owner":{"bindingId":"bnd-1","role":"supervisor","scopeKind":"store","scopeKey":"store",` +
			`"taskId":"task-supervisor","hostId":"host-supervisor","status":"active","revision":1},"depth":0}],` +
			`"gaps":[],"contention":[]}`,
	})
	section := supervisorTestSection()
	supervisorUseConfig(t, supervisorTestConfig(&section))
	e, out, errOut := supervisorEnv(exe)
	if code := supervisorShow(context.Background(), e, nil); code != 0 {
		t.Fatalf("show: exit %d, stderr %q", code, errOut.String())
	}
	if calls := supervisorCalls(t, record); len(calls) != 2 ||
		!strings.Contains(calls[0], "settings-show --task task-supervisor") ||
		!strings.Contains(calls[1], "linkage-up --task task-supervisor") {
		t.Fatalf("show called %q", calls)
	}
	var report supervisorReportOut
	if err := json.Unmarshal([]byte(out.String()), &report); err != nil {
		t.Fatalf("the output is not one JSON object: %v\n%s", err, out.String())
	}
	if !report.OK || report.TaskID != "task-supervisor" {
		t.Errorf("report %+v", report)
	}
	if !strings.Contains(string(report.Settings), `"usable":true`) {
		t.Errorf("settings = %s", report.Settings)
	}
	if !strings.Contains(string(report.Binding), `"bindingId":"bnd-1"`) {
		t.Errorf("binding = %s", report.Binding)
	}
}

// A task with no recorded store binding is reported as a null binding, not as a failure: the store
// answered and there is nothing.
func TestSupervisorShowReportsNoBindingAsNull(t *testing.T) {
	exe, _ := supervisorScript(t, map[string]string{
		"settings-show": `{"task":"task-supervisor","settings":null,"usable":false}`,
		"linkage-up":    `{"state":"unregistered","readable":true,"levels":[],"gaps":[],"contention":[]}`,
	})
	section := supervisorTestSection()
	supervisorUseConfig(t, supervisorTestConfig(&section))
	e, out, errOut := supervisorEnv(exe)
	if code := supervisorShow(context.Background(), e, nil); code != 0 {
		t.Fatalf("show: exit %d, stderr %q", code, errOut.String())
	}
	var report supervisorReportOut
	if err := json.Unmarshal([]byte(out.String()), &report); err != nil {
		t.Fatal(err)
	}
	if !report.OK || string(report.Binding) != "null" {
		t.Errorf("report %+v", report)
	}
	if !strings.Contains(string(report.Settings), `"usable":false`) {
		t.Errorf("settings = %s", report.Settings)
	}
}

// A store the reader could not read is a host failure, not a null binding: the answer says
// readable:false and exits 0, so reporting ok:true would claim the store said there is nothing.
func TestSupervisorShowReportsAnUnreadableStoreAsAFailure(t *testing.T) {
	exe, _ := supervisorScript(t, map[string]string{
		"settings-show": `{"task":"task-supervisor","settings":{"cwd":"/work/management"},"usable":true}`,
		"linkage-up":    `{"state":"unreadable","readable":false,"levels":[],"gaps":[],"contention":[],"detail":"no such table: scope_bindings"}`,
	})
	section := supervisorTestSection()
	supervisorUseConfig(t, supervisorTestConfig(&section))
	e, out, _ := supervisorEnv(exe)
	if code := supervisorShow(context.Background(), e, nil); code != supervisorExitHost {
		t.Fatalf("exit %d, want %d", code, supervisorExitHost)
	}
	if !strings.Contains(out.String(), "readable") && !strings.Contains(out.String(), "no such table") {
		t.Errorf("the unreadable answer was not passed through: %q", out.String())
	}
}

// A relay read that is refused is passed through with the relay's own exit status, so a refusal
// (exit 2) stays distinguishable from a host failure (exit 3).
func TestSupervisorShowPassesThroughARelayRefusal(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "crw")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nprintf '%s\\n' '{\"error\":\"refused\",\"reason\":\"store_absent\"}'\nexit 2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	section := supervisorTestSection()
	supervisorUseConfig(t, supervisorTestConfig(&section))
	e, out, _ := supervisorEnv(exe)
	if code := supervisorShow(context.Background(), e, nil); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(out.String(), "store_absent") {
		t.Errorf("the refusal was not passed through: %q", out.String())
	}
}

// A relay that answers a host failure (exit 3) on a read is passed through the same way, so the
// two failure kinds stay distinguishable at the command's own exit status.
func TestSupervisorShowPassesThroughARelayHostFailure(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "crw")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nprintf '%s\\n' '{\"error\":\"host\",\"detail\":\"the store is unreadable\"}'\nexit 3\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	section := supervisorTestSection()
	supervisorUseConfig(t, supervisorTestConfig(&section))
	e, out, _ := supervisorEnv(exe)
	if code := supervisorShow(context.Background(), e, nil); code != 3 {
		t.Fatalf("exit %d, want 3", code)
	}
	if !strings.Contains(out.String(), "the store is unreadable") {
		t.Errorf("the host failure was not passed through: %q", out.String())
	}
}

// The second register call is refused too: the bind stands, the pair record is reported as the
// relay's own refusal, and the command exits 1.
func TestSupervisorRegisterPassesThroughARefusedSettingsRecord(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "calls.txt")
	exe := filepath.Join(dir, "crw")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + supervisorQuote(record) + "\n" +
		"case \"$*\" in\n" +
		"  *linkage-bind*) printf '%s\\n' '{\"binding\":{\"bindingId\":\"bnd-1\"}}' ;;\n" +
		"  *settings-record*) printf '%s\\n' '{\"error\":\"refused\",\"reason\":\"settings_unavailable\"}'; exit 2 ;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(exe, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	section := supervisorTestSection()
	supervisorUseConfig(t, supervisorTestConfig(&section))
	e, out, _ := supervisorEnv(exe)
	if code := supervisorRegister(context.Background(), e, nil); code != supervisorExitRefused {
		t.Fatalf("exit %d, want %d", code, supervisorExitRefused)
	}
	if !strings.Contains(out.String(), "settings_unavailable") {
		t.Errorf("the refusal was not passed through: %q", out.String())
	}
	if calls := supervisorCalls(t, record); len(calls) != 2 {
		t.Errorf("the relay saw %q, want the bind and the record", calls)
	}
}

// A relay the helper cannot run at all is exit 3, and no relay command is reported as a refusal.
func TestSupervisorReportsARelayThatCannotRun(t *testing.T) {
	section := supervisorTestSection()
	supervisorUseConfig(t, supervisorTestConfig(&section))
	e, _, errOut := supervisorEnv(filepath.Join(t.TempDir(), "no-such-crw"))
	if code := supervisorShow(context.Background(), e, nil); code != 3 {
		t.Fatalf("exit %d, want 3 (stderr %q)", code, errOut.String())
	}
}

// The command line: -h is usage exit 0, an unknown subcommand and an unknown flag are exit 2, and a
// section that names no settings file is refused before any relay call.
func TestSupervisorCommandLine(t *testing.T) {
	section := supervisorTestSection()
	supervisorUseConfig(t, supervisorTestConfig(&section))
	if code, out, _ := supervisorRunLine(t, "supervisor", "-h"); code != 0 || !strings.Contains(out, "usage: crw manage supervisor") {
		t.Errorf("-h: exit %d, stdout %q", code, out)
	}
	for _, args := range [][]string{
		{"supervisor"},
		{"supervisor", "nope"},
		{"supervisor", "register", "--nope", "x"},
		{"supervisor", "register", "--cwd"},
		{"supervisor", "show", "--task", "t"},
	} {
		if code, _, _ := supervisorRunLine(t, args...); code != usageExit {
			t.Errorf("%q: exit %d, want %d", args, code, usageExit)
		}
	}
}

// A section that names no settings file cannot produce a pair record, so register refuses it as a
// usage error before calling the relay.
func TestSupervisorRegisterNeedsASettingsFile(t *testing.T) {
	record := supervisorFakeProcess(t, 0)
	section := supervisorTestSection()
	section.SettingsFile = ""
	supervisorUseConfig(t, supervisorTestConfig(&section))
	if code, _, _ := supervisorRunLine(t, "supervisor", "register"); code != usageExit {
		t.Fatalf("exit %d, want %d", code, usageExit)
	}
	if calls := supervisorRecordedCalls(t, record); len(calls) != 0 {
		t.Errorf("the relay was called %q", calls)
	}
}

// A section the document does not carry leaves every value empty, so register refuses rather than
// binding an unnamed thread.
func TestSupervisorRegisterRefusesWithNoSection(t *testing.T) {
	record := supervisorFakeProcess(t, 0)
	supervisorUseConfig(t, supervisorTestConfig(nil))
	code, _, errOut := supervisorRunLine(t, "supervisor", "register")
	if code != usageExit || !strings.Contains(errOut, supervisorUnconfigured) {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if calls := supervisorRecordedCalls(t, record); len(calls) != 0 {
		t.Errorf("the relay was called %q", calls)
	}
}
