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

// CRW-1298: the skill pages say in the present tense that a child's report goes only through the relay daemon's
// control.sock, state the register refusal, and carry no change history.
func TestSkillPagesStateTheChildReportPath(t *testing.T) {
	read := func(path string) string {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	pages := map[string]string{
		"relay.md":    read("../../../plugins/crw/skills/crw-run/references/relay.md"),
		"delivery.md": read("../../../plugins/crw/skills/crw-canon/references/delivery.md"),
	}
	for name, text := range pages {
		pages[name] = strings.Join(strings.Fields(text), " ")
	}
	for name, text := range pages {
		for _, want := range []string{"`emit`, `intent-claim` and `intent-disposition`", "relay daemon's `control.sock`", "danger-full-access", "workspace-write with network access", "`register --child-settings`"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not say %q", name, want)
			}
		}
	}
	relay := pages["relay.md"]
	for _, want := range []string{"`daemonRequiredCommands`", "Treat anything outside `hostRequiredCommands` and `daemonRequiredCommands` as store-only"} {
		if !strings.Contains(relay, want) {
			t.Errorf("relay.md does not say %q", want)
		}
	}
	// the child-report sentences are in the present tense: no history words in the sentences that name them
	for name, text := range pages {
		for _, sentence := range strings.Split(text, ". ") {
			if !strings.Contains(sentence, "`intent-disposition`") && !strings.Contains(sentence, "daemonRequiredCommands") {
				continue
			}
			for _, history := range []string{" now ", "no longer", "previously", "used to", "formerly", "CRW-1", "before this"} {
				if strings.Contains(sentence, history) {
					t.Errorf("%s: a child-report sentence carries change history (%q): %s", name, history, sentence)
				}
			}
		}
	}
}
