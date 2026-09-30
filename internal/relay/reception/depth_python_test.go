package reception

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// consoleDepthProblem is what the Python console answers for a document nested depth deep at
// site, reduced to its recursion problem. It runs live only when pyoracle records or checks.
func consoleDepthProblem(t *testing.T, site string, depth int) string {
	t.Helper()
	root, _ := filepath.Abs("../../..")
	if site == "settings" {
		script := `import json,os,subprocess,sys
sys.path.insert(0,'packages/codex-session-relay')
from tests.test_store_reception import AFirstAssignmentThroughCreateAndRegister as C,CHILD
x=C('test_settings_nested_too_deep_to_read_leave_their_field_unread'); x.setUp(); x.registered(); packet=x._write('packet',x.first_assignment(dispatch='dispatch-1'))
row=x.store.db.execute('select settings from authorized_settings where task_id=?',(CHILD,)).fetchone(); marker='"anthropic/claude-opus-5-5"'; deep='['*int(sys.argv[1])+'"x"'+']'*int(sys.argv[1]); x.store.db.execute('update authorized_settings set settings=? where task_id=?',(row[0].replace(marker,deep),CHILD)); x.store.db.commit()
p=subprocess.run(['.venv/bin/codex-session-relay','--state',x.state,'packet-check','--packet',packet,'--receiver',CHILD],capture_output=True,text=True); print(p.stdout); x.doCleanups()`
		cmd := exec.Command(filepath.Join(root, ".venv/bin/python"), "-c", script, strconv.Itoa(depth))
		cmd.Dir = root
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), "RecursionError: ") {
			return "maximum recursion depth exceeded while decoding a JSON array from a unicode string"
		}
		return ""
	}
	dir := t.TempDir()
	raw := strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth)
	packet, record := filepath.Join(dir, "packet.json"), filepath.Join(dir, "record.json")
	if err := os.WriteFile(packet, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(record, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--state", filepath.Join(dir, "state"), "packet-check", "--packet", packet, "--record", record}
	switch site {
	case "ledger":
		if err := os.WriteFile(packet, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
		ledger := filepath.Join(dir, "ledger.json")
		if err := os.WriteFile(ledger, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		args = []string{"--state", filepath.Join(dir, "state"), "packet-check", "--packet", packet, "--receiver", "r", "--ledger", ledger}
	case "declaration":
		if err := os.MkdirAll(filepath.Join(dir, "state"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "state", "launch-policy.json"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		args = []string{"--state", filepath.Join(dir, "state"), "packet-check", "--packet", packet, "--receiver", "r"}
	case "policy":
		if err := os.MkdirAll(filepath.Join(dir, "state"), 0700); err != nil {
			t.Fatal(err)
		}
		policy := filepath.Join(dir, "policy.json")
		if err := os.WriteFile(policy, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		declaration, _ := json.Marshal(map[string]any{"path": policy, "declaredAt": "x", "declaredBy": "x"})
		if err := os.WriteFile(filepath.Join(dir, "state", "launch-policy.json"), declaration, 0600); err != nil {
			t.Fatal(err)
		}
		args = []string{"--state", filepath.Join(dir, "state"), "packet-check", "--packet", packet, "--receiver", "r"}
	}
	if site == "declaration" || site == "policy" {
		gen := exec.Command(filepath.Join(root, ".venv/bin/python"), "-c", `import json; from tests.test_reception_findings import packet_kwargs; from codex_session_relay import packets; print(json.dumps(packets.compose(**packet_kwargs('parent_to_child','assignment'))))`)
		gen.Dir = filepath.Join(root, "packages/codex-session-relay")
		valid, err := gen.Output()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(packet, valid, 0600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(filepath.Join(root, ".venv/bin/codex-session-relay"), args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if _, ok := err.(*exec.ExitError); !ok && err != nil {
		t.Fatal(err)
	}
	var answer map[string]any
	if err := json.Unmarshal(out, &answer); err != nil {
		t.Fatalf("%s: %v: %s", site, err, out)
	}
	detail, _ := answer["detail"].(string)
	if strings.Contains(detail, "RecursionError: ") {
		_, detail, _ = strings.Cut(detail, "RecursionError: ")
		return detail
	}
	if strings.Contains(detail, "maximum recursion depth exceeded") || strings.Contains(detail, "nested deeper than the execution policy parser can read") {
		return "maximum recursion depth exceeded while decoding a JSON array from a unicode string"
	}
	return ""
}

func Test23JSONDepthBoundaryMatchesPython(t *testing.T) {
	for _, site := range []string{"packet", "ledger", "settings", "declaration", "policy"} {
		for _, depth := range []int{9997, 9998, 9999} {
			t.Run(site+"/"+strconv.Itoa(depth), func(t *testing.T) {
				want := string(pyoracle.Answer(t, "problem", func() ([]byte, error) {
					return []byte(consoleDepthProblem(t, site, depth)), nil
				}))
				raw := []byte(strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth))
				var got string
				switch site {
				case "packet", "ledger":
					got = JSONReaderDepthProblem(raw)
				case "settings":
					got = JSONSettingsDepthProblem(raw)
				case "declaration":
					got = JSONDepthProblem(raw)
				case "policy":
					// This file is parsed by rolepolicy before packetPolicy's safety scan.
					got = ""
				}
				if got != want {
					t.Fatalf("depth %d: Go=%q Python=%q", depth, got, want)
				}
			})
		}
	}
}
