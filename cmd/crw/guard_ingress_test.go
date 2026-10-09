package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"

	pabcdhook "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
)

func TestGitHubProtectionIngressFailuresDeny(t *testing.T) {
	for _, failure := range []string{"partial EIO", "directory fd", "oversized"} {
		t.Run(failure, func(t *testing.T) {
			var in io.Reader
			switch failure {
			case "partial EIO":
				in = io.MultiReader(strings.NewReader(`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"gh pr comment 1 --body-file missing.txt"}}`), iotest.ErrReader(syscall.EIO))
			case "directory fd":
				f, err := os.Open(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				in = f
			case "oversized":
				in = strings.NewReader(strings.Repeat("x", pabcdhook.GitHubPostMaxStdinBytes+1))
			}
			var out, errOut bytes.Buffer
			claimed, code := runComponentHook(invocation{ctx: context.Background(), args: []string{"pre-tool-use", "--leg", "pre-tool-use-guarding-github-post"}, stdout: &out, stderr: &errOut}, in, componentHooks())
			var answer struct {
				HookSpecificOutput struct{ HookEventName, PermissionDecision string }
			}
			err := json.Unmarshal(out.Bytes(), &answer)
			if !claimed || code != 0 || err != nil || answer.HookSpecificOutput.HookEventName != "PreToolUse" || answer.HookSpecificOutput.PermissionDecision != "deny" || errOut.Len() != 0 {
				t.Fatalf("claimed=%v code=%d output=%q error=%v stderr=%q", claimed, code, out.String(), err, errOut.String())
			}
		})
	}
}
