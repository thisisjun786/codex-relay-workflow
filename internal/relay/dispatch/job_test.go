package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
)

func TestJobCLIContract(t *testing.T) {
	for _, verb := range []string{"run", "list", "get", "cancel", "off", "on", "status", "drain", "removal"} {
		t.Run(verb, func(t *testing.T) {
			name := "job " + verb
			if !Registered(name) {
				t.Errorf("not registered: %s", name)
			}
			if _, exists := table[name]; exists {
				t.Error("job has startup registration")
			}
			if _, exists := argparse.Specs[name]; !exists {
				t.Errorf("missing spec")
			}
			for _, flag := range []string{"--help", "--definitely-not-a-flag"} {
				var out, err bytes.Buffer
				code := Execute(context.Background(), "crw relay", []string{"job", verb, flag}, &out, &err)
				if flag == "--help" {
					if code != 0 || err.Len() != 0 || !strings.HasPrefix(out.String(), "usage: crw relay "+name) {
						t.Errorf("help %d %q %q", code, out.String(), err.String())
					}
				} else if code != 2 || out.Len() != 0 || !strings.Contains(err.String(), "unrecognized arguments") {
					t.Errorf("flag %d %q %q", code, out.String(), err.String())
				}
			}
		})
	}
}

func TestJobCLIEnvelopeAndNoRelayStore(t *testing.T) {
	ws := t.TempDir()
	t.Chdir(ws)
	state := filepath.Join(ws, "relay-state")
	for _, c := range []struct {
		args []string
		code int
		out  any
	}{
		{[]string{"list"}, 0, "백그라운드 작업 없음"}, {[]string{"list", "--json"}, 0, []any{}},
		{[]string{"cancel", "missing"}, 0, "없는 id: missing"}, {[]string{"get", "missing"}, 1, "없는 id: missing"},
	} {
		var out, err bytes.Buffer
		code := Execute(context.Background(), "crw relay", append([]string{"--state", state, "job"}, c.args...), &out, &err)
		var payload map[string]any
		if code != c.code || err.Len() != 0 || json.Unmarshal(out.Bytes(), &payload) != nil {
			t.Errorf("%v: %d %q %q", c.args, code, out.String(), err.String())
			continue
		}
		b, _ := json.Marshal(payload["out"])
		want, _ := json.Marshal(c.out)
		if string(b) != string(want) {
			t.Errorf("%v: %s", c.args, b)
		}
	}
	if _, err := os.Stat(filepath.Join(state, "relay.sqlite3")); !os.IsNotExist(err) {
		t.Errorf("selected relay store: %v", err)
	}
	for _, args := range [][]string{{"job"}, {"job", "nope"}, {"job", "get"}, {"job", "cancel"}, {"job", "run"}, {"job", "run", "--"}, {"job", "drain"}} {
		var out, err bytes.Buffer
		if code := Execute(context.Background(), "crw relay", args, &out, &err); code != 2 || out.Len() != 0 {
			t.Errorf("invalid %v: %d %q", args, code, out.String())
		}
	}
}
