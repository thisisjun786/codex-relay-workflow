package hook

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// An event key digests the bytes json.dumps writes for its tag and values: for each set of values
// (testdata/fixtures/event-key-values.json, lone surrogates and all) the bytes and the digest
// are the golden, which began as Python's stopadapter.event_key's.
func Test33EventKeyGolden(t *testing.T) {
	decoded, err := Decode(golden.Fixture(t, "event-key-values.json"))
	if err != nil {
		t.Fatal(err)
	}
	cases, ok := evidence.List(decoded)
	if !ok || len(cases) < 50 {
		t.Fatalf("%d sets of values", len(cases))
	}
	keys := []any{}
	for i, c := range cases {
		values, ok := evidence.List(c)
		if !ok || len(values) != 4 {
			t.Fatalf("case %d: %v", i, c)
		}
		keys = append(keys, Object{{Key: "bytes", Value: evidence.Dumps(append([]any{EventKeyTag}, values...), true, false, true)}, {Key: "digest", Value: EventKey(values[0], values[1], values[2], values[3])}})
	}
	goldenDumps(t, "event keys", keys, false)
}

// Each Stop of the r1 transcript fixture is identified as the golden holds, which began as
// Python's stopadapter.event_identity's.
func Test33TranscriptIdentityPython(t *testing.T) {
	raw := golden.Fixture(t, "stop_event_r1.json")
	var fixture struct {
		TranscriptLines []string
		Stops           []struct {
			Payload     map[string]any
			LinesAtStop int
		}
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
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
			actual := []any{nullable(key), identity}
			goldenDumps(t, "event_identity", actual, true, golden.Substitute(home, "<HOME>"))
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
