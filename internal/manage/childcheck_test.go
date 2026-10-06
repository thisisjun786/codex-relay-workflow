package manage

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// childCheckHost starts a fake App Server answering thread/read with the given thread fields and
// mcpServerStatus/list with one row per server, and points the command's default socket at it.
func childCheckHost(t *testing.T, model, effort, statusType string, servers map[string]string) *fakehost.Server {
	t.Helper()
	host := fakehost.Start(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{
		"model": model, "reasoningEffort": effort, "status": map[string]any{"type": statusType},
	}}})
	rows := []any{}
	for name, status := range servers {
		rows = append(rows, map[string]any{"name": name, "runtimeStatus": status, "pluginId": nil})
	}
	host.Respond("mcpServerStatus/list", fakehost.Reply{Result: map[string]any{"data": rows, "nextCursor": nil}})
	hostReadEnv(t, host.SocketPath)
	return host
}

// childCheckRun runs crw manage child-check and decodes the report it writes.
func childCheckRun(t *testing.T, args ...string) (int, childCheckReport) {
	t.Helper()
	code, out := hostReadRun(t, append([]string{"child-check"}, args...)...)
	var report childCheckReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("the output is not one JSON object: %v\n%s", err, out)
	}
	return code, report
}

// C3: model, effort and every disabled server agree, so the report is ok and exits 0, and the
// report names each side of the comparison and the servers it read.
func TestChildCheckAcceptsAMatchingChild(t *testing.T) {
	childCheckHost(t, "deepseek-v4.1-flash", "none", "idle", map[string]string{"alpha": "disabled", "beta": "disabled"})
	code, report := childCheckRun(t, "--thread", "t1", "--model", "deepseek-v4.1-flash", "--effort", "none", "--disabled", "alpha,beta")
	if code != 0 || !report.OK || report.Thread != "t1" || report.ThreadStatus != "idle" {
		t.Fatalf("exit %d, report %+v", code, report)
	}
	if report.Model != (childCheckMatch{"deepseek-v4.1-flash", "deepseek-v4.1-flash", true}) {
		t.Fatalf("model block: %+v", report.Model)
	}
	if report.Effort != (childCheckMatch{"none", "none", true}) {
		t.Fatalf("effort block: %+v", report.Effort)
	}
	if report.Servers["alpha"] != "disabled" || report.Servers["beta"] != "disabled" {
		t.Fatalf("servers: %+v", report.Servers)
	}
	if len(report.NotDisabled) != 0 || len(report.Missing) != 0 {
		t.Fatalf("notDisabled/missing: %+v", report)
	}
}

// C3: each way a child can differ is a mismatch and exits 1, and the report names the difference.
func TestChildCheckReportsEachMismatch(t *testing.T) {
	cases := []struct {
		name                    string
		model, effort, status   string
		servers                 map[string]string
		args                    []string
		modelMatch, effortMatch bool
		notDisabled, missing    []string
	}{
		{"model", "deepseek-v4.1-flash", "none", "idle", map[string]string{"alpha": "disabled"},
			[]string{"--model", "glm-5.3", "--effort", "none", "--disabled", "alpha"}, false, true, nil, nil},
		{"effort", "deepseek-v4.1-flash", "none", "idle", map[string]string{"alpha": "disabled"},
			[]string{"--model", "deepseek-v4.1-flash", "--effort", "high", "--disabled", "alpha"}, true, false, nil, nil},
		{"server enabled", "m", "none", "idle", map[string]string{"alpha": "disabled", "beta": "connected"},
			[]string{"--model", "m", "--effort", "none", "--disabled", "alpha,beta"}, true, true, []string{"beta"}, nil},
		{"server missing", "m", "none", "idle", map[string]string{"alpha": "disabled"},
			[]string{"--model", "m", "--effort", "none", "--disabled", "alpha,gamma"}, true, true, nil, []string{"gamma"}},
		{"notLoaded", "m", "none", "notLoaded", map[string]string{"alpha": "disabled"},
			[]string{"--model", "m", "--effort", "none", "--disabled", "alpha"}, true, true, nil, nil},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			childCheckHost(t, test.model, test.effort, test.status, test.servers)
			code, report := childCheckRun(t, append([]string{"--thread", "t1"}, test.args...)...)
			if code != 1 || report.OK || report.ThreadStatus != test.status || report.Model.Match != test.modelMatch ||
				report.Model.Actual != test.model || report.Effort.Match != test.effortMatch || report.Effort.Actual != test.effort ||
				!slices.Equal(report.NotDisabled, test.notDisabled) || !slices.Equal(report.Missing, test.missing) {
				t.Fatalf("exit %d, report %+v", code, report)
			}
		})
	}
}

// With no --disabled the disabled servers come from the config Section child_check.
func TestChildCheckReadsTheDisabledServersFromTheConfigSection(t *testing.T) {
	host := childCheckHost(t, "m", "none", "idle", map[string]string{"alpha": "connected"})
	cfg := hostReadConfig(host.SocketPath)
	cfg.raw["child_check"] = json.RawMessage(`{"disabled_servers":["alpha"]}`)
	report, err := childCheck(context.Background(), cfg, childCheckOptions{thread: "t1", model: "m", effort: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if report.OK || !slices.Equal(report.NotDisabled, []string{"alpha"}) {
		t.Fatalf("the config section was not read: %+v", report)
	}
}

// A read failure is exit 3 and reports host_unreachable; an unusable command line is a usage error
// with exit 2, and -h writes the usage to stdout and exits 0.
func TestChildCheckFailuresAndCommandLine(t *testing.T) {
	hostReadEnv(t, "")
	code, out := hostReadRun(t, "child-check", "--thread", "t1", "--model", "m", "--effort", "none")
	failure := hostReadDecode(t, out)
	if code != 3 || failure.OK || failure.Reason != "host_unreachable" {
		t.Fatalf("no socket: exit %d, output %s", code, out)
	}
	for _, args := range [][]string{
		{"child-check"},
		{"child-check", "--thread", "t1", "--model", "m"},
		{"child-check", "--thread", "t1", "--effort", "none"},
		{"child-check", "--thread", "t1", "--model", "m", "--effort", "none", "--nope", "x"},
	} {
		if code, _ := hostReadRun(t, args...); code != usageExit {
			t.Errorf("%q: exit %d, want %d", args, code, usageExit)
		}
	}
	if code, out := hostReadRun(t, "child-check", "-h"); code != 0 || !strings.Contains(out, "usage: crw manage child-check") {
		t.Fatalf("-h: exit %d, output %q", code, out)
	}
}
