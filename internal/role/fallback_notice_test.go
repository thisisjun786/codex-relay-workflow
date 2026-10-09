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

// CRW-1146: a resumed session is not given the notice again when it was given exactly this notice; startup, compact, clear, no
// source and an unknown source answer it as before, and so does a resume the session was never given this notice for.
func TestFallbackNoticeHookResumeRepeatsOnlyWhatChanged(t *testing.T) {
	m, env := fallbackTestEnv(t)
	run := func(raw string) string {
		var out strings.Builder
		observed := 0
		code := RunFallbackNoticeHook(context.Background(), strings.NewReader(raw), &out, env, func(data []byte) string { observed++; return string(data) })
		if code != 0 || observed != 1 {
			t.Fatalf("exit %d, observed %d", code, observed)
		}
		return out.String()
	}
	resume := `{"session_id":"s","source":"resume"}`
	// A session that was never given the notice (the leg was switched off at its start, or it started before records) hears it.
	want := run(`{"session_id":"s"}`)
	if want == "" {
		t.Fatal("baseline notice is empty")
	}
	if got := run(`{"session_id":"never-given","source":"resume"}`); got != want {
		t.Errorf("resume of a session never given the notice answered %q", got)
	}
	for _, source := range []string{`"startup"`, `"compact"`, `"clear"`, `""`, `"future"`, `7`, `null`, `"Resume"`} {
		if got := run(`{"session_id":"s","source":` + source + `}`); got != want {
			t.Errorf("source %s changed the notice: %q", source, got)
		}
	}
	// The same notice is not repeated on a resume, whatever other session started since.
	if got := run(resume); got != "" {
		t.Errorf("resume of a session given this notice answered %q", got)
	}
	run(`{"session_id":"other"}`)
	if got := run(resume); got != "" {
		t.Errorf("resume after another session started answered %q", got)
	}
	// A first fallback configured while the session was closed is announced when it resumes, once.
	store := `{"roles":{"reviewer":{"fallback":{"model":"provider/fallback"}}}}`
	if err := os.WriteFile(filepath.Join(m["CRW_HOME"], StoreFile), []byte(store), 0600); err != nil {
		t.Fatal(err)
	}
	got := run(resume)
	if !strings.Contains(got, "First fallback configured for reviewer.") {
		t.Fatalf("resume after a fallback was configured answered %q", got)
	}
	if again := run(resume); again != "" {
		t.Errorf("the changed notice was repeated on the next resume: %q", again)
	}
	// So does a changed dispatch card (CRW_SPAWN_V1 is read when the card is rendered).
	m["CRW_SPAWN_V1"] = "1"
	if card := run(resume); card == "" {
		t.Error("resume after the host card changed answered nothing")
	}
	// A compact empties the context: it answers whatever the record says.
	if got := run(`{"session_id":"s","source":"compact"}`); got == "" {
		t.Error("compact answered nothing")
	}
}

// A session id the record cannot key (none, or one with a space) never reads as given: the resume answers.
func TestFallbackNoticeHookResumeWithoutAUsableSessionAnswers(t *testing.T) {
	_, env := fallbackTestEnv(t)
	for _, id := range []string{``, `"s s"`} {
		raw := `{"source":"resume"}`
		if id != "" {
			raw = `{"session_id":` + id + `,"source":"resume"}`
		}
		for range 2 {
			var out strings.Builder
			RunFallbackNoticeHook(context.Background(), strings.NewReader(raw), &out, env, func(data []byte) string { return string(data) })
			if out.String() == "" {
				t.Errorf("resume %s answered nothing", raw)
			}
		}
	}
}
