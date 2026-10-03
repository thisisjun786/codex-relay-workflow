package agy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWorkDirIsEmptyAndRemoved: agy starts in a new empty directory under WorkRoot, and nothing is left there whichever way the call ends.
func TestWorkDirIsEmptyAndRemoved(t *testing.T) {
	for _, c := range []struct {
		name  string
		spec  fakeSpec
		limit time.Duration
		want  Class
	}{
		{"normal", fakeSpec{Stdout: testdata(t, "success_schema.json")}, 0, ClassNormal},
		{"invalid", fakeSpec{Stdout: testdata(t, "denied_actions.json")}, 0, ClassInvalid},
		{"unavailable", fakeSpec{Exit: 2, Stderr: "panic: boom"}, 0, ClassUnavailable},
		{"time limit kill", fakeSpec{Sleep: time.Minute}, 1500 * time.Millisecond, ClassInvalid},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, rec := fakeCfg(t, c.spec)
			if c.limit > 0 {
				cfg.TimeLimitFloor, cfg.TimeLimitCeiling = c.limit, c.limit
			}
			if res := run(t, cfg, Request{Prompt: []byte("hi"), Schema: []byte(`{"type":"object"}`)}); res.Class != c.want {
				t.Fatalf("class %s (%s), want %s", res.Class, res.Reason, c.want)
			}
			got := rec()
			root, _ := filepath.EvalSymlinks(cfg.WorkRoot)
			if !strings.HasPrefix(got.Cwd, root+string(filepath.Separator)) || len(got.CwdEntries) != 0 {
				t.Errorf("agy started in %s with %v, want an empty directory under %s", got.Cwd, got.CwdEntries, root)
			}
			if left, err := os.ReadDir(cfg.WorkRoot); err != nil || len(left) != 0 {
				t.Errorf("left under the working root: %v (%v)", left, err)
			}
		})
	}
}
