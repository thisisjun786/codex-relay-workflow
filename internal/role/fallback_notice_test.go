package role

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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

// A session id the record cannot key (one with a space or a control character) never reads as given: the resume answers. A payload with no
// session identity gets no card at all (CRW-1130, TestFallbackNoticeLeafAndNonObjectStartups).
func TestFallbackNoticeHookResumeWithoutAUsableSessionAnswers(t *testing.T) {
	_, env := fallbackTestEnv(t)
	for _, id := range []string{`"s s"`, `"s\u0007"`} {
		raw := `{"session_id":` + id + `,"source":"resume"}`
		for range 2 {
			var out strings.Builder
			RunFallbackNoticeHook(context.Background(), strings.NewReader(raw), &out, env, func(data []byte) string { return string(data) })
			if out.String() == "" {
				t.Errorf("resume %s answered nothing", raw)
			}
		}
	}
}

// limitedWriter takes at most n bytes in all, then fails: a closed pipe (n = 0) or a short write.
type limitedWriter struct{ n int }

func (w *limitedWriter) Write(p []byte) (int, error) {
	if len(p) <= w.n {
		w.n -= len(p)
		return len(p), nil
	}
	k := w.n
	w.n = 0
	return k, errors.New("write failed")
}

// CRW-1146: the notice counts as given only when the hook wrote all of it. A start or resume whose notice could not be written
// (a closed pipe, a short write) leaves no record, so the next resume hears the notice, and only then is it not repeated.
func TestFallbackNoticeHookRecordsOnlyANoticeItWrote(t *testing.T) {
	_, env := fallbackTestEnv(t)
	run := func(raw string, out io.Writer) {
		if code := RunFallbackNoticeHook(context.Background(), strings.NewReader(raw), out, env, func(data []byte) string { return string(data) }); code != 0 {
			t.Fatalf("exit %d", code)
		}
	}
	var want strings.Builder
	run(`{"session_id":"base"}`, &want)
	if want.Len() < 20 {
		t.Fatalf("baseline notice %q", want.String())
	}
	for _, tc := range []struct {
		name, source string
		budget       int
	}{{"resume-closed", "resume", 0}, {"resume-short", "resume", 10}, {"startup-closed", "startup", 0}, {"startup-short", "startup", 10}} {
		resume := `{"session_id":"` + tc.name + `","source":"resume"}`
		run(`{"session_id":"`+tc.name+`","source":"`+tc.source+`"}`, &limitedWriter{n: tc.budget})
		var got strings.Builder
		run(resume, &got)
		if got.String() != want.String() {
			t.Errorf("%s: resume after an unwritten notice answered %q", tc.name, got.String())
		}
		var again strings.Builder
		run(resume, &again)
		if again.String() != "" {
			t.Errorf("%s: the written notice was repeated: %q", tc.name, again.String())
		}
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
