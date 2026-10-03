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
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
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
				// The journal and claim files the hook left are the golden, which began as those
				// Python's adapter left under the same failing write (pr181_claim_failure.py), its
				// first answer the one above.
				goldenDumps(t, "files", adapterFiles(t, home), true)
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
			// The state is the golden, which began as completion.read_configuration's: its
			// failure, or the reading's own state ("PRESENT").
			cfg, state, _ := ReadSettings(context.Background(), path)
			if state == "" {
				state = "PRESENT"
			}
			golden.Check(t, "state", []byte(state))
			if name == "live" && len(cfg) == 0 {
				t.Fatal("the live settings read as empty")
			}
		})
	}
}
func Test33PR181NativeAndPythonRegistration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	native := filepath.Join(home, ".local/share/crw-runtime/current/bin/crw")
	entry := filepath.Join(home, "completion_hook.py")
	// The first registration names the workspace interpreter by path, as an installed Python
	// adapter's did; the reader only reads the word. (Python's completion.names_this_adapter
	// named entry for it and nothing for the native ones until todo 44.)
	interpreter := filepath.Join(testRoot, ".venv", "bin", "python")
	commands := []string{interpreter + " " + entry + " /settings", `"$HOME/.local/share/crw-runtime/current/bin/crw" hook; exit 0`, `"` + native + `" hook`, `"` + native + `" relay`, `echo "` + native + `" hook`, `"` + native + `" hookish`}
	for i, command := range commands {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			document, err := json.Marshal(map[string]any{"hooks": map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command, "timeout": 10}}}}}})
			if err != nil {
				t.Fatal(err)
			}
			writeTest(t, filepath.Join(home, "hooks.json"), document)
			ours, readable := AdapterIdentities(filepath.Join(home, "hooks.json"), "Stop")
			want := i < 3
			if !readable || (len(ours) == 1) != want {
				t.Fatal(command, ours)
			}
		})
	}
}

// An evaluation handed an explicit store reads that store and never asks discovery, so a home or
// an XDG_STATE_HOME that names a file, which leaves the state roots unlistable, changes nothing:
// the empty store answers relationship_absent (pr181_guard_discovery.py's fixture). The relay's
// guard-evaluate command once refused such a Stop at its eager state selection.
func Test33PR181AnExplicitStoreIsReadWithoutDiscovery(t *testing.T) {
	home := t.TempDir()
	layFixture(t, "pr181-guard-discovery", home)
	raw, err := os.ReadFile(filepath.Join(home, "stop.json"))
	if err != nil {
		t.Fatal(err)
	}
	stop, err := decodeObject(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name      string
		overrides map[string]string
	}{
		{"xdg_file", map[string]string{"XDG_STATE_HOME": filepath.Join(home, "blocked")}},
		{"home_file", map[string]string{"HOME": filepath.Join(home, "blocked"), "XDG_STATE_HOME": ""}},
	} {
		t.Run(c.name, func(t *testing.T) {
			for key, value := range map[string]string{"HOME": home, "CODEX_HOME": home, "XDG_STATE_HOME": filepath.Join(home, "xdg"), "CODEX_SESSION_RELAY_STATE": ""} {
				t.Setenv(key, value)
			}
			for key, value := range c.overrides {
				t.Setenv(key, value)
			}
			options := GuardOptions{Root: filepath.Join(home, "markers"), DBPath: filepath.Join(home, "explicit.sqlite3"), Now: "2026-01-01T00:00:00Z", NoRecord: true,
				DefaultDBPath: func() (string, error) { t.Error("discovery was asked for a store"); return "", nil }}
			answer, err := Evaluate(context.Background(), stop, options)
			if err != nil || answer.Get("receiptEvidence") != "relationship_absent" {
				t.Fatalf("%v %s", err, pyjson.Dumps(answer, pyjson.Options{}))
			}
		})
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
				name = "journal/" + pyjson.Text(object(value).Get("acceptance"))
			}
			files = append(files, Field{Key: name, Value: Object{{Key: "mode", Value: int64(info.Mode().Perm())}, {Key: "value", Value: normalize(value)}}})
		}
	}
	return files
}
