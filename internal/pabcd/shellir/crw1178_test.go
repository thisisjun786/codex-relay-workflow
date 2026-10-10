package shellir

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pycHeader is the 16-byte header Python writes: magic, flags, then either mtime and source size (flags 0) or the source hash.
func pycHeader(flags uint32) []byte {
	h := make([]byte, 16)
	copy(h, []byte{0xcb, 0x0d, 0x0d, 0x0a})
	binary.LittleEndian.PutUint32(h[4:], flags)
	return h
}

func crw1178Project(t *testing.T, pyc map[string][]byte) string {
	t.Helper()
	cwd := t.TempDir()
	for name, body := range map[string]string{"calc.py": "def add(a, b):\n    return a + b\n", "test_calc.py": "import calc\n"} {
		if err := os.WriteFile(filepath.Join(cwd, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for rel, body := range pyc {
		p := filepath.Join(cwd, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return cwd
}

// Compiled code the interpreter could run without the source the reader read stays refused, and the reason names the file.
func TestCRW1178CompiledCodeWithoutReadableSourceStaysRefused(t *testing.T) {
	for _, c := range []struct {
		name string
		pyc  map[string][]byte
		want string
	}{
		{"sourceless legacy module", map[string][]byte{"foo.pyc": pycHeader(0)}, "foo.pyc"},
		{"cache without a source", map[string][]byte{"__pycache__/orphan.cpython-312.pyc": pycHeader(0)}, "orphan.cpython-312.pyc"},
		{"cache of a source in another directory", map[string][]byte{"sub/__pycache__/calc.cpython-312.pyc": pycHeader(0)}, "calc.cpython-312.pyc"},
		{"unchecked hash cache", map[string][]byte{"__pycache__/calc.cpython-312.pyc": pycHeader(1)}, "calc.cpython-312.pyc"},
		{"header too short", map[string][]byte{"__pycache__/calc.cpython-312.pyc": []byte("short")}, "calc.cpython-312.pyc"},
		{"extension module in the cache", map[string][]byte{"__pycache__/calc.cpython-312-x86_64-linux-gnu.so": []byte("x")}, ".so"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cwd := crw1178Project(t, c.pyc)
			for _, cmd := range []string{"python3 -m unittest", "python3 -m pytest"} {
				_, err := Analyze(cmd, cwd)
				var u *Unreadable
				if !errors.As(err, &u) {
					t.Fatalf("%s: compiled code without a readable source allowed: %v", cmd, err)
				}
				if !strings.Contains(u.Reason, c.want) || strings.Contains(u.Reason, "inventory refused") {
					t.Errorf("%s: the reason loses the cause: %q", cmd, u.Reason)
				}
			}
		})
	}
	// A linked cache entry is not followed.
	cwd := crw1178Project(t, map[string][]byte{"__pycache__/real.pyc": pycHeader(0)})
	if err := os.Symlink(filepath.Join(cwd, "__pycache__", "real.pyc"), filepath.Join(cwd, "__pycache__", "calc.cpython-312.pyc")); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	_, err := Analyze("python3 -m unittest", cwd)
	var u *Unreadable
	if !errors.As(err, &u) || !strings.Contains(u.Reason, "link") {
		t.Errorf("a linked cache entry must be refused with its cause: %v", err)
	}
}

// A source the cache is named for must be a regular file: a link or a directory under that name is no source the reader read.
func TestCRW1178CacheNeedsRegularSource(t *testing.T) {
	cwd := t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, "calc.py"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cwd, "__pycache__"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "__pycache__", "calc.cpython-312.pyc"), pycHeader(0), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Analyze("python3 -m unittest", cwd); err == nil {
		t.Error("a cache beside a directory named calc.py was allowed")
	}
}

// An operating-system failure in the walk names the file or directory that failed, relative to the project, not the host path.
func TestCRW1178WalkErrorNamesThePath(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	cwd := crw1178Project(t, nil)
	blocked := filepath.Join(cwd, "sub", "blocked_tests")
	if err := os.MkdirAll(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })
	_, err := Analyze("python3 -m unittest", cwd)
	var u *Unreadable
	if !errors.As(err, &u) || !strings.Contains(u.Reason, "sub/blocked_tests") || strings.Contains(u.Reason, cwd) {
		t.Errorf("reason: %v", err)
	}
}

// matchingPycHeader is the timestamp header Python writes for source when it compiles it: the cache the interpreter loads instead
// of the source while both still agree.
func matchingPycHeader(t *testing.T, source string) []byte {
	t.Helper()
	fi, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	h := pycHeader(0)
	binary.LittleEndian.PutUint32(h[8:], uint32(fi.ModTime().Unix()))
	binary.LittleEndian.PutUint32(h[12:], uint32(fi.Size()))
	return h
}

// A cache entry whose header still agrees with its source is what the interpreter runs, not the source: its code is not read, so
// it is refused, with the file and a route that leaves no cache to load. A hash-based entry is refused whatever its hash.
func TestCRW1178CacheThatMatchesItsSourceIsRefused(t *testing.T) {
	for _, c := range []struct {
		name   string
		header func(t *testing.T, cwd string) []byte
	}{
		{"timestamp entry that matches", func(t *testing.T, cwd string) []byte { return matchingPycHeader(t, filepath.Join(cwd, "calc.py")) }},
		{"checked hash entry", func(*testing.T, string) []byte { return pycHeader(3) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			cwd := crw1178Project(t, nil)
			if err := os.MkdirAll(filepath.Join(cwd, "__pycache__"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cwd, "__pycache__", "calc.cpython-312.pyc"), append(c.header(t, cwd), "code"...), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, cmd := range []string{"python3 -m unittest", "python3 -B -m unittest", "PYTHONDONTWRITEBYTECODE=1 python3 -m pytest"} {
				_, err := Analyze(cmd, cwd)
				var u *Unreadable
				if !errors.As(err, &u) {
					t.Fatalf("%s: a cache the interpreter runs instead of the source was allowed: %v", cmd, err)
				}
				if !strings.Contains(u.Reason, "__pycache__/calc.cpython-312.pyc") || !strings.Contains(u.Reason, crw1178Route) {
					t.Errorf("%s: the reason lacks the file or the route: %q", cmd, u.Reason)
				}
			}
		})
	}
}

// A command that changes a source's time or brings in a cache before the run is refused as a rewrite of module imports.
func TestCRW1178SameCommandCannotRefreshACache(t *testing.T) {
	cwd := crw1178Project(t, map[string][]byte{"__pycache__/calc.cpython-312.pyc": append(pycHeader(0), "code"...)})
	for _, cmd := range []string{
		"touch -d @0 calc.py && python3 -m unittest",
		"touch -r ref calc.py; python3 -B -m unittest",
		"cp -r /elsewhere/cache __pycache__ && python3 -m unittest",
		"mv /elsewhere/__pycache__ pkg/ && python3 -m pytest",
	} {
		_, err := Analyze(cmd, cwd)
		var u *Unreadable
		if !errors.As(err, &u) || !strings.Contains(u.Reason, "rewritten") {
			t.Errorf("%s: not refused as a rewrite of module imports: %v", cmd, err)
		}
	}
}

// CRW-1178 evaluation d1: the interpreter imports __pycache__/calc.pyc (no tag) as the sourceless module __pycache__.calc and
// never compares it with a source. Such a name, and every other .pyc, is refused with the file it names.
func TestCRW1178UntaggedOrMalformedCacheNameIsRefused(t *testing.T) {
	for _, name := range []string{
		"__pycache__/calc.pyc",
		"__pycache__/calc..pyc",
		"__pycache__/calc.cpython-312.extra.pyc",
		"__pycache__/calc.cpython-312.opt-x.pyc",
		"__pycache__/calc.cpython-312.opt-1.more.pyc",
		"__pycache__/calc.in valid.pyc",
		"__pycache__/.cpython-312.pyc",
	} {
		t.Run(name, func(t *testing.T) {
			cwd := crw1178Project(t, map[string][]byte{name: append(pycHeader(0), "code"...)})
			for _, cmd := range []string{"python3 -m unittest", "python3 -B -m unittest", "python3 -m pytest"} {
				_, err := Analyze(cmd, cwd)
				var u *Unreadable
				if !errors.As(err, &u) {
					t.Fatalf("%s: an unimportable-as-cache name with a stale header was skipped: %v", cmd, err)
				}
				if !strings.Contains(u.Reason, filepath.Base(name)) {
					t.Errorf("%s: the reason lacks the file: %q", cmd, u.Reason)
				}
			}
		})
	}
}

// crw1178Route is the way past a cache refusal the reason names: the cache is removed by a command of its own, and the run then writes
// no new one.
const crw1178Route = "remove __pycache__ in a separate command first, then run with python -B (PYTHONDONTWRITEBYTECODE=1) so no new cache is written"

// CRW-1178 (S2R2-F1) as decided after verification round 8: the first python -m unittest creates __pycache__, and a later run beside
// it is refused, as at the base. No header property of an entry the reader judges before the run survives the run itself: a test
// module or conftest can set a source's time (os.utime) or rewrite it before it is imported, and so make a stale entry current. So
// every .pyc of the inventory is refused, and the reason names the file and the route: remove __pycache__ in a separate command, then
// run with -B so no new cache is written.
func TestCRW1178EveryCacheEntryBesideAModuleRunIsRefusedWithTheRoute(t *testing.T) {
	for _, name := range []string{
		"__pycache__/calc.cpython-312.pyc",
		"__pycache__/test_calc.cpython-312.pyc",
		"__pycache__/calc.cpython-312.opt-1.pyc",
		"__pycache__/test_calc.cpython-312-pytest-8.3.2.pyc",
		"pkg/__pycache__/__init__.cpython-312.pyc",
	} {
		t.Run(name, func(t *testing.T) {
			// A stale timestamp entry: its recorded time and size (zero) disagree with the source.
			cwd := crw1178Project(t, map[string][]byte{name: append(pycHeader(0), "code"...)})
			if strings.HasPrefix(name, "pkg/") {
				if err := os.WriteFile(filepath.Join(cwd, "pkg", "__init__.py"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for _, cmd := range []string{
				"python3 -m unittest",
				"python3 -B -m unittest",
				"PYTHONDONTWRITEBYTECODE=1 python3 -m unittest",
				"env PYTHONDONTWRITEBYTECODE=1 python3 -m unittest",
				"python3 -m pytest",
				"python3 -B -m unittest test_calc.py",
				"python3 -B -m unittest > /dev/null 2>&1",
				"rm -rf __pycache__ && python3 -B -m unittest",
			} {
				_, err := Analyze(cmd, cwd)
				var u *Unreadable
				if !errors.As(err, &u) {
					t.Fatalf("%s: a cache entry beside a module run was allowed: %v", cmd, err)
				}
				if !strings.Contains(u.Reason, name) || !strings.Contains(u.Reason, crw1178Route) {
					t.Errorf("%s: the reason lacks the file or the route: %q", cmd, u.Reason)
				}
			}
		})
	}
}

// CRW-1178 verification round 8 (P0): a test module that sets its source's time with os.utime before it imports it makes a stale entry
// current, and the interpreter then runs the entry's code instead of the source the reader read. Nothing in the command text shows
// it, so a stale entry is no reason to allow the run.
func TestCRW1178TestModuleThatSetsASourceTimeCannotRevealACache(t *testing.T) {
	for _, body := range []string{
		"import os\nos.utime('calc.py', (1700000000, 1700000000))\nimport unittest\nimport calc\n",
		"import os\nos.utime(os.path.join(os.path.dirname(__file__), 'calc.py'), (1700000000, 1700000000))\nimport calc\n",
	} {
		cwd := crw1178Project(t, nil)
		src := filepath.Join(cwd, "calc.py")
		h := matchingPycHeader(t, src)
		binary.LittleEndian.PutUint32(h[8:], 1700000000) // stale now, current once the test module ran
		if err := os.MkdirAll(filepath.Join(cwd, "__pycache__"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cwd, "__pycache__", "calc.cpython-312.pyc"), append(h, "code"...), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cwd, "test_calc.py"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, cmd := range []string{"python3 -B -m unittest", "python3 -B -m unittest -q", "python3 -B -m unittest test_calc", "python3 -B -m pytest"} {
			_, err := Analyze(cmd, cwd)
			var u *Unreadable
			if !errors.As(err, &u) || !strings.Contains(u.Reason, "__pycache__/calc.cpython-312.pyc") || !strings.Contains(u.Reason, crw1178Route) {
				t.Errorf("%s: a stale entry a test module can make current was allowed or lacks the file and route: %v", cmd, err)
			}
		}
	}
}

// The route stays open: a plain run with no __pycache__ is allowed, with a log redirection too, and so is a -B run once an earlier,
// separate command removed the cache. Removing it in the same command is refused (the entry is there when the text is read).
func TestCRW1178RunWithoutCacheStaysAllowed(t *testing.T) {
	clean := crw1178Project(t, nil)
	for _, cmd := range []string{
		"python3 -m unittest",
		"python3 -B -m unittest",
		"PYTHONDONTWRITEBYTECODE=1 python3 -m unittest",
		"python3 -B -m pytest",
		"python3 -B -m unittest > out.log 2>&1",
		"python3 -B -m unittest 2>&1 | tail -n 20",
	} {
		if _, err := Analyze(cmd, clean); err != nil {
			t.Errorf("%s: refused with no cache: %v", cmd, err)
		}
	}
	cwd := crw1178Project(t, map[string][]byte{"__pycache__/calc.cpython-312.pyc": append(pycHeader(0), "code"...)})
	if _, err := Analyze("python3 -B -m unittest", cwd); err == nil {
		t.Fatal("a run beside a cache was allowed")
	}
	if _, err := Analyze("rm -rf __pycache__", cwd); err != nil {
		t.Fatalf("the route's removal was refused: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(cwd, "__pycache__")); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"python3 -B -m unittest", "PYTHONDONTWRITEBYTECODE=1 python3 -m unittest"} {
		if _, err := Analyze(cmd, cwd); err != nil {
			t.Errorf("%s: refused after a separate command removed the cache: %v", cmd, err)
		}
	}
}

// CRW-1178 verification round 8 (P1, already at the base): PYTHONPYCACHEPREFIX makes the interpreter read cache entries from
// <prefix>/<source directory>/, even with -B, and the inventory never walks there. A module run is refused when the variable is
// assigned for the run, set, exported or otherwise named by the text before it, set by a command whose names the reader does not
// know, or set in the environment the reader is given; the reason names the variable and the route. -E and -I make the interpreter
// ignore it.
func TestCRW1178PycachePrefixRefusesTheModuleRun(t *testing.T) {
	cwd := crw1178Project(t, nil)
	pfx := filepath.Join(t.TempDir(), "pfx")
	refused := func(t *testing.T, cmd string, lookup func(string) (string, bool)) {
		t.Helper()
		_, err := AnalyzeEnv(cmd, cwd, lookup)
		var u *Unreadable
		if !errors.As(err, &u) {
			t.Errorf("%s: a module run that may read caches under PYTHONPYCACHEPREFIX was allowed: %v", cmd, err)
		} else if !strings.Contains(u.Reason, "PYTHONPYCACHEPREFIX") || !strings.Contains(u.Reason, "unset PYTHONPYCACHEPREFIX") {
			t.Errorf("%s: the reason lacks the variable or the route: %q", cmd, u.Reason)
		}
	}
	for _, cmd := range []string{
		"PYTHONPYCACHEPREFIX=" + pfx + " python3 -B -m unittest",
		"PYTHONPYCACHEPREFIX=" + pfx + " python3 -m unittest",
		"PYTHONPYCACHEPREFIX=\"$CRW1178_UNSET\" python3 -B -m unittest",
		"PYTHONPYCACHEPREFIX= python3 -B -m unittest",
		"export PYTHONPYCACHEPREFIX=" + pfx + "; python3 -B -m unittest",
		"export PYTHONPYCACHEPREFIX=/tmp/zz; python3 -B -m unittest",
		"PYTHONPYCACHEPREFIX=" + pfx + "; export PYTHONPYCACHEPREFIX; python3 -B -m unittest",
		"declare -x PYTHONPYCACHEPREFIX=" + pfx + "; python3 -B -m unittest",
		"set -a; PYTHONPYCACHEPREFIX=" + pfx + "; python3 -B -m unittest",
		"env PYTHONPYCACHEPREFIX=" + pfx + " python3 -B -m unittest",
		"eval 'export PYTHONPYCACHEPREFIX=" + pfx + "'; python3 -B -m unittest",
		"N=PYTHONPYCACHE; export ${N}PREFIX=" + pfx + "; python3 -B -m unittest",
		"export \"$CRW1178_UNSET\"; python3 -B -m unittest",
		"declare -x \"$CRW1178_UNSET\"=" + pfx + "; python3 -B -m unittest",
		"N=PYTHONPYCACHE; declare -n r=${N}PREFIX; r=" + pfx + "; export r; python3 -B -m unittest",
		"printf -v \"$CRW1178_UNSET\" %s " + pfx + "; python3 -B -m unittest",
		"read -r \"$CRW1178_UNSET\" < /dev/null; python3 -B -m unittest",
		"bash -c 'export PYTHONPYCACHEPREFIX=" + pfx + "; python3 -B -m unittest'",
		"PYTHONPYCACHEPREFIX=" + pfx + " python3 -B -m pytest",
		"PYTHONPYCACHEPREFIX=" + pfx + " python3 -B -m json.tool data.json",
		"PYTHONPYCACHEPREFIX=" + pfx + " python3 -m py_compile calc.py",
	} {
		refused(t, cmd, nil)
	}
	// Set in the environment the reader is given (the hook's): refused, whatever the command says.
	env := func(k string) (string, bool) {
		if k == "PYTHONPYCACHEPREFIX" {
			return pfx, true
		}
		return "", false
	}
	for _, cmd := range []string{"python3 -B -m unittest", "python3 -m pytest", "PYTHONDONTWRITEBYTECODE=1 python3 -m unittest"} {
		refused(t, cmd, env)
	}
	// Not set, set empty (Python ignores an empty value), or ignored with -E or -I: as before.
	empty := func(k string) (string, bool) { return "", k == "PYTHONPYCACHEPREFIX" }
	gateLike := func(k string) (string, bool) {
		return map[string]string{"HOME": "/home/u", "PATH": "/usr/bin"}[k], k == "HOME" || k == "PATH"
	}
	for _, c := range []struct {
		cmd    string
		lookup func(string) (string, bool)
	}{
		{"python3 -B -m unittest", nil},
		{"export FOO=1; python3 -B -m unittest", nil},
		{"printf '%s\\n' \"$CRW1178_UNSET\"; python3 -B -m unittest", nil},
		{"python3 -B -m unittest", gateLike},
		{"python3 -B -m unittest", empty},
		{"python3 -E -B -m unittest", env},
		{"python3 -I -B -m unittest", env},
		{"PYTHONPYCACHEPREFIX=" + pfx + " python3 -E -B -m unittest", nil},
	} {
		if _, err := AnalyzeEnv(c.cmd, cwd, c.lookup); err != nil {
			t.Errorf("%s: refused: %v", c.cmd, err)
		}
	}
}

// CRW-1178 verification round 9 (P1): a name built at run time reaches PYTHONPYCACHEPREFIX through an expansion that assigns, which
// the text never spells. Under set -a, ${!N:=x} assigns and exports the variable N names; an arithmetic assignment, increment or
// let whose target is expanded at run time assigns the name it expands to; and bash evaluates a variable's value in an arithmetic
// context as an expression of its own, so x='PYTHONPYCACHEPREFIX=7' (built from parts) assigns it from $((x)), [[ x -eq 1 ]], an
// array subscript (a[x]=1, ${a[x]}, unset 'a[x]', printf -v 'a[x]', test -v 'a[x]'), a slice offset or an integer variable; and
// wait -p "$N" assigns a process id to the name N holds. A later module run is refused with the route, as for a declaration
// with a name the reader does not know (an expanded target of a parsed arithmetic assignment is a parse error, refused already);
// nothing in the text assigning through a built name leaves the run as before.
func TestCRW1178PycachePrefixThroughABuiltNameInAnExpansionIsRefused(t *testing.T) {
	cwd := crw1178Project(t, nil)
	const built = "set -a; N=PYTHONPYCACHE; N+=PREFIX; "
	const expr = "set -a; x=PYTHONPYCACHE; x+=PREFIX=7; "
	for _, cmd := range []string{
		built + ": ${!N:=../pfx}; python3 -B -m unittest",
		built + ": \"${!N:=../pfx}\"; python3 -B -m unittest",
		built + "echo ${!N=../pfx} >/dev/null; python3 -B -m unittest",
		built + "x=${!N:=../pfx}; python3 -B -m unittest",
		built + ": ${!N:=../pfx}; python3 -m pytest",
		"set -a; : ${!CRW1178_UNSET:=../pfx}; python3 -B -m unittest",
		built + "let \"$N=3\"; python3 -B -m unittest",
		expr + ": $((x)); python3 -B -m unittest",
		expr + "(( x )); python3 -B -m unittest",
		expr + "[[ x -eq 1 ]]; python3 -B -m unittest",
		expr + "let x; python3 -B -m unittest",
		expr + "s=abcdefgh; : ${s:x}; python3 -B -m unittest",
		expr + "a=(1); : ${a[x]}; python3 -B -m unittest",
		expr + "declare -i n; n=x; python3 -B -m unittest",
		expr + "b[x]=1; python3 -B -m unittest",
		expr + "b=([x]=1); python3 -B -m unittest",
		expr + ": $[x]; python3 -B -m unittest",
		expr + "case 1 in $((x))) ;; esac; python3 -B -m unittest",
		expr + "y=x; : $((y)); python3 -B -m unittest",
		"set -a; read -r x < /dev/null; (( x )); python3 -B -m unittest",
		expr + "[[ -v b[x] ]]; python3 -B -m unittest",
		expr + "b=(1); : ${#b[x]}; python3 -B -m unittest",
		expr + "b=(1); unset 'b[x]'; python3 -B -m unittest",
		expr + "printf -v 'b[x]' %s 1; python3 -B -m unittest",
		expr + "read 'b[x]' < /dev/null; python3 -B -m unittest",
		expr + "declare 'b[x]=1'; python3 -B -m unittest",
		expr + "b=(1); [ -v 'b[x]' ]; python3 -B -m unittest",
		expr + "b=(1); test -v 'b[x]'; python3 -B -m unittest",
		expr + "builtin let x; python3 -B -m unittest",
		expr + "cat > /dev/null <<E\n$((x))\nE\npython3 -B -m unittest",
		built + "sleep 0 & wait -n -p \"$N\"; python3 -B -m unittest",
	} {
		_, err := Analyze(cmd, cwd)
		var u *Unreadable
		if !errors.As(err, &u) {
			t.Errorf("%s: a module run after an assignment through a built name was allowed: %v", cmd, err)
		} else if !strings.Contains(u.Reason, "PYTHONPYCACHEPREFIX") || !strings.Contains(u.Reason, "unset PYTHONPYCACHEPREFIX") {
			t.Errorf("%s: the reason lacks the variable or the route: %q", cmd, u.Reason)
		}
	}
	// Values that name each other many times over are followed within a bound, and refused past it rather than walked for long.
	fan := ""
	for i := 0; i < 8; i++ {
		fan += fmt.Sprintf("v%d='%s'; ", i, strings.TrimSpace(strings.Repeat(fmt.Sprintf("v%d ", i+1), 16)))
	}
	start := time.Now()
	var fanErr *Unreadable
	if _, err := Analyze(fan+"(( v0 )); python3 -B -m unittest", cwd); !errors.As(err, &fanErr) {
		t.Errorf("values that name each other many times over were cleared: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("following values that name each other took %v", d)
	}
	// An expanded target of an arithmetic assignment or increment in parsed arithmetic is a parse error, already unreadable.
	for _, cmd := range []string{
		built + ": $(( $N = 5 )); python3 -B -m unittest",
		built + "(( $N++ )); python3 -B -m unittest",
		built + "for (( $N=1; 0; )); do :; done; python3 -B -m unittest",
		"set -a; : $(( $CRW1178_UNSET = 5 )); python3 -B -m unittest",
	} {
		var u *Unreadable
		if _, err := Analyze(cmd, cwd); !errors.As(err, &u) {
			t.Errorf("%s: allowed: %v", cmd, err)
		}
	}
	for _, cmd := range []string{
		built + ": ${!N:=../pfx}; python3 -E -B -m unittest",
		": ${n:=1}; python3 -B -m unittest",
		"i=1; : $((i + 1)); python3 -B -m unittest",
		"(( 2 + 3 )); python3 -B -m unittest",
		"(( i = 1 )); python3 -B -m unittest",
		"[[ 1 -eq 1 ]]; python3 -B -m unittest",
		"s=abcdefgh; : ${s:2:3}; python3 -B -m unittest",
		": ${!CRW1178_UNSET}; python3 -B -m unittest",
		"b=(1 2); echo \"${b[@]}\" \"${b[1]}\"; python3 -B -m unittest",
		"unset FOO; [ -v HOME ]; python3 -B -m unittest",
		"read -r line < /dev/null; python3 -B -m unittest",
		"sleep 0 & wait; python3 -B -m unittest",
	} {
		if _, err := Analyze(cmd, cwd); err != nil {
			t.Errorf("%s: refused: %v", cmd, err)
		}
	}
}
