package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/reception"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

type generationClock struct{}

func (generationClock) Now() float64 { return 1700000000 }
func (generationClock) ISO() string  { return "2023-11-14T22:13:20.000000+00:00" }

func Test23SyncGenerationMatchesTheGolden(t *testing.T) {
	for _, testCase := range []struct{ name, raw string }{{"json-number-3", "3"}, {"string-3", `"3"`}, {"float-3", "3.0"}, {"bool-true", "true"}, {"large-int", "1180591620717411303424"}} {
		raw := testCase.raw
		t.Run(testCase.name, func(t *testing.T) {
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
			if gotErr != nil {
				golden.Check(t, "error", []byte(gotErr.Error()))
				return
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
			golden.Check(t, "answer", []byte(evidence.Dumps(got, false, true, true)))
		})
	}
}
