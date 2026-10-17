package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostCLIEnrollmentDisabledAndExplicitEnable(t *testing.T) {
	state := t.TempDir()
	sel, err := ResolveState(state, filepath.Join(state, "synthetic.sock"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), sel.DBPath(), filepath.Join(state, "synthetic.sock"))
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	h := crwconfig.ExecutionHost{ID: "synthetic", Transport: "local", Root: filepath.Join(t.TempDir(), "worker"), Repositories: map[string]string{"owner/repo": "/synthetic/repo"}, Kind: executor.Devin, CapabilityRevision: "cap-1", LaunchProfile: strings.Repeat("c", 64), ToolchainProfile: strings.Repeat("d", 64), AuthProfile: "approved", TrustProfile: "approved", Supervision: "worker-rpc"}
	raw, _ := json.Marshal(h)
	file := filepath.Join(t.TempDir(), "host.json")
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, int) {
		var out, errout bytes.Buffer
		code := HostRun(context.Background(), append(args, "--state", state, "--socket", filepath.Join(state, "synthetic.sock")), &out, &errout)
		return out.String() + errout.String(), code
	}
	if out, code := run("add", "--file", file); code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	if out, code := run("status", "--host", h.ID); code != 0 || !strings.Contains(out, `"enabled":false`) || !strings.Contains(out, `"session_limit":1`) {
		t.Fatalf("%d %s", code, out)
	}
	if out, code := run("enable", "--host", h.ID, "--sessions", "2", "--test-slots", "1", "--cpus", "2"); code == 0 {
		t.Fatalf("unchecked host enabled: %s", out)
	}
	if out, code := run("drain", "--host", h.ID); code != 0 {
		t.Fatalf("%d %s", code, out)
	}
}
