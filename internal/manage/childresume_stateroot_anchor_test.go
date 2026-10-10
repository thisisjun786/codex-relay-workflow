package manage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// Verification round 2: a resume whose child's native root cannot be recorded as its anchor is
// refused before anything is sent; the dry run, which records nothing, still goes ahead.
func TestResumeWhoseAnchorCannotBeRecordedIsRefused(t *testing.T) {
	a := t.TempDir()
	host := resumeHost(t, "notLoaded")
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{
		"cwd": a, "model": "m", "reasoningEffort": "xhigh", "status": map[string]any{"type": "notLoaded"}}}})
	exe, _ := resumeRelayScript(t, resumeTestAssignment, resumeSettingsAt(a), 0)
	e, _, _ := resumeEnv(t, exe)
	home := filepath.Join(t.TempDir(), "crw-home")
	if err := os.WriteFile(home, []byte("a file, not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_HOME", home)
	_, err := resumeRun(context.Background(), e, resumeConfig(host, "alpha", "beta"), resumeOptions{relationship: "rel-1", message: "m"})
	var failure *resumeFailure
	if !errors.As(err, &failure) || failure.Reason != "state_root_anchor_unrecorded" {
		t.Fatalf("err = %v, want state_root_anchor_unrecorded", err)
	}
	if methods := resumeHostMethods(host); !slices.Equal(methods, []string{"thread/read"}) {
		t.Fatalf("the host saw %q", methods)
	}
}
