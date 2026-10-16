package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// CRW-1570: the run's recorded TMPDIR (env.TMPDIR of its run.json) reaches every agent call the run starts, as one of its repository
// tools: first in Env (an App Server thread's shell gets it as a leaf key), first in the prefix a session subagent puts in front of
// every command, and named by a fact. It is a tool on its own: a configuration that names no go gives the TMPDIR alone, not nil. A
// spec without env gives exactly the tools of before.
func TestRepositoryToolsCarryTheRunsTMPDIR(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(home, "config.json")
	root := filepath.Join(home, "go root")
	a := filepath.Join(t.TempDir(), "run tmp")
	getenv := func(k string) string {
		// the relaunching shell has another TMPDIR: the tools take the recorded one, never the caller's
		return map[string]string{"HOME": home, "CRW_CONFIG": config, "PATH": "/usr/local/bin:/usr/bin", "TMPDIR": "/caller/tmp"}[k]
	}
	write := func(doc string) {
		t.Helper()
		if err := os.WriteFile(config, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rec := map[string]string{"TMPDIR": a}
	tmpFact := func(t *testing.T, facts []string) {
		t.Helper()
		for _, f := range facts {
			if strings.Contains(f, a) && strings.Contains(f, "TMPDIR") && strings.Contains(f, "run.json") {
				return
			}
		}
		t.Errorf("facts %q do not name the recorded TMPDIR %s and run.json", facts, a)
	}

	// no go_root (no file, an empty section, a section the verifier ignores): the TMPDIR alone
	for _, doc := range []string{"", `{}`, `{"verifier":{"go_root":"relative"}}`} {
		if doc == "" {
			_ = os.Remove(config)
		} else {
			write(doc)
		}
		tools := repositoryTools(getenv, rec)
		if tools == nil {
			t.Fatalf("%q with a recorded TMPDIR gave no tools", doc)
		}
		if want := []string{"TMPDIR=" + a}; !slices.Equal(tools.Env, want) {
			t.Errorf("%q: env %q, want %q", doc, tools.Env, want)
		}
		if want := "TMPDIR=" + shellWord(a); tools.Prefix != want {
			t.Errorf("%q: prefix %s, want %s", doc, tools.Prefix, want)
		}
		tmpFact(t, tools.Facts)
		if len(tools.Facts) != 1 {
			t.Errorf("%q: facts %q, want the TMPDIR's only", doc, tools.Facts)
		}
	}

	// with go_root: TMPDIR first, then the go's entries, prefix and fact as before
	write(`{"verifier":{"go_root":"` + root + `"}}`)
	tools := repositoryTools(getenv, rec)
	if tools == nil {
		t.Fatal("verifier.go_root and a recorded TMPDIR gave no tools")
	}
	bin := filepath.Join(root, "bin")
	goEnv := []string{"PATH=" + bin + ":/usr/local/bin:/usr/bin", "GOROOT=" + root, "GOTOOLCHAIN=local"}
	if want := append([]string{"TMPDIR=" + a}, goEnv...); !slices.Equal(tools.Env, want) {
		t.Errorf("env %q, want %q", tools.Env, want)
	}
	goPrefix := `PATH='` + bin + `':"$PATH" GOROOT='` + root + `' GOTOOLCHAIN=local`
	if want := "TMPDIR=" + shellWord(a) + " " + goPrefix; tools.Prefix != want {
		t.Errorf("prefix %s, want %s", tools.Prefix, want)
	}
	tmpFact(t, tools.Facts)
	if len(tools.Facts) != 2 || !slices.ContainsFunc(tools.Facts, func(f string) bool { return strings.Contains(f, filepath.Join(bin, "go")) }) {
		t.Errorf("facts %q, want the TMPDIR's and the go's", tools.Facts)
	}

	// a spec without env (nil or empty): exactly the tools of before
	for _, none := range []map[string]string{nil, {}} {
		got := repositoryTools(getenv, none)
		if got == nil || !slices.Equal(got.Env, goEnv) || got.Prefix != goPrefix || len(got.Facts) != 1 || strings.Contains(got.Facts[0], "temporary directory") {
			t.Errorf("no recorded env (%v) gave tools %+v, want the go's alone", none, got)
		}
		write(`{}`)
		if got := repositoryTools(getenv, none); got != nil {
			t.Errorf("no recorded env and no go_root gave tools %+v, want nil", got)
		}
		write(`{"verifier":{"go_root":"` + root + `"}}`)
	}
}

// CRW-1570: the session prefix sets TMPDIR in a real shell. The inner program is single-quoted, so the shell under the prefix reads
// its own TMPDIR (the outer shell does not expand it first). Without the prefix the inner shell has none, as the owner session's
// shell may not; with it, the recorded directory, a short one and one over 100 bytes alike.
func TestSessionPrefixSetsTMPDIRInAShell(t *testing.T) {
	short := t.TempDir()
	long := filepath.Join(short, strings.Repeat("n", 60), "with space", strings.Repeat("d", 40))
	if len(long) <= 100 {
		t.Fatalf("the long directory %s is %d bytes", long, len(long))
	}
	if err := os.MkdirAll(long, 0o700); err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TMPDIR=") {
			env = append(env, kv)
		}
	}
	inner := " sh -c " + shellWord(`printf %s "$TMPDIR"`)
	shell := func(t *testing.T, cmd string) string {
		t.Helper()
		c := exec.Command("sh", "-c", cmd)
		c.Env = env
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("sh -c %s: %v\n%s", cmd, err, out)
		}
		return string(out)
	}
	if got := shell(t, strings.TrimPrefix(inner, " ")); got != "" {
		t.Fatalf("without the prefix the shell has TMPDIR %q, want none", got)
	}
	for _, a := range []string{short, long} {
		tools := repositoryTools(func(string) string { return "" }, map[string]string{"TMPDIR": a})
		if tools == nil {
			t.Fatalf("%s: no tools", a)
		}
		if got := shell(t, tools.Prefix+inner); got != a {
			t.Errorf("under the prefix %s the shell has TMPDIR %q, want %q", tools.Prefix, got, a)
		}
	}
}
