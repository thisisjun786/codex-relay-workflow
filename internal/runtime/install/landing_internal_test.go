package install

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
)

// A pointer that loops - placed at a candidate spelled through the pointer itself - resolves,
// without an error, to the same text as the environment spelled that way, so comparing resolved
// spellings calls it landed. landedAt resolves it as a host does and refuses it, as it refuses one
// that dangles and one whose bin/crw is not a regular executable file; a runtime reached whole is
// landed.
func TestLandedAtIsWhatAHostReaches(t *testing.T) {
	dest := t.TempDir()
	current := filepath.Join(dest, "current")
	looping := filepath.Join(current, "bin-0.9.1-000000000000")
	if err := os.Symlink(looping, current); err != nil {
		t.Fatal(err)
	}
	if names := pointer.Names(current, looping); names == nil || !*names {
		t.Fatal("the spelling comparison no longer calls a looping pointer landed; this test's premise is gone")
	}
	if landed, why := landedAt(current, looping); landed {
		t.Fatal("a looping pointer was called landed")
	} else if why == "" {
		t.Fatal("no reason given")
	}

	runtime := filepath.Join(dest, "bin-0.9.0-000000000000")
	crw := filepath.Join(runtime, "bin", Binary)
	if err := os.MkdirAll(filepath.Dir(crw), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(crw, []byte("\x7fELF"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(current); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(runtime, current); err != nil {
		t.Fatal(err)
	}
	if landed, _ := landedAt(current, runtime); landed {
		t.Fatal("a runtime whose bin/crw is not executable was called landed")
	}
	if err := os.Chmod(crw, 0o755); err != nil {
		t.Fatal(err)
	}
	if landed, why := landedAt(current, runtime); !landed {
		t.Fatalf("a runtime reached whole: %s", why)
	}
	if landed, _ := landedAt(current, filepath.Join(dest, "elsewhere")); landed {
		t.Fatal("a pointer at another directory was called landed at this one")
	}
	if err := os.RemoveAll(runtime); err != nil {
		t.Fatal(err)
	}
	if landed, _ := landedAt(current, runtime); landed {
		t.Fatal("a dangling pointer was called landed")
	}
}
