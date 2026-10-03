package bridge

import (
	"context"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"os"
	"path/filepath"
	"testing"
)

// The destination refusals the earlier tests never reached, each the golden (which began as the
// Python bridge's refusal), and the Worktree.validate order that produces each.
func Test_round3_destination_refusals_read_as_their_goldens(t *testing.T) {
	for name, place := range map[string]func(t *testing.T, input *CreateWorktree){
		"wt_parent_missing": func(t *testing.T, input *CreateWorktree) {
			input.Destination = filepath.Join(filepath.Dir(input.Destination), "no", "x")
		},
		"wt_parent_is_file": func(t *testing.T, input *CreateWorktree) {
			file := filepath.Join(filepath.Dir(input.Destination), "afile")
			if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
				t.Fatal(err)
			}
			input.Destination = filepath.Join(file, "x")
		},
		// A worktree whose directory was deleted is still registered with Git; only the
		// worktree-list check refuses to reuse its path.
		"wt_registered_worktree": func(t *testing.T, input *CreateWorktree) {
			stale := filepath.Join(filepath.Dir(input.Destination), "stale")
			gitAt(t, input.Source, "worktree", "add", "-q", "--detach", stale, input.Revision)
			if err := os.RemoveAll(stale); err != nil {
				t.Fatal(err)
			}
			input.Destination = stale
		},
	} {
		t.Run(name, func(t *testing.T) {
			b, host := testBridge(t)
			input := worktreeInput(t)
			place(t, &input)
			receipt, err := b.CreateWorktreeThread(context.Background(), input)
			if err != nil || len(hostMethods(host)) != 0 {
				t.Fatalf("receipt=%v err=%v", receipt, err)
			}
			sameJSON(t, "status and error", map[string]any{"status": receipt["status"], "error": receipt["error"]})
			if _, statErr := os.Lstat(input.Destination); statErr == nil {
				t.Fatalf("destination created: %v", statErr)
			}
		})
	}
}

// --detach is observable only when the base names a branch too: given a branch spelled exactly
// like the commit id, "git worktree add" without --detach checks that branch out, and the launch
// would then refuse its own checkout as not detached. It is accepted detached.
func Test_round3_a_base_that_is_also_a_branch_name_is_checked_out_detached(t *testing.T) {
	b, host := testBridge(t)
	input := worktreeInput(t)
	gitAt(t, input.Source, "branch", input.Revision)
	worktreeHost(host, input.Destination)
	receipt, err := b.CreateWorktreeThread(context.Background(), input)
	if err != nil {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
	sameJSON(t, "status and detached", map[string]any{"status": receipt["status"], "detached": pyjson.Map(receipt["worktree"])["detached"]})
	if head := gitAt(t, input.Destination, "rev-parse", "--abbrev-ref", "HEAD"); head != "HEAD" {
		t.Fatalf("checkout is on %q, not detached", head)
	}
}
