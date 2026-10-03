package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// deliverableCase stages one stored receipt's deliverable: an artifact, its manifest, and the
// frozen copy a receipt carries, then breaks what the case names.
type deliverableCase struct {
	name  string
	stage func(t *testing.T, artifact, reference string, entries []store.ManifestEntry)
	// refer, when set, is the frozen reference the receipt names instead of the copy's own path.
	refer func(t *testing.T, reference string, entries []store.ManifestEntry) string
}

func chmodBack(t *testing.T, path string) {
	t.Helper()
	t.Cleanup(func() {
		if err := os.Chmod(path, 0o700); err != nil && !os.IsNotExist(err) {
			t.Error(err)
		}
	})
}

func writeTestFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func chmodTest(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	chmodBack(t, path)
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// deliverableCases are the answers guard.deliverable_state gives about a frozen copy: current
// through it, changed when it disagrees or is not a manifest, and unverifiable when it, or the
// bytes behind it, could not be read at all.
func deliverableCases() []deliverableCase {
	moved := func(t *testing.T, artifact string) { writeTestFile(t, artifact, "a later revision") }
	cases := []deliverableCase{
		{"live-current", func(*testing.T, string, string, []store.ManifestEntry) {}, nil},
		{"frozen-current", func(t *testing.T, artifact, _ string, _ []store.ManifestEntry) { moved(t, artifact) }, nil},
		{"frozen-absent", func(t *testing.T, artifact, reference string, _ []store.ManifestEntry) {
			moved(t, artifact)
			if err := os.RemoveAll(reference); err != nil {
				t.Fatal(err)
			}
		}, nil},
		{"frozen-tampered", func(t *testing.T, artifact, reference string, entries []store.ManifestEntry) {
			moved(t, artifact)
			writeTestFile(t, filepath.Join(reference, "files", entries[0].SHA256), "tampered")
		}, nil},
		{"frozen-blocked", func(t *testing.T, artifact, reference string, _ []store.ManifestEntry) {
			moved(t, artifact)
			chmodTest(t, reference, 0)
		}, nil},
		{"frozen-manifest-unreadable", func(t *testing.T, artifact, reference string, _ []store.ManifestEntry) {
			moved(t, artifact)
			chmodTest(t, filepath.Join(reference, "MANIFEST.json"), 0)
		}, nil},
		{"frozen-corrupt", func(t *testing.T, artifact, reference string, _ []store.ManifestEntry) {
			moved(t, artifact)
			writeTestFile(t, filepath.Join(reference, "MANIFEST.json"), "not json")
		}, nil},
		{"frozen-not-a-manifest", func(t *testing.T, artifact, reference string, _ []store.ManifestEntry) {
			moved(t, artifact)
			writeTestFile(t, filepath.Join(reference, "MANIFEST.json"), `{"serialization": "none"}`)
		}, nil},
		{"frozen-not-utf8", func(t *testing.T, artifact, reference string, _ []store.ManifestEntry) {
			moved(t, artifact)
			writeTestFile(t, filepath.Join(reference, "MANIFEST.json"), "{\"entries\": [\xff]}")
		}, nil},
		{"frozen-integer-too-long", func(t *testing.T, artifact, reference string, _ []store.ManifestEntry) {
			moved(t, artifact)
			writeTestFile(t, filepath.Join(reference, "MANIFEST.json"), `{"entries": [], "n": 1`+strings.Repeat("0", 4300)+`}`)
		}, nil},
		{"frozen-a-list", func(t *testing.T, artifact, reference string, _ []store.ManifestEntry) {
			moved(t, artifact)
			writeTestFile(t, filepath.Join(reference, "MANIFEST.json"), "[]")
		}, nil},
		{"frozen-entries-null", func(t *testing.T, artifact, reference string, _ []store.ManifestEntry) {
			moved(t, artifact)
			writeTestFile(t, filepath.Join(reference, "MANIFEST.json"), `{"entries": null}`)
		}, nil},
		{"frozen-entries-text", func(t *testing.T, artifact, reference string, _ []store.ManifestEntry) {
			moved(t, artifact)
			writeTestFile(t, filepath.Join(reference, "MANIFEST.json"), `{"entries": "ab"}`)
		}, nil},
		{"frozen-entries-empty-object", func(t *testing.T, artifact, reference string, _ []store.ManifestEntry) {
			moved(t, artifact)
			writeTestFile(t, filepath.Join(reference, "MANIFEST.json"), `{"entries": {}}`)
		}, nil},
		{"frozen-record-without-path", func(t *testing.T, artifact, reference string, entries []store.ManifestEntry) {
			moved(t, artifact)
			writeTestFile(t, filepath.Join(reference, "MANIFEST.json"), `{"entries": [{"sha256": "`+entries[0].SHA256+`"}]}`)
		}, nil},
		{"frozen-record-a-number", func(t *testing.T, artifact, reference string, _ []store.ManifestEntry) {
			moved(t, artifact)
			writeTestFile(t, filepath.Join(reference, "MANIFEST.json"), `{"entries": [1.5]}`)
		}, nil},
		{"frozen-record-with-a-relative-path", func(t *testing.T, artifact, reference string, entries []store.ManifestEntry) {
			moved(t, artifact)
			writeTestFile(t, filepath.Join(reference, "MANIFEST.json"), `{"entries": [{"path": "relative/deliver.txt", "sha256": "`+entries[0].SHA256+`"}]}`)
		}, nil},
		{"frozen-blobs-blocked", func(t *testing.T, artifact, reference string, _ []store.ManifestEntry) {
			moved(t, artifact)
			chmodTest(t, filepath.Join(reference, "files"), 0)
		}, nil},
		{"live-gone-frozen-absent", func(t *testing.T, artifact, reference string, _ []store.ManifestEntry) {
			for _, path := range []string{artifact, reference} {
				if err := os.RemoveAll(path); err != nil {
					t.Fatal(err)
				}
			}
		}, nil},
		{"live-blocked-frozen-corrupt", func(t *testing.T, artifact, reference string, _ []store.ManifestEntry) {
			chmodTest(t, filepath.Dir(artifact), 0)
			writeTestFile(t, filepath.Join(reference, "MANIFEST.json"), "not json")
		}, nil},
		{"live-blocked-frozen-blocked", func(t *testing.T, artifact, reference string, _ []store.ManifestEntry) {
			chmodTest(t, reference, 0)
			chmodTest(t, filepath.Dir(artifact), 0)
		}, nil},
	}
	// A reference is a path the kernel walks, '..' after the component before it: through a
	// missing directory the walk fails, and through a symlink it reaches the link target's parent.
	cases = append(cases,
		deliverableCase{"frozen-parent-of-missing", func(t *testing.T, artifact, _ string, _ []store.ManifestEntry) { moved(t, artifact) },
			func(_ *testing.T, reference string, _ []store.ManifestEntry) string {
				return filepath.Dir(reference) + "/missing/../frozen"
			}},
		// The copy at the lexical parent is tampered and the one the kernel reaches is good.
		deliverableCase{"frozen-parent-through-symlink", func(t *testing.T, artifact, reference string, entries []store.ManifestEntry) {
			writeTestFile(t, filepath.Join(reference, "files", entries[0].SHA256), "tampered")
			moved(t, artifact)
		}, func(t *testing.T, reference string, entries []store.ManifestEntry) string {
			base := filepath.Dir(reference)
			if err := store.FreezeManifest(entries, filepath.Join(base, "real", "frozen")); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(base, "real", "inner"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(base, "real", "inner"), filepath.Join(base, "lnk")); err != nil {
				t.Fatal(err)
			}
			return base + "/lnk/../frozen"
		}},
		// A blocked copy is named as pathlib spells the reference: '.', a repeated '/' and a
		// trailing '/' dropped, '..' kept, and exactly two leading slashes kept.
		deliverableCase{"frozen-blocked-parent-of-a-sibling", func(t *testing.T, artifact, reference string, _ []store.ManifestEntry) {
			moved(t, artifact)
			chmodTest(t, reference, 0)
		}, func(_ *testing.T, reference string, _ []store.ManifestEntry) string {
			return filepath.Dir(reference) + "//work/./../frozen/"
		}},
		deliverableCase{"frozen-blocked-double-slash", func(t *testing.T, artifact, reference string, _ []store.ManifestEntry) {
			moved(t, artifact)
			chmodTest(t, reference, 0)
		}, func(_ *testing.T, reference string, _ []store.ManifestEntry) string { return "/" + reference }},
	)
	// A frozen MANIFEST.json no freeze writes is read as json.loads reads it.
	for _, manifest := range testsupport.FrozenManifests() {
		cases = append(cases, deliverableCase{manifest.Name, func(t *testing.T, artifact, reference string, entries []store.ManifestEntry) {
			moved(t, artifact)
			writeTestFile(t, filepath.Join(reference, "MANIFEST.json"), manifest.Document(entries[0].Path, entries[0].SHA256))
		}, nil})
	}
	return cases
}

// stageDeliverable returns the manifest, the revision, the frozen reference and the roots.
func stageDeliverable(t *testing.T, c deliverableCase) ([]store.ManifestEntry, string, string, []string) {
	t.Helper()
	// The revision covers the artifact's path, and the golden holds answers naming it: a fixed tree.
	base := parityTree(t)
	work := filepath.Join(base, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(work, "deliver.txt")
	writeTestFile(t, artifact, "the delivered bytes")
	entries, err := store.BuildManifest(context.Background(), []string{artifact}, []string{work})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := store.ManifestRevision(entries)
	if err != nil {
		t.Fatal(err)
	}
	reference := filepath.Join(base, "frozen")
	if err := store.FreezeManifest(entries, reference); err != nil {
		t.Fatal(err)
	}
	named := reference
	if c.refer != nil {
		named = c.refer(t, reference, entries)
	}
	c.stage(t, artifact, reference, entries)
	return entries, revision, named, []string{work}
}

// deliverableOf is the judgment of a stored receipt's deliverable (store.DeliverableState, which
// the lookup both the Stop hook and the omission reader use calls) as the golden spells it: the
// binding when the deliverable stands, the detail when it changed, an error when the comparison
// could not happen, and the exception the fence raises out of guard.deliverable_state.
func deliverableOf(ctx context.Context, payload any, reference string, roots any) (string, string, error) {
	state, binding, detail, err := store.DeliverableState(ctx, payload, reference, roots)
	switch {
	case err != nil:
		return "", "", err
	case state == store.DeliverableCurrent:
		return binding, "", nil
	case state == store.DeliverableUnverifiable:
		return "", "", errors.New(detail)
	}
	return "", detail, nil
}

// A stored receipt's deliverable is judged by guard.deliverable_state's rule: a frozen copy
// nobody could reach, like live bytes nobody could read, leaves the deliverable unverifiable (the
// omission's receipt_unreadable), one that was read and is not a manifest is a changed
// deliverable with the fence's exception words, and one nested deeper than json.loads descends
// raises out of it (the omission's evidence_unreadable, Test24_OMI_7b). Each answer is checked
// against the golden, which began as guard.deliverable_state's (an unverifiable one without its
// detail: the omission keeps none).
func TestOmissionDeliverableAnswersAsTheGuard(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the permissions these cases depend on")
	}
	for _, c := range deliverableCases() {
		t.Run(c.name, func(t *testing.T) {
			entries, revision, reference, roots := stageDeliverable(t, c)
			payload, rootsValue := storedReceiptValues(t, entries, revision, roots)
			binding, detail, err := deliverableOf(context.Background(), payload, reference, rootsValue)
			var got []any
			var raised *store.ManifestException
			switch {
			case errors.As(err, &raised) && raised.RuntimeError():
				got = []any{"raised", raised.StoredText()}
			case err != nil:
				got = []any{"unverifiable", nil}
			case binding != "":
				got = []any{"current", binding, nil}
			default:
				got = []any{"changed", nil, detail}
			}
			golden.CheckJSON(t, "deliverable_state", got, golden.Substitute(filepath.Dir(roots[0]), "<base>"))
		})
	}
}

// The lookup hashes and reads under the context it was given, for the Stop hook and the omission
// reader alike: a comparison an ended context cut off did not happen, so the lookup is not readable
// and the omission is left without a receipt (receipt_unreadable), where the same receipt under a
// live context is current. A context that has ended before the lookup stops at its first SQL
// statement, so this is the path a context that ends between the snapshot and the artifact reads
// takes.
func TestOmissionDeliverableLeavesACutOffComparisonUnreadable(t *testing.T) {
	entries, revision, reference, roots := stageDeliverable(t, deliverableCases()[0])
	payload, rootsValue := storedReceiptValues(t, entries, revision, roots)

	if binding, _, err := deliverableOf(context.Background(), payload, reference, rootsValue); err != nil || binding != "live" {
		t.Fatalf("a live context: %q %v", binding, err)
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	state, binding, detail, err := store.DeliverableState(ended, payload, reference, rootsValue)
	if err != nil || state != store.DeliverableUnverifiable || binding != "" || detail == "" {
		t.Fatalf("an ended context: %q %q %q %v", state, binding, detail, err)
	}
	// Unverifiable is not readable, and not readable leaves the omission receipt_unreadable.
	if got := omissionReceiptUnmeasured(false, nil); got != "receipt_unreadable" {
		t.Fatalf("the omission is left %q", got)
	}
}

// storedReceiptValues are the values the omission reader holds for a stored receipt over entries and
// its relationship's roots: the JSON the store keeps, decoded as json.loads decodes it.
func storedReceiptValues(t *testing.T, entries []store.ManifestEntry, revision string, roots []string) (payload, rootsValue any) {
	t.Helper()
	manifest := make([]map[string]any, len(entries))
	for i, e := range entries {
		manifest[i] = map[string]any{"path": e.Path, "sha256": e.SHA256, "bytes": *e.Bytes}
	}
	receipt, err := json.Marshal(map[string]any{"manifest": manifest, "revisionHash": revision})
	if err != nil {
		t.Fatal(err)
	}
	rootsText, err := json.Marshal(roots)
	if err != nil {
		t.Fatal(err)
	}
	rootsValue, payload, unreadable := store.DecodeStoredReceipt(string(rootsText), string(receipt))
	if unreadable != "" {
		t.Fatal(unreadable)
	}
	return payload, rootsValue
}
