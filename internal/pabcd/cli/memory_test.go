package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

func TestMemoryGrantShapeAndIsolation(t *testing.T) {
	cwd, _, _ := cliSeed(t)
	p := ParseMemoryCLIArgs([]string{"allow-write", "--session=rec-s1"}, cwd)
	if p.Args == nil || p.Error != "" || p.Help {
		t.Fatalf("parse: %+v", p)
	}
	for range 2 {
		out, code := RunMemoryCLI(*p.Args)
		if code != 0 || !strings.Contains(out, "ONE memory write") || !strings.Contains(out, cwd) {
			t.Fatalf("grant: %d %s", code, out)
		}
		s := state.ReadState(cwd, "rec-s1")
		if !s.MemoryWriteGrant || s.Phase != state.PhaseB || s.Slug != "keep" || len(s.UnverifiedSubagents) != 1 {
			t.Fatalf("grant lost state: %+v", s)
		}
	}
	var raw map[string]any
	b, err := os.ReadFile(state.StatePath(cwd, "rec-s1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["memoryWriteGrant"] != true {
		t.Fatalf("grant is not the exact boolean true: %v", raw["memoryWriteGrant"])
	}
	if state.ReadState(cwd, "other").MemoryWriteGrant || state.ReadState(t.TempDir(), "rec-s1").MemoryWriteGrant {
		t.Fatal("grant escaped workspace/session")
	}
	if _, err := os.Stat(filepath.Join(cwd, ".crw/ledger.jsonl")); !os.IsNotExist(err) {
		t.Fatal("memory CLI added a ledger row")
	}
}

func TestMemoryHelpDoesNotGrant(t *testing.T) {
	cwd := t.TempDir()
	for _, argv := range [][]string{{"allow-write", "--help"}, {"allow-write", "-h"}, {"allow-write", "--session", "rec-s1", "--help"}, {"allow-write", "--force", "--help"}} {
		p := ParseMemoryCLIArgs(argv, cwd)
		if !p.Help || p.Args != nil || p.Error != "" {
			t.Fatalf("help: %+v", p)
		}
	}
	if entries, err := os.ReadDir(cwd); err != nil || len(entries) != 0 {
		t.Fatal("parsing help wrote workspace state")
	}
}

// intentionally-changed: the oracle overwrites unreadable state with default IDLE plus a grant.
func TestMemoryDataLossPreservesUnreadableBytes(t *testing.T) {
	for _, before := range []string{"broken JSON", `{"phase":"B","retained":"broken`, `null`, `{"phase":"invalid","retained":"x"}`} {
		t.Run(before, func(t *testing.T) {
			cwd := t.TempDir()
			cliPut(t, state.StatePath(cwd, "rec-s1"), before)
			out, code := RunMemoryCLI(MemoryAllowWriteArgs{Verb: "allow-write", SessionID: "rec-s1", Cwd: cwd})
			if code != 1 || !strings.Contains(out, "unreadable; refusing to overwrite") {
				t.Fatalf("got %d %s", code, out)
			}
			cliUnchanged(t, cwd, []byte(before))
		})
	}
}

func TestMemoryOldSchemaAndCorruptionSentinel(t *testing.T) {
	for _, body := range []string{`{"phase":"P"}`, `{"phase":"P","unverifiedSubagents":null}`, `{"phase":"P","unverifiedCorrupt":true,"unverifiedSubagents":[]}`} {
		t.Run(body, func(t *testing.T) {
			cwd := t.TempDir()
			cliPut(t, state.StatePath(cwd, "rec-s1"), body)
			out, code := RunMemoryCLI(MemoryAllowWriteArgs{Verb: "allow-write", SessionID: "rec-s1", Cwd: cwd})
			if code != 0 {
				t.Fatalf("old schema refused: %s", out)
			}
			s := state.ReadState(cwd, "rec-s1")
			if !s.MemoryWriteGrant || s.Phase != state.PhaseP || s.UnverifiedCorrupt != strings.Contains(body, `"unverifiedCorrupt":true`) {
				t.Fatalf("state not retained: %+v", s)
			}
		})
	}
}

func TestMemoryStateDirectoryFailure(t *testing.T) {
	cwd := t.TempDir()
	cliPut(t, filepath.Join(cwd, ".crw"), "retained file")
	out, code := RunMemoryCLI(MemoryAllowWriteArgs{Verb: "allow-write", SessionID: "rec-s1", Cwd: cwd})
	if code != 1 || !strings.HasPrefix(out, "memory allow-write: could not record the grant (") {
		t.Fatalf("got %d %s", code, out)
	}
	if b, err := os.ReadFile(filepath.Join(cwd, ".crw")); err != nil || string(b) != "retained file" {
		t.Fatal("existing file overwritten")
	}
}
