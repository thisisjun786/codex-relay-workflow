package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func Test33PR181ClaimWriteFailurePython(t *testing.T) {
	for _, role := range []string{"host", "accepted"} {
		for _, code := range []syscall.Errno{syscall.ENOSPC, syscall.EIO} {
			t.Run(role+"-"+strconv.Itoa(int(code)), func(t *testing.T) {
				home := hookHome(t, 5)
				t.Setenv("CODEX_HOME", home)
				writeTest(t, filepath.Join(home, "transcript.jsonl"), []byte(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"t"}}`+"\n"+`{"type":"event_msg","payload":{"type":"item_completed","turn_id":"t","thread_id":"s","item":{"type":"AgentMessage","id":"i","content":[{"type":"Text","text":"done"}]}}}`+"\n"))
				payload := `{"session_id":"s","turn_id":"t","stop_hook_active":false,"last_assistant_message":"done","transcript_path":` + strconv.Quote(filepath.Join(home, "transcript.jsonl")) + `}`
				ctx := context.WithValue(context.Background(), claimWriteKey{}, claimWriteFunc(func(f *os.File, b []byte) (int, error) {
					target := strings.Contains(f.Name(), "/stop-events/")
					if role == "accepted" {
						target = strings.Contains(f.Name(), "/accepted/") && !strings.HasSuffix(f.Name(), ".outcome.json")
					}
					if target {
						n, err := f.Write(b[:1])
						if err != nil {
							return n, err
						}
						return n, code
					}
					return f.Write(b)
				}))
				verdict := Object{{Key: "decision", Value: "block"}, {Key: "state", Value: "receipt_missing"}, {Key: "hook_output", Value: Object{{Key: "decision", Value: "block"}, {Key: "reason", Value: "verify"}, {Key: "continue", Value: true}}}}
				var answers []string
				for range 2 {
					done, closeHost := fakeControl(t, home, func(c net.Conn) error { _, err := io.Copy(io.Discard, c); return err })
					var out bytes.Buffer
					if runAdapter(ctx, nil, strings.NewReader(payload), &out, time.Now(), func(context.Context, Object, GuardOptions) (Object, error) { return verdict, nil }) != 0 {
						t.Fatal("nonzero exit")
					}
					awaitHost(t, done)
					closeHost()
					answers = append(answers, out.String())
				}
				if answers[0] != `{"decision": "block", "reason": "verify", "continue": true}` || answers[1] != "" {
					t.Fatal(answers)
				}
				cmd := exec.Command(python(t), "testdata/pr181_claim_failure.py", testRoot, home, role, strconv.Itoa(int(code)))
				cmd.Stdin = strings.NewReader(payload)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("%v\n%s", err, out)
				}
				t.Logf("%s", out)
			})
		}
	}
}
func Test33PR181StatusSymlinkPython(t *testing.T) {
	home := hookHome(t, 5)
	path := filepath.Join(home, ConfigName)
	target := filepath.Join(home, "real.json")
	if err := os.Rename(path, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"live", "broken", "loop", "directory"} {
		t.Run(name, func(t *testing.T) {
			if name != "live" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				dest := filepath.Join(home, "missing")
				if name == "loop" {
					dest = path
				}
				if name == "directory" {
					dest = home
				}
				if err := os.Symlink(dest, path); err != nil {
					t.Fatal(err)
				}
			}
			script := `import json,sys;sys.path.insert(0,sys.argv[1]);from crw_runtime import completion;v,f,d,r=completion.read_configuration(sys.argv[2]);print(json.dumps({'state':f or r.state,'value':v}))`
			cmd := exec.Command(python(t), "-c", script, filepath.Join(testRoot, "scripts"), path)
			raw, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v %s", err, raw)
			}
			var expected map[string]any
			if err = json.Unmarshal(raw, &expected); err != nil {
				t.Fatal(err)
			}
			got := readStatusSettings(context.Background(), path)
			if got.State != expected["state"] {
				t.Fatalf("Go %s Python %s", got.State, raw)
			}
			if name == "live" {
				status := Status(context.Background(), home, nil, "Stop")
				if status["configuration"].(map[string]any)["value"] != present {
					t.Fatal(status)
				}
				cfg, failed, _ := ReadSettings(context.Background(), path)
				if failed != "" || len(cfg) == 0 {
					t.Fatal(failed)
				}
			}
		})
	}
}
func Test33PR181NativeAndPythonRegistration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	native := filepath.Join(home, ".local/share/crw-runtime/current/bin/crw")
	writeTest(t, native, []byte("#!/bin/sh\nprintf 'not Python'\n"))
	if err := os.Chmod(native, 0700); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(home, "completion_hook.py")
	writeTest(t, entry, []byte("# adapter"))
	commands := []string{python(t) + " " + entry + " /settings", `"$HOME/.local/share/crw-runtime/current/bin/crw" hook; exit 0`, `"` + native + `" hook`, `"` + native + `" relay`, `echo "` + native + `" hook`, `"` + native + `" hookish`}
	for i, command := range commands {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			writeStatusJSON(t, filepath.Join(home, "hooks.json"), map[string]any{"hooks": map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command, "timeout": 10}}}}}})
			_, ours, _ := readRegistrations(filepath.Join(home, "hooks.json"), "Stop")
			want := i < 3
			if (len(ours) == 1) != want {
				t.Fatal(command, ours)
			}
			if !want {
				return
			}
			if i == 0 {
				if ours[0].Native || ours[0].Target != entry || ours[0].Settings != "/settings" {
					t.Fatal(ours)
				}
			} else {
				if !ours[0].Native || ours[0].Target != native || ours[0].Settings != "" {
					t.Fatal(ours)
				}
				_, _, starts := probeRegistrations(context.Background(), ours)
				if starts[0]["adapter"] != present || starts[0]["interpreter"] != present {
					t.Fatal(starts)
				}
				status := Status(context.Background(), home, map[string]string{"HOME": home}, "Stop")
				registered := status["registration"].(map[string]any)
				if len(registered["thisAdapter"].([]any)) != 1 || status["registeredCommandTarget"].(map[string]any)["value"] != present {
					t.Fatal(status)
				}
			}
			script := `import json,sys;sys.path.insert(0,sys.argv[1]);from crw_runtime import completion;print(json.dumps(completion.names_this_adapter(sys.argv[2])))`
			cmd := exec.Command(python(t), "-c", script, filepath.Join(testRoot, "scripts"), command)
			raw, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v %s", err, raw)
			}
			if i == 0 && strings.TrimSpace(string(raw)) != strconv.Quote(entry) {
				t.Fatal(string(raw))
			}
			if i > 0 && strings.TrimSpace(string(raw)) != "null" {
				t.Fatal(string(raw))
			}
		})
	}
}
func Test33PR181GuardDiscoveryPython(t *testing.T) {
	cmd := exec.Command(python(t), "testdata/pr181_guard_discovery.py", binary(t), testRoot)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Logf("%s", out)
}
