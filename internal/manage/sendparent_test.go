package manage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sendParentWithConfig points the command at cfg for the rest of one test, so the fake bridge
// stands in for the real one and the state directory is temporary.
func sendParentWithConfig(t *testing.T, cfg *Config) {
	t.Helper()
	saved := sendParentConfig
	sendParentConfig = func(*Env) *Config { return cfg }
	t.Cleanup(func() { sendParentConfig = saved })
}

// sendParentRunCommand runs crw manage send-parent through the registry and returns its status
// and stdout.
func sendParentRunCommand(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out, errOut strings.Builder
	code := Run(context.Background(), append([]string{"send-parent"}, args...), strings.NewReader(""), &out, &errOut)
	return code, out.String()
}

// sendParentMessageFile writes a message file and returns its path.
func sendParentMessageFile(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "message.txt")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// sendParentReply is the report the command printed.
func sendParentReply(t *testing.T, stdout string) map[string]any {
	t.Helper()
	reply := map[string]any{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &reply); err != nil {
		t.Fatalf("the report is not JSON: %v %s", err, stdout)
	}
	return reply
}

// --queue writes the notice where the pump collects it and sends nothing at all.
func TestSendParentQueueWritesTheNoticeAndSendsNothing(t *testing.T) {
	coreTempHome(t)
	bridge, log := deliverFakeBridge(t, nil)
	cfg := deliverSendConfig(t, bridge)
	sendParentWithConfig(t, cfg)
	code, stdout := sendParentRunCommand(t, "--thread", "thread-1", "--message-file", sendParentMessageFile(t, "a notice"), "--queue")
	if code != sendParentExitAccepted {
		t.Fatalf("exit %d, want %d (%s)", code, sendParentExitAccepted, stdout)
	}
	reply := sendParentReply(t, stdout)
	if reply["class"] != sendParentClassQueued || reply["requestId"] != "" || reply["logicalId"] == "" {
		t.Errorf("the report was %v", reply)
	}
	dest := filepath.Join(cfg.StateDir, sendParentQueueDir, "thread-1", reply["logicalId"].(string)+".txt")
	if text, err := os.ReadFile(dest); err != nil || string(text) != "a notice" {
		t.Errorf("the queued notice: %q %v", text, err)
	}
	if calls := deliverSendCallsOf(t, log); len(calls) != 0 {
		t.Errorf("--queue sent anyway: %v", calls)
	}
	if entries, err := os.ReadDir(filepath.Join(cfg.StateDir, "outbox")); err == nil && len(entries) != 0 {
		t.Errorf("--queue wrote an outbox record: %v", entries)
	}
}

// The default logical id is the first 16 characters of the message sha256, so the same text names
// the same logical message without an operator choosing one.
func TestSendParentDefaultLogicalIDIsTheMessageHashPrefix(t *testing.T) {
	coreTempHome(t)
	bridge, _ := deliverFakeBridge(t, nil)
	sendParentWithConfig(t, deliverSendConfig(t, bridge))
	_, stdout := sendParentRunCommand(t, "--thread", "thread-1", "--message-file", sendParentMessageFile(t, "a notice"), "--queue")
	sum := sha256.Sum256([]byte("a notice"))
	if got, want := sendParentReply(t, stdout)["logicalId"], hex.EncodeToString(sum[:])[:16]; got != want {
		t.Errorf("logicalId = %v, want %q", got, want)
	}
}

// The command delivers with the parent settings and the parent role, and each class maps to its
// own exit status.
func TestSendParentDeliversWithTheParentSettingsAndReportsTheClass(t *testing.T) {
	cases := []struct {
		name  string
		reply map[string]any
		class string
		code  int
	}{
		{"accepted", map[string]any{"status": "accepted", "delivery": "turn_started"}, deliverClassAccepted, sendParentExitAccepted},
		{"refused", map[string]any{"status": "refused"}, deliverClassRefused, sendParentExitRefused},
		{"unknown", map[string]any{"status": "in_progress_or_unknown"}, deliverClassUnknown, sendParentExitUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			coreTempHome(t)
			bridge, log := deliverFakeBridge(t, []map[string]any{
				{"payload": map[string]any{"observation": "idle"}},
				{"payload": c.reply},
			})
			cfg := deliverSendConfig(t, bridge)
			cfg.Settings.Parent = coreSettings{Model: "a-model", ReasoningEffort: "high"}
			sendParentWithConfig(t, cfg)
			code, stdout := sendParentRunCommand(t, "--thread", "thread-1", "--message-file", sendParentMessageFile(t, "a notice"))
			if code != c.code {
				t.Fatalf("exit %d, want %d (%s)", code, c.code, stdout)
			}
			if reply := sendParentReply(t, stdout); reply["class"] != c.class || reply["requestId"] == "" {
				t.Errorf("the report was %v", reply)
			}
			sends := 0
			for _, call := range deliverSendCallsOf(t, log) {
				if call["tool"] != deliverToolSend {
					continue
				}
				sends++
				args := call["args"].(map[string]any)
				if args["role"] != "parent" {
					t.Errorf("the role was %v", args["role"])
				}
				settings, ok := args["expected_settings"].(map[string]any)
				if !ok || settings["model"] != "a-model" || settings["reasoning_effort"] != "high" {
					t.Errorf("the expected settings were %v", args["expected_settings"])
				}
			}
			if sends != 1 {
				t.Errorf("the message was sent %d times", sends)
			}
		})
	}
}

// A missing or unknown flag is a usage error, and the usage line names the command.
func TestSendParentUsageErrors(t *testing.T) {
	coreTempHome(t)
	for _, args := range [][]string{
		{"--message-file", "m.txt"},
		{"--thread", "thread-1"},
		{"--thread", "thread-1", "--message-file", "m.txt", "extra"},
		{"--thread", "thread-1", "--message-file", "m.txt", "--nope"},
	} {
		var out, errOut strings.Builder
		code := Run(context.Background(), append([]string{"send-parent"}, args...), strings.NewReader(""), &out, &errOut)
		if code != usageExit || !strings.Contains(errOut.String(), "usage: crw manage send-parent") {
			t.Errorf("%v: exit %d %q", args, code, errOut.String())
		}
	}
}

// A message file that cannot be read is a refusal, not a usage error.
func TestSendParentRefusesAnUnreadableMessageFile(t *testing.T) {
	coreTempHome(t)
	bridge, log := deliverFakeBridge(t, nil)
	sendParentWithConfig(t, deliverSendConfig(t, bridge))
	code, _ := sendParentRunCommand(t, "--thread", "thread-1", "--message-file", filepath.Join(t.TempDir(), "absent.txt"))
	if code != sendParentExitRefused {
		t.Errorf("exit %d, want %d", code, sendParentExitRefused)
	}
	if calls := deliverSendCallsOf(t, log); len(calls) != 0 {
		t.Errorf("an unreadable message still started the bridge: %v", calls)
	}
}

// A queue path whose thread or logical id is not one path element is refused, and nothing is
// written outside the state directory.
func TestSendParentQueueRefusesAnUnsafeThread(t *testing.T) {
	coreTempHome(t)
	bridge, _ := deliverFakeBridge(t, nil)
	cfg := deliverSendConfig(t, bridge)
	sendParentWithConfig(t, cfg)
	message := sendParentMessageFile(t, "a notice")
	if code, _ := sendParentRunCommand(t, "--thread", "../escape", "--message-file", message, "--queue"); code != sendParentExitRefused {
		t.Errorf("an unsafe thread: exit %d, want %d", code, sendParentExitRefused)
	}
	if code, _ := sendParentRunCommand(t, "--thread", "thread-1", "--message-file", message, "--logical-id", "../escape", "--queue"); code != sendParentExitRefused {
		t.Errorf("an unsafe logical id: exit %d, want %d", code, sendParentExitRefused)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfg.StateDir), "escape")); !os.IsNotExist(err) {
		t.Errorf("a queue file escaped the state directory: %v", err)
	}
}

// A queue directory that is a symlink is refused, so a planted link cannot redirect the notice
// outside the state directory. The outbox has the same guard in deliver.go.
func TestSendParentQueueRefusesASymlinkedQueueDirectory(t *testing.T) {
	coreTempHome(t)
	bridge, _ := deliverFakeBridge(t, nil)
	cfg := deliverSendConfig(t, bridge)
	sendParentWithConfig(t, cfg)
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(cfg.StateDir, sendParentQueueDir)); err != nil {
		t.Skipf("this host cannot make a symlink: %v", err)
	}
	if code, _ := sendParentRunCommand(t, "--thread", "thread-1", "--message-file", sendParentMessageFile(t, "a notice"), "--queue"); code != sendParentExitRefused {
		t.Errorf("a symlinked queue directory: exit %d, want %d", code, sendParentExitRefused)
	}
	if entries, err := os.ReadDir(elsewhere); err == nil && len(entries) != 0 {
		t.Errorf("the write followed the link: %v", entries)
	}
}
