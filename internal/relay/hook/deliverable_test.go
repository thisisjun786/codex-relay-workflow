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
			state, binding, detail, raised := DeliverableState(context.Background(), o.Get("payload"), named, o.Get("roots"))
			got := []any{state, nullable(binding), nullable(detail)}
			if raised != nil {
				var exception *store.ManifestException
				if !errors.As(raised, &exception) {
					t.Fatalf("raised %T %v", raised, raised)
				}
				got = []any{"raised", exception.StoredText()}
			}
			goldenDumps(t, "deliverable_state", got, false, golden.Substitute(base, "<BASE>"))
		})
	}
}

// The exported entry accepts values a caller built without decoding: a receipt as a map whose
// manifest is a list of ordered records, and the roots as a list of strings. They are judged as the
// decoded equivalents are, not answered as a receipt of the wrong shape (a map payload used to
// reach the store as "AttributeError: ... has no attribute 'get'", and []string roots as
// "TypeError: artifact roots are not a list").
func TestDeliverableStateAcceptsValuesACallerBuilt(t *testing.T) {
	base := t.TempDir()
	work := filepath.Join(base, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(work, "deliver.txt")
	if err := os.WriteFile(artifact, []byte("the delivered bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := store.BuildManifest([]string{artifact}, []string{work})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := store.ManifestRevision(entries)
	if err != nil {
		t.Fatal(err)
	}
	record := Object{{Key: "path", Value: entries[0].Path}, {Key: "sha256", Value: entries[0].SHA256}, {Key: "bytes", Value: *entries[0].Bytes}}
	for name, built := range map[string]struct{ payload, roots any }{
		"a map with a list of records and string roots": {map[string]any{"manifest": []any{record}, "revisionHash": revision}, []string{work}},
		"a map and roots as a list of any":              {map[string]any{"manifest": []any{record}, "revisionHash": revision}, []any{work}},
		"an ordered object and a list of any":           {Object{{Key: "manifest", Value: []any{record}}, {Key: "revisionHash", Value: revision}}, []any{work}},
	} {
		t.Run(name, func(t *testing.T) {
			state, binding, detail, raised := DeliverableState(context.Background(), built.payload, "", built.roots)
			if raised != nil || state != "current" || binding != "live" || detail != "" {
				t.Fatalf("%q %q %q %v", state, binding, detail, raised)
			}
		})
	}
}

// A caller that builds a receipt without decoding it may build the manifest's records as maps too
// (a []map[string]any, or a []any holding them). They are read as the ordered objects the store
// reads, so an unchanged artifact answers current, while the rest of the answer stays what it
// was: a changed artifact is changed, naming the digest the manifest claims, and a record that is
// not a map is still the TypeError the store raises for it.
func TestDeliverableStateAcceptsMapManifestRecords(t *testing.T) {
	type staged struct {
		work, path, digest, revision string
		size                         int64
	}
	// stage builds a manifest over one artifact and then, when later is set, replaces the
	// artifact's bytes with those, so the manifest describes the bytes the artifact had.
	stage := func(t *testing.T, later string) staged {
		t.Helper()
		work := filepath.Join(t.TempDir(), "work")
		if err := os.Mkdir(work, 0o700); err != nil {
			t.Fatal(err)
		}
		artifact := filepath.Join(work, "deliver.txt")
		if err := os.WriteFile(artifact, []byte("the delivered bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		entries, err := store.BuildManifest([]string{artifact}, []string{work})
		if err != nil {
			t.Fatal(err)
		}
		revision, err := store.ManifestRevision(entries)
		if err != nil {
			t.Fatal(err)
		}
		if later != "" {
			if err := os.WriteFile(artifact, []byte(later), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return staged{work: work, path: entries[0].Path, digest: entries[0].SHA256, revision: revision, size: *entries[0].Bytes}
	}
	shapes := []struct {
		name     string
		manifest func(staged) any
	}{
		{"a []map[string]any", func(s staged) any {
			return []map[string]any{{"path": s.path, "sha256": s.digest, "bytes": s.size}}
		}},
		{"a []any holding maps", func(s staged) any {
			return []any{map[string]any{"path": s.path, "sha256": s.digest, "bytes": s.size}}
		}},
		{"a map with an int byte count", func(s staged) any {
			return []any{map[string]any{"path": s.path, "sha256": s.digest, "bytes": int(s.size)}}
		}},
		{"a map without a byte count", func(s staged) any {
			return []any{map[string]any{"path": s.path, "sha256": s.digest}}
		}},
	}
	receipts := []struct {
		name  string
		build func(manifest any, revision string) any
	}{
		{"a map receipt", func(manifest any, revision string) any {
			return map[string]any{"manifest": manifest, "revisionHash": revision}
		}},
		{"an ordered receipt", func(manifest any, revision string) any {
			return Object{{Key: "manifest", Value: manifest}, {Key: "revisionHash", Value: revision}}
		}},
	}
	for _, shape := range shapes {
		for _, receipt := range receipts {
			t.Run("unchanged "+shape.name+" in "+receipt.name, func(t *testing.T) {
				s := stage(t, "")
				state, binding, detail, raised := DeliverableState(context.Background(), receipt.build(shape.manifest(s), s.revision), "", []string{s.work})
				if raised != nil || state != "current" || binding != "live" || detail != "" {
					t.Fatalf("%q %q %q %v", state, binding, detail, raised)
				}
			})
			// The same length, so only the digest tells the bytes apart. The detail is what separates
			// this from a record the store could not read, which is changed as well.
			t.Run("changed "+shape.name+" in "+receipt.name, func(t *testing.T) {
				s := stage(t, "THE DELIVERED BYTES")
				state, binding, detail, raised := DeliverableState(context.Background(), receipt.build(shape.manifest(s), s.revision), "", []string{s.work})
				if raised != nil || state != "changed" || binding != "" ||
					!strings.Contains(detail, ": bytes hash to ") || !strings.HasSuffix(detail, " but the manifest claims "+s.digest) {
					t.Fatalf("%q %q %q %v", state, binding, detail, raised)
				}
			})
		}
	}
	t.Run("a record that is not a map is still refused", func(t *testing.T) {
		s := stage(t, "")
		state, binding, detail, raised := DeliverableState(context.Background(), map[string]any{"manifest": []any{"deliver.txt"}, "revisionHash": s.revision}, "", []string{s.work})
		if raised != nil || state != "changed" || binding != "" || detail != "TypeError: string indices must be integers, not 'str'" {
			t.Fatalf("%q %q %q %v", state, binding, detail, raised)
		}
	})
}
