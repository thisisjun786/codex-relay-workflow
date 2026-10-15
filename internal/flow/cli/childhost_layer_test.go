package cli

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/layername"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// TestChildHostMismatchNamesTheChild (CRW-1478): the one-host refusal names the 자식 (the relay parent) and the issue child, and
// carries no bare role word.
func TestChildHostMismatchNamesTheChild(t *testing.T) {
	settings := map[string]any{"model": "m"}
	h := ChildHost{Schema: ChildHostSchema, MarkerRoot: "/tmp/marker",
		Parent: dagsched.Endpoint{HostID: "host-a", Settings: settings},
		Child:  dagsched.Endpoint{HostID: "host-b", Title: "t", Settings: settings}}
	got := h.Problem()
	want := "the 자식's host host-a is not the issue child's host-b: a managed start supports one local host"
	if got != want {
		t.Fatalf("Problem() = %q, want %q", got, want)
	}
	if hits := layername.Find(got); len(hits) != 0 {
		t.Errorf("bare role words %v in %q", hits, got)
	}
}
