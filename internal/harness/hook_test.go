package harness

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// probe records which legs' handlers ran; every leg of the real table gets one.
type probe struct{ ran []string }

func (p *probe) legs(out string) []Leg {
	legs := Legs()
	for i := range legs {
		id := legs[i].ID
		legs[i].Handle = func(Call) string { p.ran = append(p.ran, id); return out }
	}
	return legs
}

// only is the table's row for id with the handler given, which a scenario runs alone.
func only(id string, handle Handler) []Leg {
	for _, l := range Legs() {
		if l.ID == id {
			l.Handle = handle
			return []Leg{l}
		}
	}
	panic("no leg " + id)
}

func hook(legs []Leg, args []string, stdin string, env map[string]string) (code int, out, errOut string) {
	var o, e strings.Builder
	code = Hook(context.Background(), args, strings.NewReader(stdin), &o, &e, lookup(env), legs)
	return code, o.String(), e.String()
}

func argsOf(l Leg) []string { return []string{l.Event, "--leg", l.ID} }

func payload(event, cwd string, extra string) string {
	return `{"hook_event_name":"` + event + `","session_id":"s1","cwd":"` + cwd + `"` + extra + `}`
}

func TestOverflowAnswersBeforeAnythingElse(t *testing.T) {
	env, home, _ := hookEnv(t)
	big := strings.Repeat("x", MaxStdinBytes+1)
	want := oversizedWant()
	p := &probe{}
	for _, l := range p.legs("never") {
		code, out, errOut := hook([]Leg{l}, argsOf(l), big, env)
		wantOut, wantCode := want[l.Slug], 0
		if wantOut == "" && l.Stage != Permission {
			wantCode = 1
		}
		if code != wantCode || out != wantOut || errOut != "" {
			t.Errorf("%s (%s): %d %q %q, want %d %q", l.ID, l.Slug, code, out, errOut, wantCode, wantOut)
		}
	}
	if len(p.ran) != 0 || len(records(t, home)) != 0 {
		t.Errorf("an oversized input reached a handler or the record: %v", p.ran)
	}
	// A payload of exactly the limit is not oversized.
	if code, _, _ := hook(p.legs(""), []string{"stop", "--leg", "stop-checking-pabcd-continuation"}, strings.Repeat(" ", MaxStdinBytes), env); code != 0 || len(p.ran) != 1 {
		t.Errorf("an input of exactly %d bytes: code %d, ran %v", MaxStdinBytes, code, p.ran)
	}
}

func TestPermissionLegsRunBeforeTheRecord(t *testing.T) {
	env, home, _ := hookEnv(t)
	for _, id := range []string{"permission-request-allowing-agent-thread", "session-start-advising-agent-thread-permissions"} {
		var seen []Call
		legs := only(id, func(c Call) string { seen = append(seen, c); return "answer" })
		code, out, _ := hook(legs, argsOf(legs[0]), payload("X", t.TempDir(), `,"agent_id":"a1"`), env)
		if code != 0 || out != "answer" || len(seen) != 1 || seen[0].PabcdEnabled {
			t.Errorf("%s: %d %q %v (a subagent payload is not skipped here, and the project file is not read)", id, code, out, seen)
		}
		legs = only(id, func(Call) string { panic("boom") })
		if code, out, errOut := hook(legs, argsOf(legs[0]), payload("X", "/w", ""), env); code != 0 || out != "" || errOut != "" {
			t.Errorf("%s: a panic is swallowed, got %d %q %q", id, code, out, errOut)
		}
	}
	if len(records(t, home)) != 0 {
		t.Error("a permission event wrote an observation record")
	}
}

func TestStagesAfterTheRecord(t *testing.T) {
	cwd := t.TempDir()
	for _, c := range []struct {
		id         string
		wantCode   int
		wantStderr string
	}{
		{"pre-tool-use-guarding-managed-worktree-deletion", 0, ""},                  // Guard, recovers
		{"pre-tool-use-guarding-memory-write", 0, ""},                               // Guard, recovers
		{"pre-tool-use-guarding-automation-ownership", 1, "crw cli failed: boom\n"}, // Guard, does not recover
		{"pre-tool-use-guarding-goal-budget", 1, "crw cli failed: boom\n"},          // FailClosed
		{"stop-checking-pabcd-continuation", 0, ""},                                 // Generic
		{"pre-tool-use-linting-apply-patch", 0, ""},                                 // Generic
	} {
		env, home, _ := hookEnv(t)
		var recordedAtCall int
		legs := only(c.id, func(Call) string { recordedAtCall = len(records(t, home)); panic(errors.New("boom")) })
		code, out, errOut := hook(legs, argsOf(legs[0]), payload("X", cwd, ""), env)
		if code != c.wantCode || out != "" || errOut != c.wantStderr || recordedAtCall != 1 {
			t.Errorf("%s: %d %q %q (records at call %d), want %d %q", c.id, code, out, errOut, recordedAtCall, c.wantCode, c.wantStderr)
		}
	}
}

// cli.ts records the verb it dispatches on, not the registration that named it.
func TestTheRecordNamesTheSlugNotTheLeg(t *testing.T) {
	env, home, _ := hookEnv(t)
	legs := only("pre-tool-use-guarding-managed-worktree-deletion", nil)
	if code, _, _ := hook(legs, argsOf(legs[0]), payload("X", t.TempDir(), ""), env); code != 0 {
		t.Fatalf("exit %d", code)
	}
	files := records(t, home)
	if len(files) != 1 {
		t.Fatalf("records: %v", files)
	}
	got, _ := os.ReadFile(files[0])
	if !strings.Contains(string(got), `"component":"pabcd-state","event":"worktree-guard-pretool"`) {
		t.Errorf("the record names %s", got)
	}
}

func TestSubagentExitAndPabcdGate(t *testing.T) {
	cwd := t.TempDir()
	off := t.TempDir()
	if err := os.WriteFile(filepath.Join(off, "crw.json"), []byte(`{"pabcd":{"enabled":false}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := `,"agent_id":"a1","agent_type":"worker"`
	for _, c := range []struct {
		name, cwd, extra string
		skipped          []string // the legs whose handler must not run
	}{
		{"root, enabled", cwd, "", nil},
		{"subagent", cwd, sub, []string{"session-start-bootstrapping-pabcd-state", "user-prompt-submit-checking-pabcd-trigger", "stop-checking-pabcd-continuation",
			"pre-tool-use-guarding-goal-budget", "pre-tool-use-guarding-interview-in-goal", "pre-tool-use-guarding-goal-complete", "post-tool-use-capturing-interview-answers",
			"post-compact-resetting-reinject-cursor", "pre-tool-use-linting-apply-patch", "post-tool-use-tracking-render-observations",
			"session-start-detecting-managed-worktree", "user-prompt-submit-guiding-worktree-rename"}},
		{"pabcd off", off, "", []string{"session-start-bootstrapping-pabcd-state", "stop-checking-pabcd-continuation", "post-tool-use-capturing-interview-answers",
			"subagent-stop-verifying-evidence", "subagent-stop-observing-review", "post-compact-resetting-reinject-cursor", "post-tool-use-tracking-render-observations"}},
	} {
		env, _, _ := hookEnv(t)
		for _, l := range Legs() {
			var ran []bool
			legs := only(l.ID, func(c Call) string { ran = append(ran, c.PabcdEnabled); return "" })
			code, out, errOut := hook(legs, argsOf(l), payload("X", c.cwd, c.extra), env)
			if code != 0 || out != "" || errOut != "" || (len(ran) == 1) == slices.Contains(c.skipped, l.ID) {
				t.Errorf("%s / %s: code %d out %q err %q ran %v", c.name, l.ID, code, out, errOut, ran)
				continue
			}
			// PabcdEnabled is read after the subagent exit: a Permission or Guard leg never sees it set.
			if want := (l.Stage == Generic || l.Stage == FailClosed) && c.cwd != off; len(ran) == 1 && ran[0] != want {
				t.Errorf("%s / %s: PabcdEnabled %v, want %v", c.name, l.ID, ran[0], want)
			}
		}
	}
}

// codex-rs stamps agent_id or agent_type, either one makes a turn a subagent's; the record is made
// before the exit, and a stamp without an identity is not a root record.
func TestEitherSubagentStampEndsAGenericLeg(t *testing.T) {
	for _, c := range []struct {
		extra   string
		records int
	}{{`,"agent_id":"a1"`, 1}, {`,"agent_type":"worker"`, 0}, {`,"agent_id":"a1","agent_type":"worker"`, 1}} {
		env, home, _ := hookEnv(t)
		p := &probe{}
		for _, id := range []string{"stop-checking-pabcd-continuation", "pre-tool-use-guarding-memory-write", "subagent-stop-verifying-evidence"} {
			var legs []Leg
			for _, l := range p.legs("") {
				if l.ID == id {
					legs = []Leg{l}
				}
			}
			hook(legs, argsOf(legs[0]), payload("X", t.TempDir(), c.extra), env)
		}
		if want := []string{"pre-tool-use-guarding-memory-write", "subagent-stop-verifying-evidence"}; !slices.Equal(p.ran, want) {
			t.Errorf("%s: ran %v, want %v", c.extra, p.ran, want)
		}
		// The three legs record under three events, so a root-capable stamp leaves three files.
		if got := len(records(t, home)); got != 3*c.records {
			t.Errorf("%s: %d records, want %d", c.extra, got, 3*c.records)
		}
	}
}

// The cwd the PABCD check reads is JSON.parse(raw).cwd when that is a non-empty string, else the
// process's: 1e400 parses (as Infinity), trailing data, a BOM and a non-object do not.
func TestThePabcdCheckReadsTheCwdAsJSONParseDoes(t *testing.T) {
	off, on := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(off, "crw.json"), []byte(`{"pabcd":{"enabled":false}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	enc := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	for _, c := range []struct {
		name, raw string
		process   string // the process's directory
		enabled   bool
	}{
		{"payload cwd", `{"cwd":` + enc(off) + `}`, on, false},
		{"no cwd: the process's", `{}`, off, false},
		{"empty cwd: the process's", `{"cwd":""}`, off, false},
		{"cwd not a string: the process's", `{"cwd":7}`, off, false},
		{"a number no float64 holds elsewhere in the document", `{"n":1e400,"cwd":` + enc(off) + `}`, on, false},
		{"trailing data is not a document", `{"cwd":` + enc(off) + `} {}`, on, true},
		{"a BOM is not whitespace to JSON.parse", "\ufeff" + `{"cwd":` + enc(off) + `}`, on, true},
		{"an array has no cwd", `[{"cwd":` + enc(off) + `}]`, on, true},
		{"null has no cwd", `null`, on, true},
	} {
		t.Chdir(c.process)
		env, _, _ := hookEnv(t)
		var got *bool
		legs := only("user-prompt-submit-checking-pabcd-trigger", func(x Call) string { got = &x.PabcdEnabled; return "" })
		hook(legs, argsOf(legs[0]), c.raw, env)
		if got == nil || *got != c.enabled {
			t.Errorf("%s: PabcdEnabled %v, want %v", c.name, got, c.enabled)
		}
	}
}

// Node's default SIGINT ends the oracle's hook at once, even while it waits for its input. crw's first
// interrupt only cancels the run (cmd/crw serve), so a hook whose input stays open has to stop on it.
func TestAnInterruptEndsAHookWaitingForItsInput(t *testing.T) {
	env, home, _ := hookEnv(t)
	in, hold := io.Pipe()
	defer hold.Close()
	p := &probe{}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	got := make(chan int, 1)
	var out, errOut strings.Builder
	go func() {
		got <- Hook(ctx, []string{"stop", "--leg", "stop-checking-pabcd-continuation"}, in, &out, &errOut, lookup(env), p.legs("never"))
	}()
	select {
	case code := <-got:
		if code != Interrupted || out.Len() != 0 || errOut.Len() != 0 || len(p.ran) != 0 || len(records(t, home)) != 0 {
			t.Errorf("interrupted: %d %q %q, ran %v", code, out.String(), errOut.String(), p.ran)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an interrupted hook is still waiting for its input")
	}
	// Input that arrives after the interrupt finds a hook that has already answered: it is not
	// recorded, no handler runs and nothing is written.
	go func() {
		hold.Write([]byte(payload("Stop", t.TempDir(), "")))
		hold.Close()
	}()
	time.Sleep(200 * time.Millisecond)
	if out.Len() != 0 || errOut.Len() != 0 || len(p.ran) != 0 || len(records(t, home)) != 0 {
		t.Errorf("input after the interrupt: %q %q, ran %v, %d records", out.String(), errOut.String(), p.ran, len(records(t, home)))
	}
}

func TestOutputPassesThroughAndUnclaimedInvocationsReleaseInSilence(t *testing.T) {
	env, home, _ := hookEnv(t)
	p := &probe{}
	legs := p.legs("answer\n")
	if code, out, _ := hook(legs, []string{"user-prompt-submit", "--leg=user-prompt-submit-checking-pabcd-trigger"}, payload("X", t.TempDir(), ""), env); code != 0 || out != "answer\n" {
		t.Errorf("--leg=: %d %q", code, out)
	}
	unread := errorReader{t}
	for _, args := range [][]string{nil, {"stop"}, {"stop", "--leg"}, {"stop", "--leg", "nope"}, {"stop", "--leg", "user-prompt-submit-checking-pabcd-trigger"},
		{"nope", "--leg", "stop-checking-pabcd-continuation"}, {"stop", "--leg", "stop-checking-pabcd-continuation", "extra"}} {
		var o, e strings.Builder
		if code := Hook(context.Background(), args, unread, &o, &e, lookup(env), legs); code != 0 || o.Len() != 0 || e.Len() != 0 {
			t.Errorf("%v: %d %q %q", args, code, o.String(), e.String())
		}
	}
	if len(p.ran) != 1 || len(records(t, home)) != 1 {
		t.Errorf("only the one claimed invocation may run: ran %v", p.ran)
	}
	for args, want := range map[string]bool{"stop": true, "session-start --leg x": true, "--plugin-launch": false, "/tmp/settings.json": false, "nope": false, "": false} {
		if got := ClaimsHook(strings.Fields(args)); got != want {
			t.Errorf("ClaimsHook(%q) = %v", args, got)
		}
	}
}

type errorReader struct{ t *testing.T }

func (r errorReader) Read([]byte) (int, error) {
	r.t.Error("stdin was read")
	return 0, errors.New("unread")
}

// The table mirrors the registrations of the oracle's pabcd-state component, one row each.
func TestLegsMatchTheDeclaredRegistrations(t *testing.T) {
	raw, err := os.ReadFile("../../contract/schema/cxc/hook-declarations.json")
	if err != nil {
		t.Fatal(err)
	}
	var decl struct {
		Legs []struct {
			Leg, Event, Entry string
			Args              []string
		}
	}
	if err := json.Unmarshal(raw, &decl); err != nil {
		t.Fatal(err)
	}
	kebab := func(s string) string {
		return strings.ToLower(regexp.MustCompile("([a-z])([A-Z])").ReplaceAllString(s, "$1-$2"))
	}
	rows := map[string]Leg{}
	for _, l := range Legs() {
		if _, dup := rows[l.ID]; dup {
			t.Errorf("%s is registered twice", l.ID)
		}
		rows[l.ID] = l
	}
	pabcd := 0
	for _, d := range decl.Legs {
		if !strings.HasPrefix(d.Entry, "components/pabcd-state/") {
			if _, ok := rows[d.Leg]; ok {
				t.Errorf("%s belongs to another component", d.Leg)
			}
			continue
		}
		pabcd++
		row, ok := rows[d.Leg]
		if !ok || row.Event != kebab(d.Event) || row.Slug != d.Args[len(d.Args)-1] {
			t.Errorf("%s: row %+v, declaration %s %v", d.Leg, row, d.Event, d.Args)
		}
	}
	if pabcd != len(rows) {
		t.Errorf("%d rows for %d declared pabcd-state legs", len(rows), pabcd)
	}
	stage := map[string]Leg{}
	for _, l := range rows {
		if o, ok := stage[l.Slug]; ok && (o.Stage != l.Stage || o.Recover != l.Recover || o.SubagentExempt != l.SubagentExempt || o.Gated != l.Gated) {
			t.Errorf("legs of slug %s disagree: %+v and %+v", l.Slug, o, l)
		}
		stage[l.Slug] = l
	}
	if len(stage) != len(oversizedWant()) {
		t.Errorf("%d slugs, want %d", len(stage), len(oversizedWant()))
	}
}

func TestPabcdVerbs(t *testing.T) {
	var got []string
	verbs := []Verb{{"orchestrate", func(args []string, _ io.Reader, out, _ io.Writer) int {
		got = args
		io.WriteString(out, "ran\n")
		return 7
	}}, {"freeze", nil}}
	for _, c := range []struct {
		args           []string
		code           int
		stdout, stderr string
	}{
		{[]string{"orchestrate", "P", "--session", "s"}, 7, "ran\n", ""},
		{[]string{"-h"}, 0, "usage: crw pabcd [-h] {orchestrate,freeze} ...\n", ""},
		{[]string{"--help"}, 0, "usage: crw pabcd [-h] {orchestrate,freeze} ...\n", ""},
		{nil, 2, "", "usage: crw pabcd [-h] {orchestrate,freeze} ...\ncrw pabcd: error: the following arguments are required: verb\n"},
		{[]string{"nope"}, 2, "", "usage: crw pabcd [-h] {orchestrate,freeze} ...\ncrw pabcd: error: argument verb: invalid choice: \"nope\" (choose from 'orchestrate', 'freeze')\n"},
		{[]string{"freeze"}, 2, "", "usage: crw pabcd [-h] {orchestrate,freeze} ...\ncrw pabcd: error: argument verb: invalid choice: \"freeze\" (choose from 'orchestrate', 'freeze')\n"},
	} {
		var o, e strings.Builder
		if code := Pabcd(c.args, strings.NewReader(""), &o, &e, verbs); code != c.code || o.String() != c.stdout || e.String() != c.stderr {
			t.Errorf("%v: %d %q %q", c.args, code, o.String(), e.String())
		}
	}
	if !slices.Equal(got, []string{"P", "--session", "s"}) {
		t.Errorf("the verb got %v", got)
	}
}
