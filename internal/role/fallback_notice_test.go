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

// CRW-1146: the fallback card is static guidance a resumed session already holds from its first start, so source=resume answers
// nothing; startup, compact, clear, no source and an unknown source answer the notice as before.
func TestFallbackNoticeHookSourceResumeIsSilent(t *testing.T) {
	_, env := fallbackTestEnv(t)
	run := func(raw string) string {
		var out strings.Builder
		observed := 0
		code := RunFallbackNoticeHook(context.Background(), strings.NewReader(raw), &out, env, func(data []byte) string { observed++; return string(data) })
		if code != 0 || observed != 1 {
			t.Fatalf("exit %d, observed %d", code, observed)
		}
		return out.String()
	}
	want := run(`{"session_id":"s"}`)
	if want == "" {
		t.Fatal("baseline notice is empty")
	}
	for _, source := range []string{`"startup"`, `"compact"`, `"clear"`, `""`, `"future"`, `7`, `null`, `"Resume"`} {
		if got := run(`{"session_id":"s","source":` + source + `}`); got != want {
			t.Errorf("source %s changed the notice: %q", source, got)
		}
	}
	if got := run(`{"session_id":"s","source":"resume"}`); got != "" {
		t.Errorf("resume answered %q", got)
	}
}
