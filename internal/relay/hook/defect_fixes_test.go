package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
)

func Test33D1SurrogateBinaryPython(t *testing.T) {
	home, err := os.MkdirTemp("", "d1-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	cmd := exec.Command(python(t), "testdata/defect_surrogates.py", binary(t), testRoot, home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Logf("%s", out)
}
func Test33D3ErrnoNamesPython(t *testing.T) {
	cmd := exec.Command(python(t), "-c", "import errno,json;print(json.dumps(errno.errorcode))")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var names map[string]string
	if err = json.Unmarshal(out, &names); err != nil {
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
func Test33D3DialErrnosPython(t *testing.T) {
	cmd := exec.Command(python(t), "testdata/defect_errnos.py", binary(t), testRoot)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	value, err := decodeObject(out)
	if err != nil {
		t.Fatal(err)
	}
	readings, ok := evidence.List(get(value, "errnos"))
	if !ok || len(readings) != 2 {
		t.Fatal(value)
	}
	for _, reading := range readings {
		row := object(get(object(reading), "row"))
		if !NativePrescanUnreachable(row) {
			t.Fatalf("Go reader rejected %s", evidence.Dumps(row, false, true, true))
		}
	}
	t.Logf("%s", out)
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
			cmd := exec.Command(python(t), "testdata/defect_fault.py", testRoot, home, paths[0])
			cmd.Stdin = strings.NewReader(payload)
			comparison, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v\n%s", err, comparison)
			}
			t.Logf("%s", comparison)
		})
	}
}
