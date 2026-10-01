package hook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// deliverableStateCase stages one stored receipt's deliverable. refer, when set, is the frozen
// reference the receipt names instead of the copy's own path; claimed, when set, is the stored
// receipt's manifest record, with %[1]s the artifact's path and %[2]s its digest.
type deliverableStateCase struct {
	name    string
	stage   func(t *testing.T, artifact, reference, blob string)
	refer   func(t *testing.T, reference string, entries []store.ManifestEntry) string
	claimed string
}

// The Stop hook judges a stored receipt's deliverable by guard.deliverable_state's rule: a frozen
// copy nobody could reach leaves it unverifiable with the reason named, one that was read and is
// not a manifest is changed, answered with the exception the fence raised rather than merged into
// the live problems, and one nested deeper than json.loads descends raises out of it. Every
// record, the stored receipt's and the frozen copy's, is read as the values json.loads made of
// it. Each case's answer is its golden word for word, which began as guard.deliverable_state's.
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
	moved := func(t *testing.T, artifact string) { write(t, artifact, "a later revision") }
	cases := []deliverableStateCase{
		{name: "live-current", stage: func(*testing.T, string, string, string) {}},
		{name: "frozen-current", stage: func(t *testing.T, artifact, _, _ string) { moved(t, artifact) }},
		{name: "frozen-absent", stage: func(t *testing.T, artifact, reference, _ string) {
			moved(t, artifact)
			remove(t, reference)
		}},
		{name: "frozen-tampered", stage: func(t *testing.T, artifact, _, blob string) {
			moved(t, artifact)
			write(t, blob, "tampered")
		}},
		{name: "frozen-blocked", stage: func(t *testing.T, artifact, reference, _ string) {
			moved(t, artifact)
			block(t, reference)
		}},
		{name: "frozen-manifest-unreadable", stage: func(t *testing.T, artifact, reference, _ string) {
			moved(t, artifact)
			block(t, filepath.Join(reference, "MANIFEST.json"))
		}},
		{name: "frozen-corrupt", stage: func(t *testing.T, artifact, reference, _ string) {
			moved(t, artifact)
			write(t, filepath.Join(reference, "MANIFEST.json"), "not json")
		}},
		// The fence reads a frozen MANIFEST.json whole, so a copy past the hook's 4 MiB bound on
		// other evidence is read under the deadline alone, as decision 24's native reads are.
		{name: "frozen-past-the-evidence-bound", stage: func(t *testing.T, artifact, reference, blob string) {
			moved(t, artifact)
			path, _ := json.Marshal(artifact)
			padding := strings.Repeat("x", maxInputBytes+1<<20)
			write(t, filepath.Join(reference, "MANIFEST.json"), `{"entries": [{"path": `+string(path)+`, "sha256": "`+filepath.Base(blob)+`", "bytes": 19}], "padding": "`+padding+`"}`)
		}},
		{name: "frozen-not-utf8", stage: func(t *testing.T, artifact, reference, _ string) {
			moved(t, artifact)
			write(t, filepath.Join(reference, "MANIFEST.json"), "{\"entries\": [\xff]}")
		}},
		{name: "frozen-not-a-manifest", stage: func(t *testing.T, artifact, reference, _ string) {
			moved(t, artifact)
			write(t, filepath.Join(reference, "MANIFEST.json"), `{"serialization": "none"}`)
		}},
		{name: "frozen-a-list", stage: func(t *testing.T, artifact, reference, _ string) {
			moved(t, artifact)
			write(t, filepath.Join(reference, "MANIFEST.json"), "[]")
		}},
		{name: "frozen-entries-null", stage: func(t *testing.T, artifact, reference, _ string) {
			moved(t, artifact)
			write(t, filepath.Join(reference, "MANIFEST.json"), `{"entries": null}`)
		}},
		{name: "frozen-entries-empty-object", stage: func(t *testing.T, artifact, reference, _ string) {
			moved(t, artifact)
			write(t, filepath.Join(reference, "MANIFEST.json"), `{"entries": {}}`)
		}},
		{name: "frozen-record-a-number", stage: func(t *testing.T, artifact, reference, _ string) {
			moved(t, artifact)
			write(t, filepath.Join(reference, "MANIFEST.json"), `{"entries": [1.5]}`)
		}},
		{name: "frozen-record-with-a-relative-path", stage: func(t *testing.T, artifact, reference, blob string) {
			moved(t, artifact)
			write(t, filepath.Join(reference, "MANIFEST.json"), `{"entries": [{"path": "relative/deliver.txt", "sha256": "`+filepath.Base(blob)+`"}]}`)
		}},
		{name: "frozen-blobs-blocked", stage: func(t *testing.T, artifact, reference, _ string) {
			moved(t, artifact)
			block(t, filepath.Join(reference, "files"))
		}},
		{name: "live-gone-frozen-absent", stage: func(t *testing.T, artifact, reference, _ string) {
			remove(t, artifact)
			remove(t, reference)
		}},
		{name: "live-blocked-frozen-corrupt", stage: func(t *testing.T, artifact, reference, _ string) {
			block(t, filepath.Dir(artifact))
			write(t, filepath.Join(reference, "MANIFEST.json"), "not json")
		}},
		{name: "live-blocked-frozen-blocked", stage: func(t *testing.T, artifact, reference, _ string) {
			block(t, reference)
			block(t, filepath.Dir(artifact))
		}},
		// A reference is a path the kernel walks, '..' after the component before it: through a
		// missing directory the walk fails, and through a symlink it reaches the link target's
		// parent, whose copy is good while the one at the lexical parent is tampered.
		{name: "frozen-parent-of-missing", stage: func(t *testing.T, artifact, _, _ string) { moved(t, artifact) },
			refer: func(_ *testing.T, reference string, _ []store.ManifestEntry) string {
				return filepath.Dir(reference) + "/missing/../frozen"
			}},
		{name: "frozen-parent-through-symlink", stage: func(t *testing.T, artifact, _, blob string) {
			write(t, blob, "tampered")
			moved(t, artifact)
		}, refer: func(t *testing.T, reference string, entries []store.ManifestEntry) string {
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
		{name: "frozen-blocked-parent-of-a-sibling", stage: func(t *testing.T, artifact, reference, _ string) {
			moved(t, artifact)
			block(t, reference)
		}, refer: func(_ *testing.T, reference string, _ []store.ManifestEntry) string {
			return filepath.Dir(reference) + "//work/./../frozen/"
		}},
		{name: "frozen-blocked-double-slash", stage: func(t *testing.T, artifact, reference, _ string) {
			moved(t, artifact)
			block(t, reference)
		}, refer: func(_ *testing.T, reference string, _ []store.ManifestEntry) string { return "/" + reference }},
		// The stored receipt's own records are read the same way: its bytes compared as the
		// values they are, and a path or digest of another type raising what the fence raises.
		{name: "claimed-bytes-numeric-string", stage: func(*testing.T, string, string, string) {},
			claimed: `{"path": %[1]s, "sha256": %[2]s, "bytes": "19"}`},
		{name: "claimed-bytes-float", stage: func(*testing.T, string, string, string) {},
			claimed: `{"path": %[1]s, "sha256": %[2]s, "bytes": 19.0}`},
		{name: "claimed-bytes-true", stage: func(*testing.T, string, string, string) {},
			claimed: `{"path": %[1]s, "sha256": %[2]s, "bytes": true}`},
		{name: "claimed-bytes-nan", stage: func(*testing.T, string, string, string) {},
			claimed: `{"path": %[1]s, "sha256": %[2]s, "bytes": NaN}`},
		{name: "claimed-and-frozen-bytes-list", stage: func(t *testing.T, artifact, reference, blob string) {
			write(t, filepath.Join(reference, "MANIFEST.json"), `{"entries": [{"path": "`+artifact+`", "sha256": "`+filepath.Base(blob)+`", "bytes": [19]}]}`)
		}, claimed: `{"path": %[1]s, "sha256": %[2]s, "bytes": [19]}`},
		{name: "claimed-and-frozen-bytes-nan", stage: func(t *testing.T, artifact, reference, blob string) {
			write(t, filepath.Join(reference, "MANIFEST.json"), `{"entries": [{"path": "`+artifact+`", "sha256": "`+filepath.Base(blob)+`", "bytes": NaN}]}`)
		}, claimed: `{"path": %[1]s, "sha256": %[2]s, "bytes": NaN}`},
		{name: "claimed-and-frozen-bytes-nan-in-a-list", stage: func(t *testing.T, artifact, reference, blob string) {
			write(t, filepath.Join(reference, "MANIFEST.json"), `{"entries": [{"path": "`+artifact+`", "sha256": "`+filepath.Base(blob)+`", "bytes": [NaN]}]}`)
		}, claimed: `{"path": %[1]s, "sha256": %[2]s, "bytes": [NaN]}`},
		{name: "claimed-path-number", stage: func(*testing.T, string, string, string) {},
			claimed: `{"path": 5, "sha256": %[2]s}`},
		{name: "claimed-digest-null", stage: func(*testing.T, string, string, string) {},
			claimed: `{"path": %[1]s, "sha256": null}`},
		{name: "claimed-record-a-list", stage: func(*testing.T, string, string, string) {},
			claimed: `[1]`},
		{name: "claimed-record-without-digest", stage: func(*testing.T, string, string, string) {},
			claimed: `{"path": %[1]s}`},
	}
	// A frozen MANIFEST.json no freeze writes is read as json.loads reads it.
	for _, manifest := range testsupport.FrozenManifests() {
		cases = append(cases, deliverableStateCase{name: manifest.Name, stage: func(t *testing.T, artifact, reference, blob string) {
			moved(t, artifact)
			write(t, filepath.Join(reference, "MANIFEST.json"), manifest.Document(artifact, filepath.Base(blob)))
		}})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// A fixed length: a decoder's refusal of a MANIFEST.json counts its position past the
			// paths it names.
			base := fixedLengthDir(t, t.TempDir(), 200)
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
			named := reference
			if c.refer != nil {
				named = c.refer(t, reference, entries)
			}
			c.stage(t, artifact, reference, filepath.Join(reference, "files", entries[0].SHA256))
			quote := func(v any) string {
				raw, err := json.Marshal(v)
				if err != nil {
					t.Fatal(err)
				}
				return string(raw)
			}
			record := fmt.Sprintf(`{"path": %s, "sha256": %s, "bytes": %d}`, quote(entries[0].Path), quote(entries[0].SHA256), *entries[0].Bytes)
			switch {
			case strings.Contains(c.claimed, "%"):
				record = fmt.Sprintf(c.claimed, quote(entries[0].Path), quote(entries[0].SHA256))
			case c.claimed != "":
				record = c.claimed
			}
			raw := []byte(fmt.Sprintf(`{"payload": {"manifest": [%s], "revisionHash": %s}, "reference": %s, "roots": %s}`, record, quote(revision), quote(named), quote([]string{work})))
			spec, err := Decode(raw)
			if err != nil {
				t.Fatal(err)
			}
			o, _ := spec.(Object)
			state, binding, detail, raised := DeliverableState(context.Background(), get(o, "payload"), named, get(o, "roots"))
			got := []any{state, nullable(binding), nullable(detail)}
			if raised != nil {
				var exception *store.ManifestException
				if !errors.As(raised, &exception) {
					t.Fatalf("raised %T %v", raised, raised)
				}
				got = []any{"raised", exception.PythonText()}
			}
			goldenDumps(t, "deliverable_state", got, false, golden.Substitute(base, "<BASE>"))
		})
	}
}
