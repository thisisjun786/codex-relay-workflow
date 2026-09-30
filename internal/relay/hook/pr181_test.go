package hook

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
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
				// Python's adapter under the same failing write (pr181_claim_failure.py python): its
				// answer and the journal and claim files it left, recorded (pyoracle).
				raw := pyoracle.Answer(t, "python", func() ([]byte, error) {
					out, err := pythonScript(t, nil, []byte(payload), "testdata/pr181_claim_failure.py", testRoot, home, role, strconv.Itoa(int(code)), "python")
					if err != nil {
						return nil, err
					}
					// Keys sorted: the script lists the files in the order of their random names.
					return []byte(canonicalJSON(t, out)), nil
				}, pyoracle.Substitute(home, "<NATIVE_HOME>")) // the script spells both homes <HOME> itself
				var python struct {
					First string          `json:"first_stdout"`
					Files json.RawMessage `json:"files"`
				}
				if err := json.Unmarshal(raw, &python); err != nil {
					t.Fatalf("%v: %s", err, raw)
				}
				if python.First != answers[0] {
					t.Fatalf("Python answered %q, Go %q", python.First, answers[0])
				}
				if got, want := evidence.Dumps(adapterFiles(t, home), false, true, true), canonicalJSON(t, python.Files); got != want {
					t.Fatalf("files\n go     %s\n python %s", got, want)
				}
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
			raw := pyoracle.Answer(t, "read_configuration", func() ([]byte, error) {
				return pythonScript(t, nil, nil, "-c", script, filepath.Join(testRoot, "scripts"), path)
			}, pyoracle.Substitute(home, "<HOME>"))
			var expected map[string]any
			if err := json.Unmarshal(raw, &expected); err != nil {
				t.Fatal(err)
			}
			got := readStatusSettings(context.Background(), path, true)
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
	// The first registration names the workspace interpreter by path, as an installed Python
	// adapter's did; status only reads the word.
	interpreter := filepath.Join(testRoot, ".venv", "bin", "python")
	commands := []string{interpreter + " " + entry + " /settings", `"$HOME/.local/share/crw-runtime/current/bin/crw" hook; exit 0`, `"` + native + `" hook`, `"` + native + `" relay`, `echo "` + native + `" hook`, `"` + native + `" hookish`}
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
			raw := pyoracle.Answer(t, "names_this_adapter", func() ([]byte, error) {
				return pythonScript(t, nil, nil, "-c", script, filepath.Join(testRoot, "scripts"), command)
			}, pyoracle.Substitute(home, "<HOME>"), pyoracle.Substitute(testRoot, "<REPO>"))
			if i == 0 && strings.TrimSpace(string(raw)) != strconv.Quote(entry) {
				t.Fatal(string(raw))
			}
			if i > 0 && strings.TrimSpace(string(raw)) != "null" {
				t.Fatal(string(raw))
			}
		})
	}
}

// The guard CLI when eager state selection cannot list roots answers as the Python CLI does
// (pr181_guard_discovery.py). Python lays the fixture out and answers each case; both are
// recorded (pyoracle), and the native CLI reads the recorded fixture.
func Test33PR181GuardDiscoveryPython(t *testing.T) {
	home := t.TempDir()
	_, names := pythonFixture(t, "fixture", home, func() ([]byte, error) {
		return pythonScript(t, nil, nil, "testdata/pr181_guard_discovery.py", "-", testRoot, "prepare", home)
	}, nil)
	raw := names.answer(t, "python", func() ([]byte, error) {
		return pythonScript(t, nil, nil, "testdata/pr181_guard_discovery.py", "-", testRoot, "python", home)
	}, pyoracle.Substitute(home, "<HOME>"))
	var python map[string]struct {
		Code           int
		Stdout, Stderr string
	}
	if err := json.Unmarshal(raw, &python); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	for _, c := range []struct {
		name      string
		overrides map[string]string
		code      int
	}{
		{"unknown_override", map[string]string{"CODEX_SESSION_RELAY_STATE": "~crw_user_that_does_not_exist/state"}, 3},
		{"xdg_file", map[string]string{"XDG_STATE_HOME": filepath.Join(home, "blocked")}, 0},
		{"home_file", map[string]string{"HOME": filepath.Join(home, "blocked"), "XDG_STATE_HOME": ""}, 0},
		{"unreadable_xdg", map[string]string{"XDG_STATE_HOME": filepath.Join(home, "locked")}, 3},
	} {
		env := map[string]string{}
		for _, kv := range os.Environ() {
			key, value, _ := strings.Cut(kv, "=")
			env[key] = value
		}
		for key, value := range map[string]string{"HOME": home, "CODEX_HOME": home, "XDG_STATE_HOME": filepath.Join(home, "xdg"), "CODEX_SESSION_RELAY_STATE": ""} {
			env[key] = value
		}
		for key, value := range c.overrides {
			env[key] = value
		}
		command := exec.Command(binary(t), "relay", "guard-evaluate", "--db-path", filepath.Join(home, "explicit.sqlite3"), "--marker-root", filepath.Join(home, "markers"),
			"--stop-input", filepath.Join(home, "stop.json"), "--no-record", "--now", "2026-01-01T00:00:00Z")
		for key, value := range env {
			command.Env = append(command.Env, key+"="+value)
		}
		got := runOutcome(t, command)
		want := python[c.name]
		if got != (outcomeBytes{want.Code, want.Stdout, want.Stderr}) || got.Code != c.code {
			t.Fatalf("%s: Go %+v\nPython %+v", c.name, got, want)
		}
		if c.code == 0 {
			// The explicit store was really read: an empty readable store answers
			// relationship_absent, not state_unreadable or an unmanaged shortcut.
			answer, err := decodeObject([]byte(got.Stdout))
			if err != nil || get(answer, "receiptEvidence") != "relationship_absent" {
				t.Fatalf("%s: %v %s", c.name, err, got.Stdout)
			}
		}
	}
}

var journalRowName = regexp.MustCompile(`[0-9]{8}/[0-9a-f]{32}\.json`)

// adapterFiles is pr181_claim_failure.py's snapshot of the journal and claim files under home:
// each file's mode and value (a torn one's bytes in hex), without the process id and times, with
// home spelled <HOME> and a journal row's name <ROW>; a row is named by its acceptance.
func adapterFiles(t *testing.T, home string) Object {
	t.Helper()
	var normalize func(any) any
	normalize = func(v any) any {
		switch value := v.(type) {
		case Object:
			out := Object{}
			for _, field := range value {
				if !slices.Contains([]string{"pid", "at", "claimedAt", "elapsedMs", "guardElapsedMs", "identityScanMs"}, field.Key) {
					out = append(out, Field{Key: field.Key, Value: normalize(field.Value)})
				}
			}
			return out
		case []any:
			out := make([]any, len(value))
			for i, item := range value {
				out[i] = normalize(item)
			}
			return out
		case string:
			return journalRowName.ReplaceAllString(strings.ReplaceAll(value, home, "<HOME>"), "<ROW>")
		}
		return v
	}
	files := Object{}
	for _, directory := range []string{filepath.Join(home, "journal"), filepath.Join(home, "crw-completion-hook")} {
		var paths []string
		_ = filepath.WalkDir(directory, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(path, ".json") {
				paths = append(paths, path)
			}
			return nil
		})
		slices.Sort(paths)
		for _, path := range paths {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			value, err := Decode(raw)
			if err != nil {
				value = Object{{Key: "torn", Value: hex.EncodeToString(raw)}}
			}
			name, _ := filepath.Rel(home, path)
			if _, err := strconv.Atoi(filepath.Base(filepath.Dir(path))); err == nil {
				name = "journal/" + text(get(object(value), "acceptance"))
			}
			files = append(files, Field{Key: name, Value: Object{{Key: "mode", Value: int64(info.Mode().Perm())}, {Key: "value", Value: normalize(value)}}})
		}
	}
	return files
}
