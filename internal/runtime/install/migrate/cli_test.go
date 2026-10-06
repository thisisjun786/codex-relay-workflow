package migrate

// cli_test.go holds the M4 red-first cases of docs/port-cxc/state-migration.md: help and invalid
// flags write nothing, a dry run creates no root and no report, the scope/default/environment
// resolution, every exit code and cancellation, the legacy install help, and an end-to-end run per
// scope. Every root is a temporary directory; no test reads or writes the real ~/.codex, ~/.crw or
// ~/.codexclaw.

import (
	"bytes"
	"context"
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// cliWS lays out a workspace with a CXC project store under base/ws, and returns the workspace.
func cliWS(t *testing.T, base string, entries map[string]string) string {
	t.Helper()
	ws := filepath.Join(base, "ws")
	mkdirs(t, ws)
	invTree(t, filepath.Join(ws, ProjectSourceName), entries)
	return ws
}

// cliRun runs the command with the environment isolate set, and returns its exit code and output.
func cliRun(t *testing.T, ctx context.Context, args ...string) (int, string, string) {
	t.Helper()
	return cliRunEnv(t, ctx, os.Environ(), args...)
}

// cliRunEnv runs the command with an explicit environment.
func cliRunEnv(t *testing.T, ctx context.Context, env []string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(ctx, args, env, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestCLIHelpWritesNothing(t *testing.T) {
	base := isolate(t)
	ws := cliWS(t, base, map[string]string{"ledger.jsonl": "{}\n"})
	before := tree(t, base)
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}, {"--help", "--cwd", ws}} {
		code, out, errOut := cliRun(t, context.Background(), args...)
		if code != 0 || out != UsageText || errOut != "" {
			t.Fatalf("%v: exit %d stdout=%q stderr=%q", args, code, out, errOut)
		}
	}
	if got := tree(t, base); !reflect.DeepEqual(got, before) {
		t.Fatalf("help wrote: %v", got)
	}
}

func TestCLIInvalidArgumentsWriteNothing(t *testing.T) {
	base := isolate(t)
	ws := cliWS(t, base, map[string]string{"ledger.jsonl": "{}\n"})
	before := tree(t, base)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--nope", "--cwd", ws}, "not defined"},
		{[]string{"--cwd", ws, "--scope", "bogus"}, "unknown scope"},
		{[]string{"--cwd", ws, "--from-home", base}, "--scope user"},
		{[]string{"--cwd", ws, "--to-home", base}, "--scope user"},
		{[]string{"--cwd", ws, "--codex-home", base}, "--scope codex"},
		{[]string{"--cwd", ws, "extra"}, "unrecognized arguments"},
	} {
		code, out, errOut := cliRun(t, context.Background(), tc.args...)
		if code != 2 || out != "" || !strings.Contains(errOut, tc.want) {
			t.Errorf("%v: exit %d stdout=%q stderr=%q", tc.args, code, out, errOut)
		}
	}
	if got := tree(t, base); !reflect.DeepEqual(got, before) {
		t.Fatalf("invalid arguments wrote: %v", got)
	}
}

func TestCLIDryRunCreatesNoRootAndNoReport(t *testing.T) {
	base := isolate(t)
	ws := cliWS(t, base, map[string]string{".gitignore": "sessions/\n", "ledger.jsonl": "{}\n", "sessions/a.json": "{\"phase\":\"IDLE\"}\n"})
	report := filepath.Join(base, "report.json")
	code, out, errOut := cliRun(t, context.Background(), "--cwd", ws, "--dry-run", "--report", report)
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d stderr=%q", code, errOut)
	}
	if !strings.Contains(out, "result: dry-run") || !strings.Contains(out, "excluded=1") || !strings.Contains(out, "refused=0") {
		t.Fatalf("dry-run text:\n%s", out)
	}
	if _, err := os.Stat(report); !os.IsNotExist(err) {
		t.Fatalf("the dry run wrote a report: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, ".crw")); !os.IsNotExist(err) {
		t.Fatalf("the dry run created .crw: %v", err)
	}
}

func TestCLIScopeDefaultsAndEnvironment(t *testing.T) {
	base := isolate(t)
	ws := cliWS(t, base, map[string]string{"ledger.jsonl": "{}\n"})
	home := os.Getenv("HOME")
	// The default scope is project, and the default user roots come from the environment.
	code, out, _ := cliRun(t, context.Background(), "--cwd", ws, "--scope", "all", "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	want := []string{
		"project: " + ws + "/.codexclaw -> " + ws + "/.crw",
		"user: " + os.Getenv("CODEXCLAW_HOME") + " -> " + os.Getenv("CRW_HOME"),
		"codex: " + os.Getenv("CODEX_HOME") + " -> " + os.Getenv("CODEX_HOME"),
	}
	for _, line := range want {
		if !strings.Contains(out, line) {
			t.Errorf("roots missing %q:\n%s", line, out)
		}
	}
	// An explicit flag beats the environment.
	code, out, _ = cliRun(t, context.Background(), "--cwd", ws, "--scope", "user", "--from-home", base+"/x", "--to-home", base+"/y", "--dry-run")
	if code != 0 || !strings.Contains(out, "user: "+base+"/x -> "+base+"/y") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	// With no environment the defaults hang under the home.
	var bare []string
	for _, entry := range os.Environ() {
		switch strings.SplitN(entry, "=", 2)[0] {
		case "CODEXCLAW_HOME", "CRW_HOME", "CODEX_HOME":
			continue
		}
		bare = append(bare, entry)
	}
	code, out, _ = cliRunEnv(t, context.Background(), bare, "--cwd", ws, "--scope", "user", "--dry-run")
	if code != 0 || !strings.Contains(out, "user: "+home+"/.codexclaw -> "+home+"/.crw") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestCLIEndToEndPerScope(t *testing.T) {
	base := isolate(t)
	ws := cliWS(t, base, map[string]string{"ledger.jsonl": "{\"n\":1}\n", "sessions/a.json": "{\"phase\":\"IDLE\"}\n"})
	user, to := os.Getenv("CODEXCLAW_HOME"), os.Getenv("CRW_HOME")
	put(t, filepath.Join(user, "subagents.json"), "{\"global\":true}\n", 0o644)
	codex := os.Getenv("CODEX_HOME")
	put(t, filepath.Join(codex, installSource), "{\"restore\":1}\n", 0o644)
	put(t, filepath.Join(codex, selfHealSource), "{\"optOut\":false}\n", 0o644)
	code, out, errOut := cliRun(t, context.Background(), "--cwd", ws, "--scope", "all")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d stderr=%q\n%s", code, errOut, out)
	}
	if got := get(t, filepath.Join(ws, ".crw", "ledger.jsonl")); got != "{\"n\":1}\n" {
		t.Errorf("project ledger = %q", got)
	}
	if got := get(t, filepath.Join(to, "subagents.json")); got != "{\"global\":true}\n" {
		t.Errorf("user subagents = %q", got)
	}
	if got := get(t, filepath.Join(codex, installDest)); got != "{\"restore\":1}\n" {
		t.Errorf("codex install manifest = %q", got)
	}
	if got := get(t, filepath.Join(codex, selfHealDest)); got != "{\"optOut\":false}\n" {
		t.Errorf("codex self-heal marker = %q", got)
	}
	if _, err := os.Stat(filepath.Join(codex, installSource)); err != nil {
		t.Errorf("the source was changed: %v", err)
	}
	// A rerun finds everything equal and copies nothing.
	code, out, _ = cliRun(t, context.Background(), "--cwd", ws, "--scope", "all")
	if code != 0 || !strings.Contains(out, "result: already-equal") {
		t.Fatalf("rerun exit %d\n%s", code, out)
	}
}

func TestCLIExitCodesAndCancellation(t *testing.T) {
	base := isolate(t)
	ws := cliWS(t, base, map[string]string{"ledger.jsonl": "{}\n"})
	// 0 for a copy.
	if code, _, _ := cliRun(t, context.Background(), "--cwd", ws); code != 0 {
		t.Fatalf("copy exit %d", code)
	}
	// 1 for a destination that differs: nothing is overwritten and nothing else is written.
	put(t, filepath.Join(ws, ".crw", "ledger.jsonl"), "other\n", 0o644)
	put(t, filepath.Join(ws, ProjectSourceName, "sessions/a.json"), "{\"phase\":\"IDLE\"}\n", 0o644)
	code, out, _ := cliRun(t, context.Background(), "--cwd", ws)
	if code != 1 || !strings.Contains(out, "result: refused") {
		t.Fatalf("conflict exit %d\n%s", code, out)
	}
	if got := get(t, filepath.Join(ws, ".crw", "ledger.jsonl")); got != "other\n" {
		t.Fatalf("the destination was replaced: %q", got)
	}
	if _, err := os.Stat(filepath.Join(ws, ".crw", "sessions")); !os.IsNotExist(err) {
		t.Fatalf("a refused scope wrote: %v", err)
	}
	// 2 for usage.
	if code, _, _ := cliRun(t, context.Background(), "--scope"); code != 2 {
		t.Fatalf("usage exit %d", code)
	}
	// A cancelled run fails and writes nothing.
	base2 := isolate(t)
	ws2 := cliWS(t, base2, map[string]string{"ledger.jsonl": "{}\n"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code, out, _ = cliRun(t, ctx, "--cwd", ws2)
	if code != 1 || !strings.Contains(out, "result: failed") {
		t.Fatalf("cancelled exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(ws2, ".crw")); !os.IsNotExist(err) {
		t.Fatalf("a cancelled run wrote: %v", err)
	}
}

func TestCLIReportAndJSON(t *testing.T) {
	base := isolate(t)
	ws := cliWS(t, base, map[string]string{"ledger.jsonl": "{}\n"})
	report := filepath.Join(base, "report.json")
	code, out, errOut := cliRun(t, context.Background(), "--cwd", ws, "--json", "--report", report)
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d stderr=%q", code, errOut)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if got["schema"] != SchemaID || got["result"] != "copied" || got["dryRun"] != false || got["scope"] != "project" {
		t.Fatalf("envelope: %v", got)
	}
	if got["writesCompleted"].(float64) == 0 || got["sourceVerified"] != true {
		t.Fatalf("verification flags: %v", got)
	}
	items, ok := got["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("items: %v", got["items"])
	}
	first, _ := items[0].(map[string]any)
	for _, key := range []string{"scope", "source", "destination", "disposition", "result", "reason", "bytes", "digest", "mode"} {
		if _, ok := first[key]; !ok {
			t.Errorf("item misses %q: %v", key, first)
		}
	}
	if _, ok := got["error"]; !ok || got["error"] != nil {
		t.Errorf("error = %v", got["error"])
	}
	// The report file carries the same document, published no-replace.
	var fromFile map[string]any
	raw := get(t, report)
	if err := json.Unmarshal([]byte(raw), &fromFile); err != nil {
		t.Fatalf("report is not JSON: %v", err)
	}
	if fromFile["schema"] != SchemaID || fromFile["result"] != "copied" {
		t.Fatalf("report: %v", fromFile)
	}
	// Naming an existing report is a usage error, and the file is untouched.
	if code, _, errOut := cliRun(t, context.Background(), "--cwd", ws, "--report", report); code != 2 || !strings.Contains(errOut, "already exists") {
		t.Fatalf("existing report: exit %d stderr=%q", code, errOut)
	}
	if got := get(t, report); got != raw {
		t.Fatalf("the report was rewritten: %q", got)
	}
	// A report inside the destination tree is refused before anything is written.
	inside := filepath.Join(ws, ".crw", "r.json")
	if code, _, _ := cliRun(t, context.Background(), "--cwd", ws, "--report", inside); code != 2 {
		t.Fatalf("inside-tree report: exit %d", code)
	}
	if _, err := os.Stat(inside); !os.IsNotExist(err) {
		t.Fatalf("the refused report was written: %v", err)
	}
}

func TestCLIMigrationIsNotReachedImplicitly(t *testing.T) {
	root := moduleRoot(t)
	const self = "github.com/thisisjun786/codex-relay-workflow/internal/runtime/install/migrate"
	importers := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "testdata" || name == ".git" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if perr != nil {
			return nil
		}
		for _, spec := range file.Imports {
			if spec.Path.Value == `"`+self+`"` {
				rel, _ := filepath.Rel(root, path)
				importers[filepath.ToSlash(rel)] = true
			}
		}
		return nil
	})
	must(t, err)
	want := map[string]bool{"internal/runtime/install/cli.go": true}
	if !reflect.DeepEqual(importers, want) {
		t.Fatalf("the migration package is imported by %v, want %v", importers, want)
	}
}

// moduleRoot walks up from the test's working directory to the module root.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	must(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}
