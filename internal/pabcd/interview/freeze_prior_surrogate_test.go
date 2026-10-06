package interview

// The prior manifest is read losslessly (CRW-584). A lone surrogate escape stays the three WTF-8 bytes the repository's reader keeps
// it in, so a manifest naming "\ud800.md" and a plan holding the real file U+FFFD.md are two different paths, and the verdict is
// STALE, as the oracle's JSON.parse decides (freeze-cli.ts:110-115, freeze.ts:102-121). The port read the manifest with
// encoding/json, which turned the escape into U+FFFD, so it answered fresh; these are the regression it lacked.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// priorSurrogateFixture builds a workspace whose plan directory holds the real file U+FFFD.md and whose prior manifest names
// escape + ".md", where escape is the literal characters of a JSON escape (a Go literal cannot hold a lone surrogate). The manifest
// carries the hashes the current plan really has, so the frozen path is the only difference: the baseline reads the escape as
// U+FFFD, matches the file, and answers fresh; the lossless read keeps the escape apart and answers STALE. It returns the workspace
// and the manifest's path.
func priorSurrogateFixture(t *testing.T, escape string) (ws, manifest string) {
	t.Helper()
	ws = t.TempDir()
	plan := filepath.Join(ws, ".crw", "plan", "default")
	if err := os.MkdirAll(plan, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plan, "\ufffd.md"), []byte("x"), 0o666); err != nil {
		t.Fatal(err)
	}
	files := []PlanFileHash{{Path: "\ufffd.md", Sha256: Sha256("x")}}
	doc := "{\"planFiles\":[{\"path\":\"" + escape + ".md\",\"sha256\":\"" + Sha256("x") + "\"}],\"planHash\":\"" + ComputePlanHash(files) + "\"}"
	dir := filepath.Join(ws, ".crw", "interview")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	manifest = filepath.Join(dir, "freeze.json")
	if err := os.WriteFile(manifest, []byte(doc), 0o666); err != nil {
		t.Fatal(err)
	}
	return ws, manifest
}

// priorStaleLine is what a stale verdict over this fixture prints: the frozen path misses and the current U+FFFD path is unfrozen, so
// two files changed, while the plan hash still matches.
const priorStaleLine = "stale-check: STALE \u2014 plan changed since freeze (2 file(s), planHash matches); re-freeze before goal start \u2014 stale execution refused"

// A prior manifest naming "\ud800.md" is stale against a plan holding U+FFFD.md, as the oracle decides. The whole line is asserted:
// a bare "STALE" would already hold on the baseline for a manifest with a wrong plan hash, so the changed-file count is what pins
// this criterion.
func TestFreezePriorLoneSurrogateIsStale(t *testing.T) {
	ws, _ := priorSurrogateFixture(t, "\\ud800")
	out, err := RunFreeze(FreezeCliArgs{Cwd: ws, SessionID: "default", DryRun: true}, freezeFileReader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, priorStaleLine) {
		t.Errorf("output\n got %q\nwant it to hold %q", out, priorStaleLine)
	}
}

// The other half of the surrogate range: a lone low surrogate escape is kept as its own bytes too.
func TestFreezePriorLoneLowSurrogateIsStale(t *testing.T) {
	ws, _ := priorSurrogateFixture(t, "\\udc00")
	out, err := RunFreeze(FreezeCliArgs{Cwd: ws, SessionID: "default", DryRun: true}, freezeFileReader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, priorStaleLine) {
		t.Errorf("output\n got %q\nwant it to hold %q", out, priorStaleLine)
	}
}

// The read itself: the escape is kept as the three WTF-8 bytes U+D800 is held in, not as U+FFFD.
func TestReadPriorKeepsALoneSurrogateEscape(t *testing.T) {
	_, manifest := priorSurrogateFixture(t, "\\ud800")
	frozen, _, ok := readPrior(manifest)
	if !ok || len(frozen) != 1 {
		t.Fatalf("readPrior: %d entries, ok %v", len(frozen), ok)
	}
	want := string([]byte{0xed, 0xa0, 0x80}) + ".md"
	if frozen[0].key.kind != 's' || frozen[0].key.s != want {
		t.Errorf("key %q (kind %q), want %q", frozen[0].key.s, frozen[0].key.kind, want)
	}
}

// Ordinary names are untouched: a manifest that matches the plan stays fresh.
func TestFreezePriorOrdinaryNamesStayFresh(t *testing.T) {
	ws := t.TempDir()
	plan := filepath.Join(ws, ".crw", "plan", "default")
	if err := os.MkdirAll(plan, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plan, "a.md"), []byte("a"), 0o666); err != nil {
		t.Fatal(err)
	}
	files := []PlanFileHash{{Path: "a.md", Sha256: Sha256("a")}}
	doc := "{\"planFiles\":[{\"path\":\"a.md\",\"sha256\":\"" + Sha256("a") + "\"}],\"planHash\":\"" + ComputePlanHash(files) + "\"}"
	dir := filepath.Join(ws, ".crw", "interview")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "freeze.json"), []byte(doc), 0o666); err != nil {
		t.Fatal(err)
	}
	out, err := RunFreeze(FreezeCliArgs{Cwd: ws, SessionID: "default", DryRun: true}, freezeFileReader)
	if err != nil {
		t.Fatal(err)
	}
	if want := "stale-check: fresh \u2014 frozen manifest matches current plan"; !strings.Contains(out, want) {
		t.Errorf("output\n got %q\nwant it to hold %q", out, want)
	}
}

// The cases the read refuses stay refused: unparseable bytes, a root that is not an object, planFiles that is not an array, a null
// entry, and data after the value.
func TestFreezePriorRefusalsAreUnchanged(t *testing.T) {
	for _, tc := range []struct{ name, doc string }{
		{"unparseable", "{not json"},
		{"root_not_object", "null"},
		{"plan_files_string", "{\"planFiles\":\"abc\"}"},
		{"null_entry", "{\"planFiles\":[null]}"},
		{"trailing_data", "{\"planFiles\":[]}x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			dir := filepath.Join(ws, ".crw", "interview")
			if err := os.MkdirAll(dir, 0o777); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "freeze.json"), []byte(tc.doc), 0o666); err != nil {
				t.Fatal(err)
			}
			out, err := RunFreeze(FreezeCliArgs{Cwd: ws, SessionID: "default", DryRun: true}, freezeFileReader)
			if err != nil {
				t.Fatal(err)
			}
			if want := "stale-check: prior manifest unreadable (will re-freeze)"; !strings.Contains(out, want) {
				t.Errorf("output\n got %q\nwant it to hold %q", out, want)
			}
		})
	}
}
