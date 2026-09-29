//go:build parity

package doctor_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
)

// Findings 22 and 36, against the launcher itself: for every bridge record, the doctor judges
// the record's executable exactly when plugins/crw/wiring/crw_bridge_mcp.py gets as far as
// exec (the selected bridge is a fake binary, so an accepted record fails there with "could not
// start"), and refuses it exactly when the launcher exits before exec.
func TestParity_the_bridge_record_is_refused_where_the_launcher_refuses_it(t *testing.T) {
	h, _, _ := goHost(t, true)
	launcher := filepath.Join(golden.Root(), "plugins", "crw", "wiring", "crw_bridge_mcp.py")
	policy := filepath.Join(h.home, "policy.toml")
	write(t, policy, "roles = []\n", 0o600)
	sum := sha256.Sum256([]byte("roles = []\n"))
	digest := hex.EncodeToString(sum[:])
	mkdir(t, filepath.Join(h.home, "policy-directory"))
	crw := filepath.Join(h.current(), "bin", "crw")
	for name, changes := range map[string]map[string]any{
		"version 1":                    nil,
		"version true":                 {"recordVersion": true},
		"version 1.0":                  {"recordVersion": 1.0},
		"version 3":                    {"recordVersion": 3},
		"version false":                {"recordVersion": false},
		"no version":                   {"recordVersion": nil},
		"string args":                  {"args": "--x"},
		"a number in args":             {"args": []any{"bridge", 5}},
		"object args":                  {"args": map[string]any{"a": 1}},
		"null args":                    {"args": nil},
		"another server":               {"serverName": "other"},
		"the declared server":          {"serverName": "codex-thread-bridge"},
		"a bare executable":            {"bridgeExecutable": "codex-thread-bridge"},
		"crw bridge":                   {"bridgeExecutable": crw, "args": []any{"bridge"}},
		"v1 naming a policy":           {"executionPolicy": map[string]any{"path": policy, "digest": digest}},
		"v2 without a policy":          {"recordVersion": 2},
		"v2 with a good policy":        {"recordVersion": 2, "executionPolicy": map[string]any{"path": policy, "digest": digest}},
		"v2 with a changed policy":     {"recordVersion": 2, "executionPolicy": map[string]any{"path": policy, "digest": strings.Repeat("0", 64)}},
		"v2 with a missing policy":     {"recordVersion": 2, "executionPolicy": map[string]any{"path": filepath.Join(h.home, "gone.toml"), "digest": digest}},
		"v2 with a directory policy":   {"recordVersion": 2, "executionPolicy": map[string]any{"path": filepath.Join(h.home, "policy-directory"), "digest": digest}},
		"v2 with a padded policy path": {"recordVersion": 2, "executionPolicy": map[string]any{"path": policy + " ", "digest": digest}},
		"v2 with an extra policy key":  {"recordVersion": 2, "executionPolicy": map[string]any{"path": policy, "digest": digest, "x": 1}},
	} {
		t.Run(name, func(t *testing.T) {
			document := h.bridgeRecord(nil)
			for k, v := range changes {
				document[k] = v // a nil value is written as null, which the launcher reads as absent
			}
			write(t, filepath.Join(h.codex, "crw-bridge-mcp.json"), encodeJSON(t, document), 0o600)
			cmd := exec.Command("python3", launcher)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + h.home, "CODEX_HOME=" + h.codex, "PYTHONDONTWRITEBYTECODE=1"}
			out, _ := cmd.CombinedOutput()
			launcherExecs := strings.Contains(string(out), "could not start")
			bridge := golden.Canon(at(h.diagnose(t), "components", "codex-thread-bridge"))
			doctorJudges := strings.Contains(bridge, `"field":"bridgeExecutable"`) && !strings.Contains(bridge, "the packaged bridge launcher refuses")
			if launcherExecs != doctorJudges {
				t.Errorf("the launcher execs: %v (%s), the doctor judges the executable: %v\n%s", launcherExecs, strings.TrimSpace(string(out)), doctorJudges, bridge)
			}
		})
	}
}
