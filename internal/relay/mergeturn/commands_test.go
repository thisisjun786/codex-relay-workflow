package mergeturn_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
)

func Test26_MTG_7_restate_base_help_explains_observed_sha(t *testing.T) {
	var out, stderr bytes.Buffer
	if code := cli.Execute(context.Background(), []string{"merge-turn-restate-base", "--help"}, &out, &stderr); code != 0 || !bytes.Contains(out.Bytes(), []byte("--observed-base-sha")) {
		t.Fatal(code, out.String(), stderr.String())
	}
}

// pythonMergeTurnCommands is the merge-turn half of test_coordination_cli.NEW_COMMANDS.
var pythonMergeTurnCommands = []string{"merge-turn-attest", "merge-turn-check", "merge-turn-land",
	"merge-turn-ready", "merge-turn-release", "merge-turn-request", "merge-turn-request-return",
	"merge-turn-resolve", "merge-turn-show", "merge-turn-unknown", "merge-turn-withdraw",
	"merge-turn-acknowledge", "merge-turn-restate-base"}

// markerCommands is cli.MARKER_COMMANDS_BY_NAME.
var markerCommands = []string{"intent-declare", "intent-attempt", "intent-bind", "intent-register", "intent-claim",
	"intent-disposition", "intent-resolve", "intent-show", "guard-evaluate"}

func Test26_CCL_1_merge_turn_commands_are_registered_offline_and_not_marker_commands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cli.Execute(context.Background(), []string{"--state", filepath.Join(t.TempDir(), "state"), "doctor"}, &stdout, &stderr)
	var report any
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("doctor exit %d: %v\n%s", code, err, stdout.String())
	}
	var doctor struct{ OfflineCommands, HostRequiredCommands []string }
	var find func(any)
	find = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, item := range x {
				if list, ok := item.([]any); ok && (k == "offlineCommands" || k == "hostRequiredCommands") {
					for _, name := range list {
						if k == "offlineCommands" {
							doctor.OfflineCommands = append(doctor.OfflineCommands, name.(string))
						} else {
							doctor.HostRequiredCommands = append(doctor.HostRequiredCommands, name.(string))
						}
					}
				}
				find(item)
			}
		case []any:
			for _, item := range x {
				find(item)
			}
		}
	}
	find(report)
	if len(doctor.OfflineCommands) == 0 {
		t.Fatalf("doctor reported no offlineCommands: %s", stdout.String())
	}
	for _, name := range pythonMergeTurnCommands {
		if !cli.Registered(name) {
			t.Errorf("%s is not registered", name)
		}
		var help, errors bytes.Buffer
		if code := cli.Execute(context.Background(), []string{name, "--help"}, &help, &errors); code != 0 || help.Len() == 0 {
			t.Errorf("%s --help: exit %d %q %q", name, code, help.String(), errors.String())
		}
		if !slices.Contains(doctor.OfflineCommands, name) {
			t.Errorf("%s is missing from offlineCommands", name)
		}
		if slices.Contains(doctor.HostRequiredCommands, name) {
			t.Errorf("%s is classified host-required", name)
		}
		if slices.Contains(markerCommands, name) {
			t.Errorf("%s is a marker command", name)
		}
	}
}
