package sandboxroot_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot"
	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot/sandboxroottest"
)

// CRW-1308: Warn is every caller's answer to the rule's verdict. A refusal becomes one warning line and one record line in
// <CODEX_HOME>/crw/sandbox-root-warnings.jsonl; nil leaves neither.
func TestWarnRecordsARefusalAndWritesOneLine(t *testing.T) {
	w := sandboxroottest.Fake(t)
	project := w.Mkdir(t, w.Home, "code", "aas")
	w.Project(t, project)
	var out strings.Builder
	t.Cleanup(sandboxroot.SetWarningOutput(&out))

	if text := sandboxroot.Warn("test/allowed", sandboxroot.Check(w.Mkdir(t, w.Sandbox, "run"), "workspace-write")); text != "" || out.Len() != 0 {
		t.Fatalf("an allowed root warned: %q / %q", text, out.String())
	}
	if records := w.Warnings(); len(records) != 0 {
		t.Fatalf("an allowed root was recorded: %+v", records)
	}

	above := filepath.Dir(project)
	text := sandboxroot.Warn("test/caller", sandboxroot.Check(above, "workspace-write"))
	if !strings.HasPrefix(text, sandboxroot.WarningPrefix+"test/caller: "+sandboxroot.Code) || strings.Contains(text, "no record written") {
		t.Fatalf("warning %q", text)
	}
	if out.String() != text+"\n" {
		t.Fatalf("line %q, want %q", out.String(), text+"\n")
	}
	record := w.Warned(t, "test/caller", sandboxroot.RuleProjectAncestor, above)
	if record.SchemaVersion != 1 || record.Resolved != above || record.Project != project || record.DecidedAt == "" {
		t.Fatalf("record %+v", record)
	}
	info, err := os.Stat(sandboxroot.RecordPath(w.CodexHome))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("record file: %v %v", info, err)
	}
	if want := filepath.Join(w.CodexHome, "crw", "sandbox-root-warnings.jsonl"); sandboxroot.RecordPath(w.CodexHome) != want {
		t.Fatalf("record path %q, want %q", sandboxroot.RecordPath(w.CodexHome), want)
	}

	// Another error than a refusal is still only a warning, with no path of its own.
	sandboxroot.Warn("test/other", errors.New("boom"))
	if records := w.Warnings(); len(records) != 2 || records[1].Rule != "error" || records[1].Path != "" {
		t.Fatalf("records %+v", records)
	}
}

// A record that cannot be written never stops the caller: the warning says so.
func TestWarnSaysWhenNoRecordIsWritten(t *testing.T) {
	w := sandboxroottest.Fake(t)
	var out strings.Builder
	t.Cleanup(sandboxroot.SetWarningOutput(&out))
	// The crw directory of the Codex home is a file: nothing can be made below it.
	if err := os.WriteFile(filepath.Join(w.CodexHome, "crw"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	text := sandboxroot.Warn("test/caller", sandboxroot.Check(w.Home, "workspace-write"))
	if !strings.HasSuffix(text, "(no record written)") || !strings.Contains(text, sandboxroot.RuleAccountHome) || out.String() != text+"\n" {
		t.Fatalf("warning %q, line %q", text, out.String())
	}
}
