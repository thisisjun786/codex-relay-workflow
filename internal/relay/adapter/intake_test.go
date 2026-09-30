package adapter

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

func seedIntake(t *testing.T, path, root string) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	roots, _ := json.Marshal([]string{root})
	relationship := store.Relationship{ID: "rel-1", IssueKey: "REL-1", Status: store.StatusActive, ParentTaskID: "01parent-task", ChildTaskID: "01child-task", Generation: 1, ArtifactRoots: string(roots), AllowedRecipients: `["01parent-task"]`, CreatedAt: "2023-11-14T22:13:20.000000+00:00", UpdatedAt: "2023-11-14T22:13:20.000000+00:00"}
	generation := store.Generation{RelationshipID: "rel-1", Number: 1, DispatchRequestID: "dispatch-1", AnchorState: store.AnchorBound, DispatchTurnID: sql.NullString{String: "turn-dispatch-1", Valid: true}, OpenedAt: relationship.CreatedAt, BoundAt: sql.NullString{String: relationship.CreatedAt, Valid: true}}
	if err := storeseed.RecordRelationship(context.Background(), s, relationship, generation, "host-a", "host-a"); err != nil {
		t.Fatal(err)
	}
	return s
}
func Test28_MSC_11_IntakeAdmissionUnchanged(t *testing.T) {
	shareGoldens(t)
	kinds := []string{"frozen-good", "frozen-unreachable", "frozen-tampered", "frozen-absent", "frozen-blocked", "frozen-corrupt", "frozen-manifest-unreadable", "frozen-parent-of-missing", "frozen-parent-through-symlink", "live-good", "live-changed", "live-unreadable", "claimed-digest-newline", "claimed-revision-newline"}
	// A frozen MANIFEST.json no freeze writes is read as json.loads reads it, so the intake takes
	// or refuses it, or fails on it, as the fence's intake does.
	crafted := map[string]testsupport.FrozenManifest{}
	for _, manifest := range testsupport.FrozenManifests() {
		crafted[manifest.Name] = manifest
		kinds = append(kinds, manifest.Name)
	}
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			work := filepath.Join(root, "work")
			if err := os.Mkdir(work, 0700); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.Chmod(work, 0700); err != nil {
					t.Error(err)
				}
			})
			file := filepath.Join(work, "deliver.txt")
			if err := os.WriteFile(file, []byte("the delivered bytes"), 0600); err != nil {
				t.Fatal(err)
			}
			entries, _, err := BuildManifest([]string{file}, []string{work}, false)
			if err != nil {
				t.Fatal(err)
			}
			revision, err := RevisionHash(entries)
			if err != nil {
				t.Fatal(err)
			}
			attempt := 1
			event, err := store.EventID("rel-1", 1, revision, "ready_for_review", "turn-dispatch-1", &attempt)
			if err != nil {
				t.Fatal(err)
			}
			payload := map[string]any{"eventId": event, "relationshipId": "rel-1", "executionGeneration": 1, "attempt": 1, "revisionHash": revision, "outcome": "ready_for_review", "producer": "child", "turnRef": map[string]any{"threadId": "01child-task", "turnId": "turn-dispatch-1", "turnStatus": "completed"}, "manifest": entriesRecord(entries), "emittedAt": "2023-11-14T22:13:20.000000+00:00"}
			ref := filepath.Join(root, "frozen")
			t.Cleanup(func() {
				for _, path := range []string{ref, filepath.Join(ref, "MANIFEST.json")} {
					if err := os.Chmod(path, 0o700); err != nil && !os.IsNotExist(err) {
						t.Error(err)
					}
				}
			})
			switch {
			case strings.HasPrefix(kind, "frozen-"):
				if _, err := Freeze(entries, ref); err != nil {
					t.Fatal(err)
				}
				payload["manifestRef"] = ref
				if kind == "frozen-parent-through-symlink" {
					// The kernel takes lnk/.. to real, whose frozen copy is good. The copy at the
					// lexical parent is tampered below, so a reader that folds the '..' away itself
					// judges a different frozen copy than the fence.
					if _, err := Freeze(entries, filepath.Join(root, "real", "frozen")); err != nil {
						t.Fatal(err)
					}
					if err := os.MkdirAll(filepath.Join(root, "real", "inner"), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(filepath.Join(root, "real", "inner"), filepath.Join(root, "lnk")); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(ref, "files", entries[0].SHA256), []byte("tampered"), 0600); err != nil {
						t.Fatal(err)
					}
					payload["manifestRef"] = root + "/lnk/../frozen"
				}
				if kind == "frozen-parent-of-missing" {
					// The kernel fails the walk at the missing directory before it reaches '..'.
					payload["manifestRef"] = root + "/missing/../frozen"
				}
				if manifest, ok := crafted[kind]; ok {
					if err := os.WriteFile(filepath.Join(ref, "MANIFEST.json"), []byte(manifest.Document(entries[0].Path, entries[0].SHA256)), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(file, []byte("a later revision"), 0600); err != nil {
					t.Fatal(err)
				}
				if kind == "frozen-unreachable" {
					if err := os.RemoveAll(filepath.Join(ref, "files")); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(ref, "files"), []byte("not a directory"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "frozen-tampered" {
					if err := os.WriteFile(filepath.Join(ref, "files", entries[0].SHA256), []byte("tampered"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "frozen-absent" {
					if err := os.RemoveAll(ref); err != nil {
						t.Fatal(err)
					}
				}
				// A frozen copy nobody can reach is absent to the two-value form; bytes that are
				// read and are not a manifest, or a manifest that cannot be read, are exceptions.
				if kind == "frozen-blocked" {
					if err := os.Chmod(ref, 0); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "frozen-corrupt" {
					if err := os.WriteFile(filepath.Join(ref, "MANIFEST.json"), []byte("not json"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "frozen-manifest-unreadable" {
					if err := os.Chmod(filepath.Join(ref, "MANIFEST.json"), 0); err != nil {
						t.Fatal(err)
					}
				}
			case kind == "live-changed":
				if err := os.WriteFile(file, []byte("a later revision"), 0600); err != nil {
					t.Fatal(err)
				}
			case kind == "live-unreadable":
				if err := os.Chmod(work, 0); err != nil {
					t.Fatal(err)
				}
			// A digest is its 64 hex characters and nothing after them: the receipt's shape
			// check refuses a trailing newline before any byte is read.
			case kind == "claimed-digest-newline":
				payload["manifest"].([]any)[0].(map[string]any)["sha256"] = entries[0].SHA256 + "\n"
			case kind == "claimed-revision-newline":
				payload["revisionHash"] = revision + "\n"
			}
			goStore := seedIntake(t, filepath.Join(root, "go", "go.sqlite3"), work)
			raw, _ := json.Marshal(payload)
			intake := store.ReceiptIntake{Store: goStore, Now: func() string { return "2023-11-14T22:13:20.000000+00:00" }, Minimum: store.BestEffortDetection}
			_, err = intake.AcceptChildReceiptWith(context.Background(), raw, store.TurnReference{ThreadID: "01child-task", TurnID: "turn-dispatch-1", Status: "completed"}, store.AcceptOptions{})
			got := map[string]any{"accepted": err == nil}
			if err == nil {
				got["event"] = event
			} else if reason := store.RefusalReason(err); reason != "" {
				got["reason"] = reason
			} else {
				got["reason"] = nil
				detail, ok := store.PythonHostDetail(err)
				if !ok {
					detail = "RuntimeError: " + err.Error()
				}
				got["host"] = detail
			}
			rows, err := goStore.Querier(context.Background()).QueryContext(context.Background(), "SELECT event_id,path_binding_mode FROM events ORDER BY event_id")
			if err != nil {
				t.Fatal(err)
			}
			all := []any{}
			for rows.Next() {
				var event, binding string
				if err := rows.Scan(&event, &binding); err != nil {
					t.Fatal(err)
				}
				all = append(all, []any{event, binding})
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
			got["rows"] = all
			refusals := []any{}
			reasons, err := goStore.Querier(context.Background()).QueryContext(context.Background(), "SELECT reason FROM refusals ORDER BY id")
			if err != nil {
				t.Fatal(err)
			}
			for reasons.Next() {
				var reason string
				if err := reasons.Scan(&reason); err != nil {
					t.Fatal(err)
				}
				refusals = append(refusals, reason)
			}
			if err := errors.Join(reasons.Err(), reasons.Close()); err != nil {
				t.Fatal(err)
			}
			got["refusals"] = refusals
			// The revision and the event id are digests over the test's temporary paths, and so
			// is where a manifest naming them fails to decode.
			derived := []golden.Option{golden.Substitute(revision, "<revision>"), golden.Substitute(event, "<event>")}
			if host, ok := got["host"].(string); ok {
				if position := jsonPosition.FindString(host); position != "" {
					derived = append(derived, golden.Substitute(position, "<json-error-position>"))
				}
			}
			expectJSON(t, "intake", got, derived...)
		})
	}
}
