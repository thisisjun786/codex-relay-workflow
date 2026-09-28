package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

func reviewPython(t *testing.T, id string) {
	t.Helper()
	home := hookHome(t, 5)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python(t), "testdata/review_parity.py", binary(t), testRoot, home, id)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v\n%s", id, err, out)
	}
	t.Logf("%s", out)
}
func Test33ReviewD1(t *testing.T)      { reviewPython(t, "D1") }
func Test33ReviewD2(t *testing.T)      { reviewPython(t, "D2") }
func Test33ReviewD3(t *testing.T)      { reviewPython(t, "D3") }
func Test33ReviewD4(t *testing.T)      { reviewPython(t, "D4") }
func Test33ReviewD5(t *testing.T)      { reviewPython(t, "D5") }
func Test33ReviewD7(t *testing.T)      { reviewPython(t, "D7") }
func Test33ReviewD9Large(t *testing.T) { reviewPython(t, "D9") }
func Test33ReviewD10(t *testing.T)     { reviewPython(t, "D10") }

func Test33ReviewD1Status(t *testing.T) {
	home := t.TempDir()
	for _, override := range []string{"", filepath.Join(home, "override.json")} {
		env := map[string]string{"CODEX_HOME": home, configEnv: override}
		got := Status(context.Background(), "", env, "Stop")["configuration"].(map[string]any)
		script := `import json,sys;sys.path.insert(0,sys.argv[1]);from crw_runtime import completion;e=json.loads(sys.argv[2]);s=completion.status(environ=e);print(json.dumps({'configuration':s['configuration']['configuration'],'value':s['configuration']['value']}))`
		raw, _ := json.Marshal(env)
		cmd := exec.Command(python(t), "-c", script, filepath.Join(testRoot, "scripts"), string(raw))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v %s", err, out)
		}
		var want map[string]any
		if err = json.Unmarshal(out, &want); err != nil {
			t.Fatal(err)
		}
		if got["configuration"] != want["configuration"] || got["value"] != want["value"] {
			t.Fatal(got, want)
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
			cmd := exec.Command(python(t), "-c", `import json,sys;from codex_session_relay.stopadapter import event_key;v=json.loads(sys.argv[1]);print(event_key(*v));print(json.dumps(v,separators=(',',':')))`, raw)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v %s", err, out)
			}
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
		cmd := exec.Command(python(t), "-c", `import json,sys;print(json.dumps(json.loads(sys.argv[1]),separators=(',',':')))`, raw)
		out, err := cmd.CombinedOutput()
		if err != nil || string(out) != evidence.Dumps(v, true, false, true)+"\n" {
			t.Fatalf("%v %s %v", err, out, v)
		}
	}
}

func Test33ReviewD4Complaints(t *testing.T) {
	base := Object{{Key: "configVersion", Value: int64(1)}, {Key: "relayExecutable", Value: "/relay"}, {Key: "markerRoot", Value: "/markers"}, {Key: "mode", Value: "observe"}, {Key: "owner", Value: "plugin"}, {Key: "adapterInterpreter", Value: "/python"}, {Key: "adapterEntryPoint", Value: "/entry"}}
	for _, budget := range []any{int64(7), 7.0001, int64(8), int64(9), true, math.NaN(), math.Inf(1), math.Inf(-1), json.Number("999999999999999999999999")} {
		cfg := set(append(Object{}, base...), "timeoutSeconds", budget)
		cmd := exec.Command(python(t), "-c", `import json,sys;sys.path.insert(0,sys.argv[1]);from crw_runtime.completion import complaints;print(json.dumps(complaints(json.loads(sys.argv[2]))))`, filepath.Join(testRoot, "scripts"), evidence.Dumps(cfg, false, false, true))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v %s", err, out)
		}
		var want []string
		if err = json.Unmarshal(out, &want); err != nil {
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Signal exactly when the Python entry point starts its stdin read, then
	// supply the same delayed bytes. Python reaches invoke_guard; Go does not.
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
		t.Fatal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var signal [1]byte
	if _, err = io.ReadFull(stderr, signal[:]); err != nil || signal[0] != 'R' {
		t.Fatal(err, signal)
	}
	pyTimer := time.NewTimer(300 * time.Millisecond)
	defer pyTimer.Stop()
	<-pyTimer.C
	if _, err = io.WriteString(stdin, `{}`); err != nil {
		t.Fatal(err)
	}
	if err = stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	rows = rowsAt(t, home)
	answered := 0
	for _, row := range rows {
		if row["adapterOutcome"] == "guard_answered" {
			answered++
		}
	}
	if len(rows) != 2 || answered != 1 {
		t.Fatal(rows)
	}
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
	for _, spelling := range []string{`"$HOME/bin/crw"`, `$HOME/bin/crw`, `${HOME}/bin/crw`, `~/bin/crw`} {
		command := spelling + " hook; exit 0"
		target, _, ok := nativeRegistration(command)
		if !ok {
			t.Fatal(command)
		}
		cmd := exec.Command("sh", "-c", command)
		raw, err := cmd.CombinedOutput()
		if err != nil || string(raw) != target || target != path {
			t.Fatal(command, target, string(raw), err)
		}
		probe, _, _ := probeRegistrations(context.Background(), []statusRegistration{{Target: target, Command: command, Native: true}})
		if probe["value"] != present {
			t.Fatal(probe)
		}
	}
	for _, command := range []string{`'$HOME/bin/crw' hook`, `"~/bin/crw" hook`, `\$HOME/bin/crw hook`} {
		target, _, _ := nativeRegistration(command)
		if target == path {
			t.Fatal(command, target)
		}
	}
}
