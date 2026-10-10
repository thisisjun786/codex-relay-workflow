package shellir

import (
	"encoding/binary"
	"errors"
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

// CRW-1178 (S2R2-F1): the first python -m unittest creates __pycache__, and every later run was refused. A stale timestamp entry
// (its recorded time and size disagree with the source, as here) is discarded: the interpreter runs the .py beside it, which the
// reader already reads, so it is no reason to refuse. An entry that agrees with its source is refused (see
// TestCRW1178CacheThatMatchesItsSourceIsRefused).
func TestCRW1178StaleCacheOfReadableSourceIsNotRefused(t *testing.T) {
	cache := map[string][]byte{
		"__pycache__/calc.cpython-312.pyc":       append(pycHeader(0), "code"...),
		"__pycache__/test_calc.cpython-312.pyc":  append(pycHeader(0), "code"...),
		"__pycache__/calc.cpython-312.opt-1.pyc": append(pycHeader(0), "code"...),
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
// it is refused, with the file and a route that leaves no cache to load. Only a stale entry, which the interpreter discards and
// recompiles from the source the reader read, is skipped. A hash-based entry is refused whatever its hash.
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
				if !strings.Contains(u.Reason, "__pycache__/calc.cpython-312.pyc") || !strings.Contains(u.Reason, "-B") {
					t.Errorf("%s: the reason lacks the file or the route: %q", cmd, u.Reason)
				}
			}
		})
	}
	// A stale entry (the source changed after it was written) is discarded by the interpreter and is skipped.
	cwd := crw1178Project(t, nil)
	src := filepath.Join(cwd, "calc.py")
	h := matchingPycHeader(t, src)
	binary.LittleEndian.PutUint32(h[12:], binary.LittleEndian.Uint32(h[12:])+1)
	if err := os.MkdirAll(filepath.Join(cwd, "__pycache__"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "__pycache__", "calc.cpython-312.pyc"), append(h, "code"...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Analyze("python3 -m unittest", cwd); err != nil {
		t.Errorf("a stale cache entry (size differs) was refused: %v", err)
	}
}

// A command that changes a source's time or brings in a cache before the run could make a stale entry current after it was judged.
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
// never compares it with a source, so a stale header is no proof it is skipped. Only <module>.<tag>[.opt-N].pyc is a cache entry.
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

// CRW-1178 evaluation d2: a command that can change a source's modification time (through any name of it, such as a hard link)
// before the run could turn a stale entry judged now into one that agrees with its source. With a stale entry skipped, such a
// command in the same text is refused; the same command with no cache, and unrelated commands, stay allowed.
func TestCRW1178TimestampMutationBesideStaleCacheIsRefused(t *testing.T) {
	cwd := crw1178Project(t, map[string][]byte{"__pycache__/calc.cpython-312.pyc": append(pycHeader(0), "code"...)})
	if err := os.Link(filepath.Join(cwd, "calc.py"), filepath.Join(cwd, "stamp")); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	for _, cmd := range []string{
		"touch -d @1 stamp && python3 -B -m unittest",
		"touch stamp; python3 -m unittest",
		"env touch -d @1 stamp && python3 -m unittest",
		"command touch stamp && python3 -m unittest",
		"truncate -s 33 stamp && python3 -m unittest",
		"echo stamp | xargs touch -d @1 && python3 -m unittest",
		"find . -name stamp -exec touch -d @1 {} + && python3 -m unittest",
		"cd . && touch stamp && python3 -m unittest",
	} {
		_, err := Analyze(cmd, cwd)
		var u *Unreadable
		if !errors.As(err, &u) {
			t.Errorf("%s: a source time change beside a stale cache was allowed: %v", cmd, err)
		}
	}
	// No cache entry is relied on: nothing to refresh, so the command is as before.
	clean := crw1178Project(t, nil)
	if err := os.WriteFile(filepath.Join(clean, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"touch notes.txt && python3 -B -m unittest", "truncate -s 0 notes.txt; python3 -m unittest"} {
		if _, err := Analyze(cmd, clean); err != nil {
			t.Errorf("%s: refused with no cache present: %v", cmd, err)
		}
	}
	// A command after the run cannot affect what was judged.
	if _, err := Analyze("python3 -B -m unittest && touch notes.txt", cwd); err != nil {
		t.Errorf("a touch after the run was refused: %v", err)
	}
}

// CRW-1178 verification round 3: touch and truncate are not the only commands that set a file's time. cp -p (or -a, --preserve),
// install -p, rsync -t, an archive extractor, a write redirection or any program the reader does not know can give the source (by a
// hard link) the time and size a stale entry records, and so make the entry current before the run. With a stale entry skipped, only
// commands known to change no file may run before the module, or beside it in a pipeline or a coprocess.
func TestCRW1178MetadataChangeBesideStaleCacheIsRefused(t *testing.T) {
	cwd := crw1178Project(t, map[string][]byte{"__pycache__/calc.cpython-312.pyc": append(pycHeader(0), "code"...)})
	if err := os.Link(filepath.Join(cwd, "calc.py"), filepath.Join(cwd, "stamp")); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	for name, body := range map[string]string{"donor": "def add(a, b):\n    return a + b\n", "run.sh": "cp -p donor stamp\n"} {
		if err := os.WriteFile(filepath.Join(cwd, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, cmd := range []string{
		"cp -p donor stamp && python3 -B -m unittest",
		"cp -a donor stamp; python3 -m unittest",
		"cp --preserve=timestamps donor stamp && python3 -m pytest",
		"install -p donor stamp && python3 -m unittest",
		"rsync -t donor stamp && python3 -m unittest",
		"tar -xf calc.tar && python3 -m unittest",
		"cat donor > stamp && python3 -m unittest",
		"printf 'x' >> stamp; python3 -m unittest",
		"echo x >& stamp; python3 -m unittest",
		"python3 -c 'import os; os.utime(\"stamp\", (1, 1))' && python3 -m unittest",
		"bash run.sh && python3 -m unittest",
		"sh -c 'cp -p donor stamp' && python3 -m unittest",
		"python3 -B -m unittest | touch -d @1 stamp",
		"python3 -B -m unittest 2>&1 | cp -p donor stamp",
		"coproc python3 -B -m unittest; cp -p donor stamp",
		"./ls && python3 -m unittest",
		"LS && python3 -m unittest",
		"cat() { cp -p donor stamp; }; cat donor && python3 -m unittest",
		"echo stamp | xargs cp -p donor; python3 -m unittest",
		"find . -name stamp -exec cp -p donor {} \\; ; python3 -m unittest",
	} {
		_, err := Analyze(cmd, cwd)
		var u *Unreadable
		if !errors.As(err, &u) {
			t.Errorf("%s: a possible source time change beside a stale cache was allowed: %v", cmd, err)
		} else if !strings.Contains(u.Reason, "stale only until") && !strings.Contains(u.Reason, "rewrite") {
			t.Errorf("%s: refused for another reason: %q", cmd, u.Reason)
		}
	}
	// Commands that change no file stay allowed beside a stale entry, as does any command once the run is over.
	for _, cmd := range []string{
		"cd . && python3 -m unittest",
		"true; pwd; echo start; python3 -B -m unittest",
		"ls > /dev/null 2>&1 && python3 -m unittest",
		"python3 -B -m unittest 2>&1 | tail -n 20",
		"python3 -B -m unittest && cp -p donor other",
		"env PYTHONDONTWRITEBYTECODE=1 python3 -m unittest",
	} {
		if _, err := Analyze(cmd, cwd); err != nil {
			t.Errorf("%s: refused beside a stale cache: %v", cmd, err)
		}
	}
}

// CRW-1178 verification round 4 (P1): pytest's assertion rewriting writes and loads __pycache__/<module>.<tag>-pytest-<version>.pyc
// beside the test module it rewrote (the version has dots: test_calc.cpython-312-pytest-8.3.2.pyc). Such an entry belongs to
// <module>.py in the directory above exactly as an interpreter entry does, so a stale one (pytest discards it and rewrites the source
// the reader reads) is skipped again, while one that agrees with its source or carries other flags, and any other shape, stays refused.
func TestCRW1178PytestRewriteCacheFollowsItsSource(t *testing.T) {
	names := []string{
		"__pycache__/test_calc.cpython-312-pytest-8.3.2.pyc",
		"__pycache__/test_calc.cpython-314-pytest-9.0.2.pyc",
		"__pycache__/calc.cpython-313t-pytest-8.4.0.dev45+g1234abc.pyc",
		"__pycache__/test_calc.pypy310-pytest-7.4.4.pyc",
	}
	for _, name := range names {
		t.Run("stale "+name, func(t *testing.T) {
			cwd := crw1178Project(t, map[string][]byte{name: append(pycHeader(0), "code"...)})
			for _, cmd := range []string{"python3 -m pytest", "python3 -B -m pytest", "python3 -m unittest"} {
				if _, err := Analyze(cmd, cwd); err != nil {
					t.Errorf("%s: a stale pytest cache of a readable source was refused: %v", cmd, err)
				}
			}
		})
		t.Run("current "+name, func(t *testing.T) {
			cwd := crw1178Project(t, nil)
			stem := strings.SplitN(filepath.Base(name), ".", 2)[0]
			for _, header := range [][]byte{matchingPycHeader(t, filepath.Join(cwd, stem+".py")), pycHeader(3)} {
				p := filepath.Join(cwd, name)
				if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, append(header, "code"...), 0o600); err != nil {
					t.Fatal(err)
				}
				_, err := Analyze("python3 -B -m pytest", cwd)
				var u *Unreadable
				if !errors.As(err, &u) || !strings.Contains(u.Reason, filepath.Base(name)) || !strings.Contains(u.Reason, "-B") {
					t.Errorf("a pytest cache that agrees with its source or is not a timestamp entry was allowed or lacks the file: %v", err)
				}
			}
		})
	}
	for _, name := range []string{
		"__pycache__/test_calc.cpython-312-pytest-.pyc",
		"__pycache__/test_calc.cpython-312-pytest-8..3.pyc",
		"__pycache__/test_calc.cpython-312-pytest-x.pyc",
		"__pycache__/test_calc.cpython-312-pytest-8.3.2-1.pyc",
		"__pycache__/test_calc.cpython-312-pytest-8.3.2..pyc",
		"__pycache__/test_calc.cpython-312-pytest-8.3.2.opt-1.pyc",
		"__pycache__/test_calc.pytest-8.3.2.pyc",
		"__pycache__/test_calc.x.cpython-312-pytest-8.3.2.pyc",
		"__pycache__/nosource.cpython-312-pytest-8.3.2.pyc",
	} {
		t.Run("refused "+name, func(t *testing.T) {
			cwd := crw1178Project(t, map[string][]byte{name: append(pycHeader(0), "code"...)})
			_, err := Analyze("python3 -m pytest", cwd)
			var u *Unreadable
			if !errors.As(err, &u) || !strings.Contains(u.Reason, filepath.Base(name)) {
				t.Errorf("a malformed pytest cache name or one without a source was skipped: %v", err)
			}
		})
	}
}

// CRW-1178 verification round 4 (P2): Python compares int(st_mtime), and st_mtime is a double that rounds a time just below a whole
// second up to it (1700000000.999999999 s reads 1700000001.0). An entry recording the next second then agrees with the source and
// runs, so it is refused; a time two seconds off stays stale.
func TestCRW1178CacheTimeRoundedUpBySecondsAsDoubleIsRefused(t *testing.T) {
	cwd := crw1178Project(t, nil)
	src := filepath.Join(cwd, "calc.py")
	mtime := time.Unix(1700000000, 999999999)
	if err := os.Chtimes(src, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(src); err != nil || fi.ModTime().Nanosecond() != 999999999 {
		t.Skipf("no nanosecond times here: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cwd, "__pycache__"), 0o700); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(cwd, "__pycache__", "calc.cpython-312.pyc")
	for _, c := range []struct {
		sec     uint32
		refused bool
	}{{1700000000, true}, {1700000001, true}, {1700000002, false}, {1699999999, false}} {
		h := matchingPycHeader(t, src)
		binary.LittleEndian.PutUint32(h[8:], c.sec)
		if err := os.WriteFile(entry, append(h, "code"...), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Analyze("python3 -B -m unittest", cwd)
		if refused := err != nil; refused != c.refused {
			t.Errorf("entry time %d: refused %v, want %v (%v)", c.sec, refused, c.refused, err)
		}
	}
}

// CRW-1178 verification rounds 5 and 6 (P0): the shell opens the module run's own write redirections before the interpreter starts,
// so "python3 -B -m unittest > stamp" truncates the source through its hard link stamp (size 0, time now) and can make a stale entry
// recording that size and second current. A target reached through a directory link and '..' (../lnk/../sl, which the kernel
// resolves after the link and a text clean does not) names the source by a symlink or a hard link that no path check of the reader
// sees. Beside a stale entry, the run itself may write only to /dev/null and descriptor copies or closes; any file target is refused
// with the route, a new log file included. With no stale entry skipped the same redirections stay allowed.
func TestCRW1178RunRedirectThroughLinkBesideStaleCacheIsRefused(t *testing.T) {
	cwd := crw1178Project(t, map[string][]byte{"__pycache__/calc.cpython-312.pyc": append(pycHeader(0), "code"...)})
	if err := os.Link(filepath.Join(cwd, "calc.py"), filepath.Join(cwd, "stamp")); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "notes.log"), []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	alias := filepath.Join(other, "alias")
	if err := os.Symlink(filepath.Join(cwd, "calc.py"), alias); err != nil {
		t.Fatal(err)
	}
	// Outside the project, so the inventory never walks them: lnk -> ext/deep, ext/sl -> calc.py and ext/hard, a hard link of
	// calc.py. From cwd, ../lnk/../sl is ext/sl for the kernel and <parent>/sl (missing) for a text clean.
	parent := filepath.Dir(cwd)
	if err := os.MkdirAll(filepath.Join(parent, "ext", "deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(parent, "ext", "deep"), filepath.Join(parent, "lnk")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(cwd, "calc.py"), filepath.Join(parent, "ext", "sl")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(cwd, "calc.py"), filepath.Join(parent, "ext", "hard")); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{
		"python3 -B -m unittest > stamp",
		"python3 -B -m unittest 2> stamp",
		"sleep 3; python3 -B -m unittest > stamp",
		"sleep 3; python3 -B -m unittest 2> stamp",
		"python3 -m unittest >> stamp",
		"python3 -B -m unittest &> stamp",
		"python3 -B -m unittest >& stamp",
		"python3 -B -m unittest >| stamp",
		"python3 -B -m unittest <> stamp",
		"python3 -B -m unittest 1>stamp 2>&1",
		"python3 -B -m unittest 2>&1 > stamp",
		"python3 -B -m unittest > " + alias,
		"python3 -B -m pytest > stamp",
		"cd . && python3 -B -m unittest 2> stamp | tail -n 5",
		"timeout 60 python3 -B -m unittest > stamp",
		"env python3 -B -m unittest 2> stamp",
		"python3 -B -m unittest > \"$CRW1178_UNSET_TARGET\"",
		// A symlink and a hard link of the source reached through a directory link and '..'.
		"python3 -B -m unittest > ../lnk/../sl",
		"python3 -B -m unittest 2> ../lnk/../sl",
		"python3 -B -m unittest >> ../lnk/../sl",
		"sleep 3; python3 -B -m unittest > ../lnk/../sl",
		"sleep 3; python3 -B -m unittest 2> ../lnk/../sl",
		"python3 -B -m unittest > ../lnk/../hard",
		"python3 -B -m unittest 2> ../lnk/../hard",
		"python3 -B -m unittest >> ../lnk/../hard",
		"sleep 3; python3 -B -m unittest > ../lnk/../hard",
		// Any other file: new or existing, in the project or not.
		"python3 -B -m unittest > out.log 2>&1",
		"python3 -B -m unittest >> notes.log",
		"sleep 1; python3 -B -m unittest > out.log",
		"python3 -B -m unittest 2> " + filepath.Join(other, "new.log"),
		"{ python3 -B -m unittest; } > out.log",
	} {
		_, err := Analyze(cmd, cwd)
		var u *Unreadable
		if !errors.As(err, &u) {
			t.Errorf("%s: a write to a file beside a stale cache was allowed: %v", cmd, err)
		} else if !strings.Contains(u.Reason, "stale only until") && !strings.Contains(u.Reason, "rewrite") && !strings.Contains(u.Reason, "rewritten") {
			t.Errorf("%s: refused for another reason: %q", cmd, u.Reason)
		} else if strings.Contains(u.Reason, "stale only until") && !strings.Contains(u.Reason, "separate command first; python -B") {
			t.Errorf("%s: the reason lacks the route: %q", cmd, u.Reason)
		}
	}
	for _, cmd := range []string{
		"python3 -B -m unittest > /dev/null 2>&1",
		"python3 -B -m unittest 2>&1 >/dev/null",
		"python3 -B -m unittest 2>/dev/null",
		"python3 -B -m unittest 2>&1 | tail -n 20",
		"python3 -B -m unittest 1>&2",
		"python3 -B -m unittest 2>&-",
		"python3 -B -m unittest >&-",
		"python3 -B -m unittest < notes.log",
	} {
		if _, err := Analyze(cmd, cwd); err != nil {
			t.Errorf("%s: refused beside a stale cache: %v", cmd, err)
		}
	}
	// No stale entry skipped: a log file of the run is as before.
	fresh := crw1178Project(t, nil)
	for _, cmd := range []string{
		"python3 -B -m unittest > out.log 2>&1",
		"sleep 1; python3 -B -m unittest 2> out.log",
		"python3 -m unittest >> notes.log",
	} {
		if _, err := Analyze(cmd, fresh); err != nil {
			t.Errorf("%s: refused with no cache: %v", cmd, err)
		}
	}
}
