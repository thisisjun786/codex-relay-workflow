package agy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1579 (U-A5-4): the per-call directory's default root is tmploc's temporary directory. A TMPDIR that is
// set is used as given; a relative TMPDIR is tmploc's refusal, returned by Run before anything is made,
// where before it was made relative to the working directory without a word.
func TestRun_defaultWorkRootIsTmplocDir(t *testing.T) {
	schema := []byte(`{"type":"object"}`)
	cfg, rec := fakeCfg(t, fakeSpec{Stdout: testdata(t, "success_schema.json")})
	cfg.WorkRoot = ""
	tmp := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	res := run(t, cfg, Request{Prompt: []byte("hi"), Schema: schema})
	if res.Class != ClassNormal {
		t.Fatalf("with TMPDIR=%s: %s/%s: %s", tmp, res.Class, res.Reason, res.Detail)
	}
	if got := rec(); len(got.Args) != 9 || !strings.HasPrefix(got.Args[5], tmp+string(filepath.Separator)) {
		t.Errorf("the per-call directory is not below TMPDIR %s: %v", tmp, got.Args)
	}

	t.Chdir(cwd)
	t.Setenv("TMPDIR", "rel")
	_, err := try(cfg, Request{Prompt: []byte("hi"), Schema: schema})
	if err == nil || !strings.Contains(err.Error(), "tmpdir_not_absolute") || !strings.Contains(err.Error(), `TMPDIR="rel"`) {
		t.Errorf("a relative TMPDIR: Run answered %v, want tmploc's tmpdir_not_absolute", err)
	}
	if left, _ := os.ReadDir(cwd); len(left) != 0 {
		t.Errorf("a relative TMPDIR made %v below the working directory", left)
	}
}

// CRW-1579 (U-A5-4): the host-wide lock lives below XDG_STATE_HOME or the home. With neither there is no
// default: an error that names them and the ways to give the lock, never a path below the temporary
// directory (which is a location, not a state home).
func TestDefaultLockPath_withoutStateOrHome_isAnError(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "")
	path, err := DefaultLockPath()
	if err == nil {
		t.Fatalf("no XDG_STATE_HOME and no HOME: DefaultLockPath = %q, want an error", path)
	}
	for _, name := range []string{"XDG_STATE_HOME", "HOME", "CRW_REVIEW_LOCK", "--lock"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error %q does not name %s", err, name)
		}
	}
	if path != "" {
		t.Errorf("an error came with the path %q", path)
	}
	// Run asks for it only when the lock is not given, and returns the error before anything is made.
	cfg, _ := fakeCfg(t, fakeSpec{Stdout: testdata(t, "success_schema.json")})
	cfg.LockPath = ""
	if _, err := try(cfg, Request{Prompt: []byte("hi")}); err == nil || !strings.Contains(err.Error(), "XDG_STATE_HOME") {
		t.Errorf("Run with no lock and no home: %v, want the DefaultLockPath error", err)
	}
	if _, err := os.Stat(cfg.WorkRoot); err == nil {
		t.Errorf("Run made the working root %s although the lock had no default", cfg.WorkRoot)
	}

	t.Setenv("HOME", "/h")
	if path, err := DefaultLockPath(); err != nil || path != "/h/.local/state/crw/review/agy.lock" {
		t.Errorf("HOME=/h: %q, %v", path, err)
	}
	t.Setenv("XDG_STATE_HOME", "/s")
	if path, err := DefaultLockPath(); err != nil || path != "/s/crw/review/agy.lock" {
		t.Errorf("XDG_STATE_HOME=/s: %q, %v", path, err)
	}
}
