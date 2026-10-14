package cli_test

import (
	"os"
	"slices"
	"strings"
	"testing"
)

var childReportCommands = []string{"emit", "intent-claim", "intent-disposition"}

// CRW-1298: a child reports (emit, intent-claim, intent-disposition) only through the relay daemon's control.sock, so
// doctor lists them in daemonRequiredCommands, and in neither the store-only list nor the App Server one.
func TestDoctorListsTheChildReportCommandsAsDaemonRequired(t *testing.T) {
	state := ownerOnlyState(t, tempHome(t), "doctor")
	got := goCLI(t, "--state", state, "doctor")
	if got.code != 0 {
		t.Fatalf("doctor: exit %d %s", got.code, got.stdout)
	}
	reach, _ := object(t, got.stdout)["actorReachability"].(map[string]any)
	list := func(key string) (names []string) {
		items, _ := reach[key].([]any)
		for _, item := range items {
			names = append(names, item.(string))
		}
		return names
	}
	daemon := list("daemonRequiredCommands")
	if !slices.Equal(daemon, childReportCommands) {
		t.Errorf("daemonRequiredCommands = %v, want %v", daemon, childReportCommands)
	}
	for _, name := range childReportCommands {
		for _, key := range []string{"offlineCommands", "hostRequiredCommands"} {
			if slices.Contains(list(key), name) {
				t.Errorf("%s is in %s", name, key)
			}
		}
	}
}

// pageText is a page with its whitespace folded, so a sentence the page wraps reads as one line.
func pageText(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(strings.Fields(string(raw)), " ")
}

// historyIn reports the change-history words in the sentences of text that contain any of marks.
func historyIn(text string, marks ...string) (found []string) {
	for _, sentence := range strings.Split(text, ". ") {
		if !slices.ContainsFunc(marks, func(mark string) bool { return strings.Contains(sentence, mark) }) {
			continue
		}
		for _, history := range []string{" now ", "no longer", "previously", "used to", "formerly", "CRW-1", "not implemented", "before this"} {
			if strings.Contains(sentence, history) {
				found = append(found, history+": "+sentence)
			}
		}
	}
	return found
}

// CRW-1298: the skill pages say in the present tense that a child's report goes only through the relay daemon's
// control.sock, state the register refusal, and carry no change history.
func TestSkillPagesStateTheChildReportPath(t *testing.T) {
	pages := map[string]string{
		"relay.md":    pageText(t, "../../../plugins/crw/skills/crw-run/references/relay.md"),
		"delivery.md": pageText(t, "../../../plugins/crw/skills/crw-canon/references/delivery.md"),
	}
	for name, text := range pages {
		for _, want := range []string{"`emit`, `intent-claim` and `intent-disposition`", "relay daemon's `control.sock`", "danger-full-access", "workspace-write with network access", "`register --child-settings`"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not say %q", name, want)
			}
		}
		for _, found := range historyIn(text, "`intent-disposition`", "daemonRequiredCommands") {
			t.Errorf("%s: a child-report sentence carries change history (%s)", name, found)
		}
	}
	for _, want := range []string{"`daemonRequiredCommands`", "Treat anything outside `hostRequiredCommands` and `daemonRequiredCommands` as store-only"} {
		if !strings.Contains(pages["relay.md"], want) {
			t.Errorf("relay.md does not say %q", want)
		}
	}
}

// CRW-1297: the pages say in the present tense that a child that cannot connect reports through the bridge's report_child
// tool, which reaches the same relay daemon from outside its sandbox, and that the daemon alone decides where the record
// goes; no page says that a sandbox without network access cannot report at all.
func TestSkillPagesStateTheBridgeReportRoute(t *testing.T) {
	pages := map[string]string{
		"relay.md":      pageText(t, "../../../plugins/crw/skills/crw-run/references/relay.md"),
		"delivery.md":   pageText(t, "../../../plugins/crw/skills/crw-canon/references/delivery.md"),
		"bridge.md":     pageText(t, "../../../plugins/crw/skills/crw-run/references/bridge.md"),
		"operations.md": pageText(t, "../../../docs/relay/operations.md"),
	}
	for name, text := range pages {
		for _, want := range []string{"`report_child`", "relay daemon"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not say %q", name, want)
			}
		}
		for _, found := range historyIn(text, "`report_child`") {
			t.Errorf("%s: a report_child sentence carries change history (%s)", name, found)
		}
	}
	if !strings.Contains(pages["operations.md"], "daemon alone decides where the record goes") {
		t.Error("operations.md does not say the relay daemon alone decides where the record goes")
	}
	if strings.Contains(pages["operations.md"], "cannot report at all") || strings.Contains(pages["operations.md"], "or a bridge MCP call") {
		t.Error("operations.md still says a child without network access cannot report, or that the bridge is no route")
	}
}
