package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// D1: identities that decode to lone surrogates, through the real native hook, journal and
// claim as the Python adapter's did (defect_surrogates.py): each case's row, less its times and
// with its home spelled <HOME>, and its claim names are the case's golden.
func Test33D1SurrogateBinaryPython(t *testing.T) {
	base, err := os.MkdirTemp("", "d1-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	// Each session and item as the JSON escapes json.dumps writes for the Python str.
	cases := [][2]string{{`"s"`, `"item"`}, {`"se\ud800ss"`, `"item"`}, {`"se\udfffss"`, `"item"`},
		{`"s"`, `"\ud800item"`}, {`"s"`, `"mid\udc00dle"`}, {`"s"`, `"end\udbff"`},
		{`"s\ud83d\ude00\udc00"`, `"\ud834\udd1e\ud800item"`}}
	release := `{"decision": "release", "state": "unmanaged", "hook_output": {}}`
	for index, c := range cases {
		home := filepath.Join(base, "go"+strconv.Itoa(index))
		if err := os.Mkdir(home, 0o700); err != nil {
			t.Fatal(err)
		}
		writeTest(t, filepath.Join(home, "relay"), []byte("#!/bin/sh\nexit 0\n"))
		config := Object{{Key: "configVersion", Value: int64(1)}, {Key: "relayExecutable", Value: filepath.Join(home, "relay")},
			{Key: "markerRoot", Value: filepath.Join(home, "marker")}, {Key: "dbPath", Value: filepath.Join(home, "state/relay.sqlite3")},
			{Key: "mode", Value: "observe"}, {Key: "journalRoot", Value: filepath.Join(home, "journal")}}
		writeTest(t, filepath.Join(home, ConfigName), []byte(evidence.Dumps(config, false, false, true)))
		transcript := filepath.Join(home, "transcript.jsonl")
		quoted := evidence.Dumps(transcript, false, false, true)
		writeTest(t, transcript, []byte(`{"type": "event_msg", "payload": {"type": "task_started", "turn_id": "turn"}}`+"\n"+
			`{"type": "event_msg", "payload": {"type": "item_completed", "turn_id": "turn", "thread_id": `+c[0]+`, "item": {"type": "AgentMessage", "id": `+c[1]+`, "content": [{"type": "Text", "text": "DONE"}]}}}`+"\n"))
		payload := `{"session_id": ` + c[0] + `, "turn_id": "turn", "stop_hook_active": false, "last_assistant_message": "DONE", "transcript_path": ` + quoted + `}`
		done, _ := fakeControl(t, home, func(conn net.Conn) error {
			frame, err := readFrame(conn)
			if err != nil {
				return err
			}
			if got, want := evidence.Dumps(get(object(get(frame, "params")), "stopInput"), false, true, true), canonicalJSON(t, []byte(payload)); got != want {
				return fmt.Errorf("stopInput %s, want %s", got, want)
			}
			_, err = io.WriteString(conn, release+"\n")
			return err
		})
		env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
			key, _, _ := strings.Cut(kv, "=")
			return key == "HOME" || key == "CODEX_HOME" || key == "XDG_STATE_HOME" || key == "CODEX_SESSION_RELAY_STATE"
		})
		command := exec.Command(binary(t), "hook")
		command.Env = append(env, "HOME="+home, "CODEX_HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "xdg"))
		command.Stdin = strings.NewReader(payload)
		if got := runOutcome(t, command); got != (outcomeBytes{}) {
			t.Fatalf("case %d: %+v", index, got)
		}
		awaitHost(t, done)
		paths, err := filepath.Glob(filepath.Join(home, "journal", "[0-9]*", "*.json"))
		if err != nil || len(paths) != 1 {
			t.Fatal(paths, err)
		}
		content, err := os.ReadFile(paths[0])
		if err != nil {
			t.Fatal(err)
		}
		row, err := decodeObject(content)
		if err != nil {
			t.Fatal(err)
		}
		row = withoutKeys(row, "at", "elapsedMs", "guardElapsedMs", "identityScanMs")
		row = set(row, "configuration", strings.ReplaceAll(text(get(row, "configuration")), home, "<HOME>"))
		identity := object(get(row, "eventIdentity"))
		row = set(row, "eventIdentity", set(append(Object{}, identity...), "transcriptPath", strings.ReplaceAll(text(get(identity, "transcriptPath")), home, "<HOME>")))
		claims := []any{}
		names, _ := filepath.Glob(filepath.Join(home, "crw-completion-hook", "stop-events", "*.json"))
		for _, name := range names {
			claims = append(claims, filepath.Base(name))
		}
		goldenDumps(t, "case "+strconv.Itoa(index), Object{{Key: "row", Value: row}, {Key: "claims", Value: claims}}, true)
	}
}

// Linux's errno numbers 1 to 132 by the names Python's errno.errorcode gives them, the golden
// (the suites run on Linux); a number no name stands for (41, 58) is left out.
func Test33D3ErrnoNamesPython(t *testing.T) {
	named := map[string]string{}
	for n := 1; n <= 132; n++ {
		if name := pythonErrnoName(syscall.Errno(n)); name != "" {
			named[strconv.Itoa(n)] = name
		}
	}
	golden.CheckJSON(t, "errno names", named)
}

// D3: a real nonblocking dial failure (the owner's backlog full, EAGAIN; its state directory a
// file, ENOTDIR) is journaled with Python's errno spelling and detail: the row's errno and detail
// are the golden, which began as invoke_guard's answer to the same OSError at the same endpoint.
// The row stays the native pre-scan row the reader exempts (NativePrescanUnreachable). Python's
// own reader judged these rows too until todo 44; the Go reader's contract is
// Test33NativeJournalReader's.
func Test33D3DialErrnosPython(t *testing.T) {
	for _, kind := range []string{"EAGAIN", "ENOTDIR"} {
		t.Run(kind, func(t *testing.T) {
			home, err := os.MkdirTemp("", "d3-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(home) })
			state := filepath.Join(home, "state")
			endpoint := filepath.Join(state, "control.sock")
			config := map[string]any{"configVersion": 1, "relayExecutable": filepath.Join(home, "absent"), "markerRoot": filepath.Join(home, "marker"),
				"dbPath": filepath.Join(state, "relay.sqlite3"), "mode": "observe", "journalRoot": filepath.Join(home, "journal")}
			settings, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			writeTest(t, filepath.Join(home, ConfigName), settings)
			var number syscall.Errno
			if kind == "ENOTDIR" {
				writeTest(t, state, []byte("not a directory"))
				number = syscall.ENOTDIR
			} else {
				number = syscall.EAGAIN
				fillBacklog(t, state, endpoint)
			}
			if got := probeDial(endpoint); got != number {
				t.Fatalf("dial %s: %v, want %v", endpoint, got, number)
			}
			env := hookEnv(home)
			env = slices.DeleteFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "CODEX_SESSION_RELAY_STATE=") })
			command := exec.Command(binary(t), "hook")
			command.Env = env
			command.Stdin = strings.NewReader(`{"session_id":"s"}`)
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			if err = command.Run(); err != nil || stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("hook: %v stdout %q stderr %q", err, &stdout, &stderr)
			}
			paths, err := filepath.Glob(filepath.Join(home, "journal", "[0-9]*", "*.json"))
			if err != nil || len(paths) != 1 {
				t.Fatal(paths, err)
			}
			raw, err := os.ReadFile(paths[0])
			if err != nil {
				t.Fatal(err)
			}
			row, err := decodeObject(raw)
			if err != nil {
				t.Fatal(err)
			}
			if get(row, "errno") != kind {
				t.Fatalf("errno %v, want %s", get(row, "errno"), kind)
			}
			golden.CheckJSON(t, "row", map[string]any{"errno": get(row, "errno"), "detail": get(row, "detail")}, golden.Substitute(home, "<HOME>"))
			if !NativePrescanUnreachable(row) {
				t.Fatalf("Go reader rejected %s", evidence.Dumps(row, false, true, true))
			}
		})
	}
}

// fillBacklog listens at endpoint with a backlog of zero and connects until the kernel, not a
// sleep, reports the backlog full; the descriptors stay open until the test ends.
func fillBacklog(t *testing.T, state, endpoint string) {
	t.Helper()
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	socket := func() int {
		fd, err := cloexecSocket()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = syscall.Close(fd) })
		return fd
	}
	listener := socket()
	if err := syscall.Bind(listener, &syscall.SockaddrUnix{Name: endpoint}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Listen(listener, 0); err != nil {
		t.Fatal(err)
	}
	for range 128 {
		client := socket()
		if err := syscall.SetNonblock(client, true); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Connect(client, &syscall.SockaddrUnix{Name: endpoint}); errors.Is(err, syscall.EAGAIN) {
			return
		} else if err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("the backlog did not fill")
}

// probeDial is the errno a nonblocking connect to endpoint fails with, or 0.
func probeDial(endpoint string) syscall.Errno {
	fd, err := cloexecSocket()
	if err != nil {
		return 0
	}
	defer syscall.Close(fd)
	if err = syscall.SetNonblock(fd, true); err != nil {
		return 0
	}
	var errno syscall.Errno
	if errors.As(syscall.Connect(fd, &syscall.SockaddrUnix{Name: endpoint}), &errno) {
		return errno
	}
	return 0
}
func Test33D2FaultRowPython(t *testing.T) {
	for _, kind := range []string{"panic", "error"} {
		t.Run(kind, func(t *testing.T) {
			home := hookHome(t, 5)
			t.Setenv("CODEX_HOME", home)
			payload := `{"session_id":"s","turn_id":"t","stop_hook_active":false,"last_assistant_message":"DONE","transcript_path":` + strconv.Quote(filepath.Join(home, "transcript.jsonl")) + `}`
			transcript := `{"type":"event_msg","payload":{"type":"task_started","turn_id":"t"}}` + "\n" + `{"type":"event_msg","payload":{"type":"item_completed","turn_id":"t","thread_id":"s","item":{"type":"AgentMessage","id":"item","content":[{"type":"Text","text":"DONE"}]}}}` + "\n"
			writeTest(t, filepath.Join(home, "transcript.jsonl"), []byte(transcript))
			done, _ := fakeControl(t, home, func(conn net.Conn) error { _, err := io.Copy(io.Discard, conn); return err })
			var out bytes.Buffer
			code := runAdapter(context.Background(), nil, strings.NewReader(payload), &out, time.Now(), func(context.Context, Object, GuardOptions) (Object, error) {
				if kind == "panic" {
					panic("injected guard fault")
				}
				return nil, errors.New("injected guard fault")
			})
			awaitHost(t, done)
			if code != 0 || out.Len() != 0 {
				t.Fatal(code, out.String())
			}
			paths, err := filepath.Glob(filepath.Join(home, "journal", "[0-9]*", "*.json"))
			if err != nil || len(paths) != 1 {
				t.Fatal(paths, err)
			}
			raw, err := os.ReadFile(paths[0])
			if err != nil {
				t.Fatal(err)
			}
			row, err := decodeObject(raw)
			if err != nil {
				t.Fatal(err)
			}
			if get(row, "fault") != "RuntimeError: injected guard fault" || get(row, "processEnding") != nil || get(row, "stdoutReading") != nil {
				t.Fatal(row)
			}
			for _, key := range []string{"guardElapsedMs", "guardStderr", "exitCode", "errno", "signal", "detail"} {
				if _, ok := evidence.Lookup(row, key); ok {
					t.Fatalf("fault row contains %s", key)
				}
			}
			// The row's record bytes, less its times and with the settings path spelled
			// <SETTINGS>, are the golden, which began as Python's row for the same guard fault
			// (defect_fault.py, its BaseException path); every remaining byte is compared.
			aligned := set(withoutKeys(row, "at", "elapsedMs", "identityScanMs"), "configuration", "<SETTINGS>")
			golden.Check(t, "row", RecordBytes(aligned), golden.Substitute(home, "<HOME>"))
		})
	}
}

// cloexecSocket is a Unix stream socket no process this binary starts inherits.
func cloexecSocket() (int, error) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fd)
	}
	return fd, err
}
