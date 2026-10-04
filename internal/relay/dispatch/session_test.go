package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestSessionCLIContract(t *testing.T) {
	for _, verb := range []string{"current", "bind", "source"} {
		t.Run(verb, func(t *testing.T) {
			name := "session " + verb
			if !Registered(name) {
				t.Errorf("not registered: %s", name)
			}
			if _, exists := table[name]; exists {
				t.Error("session has startup registration")
			}
			for _, flag := range []string{"--help", "--definitely-not-a-flag"} {
				var out, err bytes.Buffer
				code := Execute(context.Background(), "crw relay", []string{"session", verb, flag}, &out, &err)
				if flag == "--help" {
					if code != 0 || err.Len() != 0 || !strings.HasPrefix(out.String(), "usage: crw relay "+name) {
						t.Errorf("help: %d %q %q", code, out.String(), err.String())
					}
				} else if code != 2 || out.Len() != 0 || !strings.Contains(err.String(), "unrecognized arguments") {
					t.Errorf("bad flag: %d %q %q", code, out.String(), err.String())
				}
			}
		})
	}
	for _, line := range [][]string{{"session"}, {"session", "other"}, {"session", "source"}, {"session", "source", "/synthetic", "--bad"}, {"session", "current", "extra"}} {
		var out, err bytes.Buffer
		if code := Execute(context.Background(), "crw relay", line, &out, &err); code != 2 || out.Len() != 0 || !strings.Contains(err.String(), "usage: crw relay session") {
			t.Errorf("%v: %d %q %q", line, code, out.String(), err.String())
		}
	}
}

func TestSessionFailureKeepsOracleAnswerWithoutSelectingRelayStore(t *testing.T) {
	t.Setenv("CODEX_THREAD_ID", "")
	if err := os.Unsetenv("CODEX_THREAD_ID"); err != nil {
		t.Fatal(err)
	}
	var out, err bytes.Buffer
	code := Execute(context.Background(), "crw relay", []string{"--state", "~user_that_does_not_exist_crw520/state", "session", "current", "--json"}, &out, &err)
	var body struct {
		Out struct {
			OK, HooksVerified bool
			Error             string
		}
	}
	if code != 1 || err.Len() != 0 || json.Unmarshal(out.Bytes(), &body) != nil || body.Out.OK || body.Out.HooksVerified || body.Out.Error != "CODEX_THREAD_ID is absent. Run this command inside the native Codex session." {
		t.Fatalf("exit=%d out=%q err=%q", code, out.String(), err.String())
	}
}
