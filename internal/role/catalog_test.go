package role

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

func catalogJSON(t *testing.T, got any, want json.RawMessage) {
	t.Helper()
	a, b := any(nil), any(nil)
	check(t, json.Unmarshal(must(Stringify(got, "")), &a))
	check(t, json.Unmarshal(want, &b))
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("got %s; want %s", must(Stringify(got, "")), want)
	}
}

// Recorded directly from the pinned oracle, including the skill's extracted model values.
// The 16 original catalog cases are the model seam, nine composition rows and six native/path cases.
func TestCatalogOracle(t *testing.T) {
	var corpus struct {
		Models, DeclaredModels []string
		Build                  []struct {
			Name     string
			IDs      []string
			Provider *ProviderCatalogInput
			Expected json.RawMessage
		}
		Native []struct {
			Name, Input string
			Expected    json.RawMessage
		}
	}
	check(t, json.Unmarshal(must(os.ReadFile("testdata/oracle-catalog.json")), &corpus))
	t.Run("model-declaration-seam", func(t *testing.T) {
		if !reflect.DeepEqual(NativeOpenAIModels(), corpus.Models) || len(corpus.DeclaredModels) == 0 {
			t.Fatal("native model literals drifted")
		}
		for _, m := range corpus.DeclaredModels {
			if !slices.Contains(NativeOpenAIModels(), m) {
				t.Fatalf("declared model %q missing", m)
			}
		}
	})
	for _, c := range corpus.Build {
		t.Run("build/"+c.Name, func(t *testing.T) {
			catalogJSON(t, BuildCatalog(CatalogDeps{ReadNativeCache: func() []string { return c.IDs }, ProviderStatus: c.Provider}), portFixedBuild(t, c.Name, c.Expected))
		})
	}
	for _, c := range corpus.Native {
		t.Run("native/"+c.Name, func(t *testing.T) {
			env, dir := home(t)
			path := filepath.Join(dir, "models_cache.json")
			check(t, os.MkdirAll(dir, 0700))
			check(t, os.WriteFile(path, []byte(c.Input), 0600))
			lookup := func(k string) (string, bool) {
				if k == "CODEX_MODELS_CACHE_PATH" {
					return path, true
				}
				return env(k)
			}
			entries := ReadNativeCatalog(lookup)
			catalogJSON(t, entries, c.Expected)
			ids := ReadNativeCacheDefault(lookup)
			if entries == nil {
				if ids != nil {
					t.Fatal(ids)
				}
				return
			}
			want := make([]string, len(entries))
			for i, e := range entries {
				want[i] = e.ID
			}
			if !reflect.DeepEqual(ids, want) {
				t.Fatalf("ID-only reader: %v", ids)
			}
		})
	}
}

// portFixedBuild is the oracle's recorded buildCatalog answer with the CRW-1132 deviations applied, so the corpus
// stays the oracle's record: an entry has the one label the live reader gives it (the ID, without the oracle's
// " (native)" or " (ocx)" suffix) and an effort ladder member (null for an ID list, which carries none); a
// native list is deduplicated by exact ID and a blank ID is dropped, from the native list and from the
// provider's.
func portFixedBuild(t *testing.T, name string, recorded json.RawMessage) json.RawMessage {
	t.Helper()
	var answer struct {
		State   string           `json:"state"`
		Entries []map[string]any `json:"entries"`
	}
	check(t, json.Unmarshal(recorded, &answer))
	var kept []map[string]any
	seen := map[string]bool{}
	for _, e := range answer.Entries {
		id := e["id"].(string)
		if strings.TrimSpace(id) == "" || seen[id] {
			continue
		}
		seen[id] = true
		e["label"] = id
		e["reasoningEfforts"] = nil
		kept = append(kept, e)
	}
	if kept == nil {
		kept = []map[string]any{}
	}
	answer.Entries = kept
	return must(json.Marshal(answer))
}

func TestNativeCatalogPaths(t *testing.T) {
	root := t.TempDir()
	codex := filepath.Join(root, "codex")
	check(t, os.Mkdir(codex, 0700))
	vars := map[string]string{"HOME": root, "CODEX_HOME": " " + codex + " "}
	env := host.LookupEnv(func(k string) (string, bool) { v, ok := vars[k]; return v, ok })
	check(t, os.WriteFile(filepath.Join(codex, "models_cache.json"), []byte(`{"models":["default"]}`), 0600))
	for _, c := range []struct{ name, config, want string }{
		{"missing-config", "", filepath.Join(codex, "models_cache.json")},
		{"literal-comment", "model_catalog_json = 'custom.json' # comment\n[profile]\nmodel_catalog_json = 'wrong'", filepath.Join(codex, "custom.json")},
		{"quoted-key", "\ufeff\u00a0\"model_catalog_json\" = \"nested/../custom.json\"", filepath.Join(codex, "custom.json")},
		{"single-key", "'model_catalog_json' = '~/selected.json'", filepath.Join(root, "selected.json")},
		{"table-key-ignored", "[profile]\nmodel_catalog_json = 'wrong'", filepath.Join(codex, "models_cache.json")},
		{"invalid", "model_catalog_json = 42", ""},
		{"invalid-escape", `model_catalog_json = "\x41"`, ""},
		{"toml-unicode-escape", `model_catalog_json = "\U0001F600.json"`, filepath.Join(codex, "\U0001F600.json")},
		{"toml-short-unicode-escape", `model_catalog_json = "caf\u00e9.json"`, filepath.Join(codex, "caf\u00e9.json")},
		{"toml-escape-e", `model_catalog_json = "a\e.json"`, filepath.Join(codex, "a\x1b.json")},
		{"toml-common-escapes", `model_catalog_json = "a\tb\\c\"d.json"`, filepath.Join(codex, "a\tb\\c\"d.json")},
		{"toml-raw-tab", "model_catalog_json = \"a\tb.json\"", filepath.Join(codex, "a\tb.json")},
		{"toml-escaped-absolute", `model_catalog_json = "` + root + `/sub\u002fmodels.json"`, root + "/sub/models.json"},
		{"toml-json-only-slash-escape", `model_catalog_json = "a\/b.json"`, ""},
		{"toml-surrogate-escape", `model_catalog_json = "\ud83d\ude00.json"`, ""},
		{"toml-out-of-range-escape", `model_catalog_json = "\UFFFFFFFF.json"`, ""},
		{"toml-short-escape", `model_catalog_json = "\u12.json"`, ""},
		{"toml-control-character", "model_catalog_json = \"a\x01b.json\"", ""},
		{"toml-literal-keeps-backslash", `model_catalog_json = 'a\U0001F600.json'`, filepath.Join(codex, "a\\U0001F600.json")},
		{"blank", `model_catalog_json = ' '`, ""},
		{"tilde-trailing-slash", "model_catalog_json = '~/models.json/'", filepath.Join(root, "models.json") + "/"},
		{"tilde-empty-remainder", "model_catalog_json = '~/'", root},
		// The oracle joins and resolves lexically (catalog.ts:67-75), so a link followed by ".." names the link's directory.
		{"symlink-traversal", "model_catalog_json = 'link/../custom.json'", filepath.Join(codex, "custom.json")},
		{"comment-cr", "model_catalog_json = 'models.json' # a\rb", ""},
		{"comment-line-separator", "model_catalog_json = 'models.json' # a\u2028b", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.config != "" {
				check(t, os.WriteFile(filepath.Join(codex, "config.toml"), []byte(c.config), 0600))
			}
			if got := NativeCatalogPath(env); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
	vars["CODEX_MODELS_CACHE_PATH"] = filepath.Join(root, "explicit.json")
	check(t, os.WriteFile(vars["CODEX_MODELS_CACHE_PATH"], []byte(`{"models":["explicit"]}`), 0600))
	if got := ReadNativeCacheDefault(env); !reflect.DeepEqual(got, []string{"explicit"}) {
		t.Fatal(got)
	}
	delete(vars, "CODEX_MODELS_CACHE_PATH")
	check(t, os.Remove(filepath.Join(codex, "config.toml")))
	if got := ReadNativeCacheDefault(env); !reflect.DeepEqual(got, []string{"default"}) {
		t.Fatal(got)
	}
	check(t, os.Remove(filepath.Join(codex, "models_cache.json")))
	if ReadNativeCacheDefault(env) != nil {
		t.Fatal("missing cache fabricated models")
	}
	vars["CODEX_MODELS_CACHE_PATH"] = " " + filepath.Join(root, "raw.json") + " "
	if NativeCatalogPath(env) != vars["CODEX_MODELS_CACHE_PATH"] {
		t.Fatal("explicit path must remain untrimmed")
	}
	delete(vars, "CODEX_MODELS_CACHE_PATH")
	check(t, os.Mkdir(filepath.Join(codex, "config.toml"), 0700))
	if NativeCatalogPath(env) != "" {
		t.Fatal("unreadable config selected default cache")
	}
}

func TestDefaultCatalogUsesProcessEnvironment(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("CODEX_MODELS_CACHE_PATH", " ")
	path := filepath.Join(root, ".codex", "models_cache.json")
	check(t, os.MkdirAll(filepath.Dir(path), 0700))
	check(t, os.WriteFile(path, []byte(`{"models":["future","provider/future"]}`), 0600))
	t.Setenv("CODEX_HOME", " ")
	if got := NativeCatalogPath(nil); got != path {
		t.Fatal(got)
	}
	cat := BuildCatalog(CatalogDeps{})
	catalogJSON(t, cat, json.RawMessage(`{"state":"native-catalog","entries":[{"id":"future","source":"native","label":"future","reasoningEfforts":null},{"id":"provider/future","source":"ocx","label":"provider/future","reasoningEfforts":null}]}`))
	check(t, os.Remove(path))
	check(t, os.Mkdir(path, 0700))
	if ReadNativeCatalog(nil) != nil {
		t.Fatal("directory was accepted as a catalog")
	}
}

// CRW-1132 (A5-07): the entries of the library's catalog come from one construction path, so the same
// native cache entry has the same label and keeps its effort ladder whichever reader produced it; the
// native list is deduplicated by exact ID and a blank ID is refused where it is produced.
func TestCatalogEntriesShareOneConstruction(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "models_cache.json")
	check(t, os.WriteFile(path, []byte(`{"models":[{"id":"gpt-5.5","reasoningEfforts":["low","high"]},{"slug":"kiro/claude","supported_reasoning_levels":[{"effort":"medium"}]},{"id":"gpt-5.5"}]}`), 0600))
	vars := map[string]string{"HOME": root, "CODEX_HOME": root, "CODEX_MODELS_CACHE_PATH": path}
	env := host.LookupEnv(func(k string) (string, bool) { v, ok := vars[k]; return v, ok })
	read := ReadNativeCatalog(env)
	built := BuildCatalog(CatalogDeps{ReadNativeEntries: func() []CatalogEntry { return read }})
	if built.State != CatalogNative || !reflect.DeepEqual(built.Entries, read) {
		t.Fatalf("the library catalog differs from the live reader's entries:\n%+v\n%+v", built.Entries, read)
	}
	catalogJSON(t, built.Entries, json.RawMessage(`[{"id":"gpt-5.5","source":"native","label":"gpt-5.5","reasoningEfforts":["low","high"]},{"id":"kiro/claude","source":"ocx","label":"kiro/claude","reasoningEfforts":["medium"]}]`))

	// The default reader of the library keeps the ladder too.
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", root)
	t.Setenv("CODEX_MODELS_CACHE_PATH", path)
	catalogJSON(t, BuildCatalog(CatalogDeps{}).Entries, json.RawMessage(`[{"id":"gpt-5.5","source":"native","label":"gpt-5.5","reasoningEfforts":["low","high"]},{"id":"kiro/claude","source":"ocx","label":"kiro/claude","reasoningEfforts":["medium"]}]`))

	// An injected ID list is deduplicated by exact ID and loses its blank IDs; the provider's blank IDs are refused.
	got := BuildCatalog(CatalogDeps{ReadNativeCache: func() []string { return []string{"x", "x", "", " ", " x "} }, ProviderStatus: &ProviderCatalogInput{Mode: "provider", OcxModels: &[]string{"", " ", "\t", "x", "y", "y", " y "}}})
	catalogJSON(t, got, json.RawMessage(`{"state":"ocx-active","entries":[{"id":"x","source":"native","label":"x","reasoningEfforts":null},{"id":" x ","source":"native","label":" x ","reasoningEfforts":null},{"id":"y","source":"ocx","label":"y","reasoningEfforts":null},{"id":" y ","source":"ocx","label":" y ","reasoningEfforts":null}]}`))
}
