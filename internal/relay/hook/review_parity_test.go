package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

func Test33ReviewD1(t *testing.T)      { reviewPython(t, "D1") }
func Test33ReviewD2(t *testing.T)      { reviewPython(t, "D2") }
func Test33ReviewD3(t *testing.T)      { reviewPython(t, "D3") }
func Test33ReviewD4(t *testing.T)      { reviewPython(t, "D4") }
func Test33ReviewD5(t *testing.T)      { reviewPython(t, "D5") }
func Test33ReviewD7(t *testing.T)      { reviewPython(t, "D7") }
func Test33ReviewD9Large(t *testing.T) { reviewPython(t, "D9") }
func Test33ReviewD10(t *testing.T)     { reviewPython(t, "D10") }

// The settings path is the Codex home's, as the retired status reading's configuration cell named
// it (completion.status; recorded).
func Test33ReviewD1Status(t *testing.T) {
	home := t.TempDir()
	// The settings override Python's status read (CRW_COMPLETION_HOOK_CONFIG) is retired, decision
	// 66; the reading with it unset is compared.
	for _, override := range []string{""} {
		env := map[string]string{"CODEX_HOME": home, "CRW_COMPLETION_HOOK_CONFIG": override}
		path, err := configurationPath("", env)
		if err != nil {
			t.Fatal(err)
		}
		script := `import json,sys;sys.path.insert(0,sys.argv[1]);from crw_runtime import completion;e=json.loads(sys.argv[2]);s=completion.status(environ=e);print(json.dumps({'configuration':s['configuration']['configuration'],'value':s['configuration']['value']}))`
		raw, _ := json.Marshal(env)
		out := pyoracle.Answer(t, "override="+strconv.FormatBool(override != ""), func() ([]byte, error) {
			return pythonScript(t, nil, nil, "-c", script, filepath.Join(testRoot, "scripts"), string(raw))
		}, pyoracle.Substitute(home, "<HOME>"))
		var want map[string]any
		if err := json.Unmarshal(out, &want); err != nil {
			t.Fatal(err)
		}
		if path != want["configuration"] {
			t.Fatal(path, want)
		}
	}
}

func Test33ReviewD3Keys(t *testing.T) {
	for _, token := range []string{"NaN", "Infinity", "-Infinity"} {
		for index := 0; index < 4; index++ {
			values := []string{`"s"`, `"t"`, `false`, `"i"`}
			values[index] = token
			raw := "[" + strings.Join(values, ",") + "]"
			value, err := Decode([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			v := value.([]any)
			key := EventKey(v[0], v[1], v[2], v[3])
			out := pyoracle.Answer(t, raw, func() ([]byte, error) {
				return pythonScript(t, nil, nil, "-c", `import json,sys;from codex_session_relay.stopadapter import event_key;v=json.loads(sys.argv[1]);print(event_key(*v));print(json.dumps(v,separators=(',',':')))`, raw)
			})
			want := key + "\n" + evidence.Dumps(v, true, false, true) + "\n"
			if string(out) != want {
				t.Fatalf("%s != %s", out, want)
			}
		}
	}
	for _, raw := range []string{`{"x":"NaN", "y":0,"s":"\\\"Infinity", "z":-Infinity}`, `[NaN,0,"\ud800",Infinity]`} {
		v, err := Decode([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		out := pyoracle.Answer(t, raw, func() ([]byte, error) {
			return pythonScript(t, nil, nil, "-c", `import json,sys;print(json.dumps(json.loads(sys.argv[1]),separators=(',',':')))`, raw)
		})
		if string(out) != evidence.Dumps(v, true, false, true)+"\n" {
			t.Fatalf("%s %v", out, v)
		}
	}
}

func Test33ReviewD4Complaints(t *testing.T) {
	base := Object{{Key: "configVersion", Value: int64(1)}, {Key: "relayExecutable", Value: "/relay"}, {Key: "markerRoot", Value: "/markers"}, {Key: "mode", Value: "observe"}, {Key: "owner", Value: "plugin"}, {Key: "adapterInterpreter", Value: "/python"}, {Key: "adapterEntryPoint", Value: "/entry"}}
	for _, budget := range []any{int64(7), 7.0001, int64(8), int64(9), true, math.NaN(), math.Inf(1), math.Inf(-1), json.Number("999999999999999999999999")} {
		cfg := set(append(Object{}, base...), "timeoutSeconds", budget)
		settings := evidence.Dumps(cfg, false, false, true)
		out := pyoracle.Answer(t, settings, func() ([]byte, error) {
			return pythonScript(t, nil, nil, "-c", `import json,sys;sys.path.insert(0,sys.argv[1]);from crw_runtime.completion import complaints;print(json.dumps(complaints(json.loads(sys.argv[2]))))`, filepath.Join(testRoot, "scripts"), settings)
		})
		var want []string
		if err := json.Unmarshal(out, &want); err != nil {
			t.Fatal(err)
		}
		if got := Complaints(cfg); !reflect.DeepEqual(got, want) {
			t.Fatal(budget, got, want)
		}
	}
}

// Time is the behavior under test: begin the 300ms delay only after Read starts.
// Both EOF and the adapter's completion are signalled, never polled.
func Test33ReviewD9Late(t *testing.T) {
	home := hookHome(t, 5)
	t.Setenv("CODEX_HOME", home)
	reader, writer := io.Pipe()
	defer reader.Close()
	started := make(chan struct{})
	input := &signalledReader{Reader: reader, started: started}
	done := make(chan int, 1)
	var out bytes.Buffer
	go func() { done <- Run(context.Background(), nil, input, &out, time.Now()) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("stdin read did not start")
	}
	timer := time.NewTimer(300 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	if _, err := writer.Write([]byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatal(code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hook did not finish")
	}
	rows := rowsAt(t, home)
	if len(rows) != 1 || rows[0]["adapterOutcome"] != "stdin_unreadable" {
		t.Fatal(rows)
	}
	// Signal exactly when the Python entry point starts its stdin read, then supply the same
	// delayed bytes. Python reaches invoke_guard; Go does not. The outcome of the row Python
	// journals is recorded (pyoracle).
	outcomes := pyoracle.Answer(t, "python_outcomes", func() ([]byte, error) {
		return pythonLateStdin(t, home)
	})
	if string(outcomes) != `["guard_answered"]` {
		t.Fatalf("Python journaled %s", outcomes)
	}
}

// pythonLateStdin runs Python's completion_hook in home with its stdin arriving 300ms after it
// starts reading, and answers the adapterOutcome of each row it journaled.
func pythonLateStdin(t *testing.T, home string) ([]byte, error) {
	before := map[string]bool{}
	paths, _ := filepath.Glob(filepath.Join(home, "journal", "[0-9]*", "*.json"))
	for _, path := range paths {
		before[path] = true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python(t), "-c", `import sys,runpy,os
sys.path.insert(0,sys.argv[1])
from crw_runtime import completion
from unittest.mock import patch
class Input:
 @property
 def buffer(self): return self
 def read(self):
  os.write(2,b'R')
  return sys.__stdin__.buffer.read()
sys.stdin=Input();sys.argv=['completion_hook.py']
with patch.object(completion,'invoke_guard',return_value={'ending':'exited','code':0,'stdout':'{"decision":"release","state":"unmanaged","hook_output":{}}','stderr':'','elapsedMs':0,'detail':None}):
 runpy.run_module('completion_hook',run_name='__main__')`, filepath.Join(testRoot, "scripts"))
	cmd.Env = hookEnv(home)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	var signal [1]byte
	if _, err = io.ReadFull(stderr, signal[:]); err != nil || signal[0] != 'R' {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("the Python entry point did not start reading: %v %q", err, signal)
	}
	timer := time.NewTimer(300 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	if _, err = io.WriteString(stdin, `{}`); err != nil {
		return nil, err
	}
	if err = stdin.Close(); err != nil {
		return nil, err
	}
	if err = cmd.Wait(); err != nil {
		return nil, err
	}
	outcomes := []any{}
	paths, _ = filepath.Glob(filepath.Join(home, "journal", "[0-9]*", "*.json"))
	for _, path := range paths {
		if before[path] {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var row map[string]any
		if err = json.Unmarshal(raw, &row); err != nil {
			return nil, err
		}
		outcomes = append(outcomes, row["adapterOutcome"])
	}
	return json.Marshal(outcomes)
}

type signalledReader struct {
	io.Reader
	started  chan struct{}
	notified bool
}

func (r *signalledReader) Read(p []byte) (int, error) {
	if !r.notified {
		close(r.started)
		r.notified = true
	}
	return r.Reader.Read(p)
}

func Test33ReviewD12(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, "bin/crw")
	writeTest(t, path, []byte("#!/bin/sh\nprintf '%s' \"$0\"\n"))
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	// Each home spelling the shell expands reaches the same program, and each is a registration
	// of this adapter; so are the spellings the shell leaves literal, which name another program.
	for _, spelling := range []string{`"$HOME/bin/crw"`, `$HOME/bin/crw`, `${HOME}/bin/crw`, `~/bin/crw`} {
		command := spelling + " hook; exit 0"
		if !nativeRegistration(command) {
			t.Fatal(command)
		}
		raw, err := exec.Command("sh", "-c", command).CombinedOutput()
		if err != nil || string(raw) != path {
			t.Fatal(command, string(raw), err)
		}
	}
	for _, command := range []string{`'$HOME/bin/crw' hook`, `"~/bin/crw" hook`, `\$HOME/bin/crw hook`} {
		if !nativeRegistration(command) {
			t.Fatal(command)
		}
	}
}
