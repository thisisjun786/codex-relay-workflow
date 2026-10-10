package doctor

// CRW-1152 correction round: the byte bound on plugin documents is selected by the harness
// caller, so the shared readers `crw doctor retrust` uses keep reading a valid document of any size,
// and the depth refusal of a document that nests past the limit costs no memory proportional to
// its depth.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

func harnessRound3Padded(t *testing.T, name string) (root string) {
	t.Helper()
	root = t.TempDir()
	harnessBoundedWrite(t, root, ".codex-plugin/plugin.json", `{"name":"crw","hooks":["./hooks/a.json"]}`)
	harnessBoundedWrite(t, root, "hooks/a.json", `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo ok"}]}]}}`)
	path := filepath.Join(root, filepath.FromSlash(name))
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	harnessBoundedWrite(t, root, name, string(body)+strings.Repeat(" ", harnessReadLimit))
	return root
}

// The shared readers read a valid manifest or hook document past the harness limit whole.
func TestHarnessRound3SharedPluginReadersAreUnbounded(t *testing.T) {
	for _, name := range []string{".codex-plugin/plugin.json", "hooks/a.json"} {
		t.Run(name, func(t *testing.T) {
			root := harnessRound3Padded(t, name)
			entries, err := ListHookTrustEntries(root, "crw@local")
			if err != nil || len(entries) != 1 {
				t.Fatalf("ListHookTrustEntries = %v, %v; want one entry", entries, err)
			}
		})
	}
	t.Run("discovery", func(t *testing.T) {
		root := harnessRound3Padded(t, ".codex-plugin/plugin.json")
		if !hookTrustRetrustDeclaresHooks(root) {
			t.Fatal("a manifest past the harness limit that declares hooks is not a retrust candidate")
		}
	})
}

// The harness hook-trust check still reads the same documents within the bound and reports them.
func TestHarnessRound3HarnessPluginReadsAreBounded(t *testing.T) {
	for _, name := range []string{".codex-plugin/plugin.json", "hooks/a.json"} {
		t.Run(name, func(t *testing.T) {
			root := harnessRound3Padded(t, name)
			codexHome := t.TempDir()
			harnessBoundedWrite(t, codexHome, "config.toml", `[plugins."crw@local"]`+"\nenabled = true\n")
			check := harnessBoundedWithin(t, "the hook-trust check", func() HarnessCheck {
				return HarnessHookTrustCheck(root, HarnessOptions{CodexHome: &codexHome}, harnessRunEnv(nil))
			})
			if check.Severity != HarnessFail || !strings.Contains(check.Evidence, errHarnessTooLarge.Error()) {
				t.Fatalf("check = %+v, want a FAIL naming the read limit", check)
			}
		})
	}
}

// A document that nests past the limit is refused as too deep without allocating a value, a frame
// or anything else in proportion to how deep it nests (4 MB of brackets is 2,000,000 levels).
func TestHarnessRound3DepthRefusalDoesNotMaterializeTheDocument(t *testing.T) {
	doc := strings.Repeat("[", 2_000_000) + strings.Repeat("]", 2_000_000)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := harnessParseBounded(doc, pyjson.LoadOptions{Surrogates: true})
	runtime.ReadMemStats(&after)
	if err != errHarnessTooDeep {
		t.Fatalf("err = %v, want errHarnessTooDeep", err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 24<<20 {
		t.Fatalf("the refusal allocated %d MiB for a %d byte document, want under 24", allocated>>20, len(doc))
	}
}
