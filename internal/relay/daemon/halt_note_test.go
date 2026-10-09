package daemon

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1031: the note a pass makes of a halted store names the command that clears the halt (CRW-885), not "by hand".
func TestHaltNoteNamesTheCommandThatClearsTheHalt(t *testing.T) {
	note := haltNote(store.HaltState{Present: true, Path: "/state/relay.halt", Marker: store.HaltMarker{Message: "database disk image is malformed", Code: 11, Site: "write", DetectedAt: "2026-10-09T00:00:00Z"}})
	if !strings.Contains(note, "store-halt-clear") || strings.Contains(note, "by hand") {
		t.Fatalf("the halt note is %q, want it to name store-halt-clear", note)
	}
}
