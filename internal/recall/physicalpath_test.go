package recall

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// recallPhysicalTree builds root/real/a/b and root/link -> real/a/b, enters link
// by its absolute path (so PWD keeps the logical spelling) and points HOME,
// CODEX_HOME and CRW_HOME at temporary directories.
func recallPhysicalTree(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "real", "a", "b"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(root, "real", "a", "b"), link); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"HOME", "CODEX_HOME", "CRW_HOME"} {
		t.Setenv(key, t.TempDir())
	}
	t.Chdir(link)
	if cwd, err := os.Getwd(); err != nil || cwd != link {
		t.Fatalf("premise: Getwd=%q err=%v, want the logical %q", cwd, err, link)
	}
	return root
}

// recallPhysicalStore writes a memories store whose jobs are all exhausted and requeueable.
func recallPhysicalStore(t *testing.T, dir string, keys ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := openDbReadWrite(filepath.Join(dir, "memories_1.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	recallSQL(t, db, "CREATE TABLE jobs (kind TEXT NOT NULL, job_key TEXT NOT NULL, status TEXT NOT NULL, retry_remaining INTEGER NOT NULL, retry_at INTEGER, last_error TEXT, input_watermark INTEGER, last_success_watermark INTEGER, finished_at INTEGER)")
	for _, key := range keys {
		recallSQL(t, db, "INSERT INTO jobs(kind, job_key, status, retry_remaining, retry_at, last_error, input_watermark, last_success_watermark) VALUES ('memory_stage1','"+key+"','error',0,999,'capacity',10,5)")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// The physical store holds one exhausted job and the store beside the symlink two,
// so the count tells which of them --home ../store named.
func TestRecallPhysicalMemoryHome(t *testing.T) {
	root := recallPhysicalTree(t)
	physical, logical := filepath.Join(root, "real", "a", "store"), filepath.Join(root, "store")
	recallPhysicalStore(t, physical, "p1")
	recallPhysicalStore(t, logical, "l1", "l2")
	exhausted := func(home string) int {
		code, out, errs := recallCLIInvoke(t, []string{"memory", "status", "--json", "--home", home}, time.Now())
		var status struct{ Exhausted int }
		if code != 0 || json.Unmarshal([]byte(out), &status) != nil {
			t.Fatalf("status %s: %d %q %q", home, code, out, errs)
		}
		return status.Exhausted
	}
	t.Run("status", func(t *testing.T) {
		if got := exhausted("../store"); got != 1 {
			t.Fatalf("--home ../store read %d exhausted jobs, want the physical store's 1", got)
		}
		if got := exhausted(logical); got != 2 {
			t.Fatalf("an absolute --home read %d exhausted jobs, want 2", got)
		}
	})
	t.Run("requeue", func(t *testing.T) {
		untouched, rows := memoryStatusFiles(t, logical), requeueTestRows(t, physical)
		code, out, errs := recallCLIInvoke(t, []string{"memory", "requeue", "--apply", "--json", "--home", "../store"}, time.Now())
		if code != 0 {
			t.Fatalf("requeue: %d %q %q", code, out, errs)
		}
		if !reflect.DeepEqual(memoryStatusFiles(t, logical), untouched) {
			t.Fatal("requeue --apply --home ../store changed the store beside the symlink")
		}
		if reflect.DeepEqual(requeueTestRows(t, physical), rows) {
			t.Fatal("requeue --apply --home ../store left the physical store unchanged")
		}
	})
}

func TestRecallPhysicalCodexHome(t *testing.T) {
	root := recallPhysicalTree(t)
	t.Setenv("CODEX_HOME", "../store")
	got, err := codexHome()
	if want := filepath.Join(root, "real", "a", "store"); err != nil || got != want {
		t.Fatalf("codexHome()=%q err=%v, want %q", got, err, want)
	}
}
func TestRecallPhysicalAbs(t *testing.T) {
	root := recallPhysicalTree(t)
	b := filepath.Join(root, "real", "a", "b")
	// A symlink among the caller's own segments stays as written; only the base is physical.
	if err := os.Symlink(root, filepath.Join(b, "alias")); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ in, want string }{
		{"../store", filepath.Join(root, "real", "a", "store")},
		{"", b},
		{".", b},
		{"x/../y", filepath.Join(b, "y")},
		{"alias/store", filepath.Join(b, "alias", "store")},
		{"missing/deep", filepath.Join(b, "missing", "deep")},
		{"/abs/../p", "/p"},
	} {
		if got, err := RecallPhysicalAbs(c.in); err != nil || got != c.want {
			t.Errorf("RecallPhysicalAbs(%q)=%q err=%v, want %q", c.in, got, err, c.want)
		}
	}
}

// Without a symlink in the working directory the helper is filepath.Abs.
func TestRecallPhysicalAbsPlainDirectory(t *testing.T) {
	plain, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(plain)
	for _, in := range []string{"", ".", "../x", "a/b/../c", plain, "/abs/../p"} {
		want, _ := filepath.Abs(in)
		if got, err := RecallPhysicalAbs(in); err != nil || got != want {
			t.Errorf("RecallPhysicalAbs(%q)=%q err=%v, want %q", in, got, err, want)
		}
	}
}
