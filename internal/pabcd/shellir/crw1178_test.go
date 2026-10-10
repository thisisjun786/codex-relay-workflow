package shellir

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// CRW-1178 (S2R2-F1): the first python -m unittest creates __pycache__, and every later run was refused. The interpreter runs the
// .py beside the cache, which the reader already reads, so a cache of a readable source is no reason to refuse.
func TestCRW1178PycacheOfReadableSourceIsNotRefused(t *testing.T) {
	cache := map[string][]byte{
		"__pycache__/calc.cpython-312.pyc":       append(pycHeader(0), "code"...),
		"__pycache__/test_calc.cpython-312.pyc":  append(pycHeader(0), "code"...),
		"__pycache__/calc.cpython-312.opt-1.pyc": append(pycHeader(3), "code"...),
	}
	for _, cmd := range []string{
		"python3 -m unittest",
		"python3 -B -m unittest",
		"PYTHONDONTWRITEBYTECODE=1 python3 -m unittest",
		"env PYTHONDONTWRITEBYTECODE=1 python3 -m unittest",
		"python3 -m pytest",
		"python3 -m unittest test_calc.py",
	} {
		t.Run(cmd, func(t *testing.T) {
			if _, err := Analyze(cmd, crw1178Project(t, cache)); err != nil {
				t.Fatalf("a cache of readable sources was refused: %v", err)
			}
		})
	}
	// A package keeps its cache below the package directory, beside its own source.
	cwd := crw1178Project(t, map[string][]byte{"pkg/__pycache__/__init__.cpython-312.pyc": append(pycHeader(0), "x"...)})
	if err := os.WriteFile(filepath.Join(cwd, "pkg", "__init__.py"), []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Analyze("python3 -m unittest", cwd); err != nil {
		t.Fatalf("a package cache was refused: %v", err)
	}
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
