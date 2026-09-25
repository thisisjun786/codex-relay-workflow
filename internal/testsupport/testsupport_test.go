package testsupport_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// pythonTaskSettings is json.dumps(support.task_settings("/w", model="m2", extra=1), indent=2)
// as printed by the Python suite's own helper.
const pythonTaskSettings = `{
  "sandbox": {
    "type": "workspaceWrite",
    "writableRoots": [],
    "networkAccess": false,
    "excludeTmpdirEnvVar": false,
    "excludeSlashTmp": false
  },
  "approvalPolicy": "never",
  "cwd": "/w",
  "runtimeWorkspaceRoots": [
    "/w"
  ],
  "model": "m2",
  "reasoningEffort": "xhigh",
  "environments": [
    {
      "environmentId": "local",
      "cwd": "/w",
      "runtimeWorkspaceRoots": [
        "/w"
      ]
    }
  ],
  "extra": 1
}
`

func TestTaskSettings_matchesPythonBytes_whenOverridingAndExtending(t *testing.T) {
	// Given
	var out bytes.Buffer

	// When
	settings := testsupport.TaskSettings("/w", contract.Field{Key: "model", Value: "m2"}, contract.Field{Key: "extra", Value: 1})

	// Then
	if err := contract.Emit(&out, settings); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if out.String() != pythonTaskSettings {
		t.Fatalf("got\n%s\nwant\n%s", out.String(), pythonTaskSettings)
	}
}

func TestFakeClock_formatsLikePythonIsoformat_whenAdvancedByFraction(t *testing.T) {
	// Given
	clock := testsupport.NewFakeClock()
	start := clock.ISO()

	// When
	clock.Advance(1500 * time.Millisecond)

	// Then: the values Python's FakeClock prints for the same moves.
	if start != "2023-11-14T22:13:20.000000+00:00" || clock.ISO() != "2023-11-14T22:13:21.500000+00:00" {
		t.Fatalf("start=%s advanced=%s", start, clock.ISO())
	}
}

func TestNewTree_pointsStateHomeInsideTree_whenCreated(t *testing.T) {
	// When
	tree := testsupport.NewTree(t)
	path := tree.Artifact("nested/out.txt", "the deliverable")

	// Then
	if !strings.HasPrefix(os.Getenv("XDG_STATE_HOME"), tree.Tmp) || filepath.Dir(tree.StatePath) != filepath.Join(tree.Tmp, "state") {
		t.Fatalf("XDG_STATE_HOME=%s tree=%+v", os.Getenv("XDG_STATE_HOME"), tree)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "the deliverable" || filepath.Dir(path) != filepath.Join(tree.Root, "nested") {
		t.Fatalf("artifact %s = %q, %v", path, got, err)
	}
}
