package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/reception"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

type generationClock struct{}

func (generationClock) Now() float64 { return 1700000000 }
func (generationClock) ISO() string  { return "2023-11-14T22:13:20.000000+00:00" }

func Test23SyncGenerationMatchesPython(t *testing.T) {
	root, _ := filepath.Abs("../../..")
	script := `import json,sys,tempfile
from pathlib import Path
from codex_session_relay.store import Store
from codex_session_relay.sync import SyncOutbox
class C:
 def iso(self): return '2023-11-14T22:13:20.000000+00:00'
 def now(self): return 1700000000
value=json.loads(sys.argv[1]); s=Store(str(Path(sys.argv[2])/'relay.sqlite3')); o=SyncOutbox(s,C()); o.set_target('r','coordination_document','doc')
try:
 with s.transaction() as db: ident=o.enqueue_in(db,relationship_id='r',issue_key='I',subject_kind='x',summary='s',generation=value)
 row=s.db.execute('select execution_generation,identity_digest from sync_outbox').fetchone(); op=o.operation(ident)
 print(json.dumps({'id':ident,'generation':row['execution_generation'],'digest':row['identity_digest'],'block':op['block']}))
except Exception as e: print(json.dumps({'error':type(e).__name__+': '+str(e)}))
finally:s.close()`
	for _, testCase := range []struct{ name, raw string }{{"json-number-3", "3"}, {"string-3", `"3"`}, {"float-3", "3.0"}, {"bool-true", "true"}, {"large-int", "1180591620717411303424"}} {
		raw := testCase.raw
		t.Run(testCase.name, func(t *testing.T) {
			wantRaw := pyoracle.Answer(t, "generation "+raw, func() ([]byte, error) {
				pyDir := t.TempDir()
				cmd := exec.Command(filepath.Join(root, ".venv/bin/python"), "-c", script, raw, pyDir)
				cmd.Dir = root
				return cmd.Output()
			})
			var want map[string]any
			wantDecoder := json.NewDecoder(bytes.NewReader(wantRaw))
			wantDecoder.UseNumber()
			if err := wantDecoder.Decode(&want); err != nil {
				t.Fatal(err)
			}

			decoder := json.NewDecoder(bytes.NewBufferString(raw))
			decoder.UseNumber()
			var generation any
			if err := decoder.Decode(&generation); err != nil {
				t.Fatal(err)
			}
			s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "relay.sqlite3"), "")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			o := New(s, generationClock{})
			if _, err = o.SetTarget(context.Background(), "r", CoordinationDocument, "doc"); err != nil {
				t.Fatal(err)
			}
			id, gotErr := o.EnqueueIn(context.Background(), Enqueue{RelationshipID: "r", IssueKey: "I", SubjectKind: "x", Summary: "s", Generation: generation})
			if expected, ok := want["error"].(string); ok {
				if gotErr == nil || gotErr.Error() != "python"+expected[len("OverflowError: Python"):] {
					t.Fatalf("Go=%v Python=%s", gotErr, expected)
				}
				return
			}
			if gotErr != nil {
				t.Fatal(gotErr)
			}
			row, err := o.Get(context.Background(), id.(string))
			if err != nil {
				t.Fatal(err)
			}
			op, err := o.Operation(context.Background(), id.(string))
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]any{"id": id, "generation": row.Get("execution_generation"), "digest": row.Get("identity_digest"), "block": reception.Get(op, "block")}
			if evidence.Dumps(got, false, true, true) != evidence.Dumps(want, false, true, true) {
				t.Fatalf("Go=%s\nPython=%s", evidence.Dumps(got, false, true, true), evidence.Dumps(want, false, true, true))
			}
		})
	}
}
