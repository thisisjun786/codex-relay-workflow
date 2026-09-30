package adapter

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

func scopeCapture(t *testing.T, spec map[string]any, got any) {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	out := pyDriver(t, "scope_capture.py", raw, runDerived(spec, got)...)
	var want any
	if err := json.Unmarshal(out, &want); err != nil {
		t.Fatal(err)
	}
	expected, _ := json.Marshal(want)
	actual, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(expected, actual) {
		t.Fatalf("Go %s\nPython %s", actual, expected)
	}
}

// jsonPosition is where a JSON decoder says a document went wrong.
var jsonPosition = regexp.MustCompile(`line \d+ column \d+ \(char \d+\)`)

// runDerived names the values of a scope answer that follow from the run's own paths (asGoAnswers):
// the digest of a frozen manifest, which lists the test's temporary files, and the offset at
// which such a manifest fails to decode.
func runDerived(spec map[string]any, got any) []pyoracle.Option {
	result, ok := got.(map[string]any)
	if !ok || spec["op"] != "frozen" {
		return nil
	}
	var options []pyoracle.Option
	if digest, ok := result["digest"].(string); ok && digest != "" {
		options = append(options, asGoAnswers(digest, "<frozen-manifest-digest>"))
	}
	if message, ok := result["error"].(string); ok {
		if position := jsonPosition.FindString(message); position != "" {
			options = append(options, asGoAnswers(position, "<json-error-position>"))
		}
	}
	return options
}
func entriesRecord(entries []Entry) []any {
	out := []any{}
	for _, e := range entries {
		r := map[string]any{"path": e.Path, "sha256": e.SHA256}
		if e.Bytes != nil {
			r["bytes"] = *e.Bytes
		}
		out = append(out, r)
	}
	return out
}
func Test28_MSC_1_CanonicalManifest(t *testing.T) {
	for _, entries := range [][]Entry{{{Path: "/b/two", SHA256: strings.Repeat("b", 64)}, {Path: "/a/one", SHA256: strings.Repeat("a", 64)}}, {{Path: "/a", SHA256: strings.Repeat("a", 64)}}, {}, {{Path: "/a/b", SHA256: strings.Repeat("1", 64)}, {Path: "/a/B", SHA256: strings.Repeat("2", 64)}, {Path: "/a/-", SHA256: strings.Repeat("3", 64)}}} {
		payload, err := CanonicalPayload(entries)
		if err != nil {
			t.Fatal(err)
		}
		revision, err := RevisionHash(entries)
		if err != nil {
			t.Fatal(err)
		}
		scopeCapture(t, map[string]any{"op": "canonical", "entries": entriesRecord(entries)}, map[string]any{"payload": payload, "revision": revision})
	}
}

// A digest is its 64 lowercase hex characters and nothing after them: Python's '$' also matched
// just before a final newline, so the fence's canonical form took '<hex>\n' where Go's refused it.
func Test28_MSC_1b_DigestEndingInANewlineIsRefused(t *testing.T) {
	entries := []Entry{{Path: "/a", SHA256: strings.Repeat("a", 64) + "\n"}}
	_, err := CanonicalPayload(entries)
	if err == nil {
		t.Fatal("a digest ending in a newline was canonicalized")
	}
	scopeCapture(t, map[string]any{"op": "canonical", "entries": entriesRecord(entries)}, map[string]any{"error": err.Error()})
}
func Test28_MSC_2_NormalizedAbsolutePaths(t *testing.T) {
	for _, path := range []string{"relative/path", "/a/../b", "/a/b/", "/a/./b", "~/a"} {
		_, err := NormalizeDeclaredPath(path)
		if err == nil {
			t.Fatal("accepted", path)
		}
		scopeCapture(t, map[string]any{"op": "normalize", "path": path}, map[string]any{"error": err.Error()})
	}
}
func Test28_MSC_3_ComponentContainment(t *testing.T) {
	for _, pair := range [][2]string{{"/a/b", "/a/b"}, {"/a/b", "/a/b/c"}, {"/a/b", "/a/bc"}, {"/a/b", "/a/bc/d"}, {"/a/b", "/a"}, {"/", "/a/b"}, {"/", "a/b"}} {
		scopeCapture(t, map[string]any{"op": "within", "root": pair[0], "path": pair[1]}, IsWithin(pair[0], pair[1]))
	}
}
func hashCapture(t *testing.T, path string, roots []string, lease bool) {
	t.Helper()
	digest, size, binding, err := HashPath(path, roots, lease)
	var result any
	if err != nil {
		result = map[string]any{"error": err.Error()}
	} else {
		result = map[string]any{"digest": digest, "size": size, "binding": binding.Record()}
	}
	scopeCapture(t, map[string]any{"op": "hash", "path": path, "roots": roots, "lease": lease}, result)
}
func Test28_MSC_4_PinnedTraversal(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(real, "file.txt")
	if err := os.WriteFile(file, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, link := range []struct{ name, target string }{{"link.txt", file}, {"alias", real}, {"outside", filepath.Join(root, "elsewhere")}} {
		if err := os.Symlink(link.target, filepath.Join(root, link.name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{filepath.Join(root, "link.txt"), filepath.Join(root, "alias/file.txt"), filepath.Join(root, "outside"), filepath.Join(root, "gone/f.txt"), real} {
		hashCapture(t, p, []string{root}, false)
	}
	hashCapture(t, filepath.Join(root, "alias/file.txt"), []string{filepath.Join(root, "alias")}, false)
	hashCapture(t, file, []string{filepath.Join(root, "other-root")}, false)
	if err := os.Rename(real, filepath.Join(root, "moved")); err != nil {
		t.Fatal(err)
	}
	hashCapture(t, file, []string{root}, false)
}
func verifyCapture(t *testing.T, entries []Entry, roots []string) {
	t.Helper()
	problems, bindings, unreadable := VerifyAgainstDiskDetailed(entries, roots, false)
	records := map[string]any{}
	for path, b := range bindings {
		records[path] = b.Record()
	}
	scopeCapture(t, map[string]any{"op": "verify", "entries": entriesRecord(entries), "roots": roots}, map[string]any{"problems": problems, "bindings": records, "unreadable": unreadable})
}
func Test28_MSC_7_VerifyAgainstDisk(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	if err := os.WriteFile(path, []byte("the full contents"), 0600); err != nil {
		t.Fatal(err)
	}
	entries, _, err := BuildManifest([]string{path}, []string{root}, false)
	if err != nil {
		t.Fatal(err)
	}
	verifyCapture(t, entries, []string{root})
	if err := os.WriteFile(path, []byte("trunc"), 0600); err != nil {
		t.Fatal(err)
	}
	verifyCapture(t, entries, []string{root})
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	verifyCapture(t, entries, []string{root})
}
func frozenCapture(t *testing.T, reference string, entries []Entry) {
	t.Helper()
	digest, problems, unreadable, err := VerifyFrozenDetailed(reference, entries)
	var result any = map[string]any{"digest": digest, "problems": problems, "unreadable": unreadable}
	if err != nil {
		result = map[string]any{"error": err.Error()}
	}
	var records any
	if entries != nil {
		records = entriesRecord(entries)
	}
	scopeCapture(t, map[string]any{"op": "frozen", "reference": reference, "entries": records}, result)
}
func frozenFixture(t *testing.T) (string, string, []Entry) {
	t.Helper()
	root := t.TempDir()
	file := filepath.Join(root, "deliver.txt")
	if err := os.WriteFile(file, []byte("the delivered bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	entries, _, err := BuildManifest([]string{file}, []string{root}, false)
	if err != nil {
		t.Fatal(err)
	}
	ref := filepath.Join(root, "frozen")
	if _, err := Freeze(entries, ref); err != nil {
		t.Fatal(err)
	}
	return file, ref, entries
}
func Test28_MSC_8_FrozenCopySurvivesRelocation(t *testing.T) {
	file, reference, entries := frozenFixture(t)
	if err := os.WriteFile(file, []byte("a later revision"), 0600); err != nil {
		t.Fatal(err)
	}
	verifyCapture(t, entries, []string{filepath.Dir(file)})
	frozenCapture(t, reference, entries)
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	frozenCapture(t, reference, nil)
	if err := os.WriteFile(filepath.Join(reference, "files", entries[0].SHA256), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	frozenCapture(t, reference, entries)
}

// frozenRaisedCapture is frozenCapture with an exception named by its class as well, which the
// fence's readers tell apart: an OSError or a ScopeError is a comparison that did not happen, and
// any other a frozen copy that is not a manifest.
func frozenRaisedCapture(t *testing.T, reference string, entries []Entry) {
	t.Helper()
	digest, problems, unreadable, err := VerifyFrozenDetailed(reference, entries)
	var result any = map[string]any{"digest": digest, "problems": problems, "unreadable": unreadable}
	if err != nil {
		raised, ok := store.PythonHostDetail(err)
		switch {
		case ok:
		case store.RefusalReason(err) != "":
			raised = "ScopeError: " + err.Error()
		default:
			raised = "RuntimeError: " + err.Error()
		}
		result = map[string]any{"error": raised}
	}
	var records any
	if entries != nil {
		records = entriesRecord(entries)
	}
	scopeCapture(t, map[string]any{"op": "frozen", "reference": reference, "entries": records, "classed": true}, result)
}

// verify_frozen_detailed reads a frozen MANIFEST.json as json.loads reads it and judges each
// record's fields as the values they are, and it reaches the copy by the path pathlib spells,
// whose '..' the kernel resolves. Go answers the same revision, problems and access failures, or
// raises the same exception, with the caller's entries and without them.
func Test28_MSC_8b_FrozenCopyIsReadAsTheFenceReadsIt(t *testing.T) {
	for _, manifest := range testsupport.FrozenManifests() {
		t.Run(manifest.Name, func(t *testing.T) {
			file, reference, entries := frozenFixture(t)
			if err := os.WriteFile(filepath.Join(reference, "MANIFEST.json"), []byte(manifest.Document(file, entries[0].SHA256)), 0o600); err != nil {
				t.Fatal(err)
			}
			frozenRaisedCapture(t, reference, entries)
			frozenRaisedCapture(t, reference, nil)
		})
	}
	t.Run("parent-steps", func(t *testing.T) {
		file, _, entries := frozenFixture(t)
		base := filepath.Dir(file)
		frozenRaisedCapture(t, base+"/missing/../frozen", entries)
		frozenRaisedCapture(t, base+"//./frozen/", entries)
		// lnk/.. is real, whose copy is not a manifest, where the lexical parent's copy is good.
		if err := os.MkdirAll(filepath.Join(base, "real", "inner"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(base, "real", "frozen"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "real", "frozen", "MANIFEST.json"), []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(base, "real", "inner"), filepath.Join(base, "lnk")); err != nil {
			t.Fatal(err)
		}
		frozenRaisedCapture(t, base+"/lnk/../frozen", entries)
		// A blocked directory stops the walk before the '..' after it could leave it.
		blocked := filepath.Join(base, "blocked")
		if err := os.MkdirAll(filepath.Join(blocked, "frozen"), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.Chmod(blocked, 0o700); err != nil {
				t.Error(err)
			}
		})
		if err := os.Chmod(blocked, 0); err != nil {
			t.Fatal(err)
		}
		frozenRaisedCapture(t, blocked+"/frozen/../../frozen", entries)
	})
}

func Test28_MSC_9_AccessFailureIsNotDisagreement(t *testing.T) {
	file, reference, entries := frozenFixture(t)
	// A declared component that exists but cannot be opened is unreadable,
	// not contrary evidence about the artifact's contents.
	component := filepath.Join(filepath.Dir(file), "inaccessible")
	if err := os.Mkdir(component, 0700); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(component, "artifact")
	if err := os.WriteFile(blocked, []byte("bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	blockedEntries, _, err := BuildManifest([]string{blocked}, []string{filepath.Dir(file)}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(component, 0700); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(component, 0); err != nil {
		t.Fatal(err)
	}
	verifyCapture(t, blockedEntries, []string{filepath.Dir(file)})
	t.Cleanup(func() {
		if err := os.Chmod(reference, 0700); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(reference, 0); err != nil {
		t.Fatal(err)
	}
	frozenCapture(t, reference, entries)
	if err := os.Chmod(reference, 0700); err != nil {
		t.Fatal(err)
	}
	files := filepath.Join(reference, "files")
	t.Cleanup(func() {
		if err := os.Chmod(files, 0700); err != nil && !os.IsNotExist(err) {
			t.Error(err)
		}
	})
	if err := os.Chmod(files, 0); err != nil {
		t.Fatal(err)
	}
	frozenCapture(t, reference, entries)
	if err := os.Chmod(files, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(files, entries[0].SHA256)); err != nil {
		t.Fatal(err)
	}
	frozenCapture(t, reference, entries)
	if err := os.Remove(files); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	frozenCapture(t, reference, entries)
	frozenCapture(t, filepath.Join(filepath.Dir(file), "never-frozen"), nil)
	frozenCapture(t, file, nil)
	verifyCapture(t, entries, []string{filepath.Join(filepath.Dir(file), "outside")})
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	verifyCapture(t, entries, []string{filepath.Dir(file)})
}
