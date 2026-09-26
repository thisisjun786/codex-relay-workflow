package bridge

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

func gitAt(t *testing.T, dir string, args ...string) string {
	t.Helper()
	output, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, output)
	}
	return strings.TrimSuffix(string(output), "\n")
}

func worktreeInput(t *testing.T) CreateWorktree {
	t.Helper()
	root, err := os.MkdirTemp("/dev/shm", "crw-bridge-owned-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	gitAt(t, source, "init")
	gitAt(t, source, "config", "user.email", "test@example.invalid")
	gitAt(t, source, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(source, "tracked"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitAt(t, source, "add", "tracked")
	gitAt(t, source, "commit", "-m", "base")
	return CreateWorktree{RequestID: "isolated", Source: source, Revision: gitAt(t, source, "rev-parse", "HEAD"), Destination: filepath.Join(root, "isolated"), Mode: "bridge-managed-retained", Sandbox: "read-only", Policy: map[string]any{"type": "readOnly", "networkAccess": false}, Model: "explicit-model", Effort: "high"}
}

func worktreeHost(host *fakehost.Server, destination string) {
	host.Respond("thread/start", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1", "cwd": destination}, "cwd": destination, "model": "explicit-model", "reasoningEffort": "high", "approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly", "networkAccess": false}, "runtimeWorkspaceRoots": []any{destination}}})
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
}

func Test_test_invalid_permission_contract_has_no_filesystem_or_api_effects(t *testing.T) {
	for _, policy := range []map[string]any{
		{"type": "readOnly"},
		{"type": "readOnly", "networkAccess": "false"},
		{"type": "readOnly", "networkAccess": false, "unknown": true},
	} {
		t.Run(fmt.Sprint(policy), func(t *testing.T) {
			b, host := testBridge(t)
			input := worktreeInput(t)
			input.Policy = policy
			_, err := b.CreateWorktreeThread(context.Background(), input)
			if err == nil || !strings.Contains(err.Error(), "sandbox policy") || host.Count("thread/start") != 0 {
				t.Fatalf("err=%v calls=%v", err, host.Requests())
			}
			if _, err := os.Lstat(input.Destination); !os.IsNotExist(err) {
				t.Fatalf("destination created: %v", err)
			}
		})
	}
}

func Test_test_only_available_full_commit_ids_are_accepted(t *testing.T) {
	for _, revision := range []string{"HEAD", "main", "HEAD~1", "aaaaaaaaaaaa", strings.Repeat("0", 40)} {
		t.Run(revision, func(t *testing.T) {
			b, host := testBridge(t)
			input := worktreeInput(t)
			input.Revision = revision
			receipt, err := b.CreateWorktreeThread(context.Background(), input)
			if err != nil || receipt["status"] != "failed" || host.Count("thread/start") != 0 {
				t.Fatalf("receipt=%v err=%v", receipt, err)
			}
			if _, err := os.Lstat(input.Destination); !os.IsNotExist(err) {
				t.Fatalf("destination created: %v", err)
			}
		})
	}
}

func Test_test_path_collisions_leave_existing_content_untouched(t *testing.T) {
	for _, collision := range []string{"directory", "file", "symlink", "inside_source", "inside_other_repo"} {
		t.Run(collision, func(t *testing.T) {
			b, host := testBridge(t)
			input := worktreeInput(t)
			switch collision {
			case "directory":
				if err := os.Mkdir(input.Destination, 0700); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.WriteFile(input.Destination, []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(filepath.Join(filepath.Dir(input.Destination), "missing"), input.Destination); err != nil {
					t.Fatal(err)
				}
			case "inside_source":
				input.Destination = filepath.Join(input.Source, "nested")
			case "inside_other_repo":
				other := filepath.Join(filepath.Dir(input.Source), "other")
				if err := os.Mkdir(other, 0700); err != nil {
					t.Fatal(err)
				}
				gitAt(t, other, "init")
				input.Destination = filepath.Join(other, "nested")
			}
			receipt, err := b.CreateWorktreeThread(context.Background(), input)
			if err != nil || receipt["status"] != "failed" || host.Count("thread/start") != 0 {
				t.Fatalf("receipt=%v err=%v", receipt, err)
			}
			switch collision {
			case "directory":
				entries, err := os.ReadDir(input.Destination)
				if err != nil || len(entries) != 0 {
					t.Fatalf("entries=%v err=%v", entries, err)
				}
			case "file":
				data, err := os.ReadFile(input.Destination)
				if err != nil || string(data) != "preserve" {
					t.Fatalf("data=%q err=%v", data, err)
				}
			case "symlink":
				info, err := os.Lstat(input.Destination)
				if err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("symlink=%v err=%v", info, err)
				}
			default:
				if _, err := os.Lstat(input.Destination); !os.IsNotExist(err) {
					t.Fatalf("destination touched: %v", err)
				}
			}
		})
	}
}

func Test_test_readiness_launch_retains_exact_base_without_carrying_dirty_changes(t *testing.T) {
	b, host := testBridge(t)
	input := worktreeInput(t)
	if err := os.WriteFile(filepath.Join(input.Source, "tracked"), []byte("staged\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitAt(t, input.Source, "add", "tracked")
	if err := os.WriteFile(filepath.Join(input.Source, "tracked"), []byte("unstaged\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input.Source, "untracked"), []byte("untracked\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input.Source, ".gitignore"), []byte("ignored\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input.Source, "ignored"), []byte("ignored\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before := gitAt(t, input.Source, "status", "--porcelain", "--ignored")
	worktreeHost(host, input.Destination)
	input.Prompt = "READY"
	receipt, err := b.CreateWorktreeThread(context.Background(), input)
	if err != nil || receipt["status"] != "accepted" || object(receipt["worktree"])["initialRevision"] != input.Revision || object(receipt["creation"])["cwd"] != input.Destination || receipt["checkoutBeforeDispatch"].(map[string]any)["initialRevision"] != input.Revision {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
	checked, err := os.ReadFile(filepath.Join(input.Destination, "tracked"))
	if err != nil || string(checked) != "base\n" {
		t.Fatalf("checkout=%q err=%v", checked, err)
	}
	origin, err := os.ReadFile(filepath.Join(input.Source, "tracked"))
	if err != nil || string(origin) != "unstaged\n" {
		t.Fatalf("source=%q err=%v", origin, err)
	}
	if _, err := os.Stat(filepath.Join(input.Destination, "untracked")); !os.IsNotExist(err) {
		t.Fatalf("untracked copied: %v", err)
	}
	if gitAt(t, input.Destination, "rev-parse", "--abbrev-ref", "HEAD") != "HEAD" || gitAt(t, input.Destination, "status", "--porcelain") != "" {
		t.Fatal("checkout is not clean and detached")
	}
	if gitAt(t, input.Source, "status", "--porcelain", "--ignored") != before {
		t.Fatal("source index or status changed")
	}
	for _, name := range []string{"ignored", "untracked"} {
		if _, err := os.Stat(filepath.Join(input.Destination, name)); !os.IsNotExist(err) {
			t.Fatalf("%s copied: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(input.Source, name)); err != nil {
			t.Fatalf("source %s missing: %v", name, err)
		}
	}
	if object(receipt["worktree"])["ownership"] != "bridge-managed" || object(receipt["worktree"])["lifecycle"] != "retained-until-manual-cleanup" || receipt["permissionReceipt"] == nil || receipt["desktopProjectAssociation"] == nil || host.Count("thread/goal/set") != 0 {
		t.Fatalf("receipt=%v", receipt)
	}
	common := gitAt(t, input.Destination, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if _, err := os.Stat(filepath.Join(common, "worktrees", filepath.Base(input.Destination), "locked")); err != nil {
		t.Fatalf("worktree unlocked: %v", err)
	}
	stored, err := b.GetOperation(context.Background(), input.RequestID)
	if err != nil || stored["status"] != "accepted" {
		t.Fatalf("ledger=%v err=%v", stored, err)
	}
}
