package manage

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/layername"
)

// CRW-1477 (bundle CRW-1448): the text crw manage writes for a person names the session layers 부모 and
// 자식, not the relay role ids (plugins/crw/skills/crw-canon/references/hierarchy.md "The levels"). The
// relay role id `parent` is the 자식 and `supervisor` is the 부모; command names, flag names and flag
// values stay as they are.

// layerWordsCheck fails the test when text has a bare role word or lacks one of the layer names.
func layerWordsCheck(t *testing.T, what, text string, names ...string) {
	t.Helper()
	if hits := layername.Find(text); len(hits) != 0 {
		t.Errorf("%s names a role id as a layer %v:\n%s", what, hits, text)
	}
	for _, name := range names {
		if !strings.Contains(text, name) {
			t.Errorf("%s does not name the %s:\n%s", what, name, text)
		}
	}
}

// The whole stderr of `send-parent -h`, including the flag defaults flag.PrintDefaults writes, and of a
// refused --role names both layers and no bare role id.
func TestSendParentUsageNamesLayers(t *testing.T) {
	coreTempHome(t)
	for _, c := range []struct {
		name string
		args []string
	}{
		{"-h", []string{"send-parent", "-h"}},
		{"--role child", []string{"send-parent", "--thread", "thread-1", "--message-file", "m.txt", "--role", "child"}},
	} {
		var out, errOut strings.Builder
		if code := Run(context.Background(), c.args, strings.NewReader(""), &out, &errOut); code != usageExit {
			t.Errorf("%s: exit %d, want %d (stderr %q)", c.name, code, usageExit, errOut.String())
		}
		layerWordsCheck(t, c.name+" stderr", errOut.String(), "자식", "부모")
	}
}

// The default role stays `parent` (D13): the parse without --role yields it, the delivery is sent with it,
// and an explicit empty --role is a usage error, not the default.
func TestSendParentDefaultRoleStaysParent(t *testing.T) {
	parsed, err := sendParentParse([]string{"--thread", "thread-1", "--message-file", "m.txt"}, io.Discard)
	if err != nil || parsed.role != "parent" {
		t.Fatalf("the parse without --role is %+v, %v; want the role parent", parsed, err)
	}
	if _, err := sendParentParse([]string{"--thread", "thread-1", "--message-file", "m.txt", "--role", ""}, io.Discard); err == nil {
		t.Error("an explicit empty --role was accepted")
	}
	coreTempHome(t)
	var out, errOut strings.Builder
	if code := Run(context.Background(), []string{"send-parent", "--thread", "thread-1", "--message-file", "m.txt", "--role", ""}, strings.NewReader(""), &out, &errOut); code != usageExit {
		t.Errorf("--role \"\": exit %d, want %d (stderr %q)", code, usageExit, errOut.String())
	}
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
	})
	cfg := deliverSendConfig(t, bridge)
	cfg.Settings.Parent = coreSettings{Model: "parent-model", ReasoningEffort: "low"}
	sendParentWithConfig(t, cfg)
	if code, stdout := sendParentRunCommand(t, "--thread", "thread-1", "--message-file", sendParentMessageFile(t, "a notice"), "--logical-id", "m1"); code != sendParentExitAccepted {
		t.Fatalf("exit %d: %s", code, stdout)
	}
	var roles []any
	for _, call := range deliverSendCallsOf(t, log) {
		if call["tool"] == deliverToolSend {
			roles = append(roles, call["args"].(map[string]any)["role"])
		}
	}
	if !reflect.DeepEqual(roles, []any{"parent"}) {
		t.Errorf("the send went out with the roles %v, want [parent]", roles)
	}
}

// Every command summary `crw manage -h` lists names a layer by its layer name, never by a role id.
func TestManageSummariesNameLayers(t *testing.T) {
	want := map[string][]string{"send-parent": {"자식", "부모"}, "capacity": {"자식"}, "premerge": {"자식"}}
	for _, c := range coreCommands() {
		layerWordsCheck(t, c.Name+" summary", c.Summary, want[c.Name]...)
		delete(want, c.Name)
	}
	if len(want) != 0 {
		t.Errorf("the commands %v are not registered", want)
	}
}

// `crw manage supervisor -h` says which layer the command's seat is.
func TestSupervisorUsageNamesTheParent(t *testing.T) {
	code, out, errOut := supervisorRunLine(t, "supervisor", "-h")
	if code != 0 {
		t.Fatalf("exit %d (stderr %q)", code, errOut)
	}
	layerWordsCheck(t, "supervisor -h", out, "supervisor (부모)")
}

// The texts of the 부모 queue name the 부모: the report-undelivered event, a notice that is not JSON and a
// logical id that already belongs to another notice.
func TestSupervisorQueueTextNamesTheParent(t *testing.T) {
	now := pumpTestNow
	pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "bridge")
	st := pumpTestReadStatePtr(t, cfg)
	n := supervisorNotice{LogicalID: "run-1:M1:report", Thread: "lead-thread", BindingTask: "lead-thread", ReportID: "rep_1", Initiative: "I-23", Text: "report is ready", CreatedAt: now.UTC().Format(time.RFC3339)}
	pumpSupervisorUndelivered(cfg, st, n, "notLoaded", time.Hour)
	if len(st.Pending) != 1 {
		t.Fatalf("%d pending events, want the report_undelivered one", len(st.Pending))
	}
	layerWordsCheck(t, "the report-undelivered event", st.Pending[0].Text, "부모")

	dir := supervisorQueueDirOf(cfg, "lead-thread")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisorQueueRead(dir, "broken.json"); err == nil {
		t.Error("a notice that is not JSON was read")
	} else {
		layerWordsCheck(t, "the not-JSON refusal", err.Error(), "부모")
	}

	if _, err := supervisorQueueWrite(cfg, n); err != nil {
		t.Fatal(err)
	}
	other := n
	other.Text = "another text"
	if _, err := supervisorQueueWrite(cfg, other); err == nil {
		t.Error("a second text under the same logical id was queued")
	} else {
		layerWordsCheck(t, "the duplicate logical id refusal", err.Error(), "부모")
	}
}
