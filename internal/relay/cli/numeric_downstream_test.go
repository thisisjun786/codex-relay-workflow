package cli_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// Exercise every implemented numeric action beyond argparse and module loading.
// Each process starts from a byte copy of the same Python-created store.
func Test24NumericDownstreamBytes(t *testing.T) {
	t.Parallel()
	root, _ := filepath.Abs("../../..")
	_, alias := packageBinary(t)
	home := t.TempDir()
	env := append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home+"/xdg", "XDG_CONFIG_HOME="+home+"/config", "XDG_DATA_HOME="+home+"/data", "CODEX_HOME="+home+"/codex", "CRW_ALLOW_LIVE_STATE=1", "PATH="+root+"/internal/relay/cli/testdata:"+os.Getenv("PATH"))
	python := filepath.Join(root, ".venv/bin/python")
	setup := runParityProcess(t, env, python, "testdata/numeric_downstream.py", home)
	if setup.code != 0 {
		t.Fatalf("setup: %+v", setup)
	}
	var ids map[string]string
	if err := json.Unmarshal([]byte(setup.out), &ids); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(home, "state", "relay.sqlite3")
	// The setup left a store Python owns. Each run starts from a byte copy of it put back in
	// place, whose mirror is rebuilt from the copy's own durable stamp (Rehome); the Go run then
	// follows a takeover, as on a host (runParityProcess hands the selected store to Go).
	baseline, err := os.ReadFile(db)
	if err != nil {
		t.Fatal(err)
	}
	restore := func(t *testing.T) {
		t.Helper()
		for _, name := range []string{db + "-wal", db + "-shm", filepath.Join(filepath.Dir(db), "takeover.json")} {
			if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(db, baseline, 0600); err != nil {
			t.Fatal(err)
		}
		testsupport.Rehome(t, db)
	}
	assert := func(t *testing.T, args []string) {
		t.Helper()
		args = append([]string{"--state", filepath.Join(home, "state")}, args...)
		restore(t)
		want := runParityProcess(t, env, python, append([]string{"-m", "codex_session_relay.cli"}, args...)...)
		restore(t)
		got := runParityProcess(t, env, alias, args...)
		want.out = evidenceTimestamp.ReplaceAllString(want.out, "<time>")
		got.out = evidenceTimestamp.ReplaceAllString(got.out, "<time>")
		if got != want {
			t.Fatalf("downstream byte diff %v\nGo=%+v\nPython=%+v", args, got, want)
		}
	}
	value := func(a argparse.Action) string {
		if len(a.Choices) > 0 {
			return a.Choices[0]
		}
		if a.Type == "int" || a.Type == "float" {
			return "1"
		}
		switch a.Dest {
		case "repository":
			return "o/r"
		case "relationship":
			return ids["relationship"]
		case "fault":
			return ids["fault"]
		case "request_id":
			return "request"
		case "fingerprint":
			return "fingerprint"
		case "project":
			return "PROJ"
		case "task", "parent_task":
			return "P"
		case "turn_thread":
			return "C"
		case "turn_id":
			return "turn"
		}
		return "v"
	}
	represented := map[string]bool{}
	for name, spec := range argparse.Specs {
		if !cli.Registered(name) || name == "slot-release" || name == "limit-declare" || name == "usage-observe" {
			continue
		}
		for index, a := range spec.Actions {
			if a.Type != "int" && a.Type != "float" {
				continue
			}
			if !exhaustiveParity && represented[a.Type] {
				continue
			}
			represented[a.Type] = true
			var base []string
			for j, b := range spec.Actions {
				if j != index && b.Required {
					base = append(base, b.Flags[len(b.Flags)-1], value(b))
				}
			}
			for _, g := range spec.Groups {
				if g.Required {
					b := spec.Actions[g.Actions[0]]
					base = append(base, b.Flags[len(b.Flags)-1])
					if b.Kind != "_StoreTrueAction" {
						base = append(base, value(b))
					}
				}
			}
			values := []string{"9999999999999999999999999", "-9999999999999999999999999", "١٢", "1_0"}
			if a.Type == "float" {
				values = []string{"NaN", "inf", "١٢.٣", "1_0"}
			}
			if !exhaustiveParity {
				values = values[:1]
			}
			for i, v := range values {
				t.Run(fmt.Sprintf("%s/%s/%d", name, a.Dest, i), func(t *testing.T) {
					args := append([]string{name}, base...)
					args = append(args, a.Flags[len(a.Flags)-1]+"="+v)
					assert(t, args)
				})
			}
		}
	}
	for _, v := range []string{"9999999999999999999999999", "١٢", "1_0", "NaN", "inf", " 60 "} {
		t.Run("policy-write/"+v, func(t *testing.T) {
			assert(t, []string{"fault-policy", "--product", "crw", "--fault-class", "report_omitted", "--severity", "degraded", "--reason=needs review", "--threshold=9999999999999999999999999", "--window=" + v})
		})
		t.Run("limit-write/"+v, func(t *testing.T) {
			assert(t, []string{"fault-limit", "--product", "crw", "--kind", "notification", "--max-count=9999999999999999999999999", "--window=" + v})
		})
	}
	for _, field := range []string{"generation", "attempt"} {
		for _, v := range []string{"9999999999999999999999999", "-9999999999999999999999999"} {
			t.Run("emit-execution/"+field+"/"+v, func(t *testing.T) {
				assert(t, []string{"emit", "--relationship", ids["relationship"], "--generation=1", "--attempt=1", "--outcome=interrupted", "--turn-thread=C", "--turn-id=turn", "--turn-status=interrupted", "--no-enqueue", "--" + field + "=" + v})
			})
		}
	}
}
