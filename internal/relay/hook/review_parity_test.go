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
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
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
// it (completion.status), the golden. The settings override Python's status read
// (CRW_COMPLETION_HOOK_CONFIG) is retired, decision 66; the reading with it unset is compared.
func Test33ReviewD1Status(t *testing.T) {
	home := t.TempDir()
	path, err := configurationPath("", map[string]string{"CODEX_HOME": home, "CRW_COMPLETION_HOOK_CONFIG": ""})
	if err != nil {
		t.Fatal(err)
	}
	golden.Check(t, "configuration", []byte(path), golden.Substitute(home, "<HOME>"))
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
			// The key and the values' compact bytes, which began as stopadapter.event_key's and
			// json.dumps's.
			key := EventKey(v[0], v[1], v[2], v[3])
			golden.Check(t, raw, []byte(key+"\n"+pyjson.Dumps(v, pyjson.Options{Compact: true})+"\n"))
		}
	}
	for _, raw := range []string{`{"x":"NaN", "y":0,"s":"\\\"Infinity", "z":-Infinity}`, `[NaN,0,"\ud800",Infinity]`} {
		v, err := Decode([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		golden.Check(t, raw, []byte(pyjson.Dumps(v, pyjson.Options{Compact: true})+"\n"))
	}
}

func Test33ReviewD4Complaints(t *testing.T) {
	base := Object{{Key: "configVersion", Value: int64(1)}, {Key: "relayExecutable", Value: "/relay"}, {Key: "markerRoot", Value: "/markers"}, {Key: "mode", Value: "observe"}, {Key: "owner", Value: "plugin"}, {Key: "adapterInterpreter", Value: "/python"}, {Key: "adapterEntryPoint", Value: "/entry"}}
	for _, budget := range []any{int64(7), 7.0001, int64(8), int64(9), true, math.NaN(), math.Inf(1), math.Inf(-1), json.Number("999999999999999999999999")} {
		cfg := set(append(Object{}, base...), "timeoutSeconds", budget)
		// The complaints are the golden, which began as completion.complaints's.
		golden.CheckJSON(t, pyjson.Dumps(cfg, pyjson.Options{}), Complaints(cfg))
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
	// Python's entry point, given the same delayed bytes, reached invoke_guard and journaled
	// guard_answered; Go does not reach the guard.
	rows := rowsAt(t, home)
	if len(rows) != 1 || rows[0]["adapterOutcome"] != "stdin_unreadable" {
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
