package hook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// Ported from comment-lint.test.ts:12-150; expectations come from the Node oracle.
func TestLintApplyPatch(t *testing.T) {
	prose := "which is the reason to treat it as the test rather than as any of them" // justified: oracle prose fixture
	cast := "const v = foo as any;"                                                   // justified: oracle code fixture
	cases := []struct {
		name, patch string
		ok          bool
	}{
		{"cast", "+++ b/x.ts\n+" + cast, false},
		{"justified", "+" + cast + " // justified: third-party untyped", true},
		{"hash justified", "+eval(x); # JUSTIFIED:", true}, // justified: oracle fixture
		{"clean", "+++ b/x.ts\n+const x = 1;", true},
		{"eval", "+eval(userInput);", false}, // justified: oracle fixture
		{"debugger", "+debugger;", false},    // justified: oracle fixture
		{"only added", "+++ b/x.ts\n-" + cast + "\n " + cast + "\n+const v: Foo = foo;", true},
		{"markdown", "*** Add File: docs/report.md\n+" + prose, true},
		{"fenced report", "*** Update File: docs/report.md\n+> " + prose + "\n+```ts\n+" + cast + "\n+```", true},
		{"source", "*** Update File: src/x.ts\n+" + cast, false},
		{"mixed target reset", "*** Add File: docs/report.md\n+" + prose + "\n*** Update File: src/x.ts\n+" + cast, false},
		{"temporary source", "*** Add File: /tmp/example.ts\n+" + cast, false},
		{"move prose", "*** Move to: docs/moved.md\n+" + prose, true},
		{"move source", "*** Move to: src/moved.ts\n+" + cast, false},
		{"unknown target", "+" + cast, false},
		{"CRLF prose", "*** Add File: docs/report.md\r\n+" + prose + "\r\n", true},
		{"CRLF source", "*** Add File: src/x.ts\r\n+" + cast + "\r\n", false},
		{"deleted prose", "*** Delete File: gone.md\n+" + cast, true},
		{"dev null unknown", "+++ /dev/null\n+" + cast, false},
		{"triple plus content skipped", "*** Add File: x.ts\n+++debugger;", true}, // justified: kept oracle blind spot
		{"string literal false positive", "+const x = \"debugger\";", false},      // justified: kept oracle false positive
		{"word boundary", "+debuggers; evaluate(x); as anything", true},
		{"long s not justified", "+debugger; // juſtified:", false}, // justified: JS non-Unicode case fold fixture
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hook.LintApplyPatch(tc.patch)
			if got.OK != tc.ok || (!got.OK && got.Reason == "") {
				t.Fatalf("got %#v, want ok=%v", got, tc.ok)
			}
		})
	}
	for _, name := range []string{"notes.txt", "NOTES.TXT", "a.markdown", "b.mdx", "c.rst", "d.adoc"} {
		if !hook.LintApplyPatch("*** Add File: " + name + "\n+" + prose).OK {
			t.Fatal(name)
		}
	}
	for _, input := range []any{nil, 7, true, map[string]any{}, ""} {
		if !hook.LintApplyPatch(input).OK {
			t.Fatalf("non-command %v denied", input)
		}
	}
	patterns := hook.ForbiddenPatterns()
	if len(patterns) != 3 {
		t.Fatal(patterns)
	}
	for _, p := range patterns {
		if p.Re == nil || p.Msg == "" {
			t.Fatal(p)
		}
	}
}

func TestAddedRecordsAndLines(t *testing.T) {
	patch := "+++ b/x.ts\r\n+alpha\r\n unchanged\r\n-removed\r\n*** Add File: docs/a.md\n+beta\n*** Move to: src/b.ts\n+gamma\n+++\n+delta\n+ +header"
	x, a, b := "x.ts", "docs/a.md", "src/b.ts"
	want := []hook.AddedRecord{{Line: "alpha", Target: &x}, {Line: "beta", Target: &a}, {Line: "gamma", Target: &b}, {Line: "delta"}}
	if got := hook.AddedRecords(patch); !reflect.DeepEqual(got, want) {
		t.Fatalf("%#v != %#v", got, want)
	}
	if got := hook.AddedLines(patch); !reflect.DeepEqual(got, []string{"alpha", "beta", "gamma", "delta"}) {
		t.Fatal(got)
	}
	if got := hook.AddedLines(""); len(got) != 0 {
		t.Fatal(got)
	}
}

func lintPayload(event, tool string, input any) string {
	b, err := json.Marshal(map[string]any{"hook_event_name": event, "tool_name": tool, "tool_input": input})
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestHandleApplyPatchLint(t *testing.T) {
	for _, tool := range []string{"apply_patch", "Write", "Edit"} {
		raw := lintPayload("PreToolUse", tool, map[string]any{"command": "+++ b/x.ts\n+debugger;"})                                                                                                                                                                   // justified: oracle fixture
		want := "{\"hookSpecificOutput\":{\"hookEventName\":\"PreToolUse\",\"permissionDecision\":\"deny\",\"permissionDecisionReason\":\"[crw comment-lint] `debugger` statement — remove before committing or add `// justified: <reason>` (line: debugger;)\"}}\n" // justified: exact oracle envelope
		if got := hook.HandleApplyPatchLint("\ufeff " + raw + "\r\n"); got != want {
			t.Fatalf("%q != %q", got, want)
		}
	}
	for _, raw := range []string{
		"{not json", "null", "[]", "1", "{}", "{} {}",
		lintPayload("Stop", "apply_patch", map[string]any{"command": "+debugger;"}),        // justified: oracle fixture
		lintPayload("PreToolUse", "exec_command", map[string]any{"command": "+debugger;"}), // justified: oracle fixture
		lintPayload("PreToolUse", "Write", map[string]any{"content": "debugger;"}),         // justified: kept native shape blind spot
		lintPayload("PreToolUse", "Edit", map[string]any{"new_string": "debugger;"}),       // justified: kept native shape blind spot
		lintPayload("PreToolUse", "apply_patch", map[string]any{"command": "+ok();"}),
		lintPayload("PreToolUse", "apply_patch", map[string]any{"command": "*** Add File: report.md\n+debugger;"}), // justified: prose fixture
		lintPayload("PreToolUse", "apply_patch", map[string]any{"command": 7}),
	} {
		if got := hook.HandleApplyPatchLint(raw); got != "" {
			t.Fatalf("%q answered %q", raw, got)
		}
	}
	raw := `{"hook_event_name":"PreToolUse","tool_name":"apply_patch","extra":1e400,"tool_input":{"command":"+debugger;"}}` // justified: JSON number fixture
	if hook.HandleApplyPatchLint(raw) == "" {
		t.Fatal("unrelated large number discarded payload")
	}
}

func TestLintJavaScriptTextSemantics(t *testing.T) {
	for _, space := range []string{"\t", "\v", "\f", "\r", " ", "\u00a0", "\u1680", "\u2000", "\u2028", "\u2029", "\u202f", "\u205f", "\u3000", "\ufeff"} {
		if hook.LintApplyPatch("+eval" + space + "(x)").OK { // justified: regex whitespace fixture
			t.Fatalf("eval whitespace %q missed", space)
		}
		if !hook.LintApplyPatch("*** Add File:" + space + "report.md\n+debugger;").OK { // justified: header whitespace fixture
			t.Fatalf("header whitespace %q missed", space)
		}
	}
	if !hook.LintApplyPatch("+eval\u0085(x)").OK { // justified: JS excludes NEL
		t.Fatal("NEL treated as JS whitespace")
	}
	got := hook.LintApplyPatch("+  debugger;\b\f inner\u2028x  ")             // justified: trim/JSON fixture
	if !strings.HasSuffix(got.Reason, "(line: debugger;\b\f inner\u2028x)") { // justified: exact oracle preview
		t.Fatal(got.Reason)
	}
	out := hook.HandleApplyPatchLint(lintPayload("PreToolUse", "apply_patch", map[string]any{"command": "+debugger;\b\f inner\u2028x"})) // justified: exact escapes
	if !strings.Contains(out, `debugger;\b\f inner`+"\u2028x") || strings.Contains(out, `\u2028`) {                                      // justified: oracle serialization
		t.Fatal(out)
	}
	preview := strings.Repeat("😀", 60)
	if got := hook.LintApplyPatch("+" + preview + "debugger;"); !strings.HasSuffix(got.Reason, "(line: "+preview+")") { // justified: UTF-16 preview fixture
		t.Fatal(got.Reason)
	}
	if got := hook.LintApplyPatch("+eval(x);\n+debugger;"); !strings.HasPrefix(got.Reason, "`eval(`") { // justified: first finding order
		t.Fatal(got)
	}
}

func TestEditLegLintPrecedenceAndPabcdOff(t *testing.T) {
	cwd := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("CODEX_SQLITE_HOME", t.TempDir())
	t.Setenv("CRW_HOME", t.TempDir())
	t.Setenv("CRW_BIN", "crw")
	s := state.DefaultState("s1", "")
	s.LoopArmSeen = true
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ patch, mode, decision string }{
		{"+debugger;", "1", "deny"}, // justified: precedence fixture
		{"+ok();", "0", ""},
		{"+debugger;", "0", "deny"}, // justified: PABCD-off fixture
		{"+ok();", "1", "allow"},
	} {
		t.Setenv("CRW_PABCD", tc.mode)
		b, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "apply_patch", "session_id": "s1", "cwd": cwd, "tool_input": map[string]any{"command": tc.patch}})
		var out, stderr bytes.Buffer
		code := harness.Hook(context.Background(), []string{"pre-tool-use", "--leg", "pre-tool-use-linting-apply-patch"}, strings.NewReader(string(b)), &out, &stderr, os.LookupEnv, harness.Legs())
		if code != 0 || stderr.Len() != 0 || (tc.decision == "" && out.Len() != 0) || (tc.decision != "" && !strings.Contains(out.String(), `"permissionDecision":"`+tc.decision+`"`)) {
			t.Fatalf("code=%d out=%q err=%q", code, out.String(), stderr.String())
		}
	}
	if got := state.ReadState(cwd, "s1").IdleEditNudges; got != 1 {
		t.Fatalf("lint denies and disabled advisory mutated count: %v", got)
	}
}

func TestLintJSONDepthPlatformLimit(t *testing.T) {
	for _, depth := range []int{9999, 10001} {
		raw := `{"hook_event_name":"PreToolUse","tool_name":"apply_patch","tool_input":{"command":"+debugger;"},"extra":` + strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth) + "}" // justified: recorded decoder-limit fixture
		out := hook.HandleApplyPatchLint(raw)
		if (out == "") != (depth == 10001) {
			t.Fatalf("depth %d returned %q", depth, out)
		}
	}
}
