//go:build dev

package ci

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pluginversion"
)

// The version internal/pluginversion recomputes from a commit is the version crw-dev ci plugin
// reports for that commit's work tree, so the base-refresh check and the packaging check cannot
// disagree about the same head.
func Test47_PLG_17_VersionOfTreeMatchesThePackagingCheck(t *testing.T) {
	r := pluginRepo(t, goodFiles(t))
	got := pluginCLI(t, r.root, "--json")
	if got.code != 0 {
		t.Fatalf("crw-dev ci plugin: %+v", got)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &report); err != nil {
		t.Fatalf("the report is not JSON: %v\n%s", err, got.stdout)
	}
	version, _ := report["version"].(string)
	if version == "" {
		t.Fatalf("the report names no version: %s", got.stdout)
	}
	fromTree, err := pluginversion.VersionOfTree(context.Background(), r.root, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	expectEqual(t, "VersionOfTree", fromTree, version)
}
