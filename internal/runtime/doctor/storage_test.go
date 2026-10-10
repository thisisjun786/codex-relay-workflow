package doctor_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
)

// CRW-958: `crw doctor storage` is the storage check `crw config check` runs, so a host reads one
// verdict from either command: the same report, exit 1 on a structural fault, exit 0 otherwise.
func TestDoctorStorageIsTheConfigCheck(t *testing.T) {
	home := t.TempDir()
	short, err := os.MkdirTemp("", "ds")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", short)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("CRW_CONFIG", "")
	var out, errOut bytes.Buffer
	if code := doctor.Run(context.Background(), []string{"storage"}, &out, &errOut); code != 0 {
		t.Fatalf("clean home: exit %d\n%s%s", code, out.String(), errOut.String())
	}
	for _, want := range []string{`"roots"`, `"findings"`, `"relay_state"`, `"build_cache"`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report lacks %s:\n%s", want, out.String())
		}
	}
	plain := filepath.Join(home, "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(home, "faulty.json")
	if err := os.WriteFile(config, []byte(`{"paths":{"worktree_root":"`+plain+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := doctor.Run(context.Background(), []string{"storage", "--config", config}, &out, &errOut); code != 1 || !strings.Contains(out.String(), "not_directory") {
		t.Errorf("a fault: exit %d\n%s", code, out.String())
	}
}
