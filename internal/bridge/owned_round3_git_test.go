package bridge

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Python records (testdata/python_round3.json refusals) for the destination refusals the
// earlier tests never reached, and the Worktree.validate order that produces each.
func Test_round3_destination_refusals_read_exactly_as_python(t *testing.T) {
	refusals := object(pythonRound3(t)["refusals"])
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
			want := object(refusals[name])
			if err != nil || receipt["status"] != want["status"] || receipt["error"] != want["error"] || len(hostMethods(host)) != 0 {
				t.Fatalf("receipt=%v err=%v\nwant %v", receipt, err, want)
			}
			if _, statErr := os.Lstat(input.Destination); statErr == nil {
				t.Fatalf("destination created: %v", statErr)
			}
		})
	}
}

// --detach is observable only when the base names a branch too: given a branch spelled exactly
// like the commit id, "git worktree add" without --detach checks that branch out, and the launch
// would then refuse its own checkout as not detached. Python accepts it detached.
func Test_round3_a_base_that_is_also_a_branch_name_is_checked_out_detached(t *testing.T) {
	want := object(pythonRound3(t)["branch_named_like_sha"])
	b, host := testBridge(t)
	input := worktreeInput(t)
	gitAt(t, input.Source, "branch", input.Revision)
	worktreeHost(host, input.Destination)
	receipt, err := b.CreateWorktreeThread(context.Background(), input)
	if err != nil || receipt["status"] != want["status"] || object(receipt["worktree"])["detached"] != want["detached"] {
		t.Fatalf("receipt=%v err=%v want=%v", receipt, err, want)
	}
	if head := gitAt(t, input.Destination, "rev-parse", "--abbrev-ref", "HEAD"); head != "HEAD" {
		t.Fatalf("checkout is on %q, not detached", head)
	}
}
