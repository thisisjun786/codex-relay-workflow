package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor/transport"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor/worker"
)

func crw1650Toolchain(t *testing.T, files map[string]bool) worker.Toolchain {
	t.Helper()
	var out []worker.ToolFile
	for path, executable := range files {
		body := "#!/bin/sh\nexit 99\n"
		src := filepath.Join(t.TempDir(), strings.ReplaceAll(path, "/", "_")+".src")
		if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		out = append(out, worker.ToolFile{Path: path, Version: "v-" + path, SHA256: executor.Digest([]byte(body)), Source: src, Executable: executable})
	}
	return worker.Toolchain{OS: runtime.GOOS, Arch: runtime.GOARCH, Files: out}
}

func crw1650Fixture(t *testing.T, active worker.Toolchain) (cfgPath, root string) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "worker")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.InstallToolchain(root, active); err != nil {
		t.Fatal(err)
	}
	if err := worker.ActivateToolchain(root, active); err != nil {
		t.Fatal(err)
	}
	cfg := WorkerConfig{
		Host: crwconfig.ExecutionHost{ID: "synthetic", Transport: "local", Root: root,
			Repositories: map[string]string{"owner/repo": filepath.Join(t.TempDir(), "checkout")},
			Kind:         executor.Devin, CapabilityRevision: "cap-1", LaunchProfile: strings.Repeat("c", 64),
			ToolchainProfile: active.Digest(), AuthProfile: "approved", TrustProfile: "approved", Supervision: "worker-rpc"},
		Incarnation: "inc-1", Backend: "bin/backend", Toolchain: active,
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath = filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, root
}

func crw1650Apply(t *testing.T, cfgPath string, request transport.Request) (int, transport.Response) {
	t.Helper()
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	code := WorkerRun(context.Background(), []string{"serve", "--config", cfgPath}, bytes.NewReader(raw), &out, &errout)
	var response transport.Response
	if code != 0 {
		t.Logf("WorkerRun exit=%d stderr=%q", code, errout.String())
		return code, response
	}
	if err := transport.Decode(&out, &response); err != nil {
		t.Fatalf("the framed answer does not decode: %v %q", err, out.String())
	}
	if response.Schema != transport.Schema || response.Peer != (executor.Peer{HostID: "synthetic", Incarnation: "inc-1"}) {
		t.Fatalf("the framed answer carries wrong schema/peer: %+v", response)
	}
	return code, response
}

func crw1650ApplyTarget(t *testing.T, cfgPath string, target worker.Toolchain) (int, transport.Response) {
	t.Helper()
	return crw1650Apply(t, cfgPath, transport.Request{Schema: transport.Schema, Method: "toolchain-apply", Toolchain: target})
}

func crw1650ActiveBytes(t *testing.T, root string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "active-toolchain.json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCRW1650ToolchainRejectsUnusableBackend(t *testing.T) {
	good := crw1650Toolchain(t, map[string]bool{"bin/backend": true})
	for name, target := range map[string]worker.Toolchain{
		"backend missing":        crw1650Toolchain(t, map[string]bool{"bin/other": true}),
		"backend not executable": crw1650Toolchain(t, map[string]bool{"bin/backend": false, "bin/other": true}),
	} {
		t.Run(name, func(t *testing.T) {
			cfgPath, root := crw1650Fixture(t, good)
			before := crw1650ActiveBytes(t, root)
			code, response := crw1650ApplyTarget(t, cfgPath, target)
			if code != 0 || !strings.Contains(response.Error, "adapter is not a pinned executable") {
				t.Fatalf("an unusable backend toolchain was not refused in the frame: code=%d error=%q", code, response.Error)
			}
			if !bytes.Equal(crw1650ActiveBytes(t, root), before) {
				t.Fatal("the refused apply rewrote the active toolchain")
			}
			active, err := worker.ActiveToolchain(root, good)
			if err != nil || active.Digest() != good.Digest() {
				t.Errorf("the active toolchain is not the healthy one: %v", err)
			}
		})
	}
	t.Run("candidate file mode", func(t *testing.T) {
		cfgPath, root := crw1650Fixture(t, good)
		target := crw1650Toolchain(t, map[string]bool{"bin/backend": true, "bin/other": true})
		installed, err := worker.InstallToolchain(root, target)
		if err != nil {
			t.Fatal(err)
		}
		corrupted := filepath.Join(installed.Root, "bin", "backend")
		if err := os.Chmod(corrupted, 0o600); err != nil {
			t.Fatal(err)
		}
		before := crw1650ActiveBytes(t, root)
		code, response := crw1650ApplyTarget(t, cfgPath, target)
		if code != 0 || !strings.Contains(response.Error, "toolchain mode drift") {
			t.Fatalf("a non-executable cached backend was not refused in the frame: code=%d error=%q", code, response.Error)
		}
		if !bytes.Equal(crw1650ActiveBytes(t, root), before) {
			t.Fatal("the refused apply rewrote the active toolchain")
		}
	})
	t.Run("source missing", func(t *testing.T) {
		cfgPath, root := crw1650Fixture(t, good)
		target := crw1650Toolchain(t, map[string]bool{"bin/backend": true})
		for i := range target.Files {
			if err := os.Remove(target.Files[i].Source); err != nil {
				t.Fatal(err)
			}
		}
		before := crw1650ActiveBytes(t, root)
		if _, err := worker.InstallToolchain(root, target); err == nil {
			t.Error("a missing source installed")
		}
		code, response := crw1650ApplyTarget(t, cfgPath, target)
		if code != 0 || response.Error == "" {
			t.Fatalf("a toolchain with no source was not refused in the frame: code=%d error=%q", code, response.Error)
		}
		if !bytes.Equal(crw1650ActiveBytes(t, root), before) {
			t.Fatal("the refused apply rewrote the active toolchain")
		}
	})
}

func TestCRW1650ToolchainCanRecoverWithoutActiveBackend(t *testing.T) {
	good := crw1650Toolchain(t, map[string]bool{"bin/backend": true})
	t.Run("active backend missing", func(t *testing.T) {
		bad := crw1650Toolchain(t, map[string]bool{"bin/other": true})
		cfgPath, root := crw1650Fixture(t, good)
		if _, err := worker.InstallToolchain(root, bad); err != nil {
			t.Fatal(err)
		}
		if err := worker.ActivateToolchain(root, bad); err != nil {
			t.Fatal(err)
		}
		if code, _ := crw1650Apply(t, cfgPath, transport.Request{Schema: transport.Schema, Method: "check"}); code == 0 {
			t.Fatal("an ordinary request ran without an active backend")
		}
		code, response := crw1650ApplyTarget(t, cfgPath, good)
		if code != 0 || response.Error != "" {
			t.Fatalf("the recovery apply failed: code=%d error=%q", code, response.Error)
		}
		if response.Toolchain.Digest != good.Digest() {
			t.Errorf("the active digest %q, want the healthy %q", response.Toolchain.Digest, good.Digest())
		}
		active, err := worker.ActiveToolchain(root, good)
		if err != nil || active.Digest() != good.Digest() {
			t.Errorf("the restored toolchain does not read back: %v", err)
		}
		if _, err := worker.ReadToolchain(root, good); err != nil {
			t.Errorf("the healthy version is gone: %v", err)
		}
	})
	for name, corrupt := range map[string]func(t *testing.T, root string, bad worker.Toolchain){
		"mode stripped": func(t *testing.T, root string, bad worker.Toolchain) {
			if err := os.Chmod(filepath.Join(root, "versions", bad.Digest(), "bin", "backend"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"file removed": func(t *testing.T, root string, bad worker.Toolchain) {
			if err := os.Remove(filepath.Join(root, "versions", bad.Digest(), "bin", "backend")); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			bad := crw1650Toolchain(t, map[string]bool{"bin/backend": true, "bin/other": true})
			cfgPath, root := crw1650Fixture(t, good)
			if _, err := worker.InstallToolchain(root, bad); err != nil {
				t.Fatal(err)
			}
			if err := worker.ActivateToolchain(root, bad); err != nil {
				t.Fatal(err)
			}
			corrupt(t, root, bad)
			code, response := crw1650ApplyTarget(t, cfgPath, good)
			if code != 0 || response.Error != "" {
				t.Fatalf("the corrupted active toolchain blocked recovery: code=%d error=%q", code, response.Error)
			}
			if response.Toolchain.Digest != good.Digest() {
				t.Errorf("the active digest %q, want the healthy %q", response.Toolchain.Digest, good.Digest())
			}
			active, err := worker.ActiveToolchain(root, good)
			if err != nil || active.Digest() != good.Digest() {
				t.Errorf("the restored active toolchain does not read back: %v", err)
			}
			if _, err := worker.ReadToolchain(root, good); err != nil {
				t.Errorf("the healthy version is gone: %v", err)
			}
		})
	}
}

func TestCRW1650ToolchainApplyKeepsDrainAndFrameGuards(t *testing.T) {
	good := crw1650Toolchain(t, map[string]bool{"bin/backend": true})
	target := crw1650Toolchain(t, map[string]bool{"bin/backend": true, "bin/other": true})
	for name, request := range map[string]transport.Request{
		"wrong schema":      {Schema: "crw-worker/0", Method: "toolchain-apply", Toolchain: target},
		"workspace carried": {Schema: transport.Schema, Method: "toolchain-apply", Workspace: "w", Toolchain: target},
	} {
		t.Run(name, func(t *testing.T) {
			cfgPath, root := crw1650Fixture(t, good)
			before := crw1650ActiveBytes(t, root)
			code, response := crw1650Apply(t, cfgPath, request)
			if code != 0 || response.Error != "unsupported worker schema" {
				t.Fatalf("invalid frame was not explicitly refused: code=%d error=%q", code, response.Error)
			}
			if !bytes.Equal(crw1650ActiveBytes(t, root), before) {
				t.Fatal("malformed frame changed active toolchain")
			}
		})
	}
	t.Run("unresolved executions", func(t *testing.T) {
		cfgPath, root := crw1650Fixture(t, good)
		peer := executor.Peer{HostID: "synthetic", Incarnation: "inc-1"}
		w, err := worker.Open(filepath.Join(root, "journal"), peer, transport.Adapter{})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", filepath.Join(root, "journal", "delivery.sqlite3"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO starts(execution_id,reference_json,admission_json,grant_json,state) VALUES ('busy','{}','{}','{}','running')"); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		before := crw1650ActiveBytes(t, root)
		code, response := crw1650ApplyTarget(t, cfgPath, target)
		if code != 0 || !strings.Contains(response.Error, "worker still holds unresolved executions") {
			t.Fatalf("an apply ran over unresolved executions: code=%d error=%q", code, response.Error)
		}
		if !bytes.Equal(crw1650ActiveBytes(t, root), before) {
			t.Fatal("the drained-refusal apply changed the active toolchain")
		}
	})
}
