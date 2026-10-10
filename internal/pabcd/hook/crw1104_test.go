package hook

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/shellir"
)

func TestCRW1104EachContextAnalyzedOnce(t *testing.T) {
	cwd, _, env := gateScene(t)
	for _, command := range []string{"ls", "printf '%s' ok", "awk 'END {print NR}' notes.txt"} {
		counts := [3]int{}
		factory := func(command, dir string, env host.LookupEnv) *memoryShellAnalysis {
			a := newMemoryShellAnalysis(command, dir, env)
			a.env = func(cmd, dir string, lookup func(string) (string, bool)) (shellir.Result, error) {
				counts[0]++
				return shellir.AnalyzeEnvProvenDirectory(cmd, dir, lookup)
			}
			a.plain = func(cmd, dir string) (shellir.Result, error) { counts[1]++; return shellir.Analyze(cmd, dir) }
			a.noDir = func(cmd string) (shellir.Result, error) { counts[2]++; return shellir.AnalyzeNoDir(cmd) }
			return a
		}
		if got := memoryGateClassifyUsing("Bash", map[string]any{"command": command}, cwd, env, factory); got.Surface != "" {
			t.Fatalf("readable control classified %+v", got)
		}
		for context, n := range counts {
			if n != 1 {
				t.Errorf("%s context %d ran %d analyses, want 1", command, context, n)
			}
		}
	}
}

func BenchmarkCRW1104MemoryGate(b *testing.B) {
	cwd := b.TempDir()
	env := gateEnvOf(map[string]string{"HOME": cwd, "CODEX_HOME": cwd + "/codex"})
	for name, command := range map[string]string{"short": "ls", "near-limit": "printf '%s' '" + strings.Repeat("x", shellir.MaxCommandBytes-30) + "'"} {
		b.Run(name, func(b *testing.B) {
			input := map[string]any{"command": command}
			b.ReportAllocs()
			for b.Loop() {
				memoryGateClassify("Bash", input, cwd, env)
			}
		})
	}
}

func TestCRW1104CacheErrorsAndReadOnlyRecords(t *testing.T) {
	cwd, _, env := gateScene(t)
	a := newMemoryShellAnalysis("echo ok", cwd, env)
	count := 0
	want := errors.New("reader error")
	a.env = func(string, string, func(string) (string, bool)) (shellir.Result, error) {
		count++
		return shellir.Result{}, want
	}
	for range 2 {
		if _, err := a.withEnv(); err != want {
			t.Fatal("error changed")
		}
	}
	if count != 1 {
		t.Fatalf("error analyzed %d times", count)
	}
	a = newMemoryShellAnalysis("python3 -c 'print(1)'", cwd, env)
	res, err := a.withEnv()
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(res)
	shellIRDestsResult(res, cwd, env, true, 0, nil)
	shellIRFStringResult(res, nil)
	after, _ := json.Marshal(res)
	if string(before) != string(after) {
		t.Fatal("consumer mutated analysis records")
	}
}

// The pre-cache path runs the same three contexts without memoization. Compare
// all existing commands' attempts and destinations, including reader errors.
func TestCRW1104CorpusParity(t *testing.T) {
	for _, row := range reproductionRows() {
		t.Run(row.id, func(t *testing.T) {
			r := newDelRig(t)
			cwd, root, env := gateScene(t)
			tmp := t.TempDir()
			fill := strings.NewReplacer("{MEMORY}", root, "{CODEXHOME}", filepath.Dir(root), "{MEMORY_NOSLASH}", strings.TrimPrefix(root, "/"), "{PAD1M}", strings.Repeat("#", 1<<20+1), "{CHECKOUT}", r.checkout, "{WORK}", cwd, "{TMP}", tmp, "{NUL}", "\x00", "{DEL}", "\x7f", "{CR}", "\r", "{TAB}", "\t")
			reproScene(t, row, cwd, r.checkout, tmp, fill)
			cmd := fill.Replace(row.cmd)
			uncached := func(command, dir string, env host.LookupEnv) *memoryShellAnalysis {
				a := newMemoryShellAnalysis(command, dir, env)
				a.cached = false
				return a
			}
			input := map[string]any{"command": cmd}
			before := memoryGateClassifyUsing("Bash", input, cwd, env, uncached)
			after := memoryGateClassify("Bash", input, cwd, env)
			if before != after {
				t.Fatalf("attempt changed: %+v / %+v", before, after)
			}
			old, ok := shellIRWriteDestsResolved(cmd, cwd, env)
			a := newMemoryShellAnalysis(cmd, cwd, env)
			res, err := a.withEnv()
			var now []string
			if err == nil {
				now = shellIRDestsResult(res, cwd, env, true, 0, nil)
			}
			if ok != (err == nil) || !reflect.DeepEqual(old, now) {
				t.Fatal("destinations changed")
			}
		})
	}
}
