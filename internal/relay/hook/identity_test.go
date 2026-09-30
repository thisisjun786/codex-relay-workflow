package hook

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

func Test33EventKeyGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/event_keys.json")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	cases, ok := evidence.List(decoded)
	if !ok {
		t.Fatal("golden is not a list")
	}
	if len(cases) < 50 {
		t.Fatal("fewer than 50 keys")
	}
	for i, rawCase := range cases {
		c := object(rawCase)
		values, ok := evidence.List(get(c, "values"))
		if !ok || len(values) != 4 {
			t.Fatal(c)
		}
		serialized := evidence.Dumps(append([]any{EventKeyTag}, values...), true, false, true)
		if serialized != get(c, "bytes") || EventKey(values[0], values[1], values[2], values[3]) != get(c, "digest") {
			t.Fatalf("case %d bytes=%q key=%s", i, serialized, EventKey(values[0], values[1], values[2], values[3]))
		}
	}
}
func Test33TranscriptIdentityPython(t *testing.T) {
	raw, err := os.ReadFile("testdata/stop_event_r1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		TranscriptLines []string
		Stops           []struct {
			Payload     map[string]any
			LinesAtStop int
		}
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	path := filepath.Join(home, "transcript.jsonl")
	for index, s := range fixture.Stops {
		t.Run(string(rune('A'+index)), func(t *testing.T) {
			writeTest(t, path, []byte(strings.Join(fixture.TranscriptLines[:s.LinesAtStop], "\n")+"\n"))
			s.Payload["transcript_path"] = path
			payload, _ := json.Marshal(s.Payload)
			stop, err := decodeObject(payload)
			if err != nil {
				t.Fatal(err)
			}
			key, identity := EventIdentity(context.Background(), stop)
			want := pyoracle.Answer(t, "event_identity", func() ([]byte, error) {
				cmd := exec.Command(python(t), "-c", "import json,sys;from codex_session_relay.stopadapter import event_identity;print(json.dumps(event_identity(json.load(sys.stdin))))")
				cmd.Stdin = strings.NewReader(string(payload))
				want, err := cmd.CombinedOutput()
				if err != nil {
					return nil, fmt.Errorf("%v %s", err, want)
				}
				return want, nil
			}, pyoracle.Substitute(home, "<HOME>"))
			actual := []any{nullable(key), identity}
			var normalized any
			encoded := evidence.Dumps(actual, false, false, true)
			if err = json.Unmarshal([]byte(encoded), &normalized); err != nil {
				t.Fatal(err)
			}
			var expected any
			if err = json.Unmarshal(want, &expected); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(normalized, expected) {
				t.Fatalf("Go %s\nPython %s", encoded, want)
			}
		})
	}
}
func Test33TranscriptBounds(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "transcript")
	stop := Object{{Key: "session_id", Value: "s"}, {Key: "turn_id", Value: "t"}, {Key: "stop_hook_active", Value: false}, {Key: "last_assistant_message", Value: "done"}, {Key: "transcript_path", Value: path}}
	writeTest(t, path, []byte("unfinished"))
	_, identity := EventIdentity(context.Background(), stop)
	if get(identity, "reason") != "transcript_tail_incomplete" {
		t.Fatal(identity)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(ScanMaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteAt([]byte{'\n'}, ScanMaxBytes); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	_, identity = EventIdentity(context.Background(), stop)
	if get(identity, "reason") != "scan_bound_exceeded" {
		t.Fatal(identity)
	}
	if get(identity, "scannedBytes") != 64<<20 {
		t.Fatal(identity)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, identity = EventIdentity(ctx, stop)
	if get(identity, "reason") != "scan_timed_out" {
		t.Fatal(identity)
	}
}
