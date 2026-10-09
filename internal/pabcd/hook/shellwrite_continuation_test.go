package hook

import (
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-851 (line-continuation bypass of the 10-09 re-check, pre-merge evaluation d1 and d2): checks beside the reproduction rows
// of testdata/shellir/rows/21-crw-851-line-continuation.txt that the three-gate verdict of TestReproductionRows cannot make.

// TestCRW851ContinuedCompileNamesItsDestination (d2): a continued builtins.compile is identified by itself. The program only
// compiles a code object and never calls exec on a literal, so the destination the reader names, and the Target the memory gate
// reports, can come from nothing but the compile recognition. The exec(c) variant is unreadable on its own (its argument is no
// literal), so only the named path shows the compile call was read.
func TestCRW851ContinuedCompileNamesItsDestination(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, c := range []struct{ name, program string }{
		{"continuation before the dot", "import builtins; c = builtins \\\n.compile(\"\"\"open(file=\"{MEMORY}/a\",mode=\"w\")\"\"\", \"f\", \"exec\")"},
		{"continuation after the dot", "import builtins; c = builtins.\\\ncompile(\"\"\"open(file=\"{MEMORY}/a\",mode=\"w\")\"\"\", \"f\", \"exec\")"},
		{"continuation before the parenthesis", "import builtins; c = builtins.compile \\\n(\"\"\"open(file=\"{MEMORY}/a\",mode=\"w\")\"\"\", \"f\", \"exec\")"},
		{"__builtins__ with CR LF", "import builtins; c = __builtins__ \\\r\n.compile(\"\"\"open(file=\"{MEMORY}/a\",mode=\"w\")\"\"\", \"f\", \"exec\")"},
		{"parenthesized callee", "import builtins; c = (builtins \\\n.compile)(\"\"\"open(file=\"{MEMORY}/a\",mode=\"w\")\"\"\", \"f\", \"exec\")"},
	} {
		t.Run(c.name, func(t *testing.T) {
			program := strings.ReplaceAll(c.program, "{MEMORY}", root)
			command := "python3 -c '" + program + "'"
			if got := ShellWriteDestinations(command); !slices.Contains(got, root+"/a") {
				t.Errorf("ShellWriteDestinations named %q, want %q: %q", got, root+"/a", command)
			}
			got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env)
			if got.Surface != "shell" || !strings.HasSuffix(got.Target, "/a") || strings.HasPrefix(got.Target, "(") {
				t.Errorf("memoryGateClassify = %+v, want a shell attempt that names %s/a: %q", got, root, command)
			}
		})
	}
	// The same shapes of a module that is not builtins name nothing.
	for _, program := range []string{
		"import re; c = re \\\n.compile(\"\"\"open(file=\"{MEMORY}/a\",mode=\"w\")\"\"\")",
		"import re; c = (re \\\n.compile)(\"\"\"open(file=\"{MEMORY}/a\",mode=\"w\")\"\"\")",
	} {
		command := "python3 -c '" + strings.ReplaceAll(program, "{MEMORY}", root) + "'"
		if got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env); got.Surface != "" {
			t.Errorf("memoryGateClassify = %+v, want none: %q", got, command)
		}
	}
}

// TestCRW851AttributeChainBeforeBuiltins (d1): builtins reached as an attribute of another object is no builtins call, whether
// blanks or a continuation stand before the dot, after it, or both. The memory gate names nothing and asks no grant.
func TestCRW851AttributeChainBeforeBuiltins(t *testing.T) {
	cwd, _, env := gateScene(t)
	for _, c := range []struct{ name, program string }{
		{"blanks around the dot, continuation before the call dot", "runner . builtins \\\n.exec(x)"},
		{"continuation before the dot in front of builtins", "runner \\\n. builtins.exec(x)"},
		{"continuation after the dot in front of builtins", "runner. \\\nbuiltins.exec(x)"},
		{"continuation on both sides, CR LF", "runner \\\r\n. \\\r\nbuiltins \\\r\n. \\\r\nexec(x)"},
		{"__builtins__ eval", "runner \\\n. \\\n__builtins__ \\\n. \\\neval(x)"},
		{"parenthesized callee", "(runner \\\n. builtins \\\n.exec)(x)"},
		{"plain blanks (no continuation)", "runner . builtins.exec(x)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := shellVerbOpenWrites(c.program); len(got) != 0 {
				t.Errorf("%q named %q", c.program, got)
			}
			if _, what := shellWriteExecScan(shellVerbWithoutComments(c.program, true), true, 0); what != "" {
				t.Errorf("%q reported the unreadable program %q", c.program, what)
			}
			command := "python3 -c '" + c.program + "'"
			if got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env); got.Surface != "" {
				t.Errorf("memoryGateClassify = %+v, want none: %q", got, command)
			}
		})
	}
}

// TestCRW851ContinuationRowsThroughTheMemoryGate runs the rows of file 21 through HandleMemoryWriteGate with the state a session
// has: an E row (a write the gate must see) is denied without a grant, and a grant is consumed by it; a control row passes with
// no grant and leaves a granted session's grant unspent.
func TestCRW851ContinuationRowsThroughTheMemoryGate(t *testing.T) {
	rows := 0
	for _, row := range reproductionRows() {
		if row.file != "21-crw-851-line-continuation.txt" {
			continue
		}
		rows++
		row := row
		t.Run(row.id, func(t *testing.T) {
			cwd, root, env := gateScene(t)
			fill := strings.NewReplacer("{MEMORY}", root, "{CR}", "\r")
			payload := func() string {
				return gatePayload(t, cwd, map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": fill.Replace(row.cmd)}})
			}
			grant := func(s *state.State) { s.MemoryWriteGrant = true }
			gateSeed(t, cwd, func(s *state.State) {})
			if row.want[0] == "attempt" {
				gateDeny(t, HandleMemoryWriteGate(payload(), env))
				gateSeed(t, cwd, grant)
				if got := HandleMemoryWriteGate(payload(), env); got != "" {
					t.Fatalf("granted call: %q", got)
				}
				if state.ReadState(cwd, gateSession).MemoryWriteGrant {
					t.Error("the grant was not consumed")
				}
				gateDeny(t, HandleMemoryWriteGate(payload(), env))
				return
			}
			if got := HandleMemoryWriteGate(payload(), env); got != "" {
				t.Fatalf("control without a grant: %q", got)
			}
			gateSeed(t, cwd, grant)
			if got := HandleMemoryWriteGate(payload(), env); got != "" {
				t.Fatalf("control with a grant: %q", got)
			}
			if !state.ReadState(cwd, gateSession).MemoryWriteGrant {
				t.Error("a control spent the grant")
			}
		})
	}
	if rows < 22 {
		t.Errorf("read %d rows of file 21, want at least 22", rows)
	}
}
