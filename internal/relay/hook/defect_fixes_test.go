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
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// D1: identities that decode to lone surrogates, through the real native hook, journal and
// claim as the Python adapter's do. Python's side (defect_surrogates.py python: each case's row,
// less its times and with its home spelled <HOME>, and its claim names) is recorded (pyoracle).
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
	raw := pyoracle.Answer(t, "python", func() ([]byte, error) {
		python, err := os.MkdirTemp("", "d1-python-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(python)
		return pythonScript(t, nil, nil, "testdata/defect_surrogates.py", "-", testRoot, python, "python")
	})
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	want, ok := evidence.List(decoded)
	if !ok || len(want) != len(cases) {
		t.Fatalf("recorded %d cases: %s", len(want), raw)
	}
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
		got := evidence.Dumps(Object{{Key: "row", Value: row}, {Key: "claims", Value: claims}}, false, true, true)
		if expected := evidence.Dumps(want[index], false, true, true); got != expected {
			t.Fatalf("case %d:\n go     %s\n python %s", index, got, expected)
		}
	}
}

func Test33D3ErrnoNamesPython(t *testing.T) {
	out := pyoracle.Answer(t, "errno.errorcode", func() ([]byte, error) {
		return pythonScript(t, nil, nil, "-c", "import errno,json;print(json.dumps(errno.errorcode))")
	})
	var names map[string]string
	if err := json.Unmarshal(out, &names); err != nil {
		t.Fatal(err)
	}
	numbers := make([]string, 0, len(names))
	for number := range names {
		numbers = append(numbers, number)
	}
	slices.Sort(numbers)
	for _, number := range numbers {
		want := names[number]
		n, err := strconv.Atoi(number)
		if err != nil {
			t.Fatal(err)
		}
		if got := pythonErrnoName(syscall.Errno(n)); got != want {
			t.Fatalf("errno %d: Go %s Python %s", n, got, want)
		}
	}
}

// D3: a real nonblocking dial failure (the owner's backlog full, EAGAIN; its state directory a
// file, ENOTDIR) is journaled with Python's errno spelling and detail: invoke_guard's answer to
// the same OSError at the same endpoint is recorded (pyoracle). The row stays the native
// pre-scan row the reader exempts (NativePrescanUnreachable). Python's own reader judged these
// rows too until todo 44; the Go reader's contract is Test33NativeJournalReader's.
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
			answer := pyoracle.Answer(t, kind, func() ([]byte, error) {
				return pythonScript(t, nil, nil, "-c", `import json,os,sys
from unittest import mock
from codex_session_relay import stopadapter
config, number, endpoint = json.loads(sys.argv[1]), int(sys.argv[2]), sys.argv[3]
with mock.patch.object(stopadapter.subprocess, 'Popen', side_effect=OSError(number, os.strerror(number), endpoint)):
    python = stopadapter.invoke_guard(config, b'{}')
print(json.dumps({'errno': python['errno'], 'detail': python['detail']}))`, string(settings), strconv.Itoa(int(number)), endpoint)
			}, pyoracle.Substitute(home, "<HOME>"))
			var python struct{ Errno, Detail string }
			if err = json.Unmarshal(answer, &python); err != nil {
				t.Fatalf("%v: %s", err, answer)
			}
			if get(row, "errno") != kind || python.Errno != kind || get(row, "detail") != python.Detail {
				t.Fatalf("Go errno %v detail %v\nPython errno %s detail %s", get(row, "errno"), get(row, "detail"), python.Errno, python.Detail)
			}
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
			// Python's row for the same guard fault (defect_fault.py, its BaseException path) is
			// recorded (pyoracle) as its record bytes, the settings path and times aligned; every
			// remaining byte is compared.
			want := pyoracle.Answer(t, "python_row", func() ([]byte, error) {
				return pythonScript(t, nil, []byte(payload), "testdata/defect_fault.py", testRoot, home, "-")
			}, pyoracle.Substitute(home, "<HOME>"))
			aligned := set(withoutKeys(row, "at", "elapsedMs", "identityScanMs"), "configuration", "<SETTINGS>")
			if got := RecordBytes(aligned); !bytes.Equal(got, want) {
				t.Fatalf("go     %s\npython %s", got, want)
			}
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
