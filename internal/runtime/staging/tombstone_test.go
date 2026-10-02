package staging_test

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/staging"
)

// A tombstone is the prefix and a runtime directory's name, and nothing else: the name crw
// install remove accepts. The residue survey and crw install status both ask this, so a name
// that only carries the prefix is never given crw install remove as its recovery.
func TestTombstoneOfIsAPrefixedRuntimeDirectory(t *testing.T) {
	for name, want := range map[string]struct {
		original string
		ok       bool
	}{
		".crw-removing-bin-0.9.0-aaaaaaaaaaaa": {"bin-0.9.0-aaaaaaaaaaaa", true},
		".crw-removing-bin-":                   {"bin-", true},
		".crw-removing-notaruntime":            {"notaruntime", false},
		".crw-removing-":                       {"", false},
		"bin-0.9.0-aaaaaaaaaaaa":               {"bin-0.9.0-aaaaaaaaaaaa", false},
		".bin-0.9.0-aaaaaaaaaaaa.tmp-1":        {".bin-0.9.0-aaaaaaaaaaaa.tmp-1", false},
	} {
		original, ok := staging.TombstoneOf(name)
		if ok != want.ok || (ok && original != want.original) {
			t.Errorf("TombstoneOf(%q) = %q, %v; want %q, %v", name, original, ok, want.original, want.ok)
		}
	}
	if !staging.RuntimeDirectory("bin-0.9.0-aaaaaaaaaaaa") || staging.RuntimeDirectory("current") || staging.RuntimeDirectory(".bin-0.9.0") {
		t.Error("RuntimeDirectory is the bin- prefix")
	}
}
