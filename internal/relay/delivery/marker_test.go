package delivery

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// test_marker.py properties MRK-1..MRK-8. Every answer is compared whole with the real
// marker.py run over the same tree (testdata/markerops.py).

func TestMRK01_the_first_writer_wins_and_the_rest_are_told_they_lost(t *testing.T) {
	answers := sameOps(t, nil,
		markerOp{"op": "publish", "target": "<tree>/markers/a/intent.json", "payload": map[string]any{"one": 1}},
		markerOp{"op": "publish", "target": "<tree>/markers/a/intent.json", "payload": map[string]any{"two": 2}},
		markerOp{"op": "read_file", "target": "<tree>/markers/a/intent.json"},
	)
	if ok(t, answers[0]) != Published || ok(t, answers[1]) != Exists {
		t.Fatalf("outcomes %v", answers)
	}
	// Eight concurrent writers: exactly one link() wins, and the survivor is a whole record.
	target := filepath.Join(t.TempDir(), "a", "bound.json")
	mustDo(t, os.MkdirAll(filepath.Dir(target), 0o700))
	var wg sync.WaitGroup
	var mu sync.Mutex
	start := make(chan struct{})
	outcomes := map[string]int{}
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			outcome, err := Publish(target, Obj{{Key: "writer", Value: int64(i)}}, "")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				outcomes["error"]++
				return
			}
			outcomes[outcome]++
		}()
	}
	close(start)
	wg.Wait()
	if outcomes[Published] != 1 || outcomes[Exists] != 7 {
		t.Fatalf("outcomes %v", outcomes)
	}
	data, err := os.ReadFile(target)
	mustDo(t, err)
	if !strings.Contains(string(data), `"writer":`) {
		t.Fatalf("torn survivor %q", data)
	}
}

func TestMRK02_publication_is_atomic_and_an_orphan_temp_is_not_a_fact(t *testing.T) {
	answers := sameOps(t, nil,
		markerOp{"op": "publish", "target": "<tree>/markers/a/intent.json", "payload": map[string]any{"one": 1}},
		markerOp{"op": "publish", "target": "<tree>/markers/a/intent.json", "payload": map[string]any{"two": 2}},
		markerOp{"op": "listdir", "target": "<tree>/markers/a"},
		// A writer that died mid-write leaves a temp and no target: readers see absence.
		markerOp{"op": "write_raw", "path": "attempts/.0.json.tmp.999.deadbeef", "text": "{}"},
		markerOp{"op": "facts"},
		// And a retry after an orphan temp of the same name still publishes.
		markerOp{"op": "write_raw", "target": "<tree>/markers/b/.intent.json.tmp.1.abc", "text": "{}"},
		markerOp{"op": "publish", "target": "<tree>/markers/b/intent.json", "payload": map[string]any{"one": 1}},
	)
	if names := ok(t, answers[2]).([]any); len(names) != 1 || names[0] != "intent.json" {
		t.Fatalf("temp left behind: %v", names)
	}
	facts := ok(t, answers[4]).(map[string]any)
	if attempts := facts["facts"].(map[string]any)["attempts"].([]any); len(attempts) != 0 || len(facts["unreadable"].([]any)) != 0 {
		t.Fatalf("orphan read as a fact: %v", facts)
	}
	if ok(t, answers[6]) != Published {
		t.Fatal("retry after an orphan temp")
	}
}

func TestMRK03_the_fact_digest_reproduces_the_contract_vector_and_excludes_factid(t *testing.T) {
	answers := sameOps(t, nil,
		markerOp{"op": "digest", "payload": map[string]any{"factId": "conflicts/0", "at": "2026-01-01T00:06:00+00:00"}},
		markerOp{"op": "digest", "payload": map[string]any{"factId": "a", "x": 1}},
		markerOp{"op": "digest", "payload": map[string]any{"factId": "b", "x": 1}},
		markerOp{"op": "digest", "payload": map[string]any{"b": []any{1, "\u00e9", nil, true}, "a": map[string]any{"z": 1.5, "y": "q\""}}},
	)
	if ok(t, answers[0]) != "30250e28118e703a042e74d53844e078bbd318ae45a4479eac217c385a5c284a" || ok(t, answers[1]) != ok(t, answers[2]) {
		t.Fatalf("digests %v", answers)
	}
}

func TestMRK04_nothing_names_nothing_and_two_unnamed_never_match(t *testing.T) {
	answers := sameOps(t, nil,
		markerOp{"op": "named", "values": []any{nil, "", "   ", 3, []any{}, map[string]any{}, true, "a", " \u3000"}},
		markerOp{"op": "same", "pairs": []any{[]any{nil, nil}, []any{"", ""}, []any{"  ", "  "}, []any{"a", nil}, []any{"a", "a"}}},
	)
	named := ok(t, answers[0]).([]any)
	same := ok(t, answers[1]).([]any)
	for i, want := range []bool{false, false, false, false, false, false, false, true, false} {
		if named[i] != want {
			t.Fatalf("named %v", named)
		}
	}
	for i, want := range []bool{false, false, false, false, true} {
		if same[i] != want {
			t.Fatalf("same %v", same)
		}
	}
}

func TestMRK05_the_reader_assigns_factids_and_reports_what_it_could_not_read(t *testing.T) {
	answers := sameOps(t, nil,
		markerOp{"op": "publish", "path": "intent.json", "payload": map[string]any{"issueKey": "REL-1"}},
		markerOp{"op": "publish", "path": "attempts/0.json", "payload": map[string]any{"outcome": "accepted"}},
		markerOp{"op": "publish", "path": "claims/sess/claim.json", "payload": map[string]any{"sessionId": "sess"}},
		markerOp{"op": "facts"},
		// An unreadable fact is reported and never read as absent.
		markerOp{"op": "write_raw", "dispatch": "second", "path": "intent.json", "text": "{not json"},
		markerOp{"op": "facts", "dispatch": "second"},
		// A fact that is not a record reaches the reader as the wrong shape.
		markerOp{"op": "unlink", "path": "claims/sess/claim.json"},
		markerOp{"op": "write_raw", "path": "claims/sess/claim.json", "text": `"bare"`},
		markerOp{"op": "facts"},
		// Bytes that are not text are unreadable too, not a crash.
		markerOp{"op": "write_raw", "dispatch": "third", "path": "attempts/0.json", "text": "\xff\xfe"},
		markerOp{"op": "facts", "dispatch": "third"},
	)
	read := ok(t, answers[3]).(map[string]any)["facts"].(map[string]any)
	if read["intent"].(map[string]any)["factId"] != "intent" || read["attempts"].([]any)[0].(map[string]any)["factId"] != "attempts/0" || read["claims"].([]any)[0].(map[string]any)["factId"] != "claims/sess/claim.json" {
		t.Fatalf("factIds %v", read)
	}
	if unreadable := ok(t, answers[5]).(map[string]any)["unreadable"].([]any); len(unreadable) != 1 || unreadable[0] != "intent" {
		t.Fatalf("unreadable %v", unreadable)
	}
	if claims := ok(t, answers[8]).(map[string]any)["facts"].(map[string]any)["claims"].([]any); claims[0] != "bare" {
		t.Fatalf("claims %v", claims)
	}
}

func TestMRK06_a_symlinked_workspace_reaches_the_same_assignment(t *testing.T) {
	answers := sameOps(t, nil,
		markerOp{"op": "symlink", "to": "<tree>/work", "link": "<tree>/alias"},
		markerOp{"op": "workspace_key", "workspace": "<tree>/alias"},
		markerOp{"op": "workspace_key", "workspace": "<tree>/work"},
	)
	if ok(t, answers[1]) != ok(t, answers[2]) {
		t.Fatal("keys differ")
	}
}

func TestMRK07_a_disposition_is_read_where_the_stop_identity_derives(t *testing.T) {
	answers := sameOps(t, nil,
		markerOp{"op": "publish", "path": "dispositions/sess/turn-1.json", "payload": map[string]any{"sessionId": "sess", "turnId": "turn-1", "outcome": "interrupted"}},
		markerOp{"op": "read_disposition", "session": "sess", "turn": "turn-1"},
		markerOp{"op": "read_disposition", "session": "sess", "turn": "turn-2"},
		markerOp{"op": "read_disposition", "session": "sess", "turn": ""},
		markerOp{"op": "read_disposition", "session": "..", "turn": "turn-1"},
	)
	found := ok(t, answers[1]).(map[string]any)
	if found["found"].(map[string]any)["outcome"] != "interrupted" || found["readable"] != true {
		t.Fatalf("found %v", found)
	}
	for _, i := range []int{2, 3, 4} {
		if a := ok(t, answers[i]).(map[string]any); a["found"] != nil || a["readable"] != true {
			t.Fatalf("answer %d %v", i, a)
		}
	}
}

func TestMRK08_the_marker_root_is_flag_then_env_then_xdg_then_home_and_never_the_state_dir(t *testing.T) {
	answers := sameOps(t, map[string]any{MarkerEnv: nil, "XDG_STATE_HOME": nil, "HOME": "<tree>/home"},
		markerOp{"op": "marker_root", "explicit": "/explicit/./x/"},
		markerOp{"op": "set_env", "name": MarkerEnv, "value": "/from-env"},
		markerOp{"op": "marker_root"},
		markerOp{"op": "set_env", "name": MarkerEnv, "value": nil},
		markerOp{"op": "set_env", "name": "XDG_STATE_HOME", "value": "<tree>/xdg"},
		markerOp{"op": "marker_root"},
		markerOp{"op": "set_env", "name": "XDG_STATE_HOME", "value": nil},
		markerOp{"op": "marker_root"},
	)
	for i, source := range map[int]string{0: "flag", 2: "env", 5: "xdg", 7: "home"} {
		record := ok(t, answers[i]).(map[string]any)
		if record["source"] != source || strings.Contains(record["path"].(string), "codex-session-relay") {
			t.Fatalf("answer %d %v", i, record)
		}
	}
}
