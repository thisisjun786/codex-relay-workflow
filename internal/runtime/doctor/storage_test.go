package doctor_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
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

// CRW-958 round 2: the plain `crw doctor` report carries the storage check of the environment it
// was given, without the separate storage command: the findings, and the refusal a structural fault
// of a configured root makes. Nothing configured is a clean report.
func TestDoctorReportCarriesTheStorageCheck(t *testing.T) {
	h := newHost(t)
	report := h.diagnose(t)
	storage := golden.Obj(record.Get(report, "storage"))
	if storage == nil {
		t.Fatalf("the doctor report has no storage entry:\n%s", golden.Canon(report))
	}
	if refusal := record.Get(storage, "refusal"); refusal != nil {
		t.Errorf("clean home: refusal %v", refusal)
	}
	plain := filepath.Join(h.home, "plain")
	write(t, plain, "", 0o600)
	config := filepath.Join(h.home, "crw.json")
	write(t, config, `{"paths":{"worktree_root":"`+plain+`"}}`, 0o600)
	h.env = append(h.env, "CRW_CONFIG="+config)
	storage = golden.Obj(record.Get(h.diagnose(t), "storage"))
	refusal, _ := record.Get(storage, "refusal").(string)
	if !strings.Contains(refusal, "not_directory") || !strings.Contains(golden.Canon(record.Get(storage, "findings")), "worktree_root") {
		t.Errorf("a configured root that is a file:\n%s", golden.Canon(storage))
	}
}
