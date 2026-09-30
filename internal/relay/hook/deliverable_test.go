package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The Stop hook judges a stored receipt's deliverable by guard.deliverable_state's rule: a frozen
// copy nobody could reach leaves it unverifiable with the reason named, and one that was read and
// is not a manifest is changed, answered with the exception the fence raised rather than merged
// into the live problems. Each case is staged once and answered by both, word for word.
func TestDeliverableStateAnswersAsTheGuard(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the permissions these cases depend on")
	}
	write := func(t *testing.T, path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	block := func(t *testing.T, path string) {
		t.Helper()
		t.Cleanup(func() {
			if err := os.Chmod(path, 0o700); err != nil && !os.IsNotExist(err) {
				t.Error(err)
			}
		})
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
	}
	remove := func(t *testing.T, path string) {
		t.Helper()
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name  string
		stage func(t *testing.T, artifact, reference, blob string)
	}{
		{"live-current", func(*testing.T, string, string, string) {}},
		{"frozen-current", func(t *testing.T, artifact, _, _ string) { write(t, artifact, "a later revision") }},
		{"frozen-absent", func(t *testing.T, artifact, reference, _ string) {
			write(t, artifact, "a later revision")
			remove(t, reference)
		}},
		{"frozen-tampered", func(t *testing.T, artifact, _, blob string) {
			write(t, artifact, "a later revision")
			write(t, blob, "tampered")
		}},
		{"frozen-blocked", func(t *testing.T, artifact, reference, _ string) {
			write(t, artifact, "a later revision")
			block(t, reference)
		}},
		{"frozen-manifest-unreadable", func(t *testing.T, artifact, reference, _ string) {
			write(t, artifact, "a later revision")
			block(t, filepath.Join(reference, "MANIFEST.json"))
		}},
		{"frozen-corrupt", func(t *testing.T, artifact, reference, _ string) {
			write(t, artifact, "a later revision")
			write(t, filepath.Join(reference, "MANIFEST.json"), "not json")
		}},
		{"frozen-not-utf8", func(t *testing.T, artifact, reference, _ string) {
			write(t, artifact, "a later revision")
			write(t, filepath.Join(reference, "MANIFEST.json"), "{\"entries\": [\xff]}")
		}},
		{"frozen-not-a-manifest", func(t *testing.T, artifact, reference, _ string) {
			write(t, artifact, "a later revision")
			write(t, filepath.Join(reference, "MANIFEST.json"), `{"serialization": "none"}`)
		}},
		{"frozen-a-list", func(t *testing.T, artifact, reference, _ string) {
			write(t, artifact, "a later revision")
			write(t, filepath.Join(reference, "MANIFEST.json"), "[]")
		}},
		{"frozen-entries-null", func(t *testing.T, artifact, reference, _ string) {
			write(t, artifact, "a later revision")
			write(t, filepath.Join(reference, "MANIFEST.json"), `{"entries": null}`)
		}},
		{"frozen-entries-empty-object", func(t *testing.T, artifact, reference, _ string) {
			write(t, artifact, "a later revision")
			write(t, filepath.Join(reference, "MANIFEST.json"), `{"entries": {}}`)
		}},
		{"frozen-record-a-number", func(t *testing.T, artifact, reference, _ string) {
			write(t, artifact, "a later revision")
			write(t, filepath.Join(reference, "MANIFEST.json"), `{"entries": [1.5]}`)
		}},
		{"frozen-record-with-a-relative-path", func(t *testing.T, artifact, reference, blob string) {
			write(t, artifact, "a later revision")
			write(t, filepath.Join(reference, "MANIFEST.json"), `{"entries": [{"path": "relative/deliver.txt", "sha256": "`+filepath.Base(blob)+`"}]}`)
		}},
		{"frozen-blobs-blocked", func(t *testing.T, artifact, reference, _ string) {
			write(t, artifact, "a later revision")
			block(t, filepath.Join(reference, "files"))
		}},
		{"live-gone-frozen-absent", func(t *testing.T, artifact, reference, _ string) {
			remove(t, artifact)
			remove(t, reference)
		}},
		{"live-blocked-frozen-corrupt", func(t *testing.T, artifact, reference, _ string) {
			block(t, filepath.Dir(artifact))
			write(t, filepath.Join(reference, "MANIFEST.json"), "not json")
		}},
		{"live-blocked-frozen-blocked", func(t *testing.T, artifact, reference, _ string) {
			block(t, reference)
			block(t, filepath.Dir(artifact))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			base := t.TempDir()
			work := filepath.Join(base, "work")
			if err := os.Mkdir(work, 0o700); err != nil {
				t.Fatal(err)
			}
			artifact := filepath.Join(work, "deliver.txt")
			write(t, artifact, "the delivered bytes")
			entries, err := store.BuildManifest([]string{artifact}, []string{work})
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
			c.stage(t, artifact, reference, filepath.Join(reference, "files", entries[0].SHA256))
			raw, err := json.Marshal(map[string]any{"payload": map[string]any{"manifest": []any{map[string]any{"path": entries[0].Path, "sha256": entries[0].SHA256, "bytes": *entries[0].Bytes}}, "revisionHash": revision}, "reference": reference, "roots": []string{work}})
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(python(t), "-c", `import json, sys
from codex_session_relay.guard import deliverable_state
spec = json.load(sys.stdin)
print(json.dumps(list(deliverable_state(spec["payload"], spec["reference"], spec["roots"]))))`)
			cmd.Stdin = bytes.NewReader(raw)
			want, err := cmd.Output()
			if err != nil {
				t.Fatalf("python: %v", err)
			}
			spec, err := Decode(raw)
			if err != nil {
				t.Fatal(err)
			}
			o, _ := spec.(Object)
			state, binding, detail := DeliverableState(context.Background(), get(o, "payload"), reference, get(o, "roots"))
			var guard []any
			if err := json.Unmarshal(want, &guard); err != nil {
				t.Fatalf("%v: %s", err, want)
			}
			got := []any{state, nullable(binding), nullable(detail)}
			if !reflect.DeepEqual(got, guard) {
				t.Fatalf("hook  %q\nguard %q", got, guard)
			}
		})
	}
}
