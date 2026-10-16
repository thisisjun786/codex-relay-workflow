package tmploc

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// env is a getenv over a fixed map that also records which names were asked for.
type env struct {
	vars  map[string]string
	asked []string
}

func (e *env) get(name string) string {
	e.asked = append(e.asked, name)
	return e.vars[name]
}

// Lookup hands back TMPDIR's text exactly as written (no cleaning, no validation) and reads no
// other variable; an unset or empty TMPDIR is the default /tmp, reported as not from the
// environment (G-D1 = A, Q63ANS1722).
func TestLookup_isTheRawValue(t *testing.T) {
	cases := []struct {
		name     string
		vars     map[string]string
		want     string
		wantFrom bool
	}{
		{"relative", map[string]string{"TMPDIR": "rel/dir"}, "rel/dir", true},
		{"trailing slash", map[string]string{"TMPDIR": "/x/y/"}, "/x/y/", true},
		{"doubled separator", map[string]string{"TMPDIR": "/x//y"}, "/x//y", true},
		{"unset", map[string]string{}, "/tmp", false},
		{"empty", map[string]string{"TMPDIR": ""}, "/tmp", false},
		{"TMP and TEMP are not read", map[string]string{"TMP": "/a", "TEMP": "/b"}, "/tmp", false},
	}
	for _, c := range cases {
		e := &env{vars: c.vars}
		got, from := Lookup(e.get)
		if got != c.want || from != c.wantFrom {
			t.Errorf("%s: Lookup = (%q, %v), want (%q, %v)", c.name, got, from, c.want, c.wantFrom)
		}
		if len(e.asked) != 1 || e.asked[0] != "TMPDIR" {
			t.Errorf("%s: Lookup read %q, want only TMPDIR", c.name, e.asked)
		}
	}
	if got, from := Lookup(nil); got != DefaultDir || from {
		t.Errorf("Lookup(nil) = (%q, %v), want (%q, false): no environment is an unset TMPDIR", got, from, DefaultDir)
	}
	if DefaultDir != "/tmp" {
		t.Errorf("DefaultDir = %q, want /tmp", DefaultDir)
	}
}

// An absolute TMPDIR is the directory, as written, from source env.
func TestDirFrom_isTMPDIRAsWritten(t *testing.T) {
	for _, v := range []string{"/scratch/t", "/x/y/", "/x//y", "/"} {
		e := &env{vars: map[string]string{"TMPDIR": v}}
		dir, source, err := DirFrom(e.get)
		if err != nil || dir != v || source != SourceEnv {
			t.Errorf("DirFrom(TMPDIR=%q) = (%q, %q, %v), want (%q, %q, nil)", v, dir, source, err, v, SourceEnv)
		}
	}
}

// A TMPDIR that is not absolute stops with its reason; nothing else is chosen in its place.
func TestDirFrom_relativeIsAnError(t *testing.T) {
	for _, v := range []string{"rel", "./x", "../x", "~/tmp", "x/"} {
		e := &env{vars: map[string]string{"TMPDIR": v}}
		dir, source, err := DirFrom(e.get)
		want := `tmpdir_not_absolute: TMPDIR="` + v + `"; set TMPDIR to an absolute directory`
		if err == nil || err.Error() != want {
			t.Errorf("DirFrom(TMPDIR=%q) error = %v, want %q", v, err, want)
		}
		if dir != "" {
			t.Errorf("DirFrom(TMPDIR=%q) dir = %q with the error, want none", v, dir)
		}
		if source != SourceEnv {
			t.Errorf("DirFrom(TMPDIR=%q) source = %q, want %q", v, source, SourceEnv)
		}
	}
}

// G-D1 = A: an unset or empty TMPDIR is /tmp with source default.
func TestDirFrom_unsetIsTheDefault(t *testing.T) {
	for name, vars := range map[string]map[string]string{"unset": {}, "empty": {"TMPDIR": ""}} {
		e := &env{vars: vars}
		dir, source, err := DirFrom(e.get)
		if err != nil || dir != "/tmp" || source != SourceDefault {
			t.Errorf("%s: DirFrom = (%q, %q, %v), want (/tmp, %q, nil)", name, dir, source, err, SourceDefault)
		}
	}
	if SourceDefault != "default" || SourceEnv != "env" {
		t.Errorf("sources = (%q, %q), want (env, default)", SourceEnv, SourceDefault)
	}
}

// Dir and Env read the process environment through the same one place.
func TestDir_andEnv_readTheProcessTMPDIR(t *testing.T) {
	t.Setenv("TMPDIR", "/some/where/")
	if dir, err := Dir(); err != nil || dir != "/some/where/" {
		t.Errorf("Dir() = (%q, %v), want (/some/where/, nil)", dir, err)
	}
	if got := Env(); got != "/some/where/" {
		t.Errorf("Env() = %q, want the raw value", got)
	}
	t.Setenv("TMPDIR", "rel")
	if _, err := Dir(); err == nil || !strings.HasPrefix(err.Error(), "tmpdir_not_absolute: ") {
		t.Errorf("Dir() with a relative TMPDIR = %v, want tmpdir_not_absolute", err)
	}
	if got := Env(); got != "rel" {
		t.Errorf("Env() = %q, want the raw value", got)
	}
	t.Setenv("TMPDIR", "")
	if dir, err := Dir(); err != nil || dir != DefaultDir {
		t.Errorf("Dir() unset = (%q, %v), want (%q, nil)", dir, err, DefaultDir)
	}
	if got := Env(); got != "" {
		t.Errorf("Env() unset = %q, want the raw empty value", got)
	}
}

// An empty or relative parent is refused before anything is made: "" never means "TMPDIR,
// decided elsewhere", and a relative parent never lands below the working directory.
func TestMkdirTempIn_refusesAnEmptyOrRelativeParent(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)
	for _, parent := range []string{"", "rel", "./x", "x/y"} {
		dir, err := MkdirTempIn(parent, "crw-x-")
		want := `temp_parent_not_absolute: parent ` + quote(parent) + `; give an absolute directory`
		if err == nil || err.Error() != want {
			t.Errorf("MkdirTempIn(%q) error = %v, want %q", parent, err, want)
		}
		if dir != "" {
			t.Errorf("MkdirTempIn(%q) = %q with the error, want none", parent, dir)
		}
	}
	if entries, err := os.ReadDir(work); err != nil || len(entries) != 0 {
		t.Errorf("the working directory holds %v (%v), want nothing made there", entries, err)
	}
	parent := t.TempDir()
	dir, err := MkdirTempIn(parent, "crw-x-")
	if err != nil || filepath.Dir(dir) != parent || !strings.HasPrefix(filepath.Base(dir), "crw-x-") {
		t.Fatalf("MkdirTempIn(%q) = (%q, %v), want a crw-x- directory directly below it", parent, dir, err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Errorf("MkdirTempIn made %q: %v", dir, err)
	}
	if _, err := MkdirTempIn(filepath.Join(parent, "missing"), "crw-x-"); err == nil || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("MkdirTempIn below a missing parent = %v, want the not-exist error", err)
	}
}

func quote(s string) string { return `"` + s + `"` }

// Check refuses a TMPDIR that is a file, a missing directory or a directory it cannot write, and
// says which directory, from which source, and why.
func TestCheck_refusesAFileAMissingDirAndAnUnwritableDir_withTheReason(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(base, "missing")
	cases := []struct {
		name, tmpdir, why string
	}{
		{"file", file, "is not a directory"},
		{"missing", missing, "does not exist"},
	}
	if os.Geteuid() != 0 {
		ro := filepath.Join(base, "ro")
		if err := os.Mkdir(ro, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
		cases = append(cases, struct{ name, tmpdir, why string }{"unwritable", ro, "is not writable"})
	} else {
		t.Log("running as root: the unwritable row cannot be built (root writes everywhere)")
	}
	for _, c := range cases {
		t.Setenv("TMPDIR", c.tmpdir)
		got, err := Check()
		if err == nil {
			t.Errorf("%s: Check() = %q, want an error", c.name, got)
			continue
		}
		msg := err.Error()
		prefix := "temp_dir_unusable: " + c.tmpdir + " (env): "
		if !strings.HasPrefix(msg, prefix) || !strings.Contains(msg, c.why) || !strings.HasSuffix(msg, "; set TMPDIR to a writable directory") {
			t.Errorf("%s: Check() error = %q, want %q...%q...; set TMPDIR to a writable directory", c.name, msg, prefix, c.why)
		}
	}
	t.Setenv("TMPDIR", "rel")
	if _, err := Check(); err == nil || !strings.HasPrefix(err.Error(), "tmpdir_not_absolute: ") {
		t.Errorf("Check() with a relative TMPDIR = %v, want tmpdir_not_absolute", err)
	}
	// a usable TMPDIR comes back resolved, and the probe leaves nothing behind
	target := t.TempDir()
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", link)
	got, err := Check()
	want, _ := filepath.EvalSymlinks(target)
	if err != nil || got != want {
		t.Errorf("Check() = (%q, %v), want (%q, nil)", got, err, want)
	}
	if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
		t.Errorf("Check left %v (%v) in the temp dir", entries, err)
	}
}

// longDir returns a directory below t.TempDir() whose path is at least n bytes long.
func longDir(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	for len(dir) < n {
		seg := n - len(dir) - 1
		if seg > 200 {
			seg = 200
		}
		if seg < 1 {
			seg = 1
		}
		dir = filepath.Join(dir, strings.Repeat("l", seg))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// MkdirTemp and CreateTemp make their entry directly below TMPDIR, whatever its length: no length
// rule moves them elsewhere.
func TestMkdirTemp_isBelowDir_whateverItsLength(t *testing.T) {
	for _, n := range []int{0, 120, 200} {
		parent := t.TempDir()
		if n > 0 {
			parent = longDir(t, n)
		}
		t.Setenv("TMPDIR", parent)
		dir, err := MkdirTemp("crw-m-")
		if err != nil || filepath.Dir(dir) != parent || !strings.HasPrefix(filepath.Base(dir), "crw-m-") {
			t.Errorf("TMPDIR %d bytes: MkdirTemp = (%q, %v), want a crw-m- directory directly below %q", len(parent), dir, err, parent)
		}
		f, err := CreateTemp("crw-c-*.txt")
		if err != nil {
			t.Errorf("TMPDIR %d bytes: CreateTemp: %v", len(parent), err)
			continue
		}
		name := f.Name()
		_ = f.Close()
		if filepath.Dir(name) != parent || !strings.HasPrefix(filepath.Base(name), "crw-c-") || !strings.HasSuffix(name, ".txt") {
			t.Errorf("TMPDIR %d bytes: CreateTemp = %q, want a crw-c-*.txt file directly below %q", len(parent), name, parent)
		}
	}
	// a failure names the location and its source, and keeps the cause
	missing := filepath.Join(t.TempDir(), "missing")
	t.Setenv("TMPDIR", missing)
	_, err := MkdirTemp("crw-m-")
	if err == nil || !strings.HasPrefix(err.Error(), "temp_dir_unusable: "+missing+" (env): ") || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("MkdirTemp under a missing TMPDIR = %v, want temp_dir_unusable naming it and wrapping the not-exist error", err)
	}
	if _, err := CreateTemp("crw-c-"); err == nil || !strings.HasPrefix(err.Error(), "temp_dir_unusable: "+missing+" (env): ") {
		t.Errorf("CreateTemp under a missing TMPDIR = %v, want temp_dir_unusable naming it", err)
	}
	t.Setenv("TMPDIR", "rel")
	if _, err := MkdirTemp("crw-m-"); err == nil || !strings.HasPrefix(err.Error(), "tmpdir_not_absolute: ") {
		t.Errorf("MkdirTemp with a relative TMPDIR = %v, want tmpdir_not_absolute", err)
	}
	if _, err := CreateTemp("crw-c-"); err == nil || !strings.HasPrefix(err.Error(), "tmpdir_not_absolute: ") {
		t.Errorf("CreateTemp with a relative TMPDIR = %v, want tmpdir_not_absolute", err)
	}
}
