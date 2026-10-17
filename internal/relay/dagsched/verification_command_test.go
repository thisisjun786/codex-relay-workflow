package dagsched

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func commandConfig(t *testing.T, repo, verify, integrate string) {
	t.Helper()
	entry := map[string]string{}
	if verify != "" {
		entry["verify_command"] = verify
	}
	if integrate != "" {
		entry["integrate_command"] = integrate
	}
	raw, _ := json.Marshal(map[string]any{"repositories": map[string]any{repo: entry}})
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_CONFIG", path)
}

func TestRepositoryCommandRecordJudgesTheConfiguredCommandAndExit(t *testing.T) {
	r := newGitRepo(t)
	head := r.git("rev-parse", "HEAD")
	commandConfig(t, r.path, "printf foundation", "")
	rec, err := VerifyRepositoryCommand(context.Background(), r.path, head, head, "printf foundation")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := CommitVerificationKeys(context.Background(), r.path, head)
	if err != nil {
		t.Fatal(err)
	}
	keys.Base, keys.OS, keys.Arch = head, runtime.GOOS, runtime.GOARCH
	if _, err := JudgeVerificationRecord(rec.Raw, keys); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(map[string]any){
		"exit missing":    func(m map[string]any) { delete(m["commandVerification"].(map[string]any), "exitCode") },
		"exit failed":     func(m map[string]any) { m["commandVerification"].(map[string]any)["exitCode"] = 7 },
		"command differs": func(m map[string]any) { m["commandVerification"].(map[string]any)["command"] = "true" },
		"log missing":     func(m map[string]any) { delete(m["commandVerification"].(map[string]any), "logDigest") },
		"runner differs":  func(m map[string]any) { m["runner"] = "local" },
		"tree differs":    func(m map[string]any) { m["treeHash"] = "wrong" },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := JudgeVerificationRecord(reseal(t, rec.Raw, edit), keys); err == nil {
				t.Fatal("accepted invalid command record")
			}
		})
	}
	keys.RepositoryCommand = "printf changed"
	if _, err := JudgeVerificationRecord(rec.Raw, keys); err == nil {
		t.Fatal("reused old command after configuration changed")
	}
	keys.RepositoryCommand = ""
	if _, err := JudgeVerificationRecord(rec.Raw, keys); err == nil {
		t.Fatal("command record accepted with no command configured")
	}
}

func TestRepositoryCommandRecordsFailureAndIgnoresDirtyCallerFiles(t *testing.T) {
	r := newGitRepo(t)
	head := r.git("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(r.path, "base.txt"), []byte("dirty"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"test \"$(cat base.txt)\" = base; exit 7", "echo changed > base.txt"} {
		commandConfig(t, r.path, command, "")
		rec, err := VerifyRepositoryCommand(context.Background(), r.path, head, head, command)
		if err != nil || rec.Result != "fail" || len(rec.Raw) == 0 {
			t.Fatalf("failed record: %+v %v", rec, err)
		}
		keys, err := CommitVerificationKeys(context.Background(), r.path, head)
		if err != nil {
			t.Fatal(err)
		}
		keys.Base, keys.OS, keys.Arch = head, runtime.GOOS, runtime.GOARCH
		if _, err := JudgeVerificationRecord(rec.Raw, keys); err == nil {
			t.Fatal("failed verification accepted")
		}
	}
	commandConfig(t, r.path, "test \"$(cat base.txt)\" = base", "")
	if rec, err := VerifyRepositoryCommand(context.Background(), r.path, head, head, "test \"$(cat base.txt)\" = base"); err != nil || rec.Result != "pass" {
		t.Fatalf("dirty caller leaked into verification: %+v %v", rec, err)
	}
	if raw, _ := os.ReadFile(filepath.Join(r.path, "base.txt")); string(raw) != "dirty" {
		t.Fatal("caller files changed")
	}
}

func TestQueueVerificationKeysKeepLocalAndGitHubEvidence(t *testing.T) {
	r := newGitRepo(t)
	head := r.git("rev-parse", "HEAD")
	commandConfig(t, r.path, "true", "")
	keys, err := QueueVerificationKeys(context.Background(), r.path, head)
	if err != nil {
		t.Fatal(err)
	}
	keys.Base, keys.OS, keys.Arch = head, runtime.GOOS, runtime.GOARCH
	local := VerificationRecord{Runner: "local", Repository: "owner/repo", BaseCommit: head, HeadCommit: head, TreeHash: keys.Tree,
		CiDigest: keys.CiDigest, Tools: keys.Pins, Pins: keys.Pins, Dependencies: keys.Dependencies,
		OS: runtime.GOOS, Arch: runtime.GOARCH, Result: "pass", PinMismatch: []string{}}
	remote := ghRemoteRecord(head, head, keys.Tree, keys.Dependencies, keys.Pins)
	remote.CiDigest = keys.CiDigest
	flowKeys, err := CommitVerificationKeys(context.Background(), r.path, head)
	if err != nil {
		t.Fatal(err)
	}
	flowKeys.Base, flowKeys.OS, flowKeys.Arch = head, runtime.GOOS, runtime.GOARCH
	for _, record := range []VerificationRecord{local, remote} {
		t.Run(record.Runner, func(t *testing.T) {
			raw, err := SealVerificationRecord(record)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := JudgeVerificationRecord(raw, keys); err != nil {
				t.Fatalf("queue evidence refused by flow override: %v", err)
			}
			if _, err := JudgeVerificationRecord(raw, flowKeys); err == nil {
				t.Fatal("queue evidence substituted for flow's configured command")
			}
		})
	}
}
