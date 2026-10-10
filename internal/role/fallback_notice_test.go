package role

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFallbackNoticeOracleAndRepeatedStartup(t *testing.T) {
	var cases []struct {
		Name  string
		Roles json.RawMessage
		Want  string
	}
	fallbackFixture(t, "notices.json", &cases)
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			m, env := fallbackTestEnv(t)
			b := append(append([]byte(`{"roles":`), c.Roles...), '}')
			if err := os.WriteFile(filepath.Join(m["CRW_HOME"], StoreFile), b, 0600); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				got, err := SessionFallbackNotice(env)
				if err != nil || got != c.Want {
					t.Fatalf("notice differs: %v %q", err, got)
				}
			}
		})
	}
	if DispatchGuidance != string(fallbackFixture(t, "guidance.golden", nil)) {
		t.Fatal("guidance differs from oracle")
	}
}

func TestFallbackNoticeUTF16BudgetAndRenderFailure(t *testing.T) {
	_, env := fallbackTestEnv(t)
	for _, s := range []string{strings.Repeat("x", 4096), strings.Repeat("😀", 2048)} {
		if _, err := fallbackSessionNotice(env, func() (string, error) { return s, nil }); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []string{strings.Repeat("x", 4097), strings.Repeat("😀", 2048) + "x"} {
		if _, err := fallbackSessionNotice(env, func() (string, error) { return s, nil }); err == nil || err.Error() != "SessionStart dispatch context exceeds 4096 characters" {
			t.Fatalf("budget: %v", err)
		}
	}
	fault := errors.New("render failure")
	if _, err := fallbackSessionNotice(env, func() (string, error) { return "", fault }); !errors.Is(err, fault) {
		t.Fatal("render failure lost")
	}
}

// TestFallbackNoticeLeafAndNonObjectStartups pins CRW-1130: the notice is a root-session card. A child named by agent_id or by
// agent_type is a leaf and receives none, and a startup that is not an object carrying a session identity receives none.
func TestFallbackNoticeLeafAndNonObjectStartups(t *testing.T) {
	cases := []struct {
		name, input string
		card        bool
	}{
		{"root", `{"session_id":"s1","hook_event_name":"SessionStart"}`, true},
		{"agent_type only is a leaf", `{"session_id":"s1","agent_type":"executor"}`, false},
		{"agent_id is a leaf", `{"session_id":"s1","agent_id":"child"}`, false},
		{"agent_id and agent_type is a leaf", `{"session_id":"s1","agent_id":"c","agent_type":"executor"}`, false},
		{"empty agent_type is not a child", `{"session_id":"s1","agent_type":""}`, true},
		{"array", `[]`, false},
		{"boolean", `true`, false},
		{"string", `"x"`, false},
		{"number", `7`, false},
		{"null", `null`, false},
		{"object without a session identity", `{}`, false},
		{"empty session id", `{"session_id":""}`, false},
		{"numeric session id", `{"session_id":7}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, env := fallbackTestEnv(t)
			var out strings.Builder
			code := RunFallbackNoticeHook(context.Background(), strings.NewReader(c.input), &out, env, func(b []byte) string { return string(b) })
			if code != 0 {
				t.Fatalf("exit %d", code)
			}
			if got := out.Len() > 0; got != c.card {
				t.Fatalf("card=%v want %v: %q", got, c.card, out.String())
			}
		})
	}
}
