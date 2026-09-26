//go:build dev

package ci

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// pluginDriver calls scripts/ci/plugin.py's functions on inputs the Go test sends, so each
// Go function is compared with the Python one on the same manifest text and payload bytes.
const pluginDriver = `
import importlib.util, json, sys
spec = importlib.util.spec_from_file_location("plugin_under_test", sys.argv[1])
p = importlib.util.module_from_spec(spec); spec.loader.exec_module(p)
req = json.load(sys.stdin)
def load(files):
    return None if files is None else {k: (m, bytes.fromhex(h)) for k, (m, h) in files.items()}
m = json.loads(req["manifest"]) if req.get("manifest") is not None else None
payload = load(req.get("payload"))
fn = req["fn"]
if fn == "manifest_errors":
    out = p.manifest_errors(m, req["root"], req["label"], payload)
elif fn == "hygiene":
    out = p.hygiene(payload, m, req["label"])
elif fn == "skills":
    errors, found = p.skills(payload, m, req["label"])
    out = [errors, sorted(found)]
elif fn == "marketplace_errors":
    out = p.marketplace_errors(json.loads(req["catalog"]), m, "plugins/crw")
elif fn == "declared_skills_path":
    try:
        out = ["ok", p.declared_skills_path(m)]
    except ValueError as exc:
        out = ["error", str(exc)]
elif fn == "digest":
    out = p.digest(payload)
elif fn == "payload_version":
    try:
        out = ["ok", p.payload_version(payload, req["version"])]
    except ValueError as exc:
        out = ["error", str(exc)]
json.dump(out, sys.stdout)
`

// callPython runs one pluginDriver request and decodes its JSON answer into out.
func callPython(t *testing.T, request map[string]any, out any) {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", "-c", pluginDriver, filepath.Join(repoRoot(), "scripts/ci/plugin.py"))
	cmd.Stdin = strings.NewReader(string(body))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	answer, err := cmd.Output()
	if err != nil {
		t.Fatalf("plugin.py driver: %v\n%s", err, stderr.String())
	}
	if err := json.Unmarshal(answer, out); err != nil {
		t.Fatalf("driver answer %q: %v", answer, err)
	}
}

// files is a payload fixture as test_plugin.py writes it: text files of mode 100644.
type files map[string]string

func (f files) payload() payload {
	p := payload{}
	for name, text := range f {
		p[name] = entry{"100644", []byte(text)}
	}
	return p
}

func (f files) with(name, text string) files {
	out := files{}
	for k, v := range f {
		out[k] = v
	}
	out[name] = text
	return out
}

func (f files) without(name string) files {
	out := files{}
	for k, v := range f {
		if k != name {
			out[k] = v
		}
	}
	return out
}

func wire(p payload) map[string][2]string {
	if p == nil {
		return nil
	}
	out := map[string][2]string{}
	for name, e := range p {
		out[name] = [2]string{e.mode, hex.EncodeToString(e.data)}
	}
	return out
}

// set returns o with key replaced (or appended), keeping Python dict order.
func (o jsonObject) set(key string, value any) jsonObject {
	out := slices.Clone(o)
	for i := range out {
		if out[i].Key == key {
			out[i].Value = value
			return out
		}
	}
	return append(out, jsonKV{key, value})
}

func (o jsonObject) del(key string) jsonObject {
	return slices.DeleteFunc(slices.Clone(o), func(kv jsonKV) bool { return kv.Key == key })
}

func (o jsonObject) get(key string) any {
	for _, kv := range o {
		if kv.Key == key {
			return kv.Value
		}
	}
	return nil
}

func iface() jsonObject {
	return jsonObject{{"displayName", "CRW"}, {"shortDescription", "s"}, {"longDescription", "l"},
		{"developerName", "a"}, {"category", "Developer Tools"},
		{"capabilities", []string{"Skills"}}, {"defaultPrompt", []string{"p"}}}
}

// testManifest is test_plugin.py's manifest().
func testManifest() jsonObject {
	return jsonObject{{"name", "crw"}, {"version", "0.1.0"}, {"description", "d"},
		{"author", jsonObject{{"name", "a"}}}, {"repository", "https://example.invalid/repo"},
		{"license", "MIT"}, {"keywords", []string{"codex"}}, {"skills", "./skills/"}, {"interface", iface()}}
}

func withIface(key string, value any) jsonObject {
	return testManifest().set("interface", iface().set(key, value))
}

// testCatalog is test_plugin.py's catalog().
func testCatalog() jsonObject {
	return jsonObject{{"name", "crw"}, {"interface", jsonObject{{"displayName", "CRW"}}},
		{"plugins", []any{testEntry()}}}
}

func testEntry() jsonObject {
	return jsonObject{{"name", "crw"}, {"source", jsonObject{{"source", "local"}, {"path", "./plugins/crw"}}},
		{"policy", jsonObject{{"installation", "AVAILABLE"}, {"authentication", "ON_USE"}}},
		{"category", "Developer Tools"}}
}

func catalogWithEntry(e jsonObject) jsonObject {
	return testCatalog().set("plugins", []any{e})
}

const testSkill = "---\nname: crw-run\ndescription: d\n---\n"

func testInterface(name string) string {
	return "interface:\n  display_name: \"A skill\"\n  short_description: \"What it does\"\n  default_prompt: \"$" + name + " do the thing\"\n"
}

func dumps(value any) string { return pyJSON(value, false) }

// recordedFiles is test_plugin.py's recorded(): the manifest names its own payload.
func recordedFiles(t *testing.T, f files, m jsonObject) files {
	t.Helper()
	version, err := payloadVersion(f.with(manifestPath, dumps(m)).payload(), m.get("version").(string))
	if err != nil {
		t.Fatal(err)
	}
	return f.with(manifestPath, dumps(m.set("version", version)))
}

func goodFiles(t *testing.T) files {
	return recordedFiles(t, files{"skills/crw-run/SKILL.md": testSkill,
		"skills/crw-run/agents/openai.yaml": testInterface("crw-run"), "LICENSE": "MIT"}, testManifest())
}

func parse(t *testing.T, text string) *pyDict {
	t.Helper()
	value, err := pyJSONLoadsOrdered(text)
	if err != nil {
		t.Fatal(err)
	}
	return value.(*pyDict)
}

// manifestParity compares manifest_errors in Go and Python and returns the errors.
func manifestParity(t *testing.T, m jsonObject, root string, p payload) []string {
	t.Helper()
	text := dumps(m)
	var rootArg any
	if root != "" {
		rootArg = root
	}
	var want []string
	callPython(t, map[string]any{"fn": "manifest_errors", "manifest": text, "root": rootArg, "label": "t", "payload": wire(p)}, &want)
	got := manifestErrors(parse(t, text), root, "t", p)
	expectEqual(t, "manifest_errors "+text, nonNil(got), nonNil(want))
	return got
}

func hygieneParity(t *testing.T, f files, m jsonObject) []string {
	t.Helper()
	var want []string
	callPython(t, map[string]any{"fn": "hygiene", "manifest": dumps(m), "label": "t", "payload": wire(f.payload())}, &want)
	got := hygiene(f.payload(), parse(t, dumps(m)), "t")
	expectEqual(t, "hygiene", nonNil(got), nonNil(want))
	return got
}

func skillsParity(t *testing.T, f files, m jsonObject) ([]string, []string) {
	t.Helper()
	var want [2][]string
	callPython(t, map[string]any{"fn": "skills", "manifest": dumps(m), "label": "t", "payload": wire(f.payload())}, &want)
	errs, found := skillSet(f.payload(), parse(t, dumps(m)), "t")
	expectEqual(t, "skills errors", nonNil(errs), nonNil(want[0]))
	expectEqual(t, "skills found", nonNil(sortedKeys(found)), nonNil(want[1]))
	return errs, sortedKeys(found)
}

func marketplaceParity(t *testing.T, c jsonObject) []string {
	t.Helper()
	var want []string
	callPython(t, map[string]any{"fn": "marketplace_errors", "manifest": dumps(testManifest()), "catalog": dumps(c)}, &want)
	got, err := marketplaceErrors(parse(t, dumps(c)), parse(t, dumps(testManifest())))
	if err != nil {
		t.Fatal(err)
	}
	expectEqual(t, "marketplace_errors", nonNil(got), nonNil(want))
	return got
}

func anyContains(errs []string, fragment string) bool {
	return slices.ContainsFunc(errs, func(e string) bool { return strings.Contains(e, fragment) })
}

func expectFragment(t *testing.T, label string, errs []string, fragment string) {
	t.Helper()
	if !anyContains(errs, fragment) {
		t.Errorf("%s: no error contains %q: %q", label, fragment, errs)
	}
}

func expectNone(t *testing.T, label string, errs []string) {
	t.Helper()
	if len(errs) != 0 {
		t.Errorf("%s: unexpected errors %q", label, errs)
	}
}

func Test47_PLG_1_RequiredManifestFieldsAreTypedAndNonblank(t *testing.T) {
	expectNone(t, "valid", manifestParity(t, testManifest(), "crw", nil))
	for _, row := range []struct {
		key      string
		value    any
		fragment string
	}{
		{"author", jsonObject{{"url", "u"}}, "author.name"},
		{"keywords", []string{}, "keywords"},
		{"repository", 5, "repository"},
		{"skills", "", "skills"},
	} {
		expectFragment(t, row.key, manifestParity(t, testManifest().set(row.key, row.value), "crw", nil), row.fragment)
	}
	typed := testManifest().set("author", jsonObject{{"name", 1}}).set("keywords", []any{1}).
		set("interface", iface().set("displayName", 1).set("capabilities", []any{1}).set("defaultPrompt", []string{""}))
	errs := manifestParity(t, typed, "crw", nil)
	for _, fragment := range []string{"author.name", "keywords", "interface.displayName", "interface.capabilities", "interface.defaultPrompt"} {
		expectFragment(t, "typed", errs, fragment)
	}
	blank := testManifest().set("description", "   ").set("author", jsonObject{{"name", " "}}).set("keywords", []string{" "}).
		set("interface", iface().set("displayName", "  ").set("capabilities", []string{"  "}))
	errs = manifestParity(t, blank, "crw", nil)
	for _, fragment := range []string{"description", "author.name", "keywords", "interface.displayName", "interface.capabilities"} {
		expectFragment(t, "blank", errs, fragment)
	}
	expectFragment(t, "no defaultPrompt", manifestParity(t, testManifest().set("interface", iface().del("defaultPrompt")), "crw", nil),
		"interface.defaultPrompt must be a nonempty string list")
	// Non-object manifest members and odd JSON values report rather than crash.
	manifestParity(t, testManifest().set("interface", "x").set("author", []string{"a"}).set("version", 1.5), "crw", nil)
	manifestParity(t, testManifest().set("version", nil).set("name", nil).set("license", true), "crw", nil)
}

func Test47_PLG_2_VersionIsSemVer(t *testing.T) {
	for _, good := range []string{"1.0.0", "0.1.0-alpha.1", "1.2.3+build.5", "1.0.0-rc.1+exp.sha.5114f85"} {
		expectNone(t, good, manifestParity(t, testManifest().set("version", good), "crw", nil))
	}
	for _, bad := range []string{"1.0", "01.0.0", "1.0.0-01", "1.0.0-..", "1.0.0-", "1.0.0+", "1.0.0\n"} {
		expectFragment(t, bad, manifestParity(t, testManifest().set("version", bad), "crw", nil),
			"version "+pyRepr(bad)+" is not a semantic version")
	}
	r := pluginRepo(t, goodFiles(t).with(manifestPath, dumps(testManifest().set("version", "1.0.0\n"))), "plugins/crw/skills")
	got := pluginCLIParity(t, r.root)
	if got.code != 1 || !strings.Contains(got.stderr, "semantic version") {
		t.Errorf("CLI trailing newline: %+v", got)
	}
}

func Test47_PLG_3_NameMatchesThePluginDirectory(t *testing.T) {
	expectFragment(t, "other", manifestParity(t, testManifest(), "other", nil),
		"t manifest: name 'crw' must match the plugin directory 'other'")
	expectNone(t, "installed", manifestParity(t, testManifest(), "", nil))
}

func Test47_PLG_4_KeysAreAllowListed(t *testing.T) {
	expectFragment(t, "manifest", manifestParity(t, testManifest().set("unsupported", "x"), "crw", nil),
		"t manifest: 'unsupported' is not a supported manifest key")
	expectFragment(t, "interface", manifestParity(t, withIface("unsupported", "x"), "crw", nil),
		"t manifest: interface.unsupported is not a supported interface key")
	expectFragment(t, "author", manifestParity(t, testManifest().set("author", jsonObject{{"name", "a"}, {"unsupported", "x"}}), "crw", nil),
		"t manifest: author carries unsupported keys ['unsupported']")
	expectFragment(t, "apps", manifestParity(t, testManifest().set("apps", "./x.json"), "crw", nil),
		"t manifest: apps is not declared by this package")
}

func Test47_PLG_5_DeclaredComponentsMustShip(t *testing.T) {
	good := goodFiles(t).payload()
	for _, field := range []string{"hooks", "mcpServers"} {
		expectFragment(t, field, manifestParity(t, testManifest().set(field, "./x.json"), "crw", good),
			field+" names './x.json', which the package does not ship")
	}
	// Shipped component documents are checked with the same rules in both languages.
	hooks := files{"hooks/hooks.json": `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "x", "timeout": 11}, {"type": "prompt"}, {"type": "command", "timeout": true}]}], "Start": []}}`}
	mcp := files{"mcp.json": `{"mcpServers": {"codex-thread-bridge": {"command": "$HOME/x", "args": ["/abs", "./missing", "%X%"], "tools": {"create_thread": {"approval_mode": "never"}, "t2": {"x": 1}}}, "b": 5, "c": {"command": "./ok", "cwd": ".", "args": []}}}`}
	m := testManifest().set("hooks", []string{"./hooks/hooks.json", "../out.json"}).set("mcpServers", "./mcp.json")
	all := goodFiles(t)
	for name, text := range hooks {
		all = all.with(name, text)
	}
	for name, text := range mcp {
		all = all.with(name, text)
	}
	errs := manifestParity(t, m, "crw", all.payload())
	for _, fragment := range []string{"a hook timeout may not exceed 10 seconds", "every hook must be a command hook",
		"every hook needs a positive integer timeout", "Start must hold a nonempty list", "carries a variable",
		"is an absolute path", "names a file the package does not ship",
		"approval_mode 'never' is not one of", "must gate send_message_to_thread with 'approve'", "a server must be an object",
		"cwd must be",
		"must be a ./ relative path inside the plugin root"} {
		expectFragment(t, "components", errs, fragment)
	}
	manifestParity(t, testManifest().set("mcpServers", jsonObject{{"x", 1}}).set("hooks", 5), "crw", all.payload())
	badArgs := all.with("mcp.json", `{"mcpServers": {"s": {"command": 1, "cwd": ".", "args": [1], "tools": {}}}}`)
	expectFragment(t, "args", manifestParity(t, testManifest().set("mcpServers", "./mcp.json"), "crw", badArgs.payload()), "args must be a list of strings")
	manifestParity(t, testManifest().set("hooks", "./bad.json"), "crw", all.with("bad.json", "{not json").payload())
}

func Test47_PLG_6_OptionalValuesFollowIngestionRules(t *testing.T) {
	expectNone(t, "good optional", manifestParity(t, withIface("websiteURL", "https://example.invalid").
		set("interface", iface().set("websiteURL", "https://example.invalid").set("screenshots", []string{"./assets/one.png"})), "crw", nil))
	for _, row := range []struct {
		key   string
		value any
	}{{"websiteURL", 5}, {"screenshots", []string{}}, {"screenshots", []string{""}}} {
		expectFragment(t, row.key, manifestParity(t, withIface(row.key, row.value), "crw", nil), "not a usable value")
	}
	goodIface := iface().set("websiteURL", "https://example.invalid").set("brandColor", "#D7010F").set("logo", "./assets/logo.png")
	shippedFiles := recordedFiles(t, goodFiles(t).with("assets/logo.png", "png"), testManifest().set("interface", goodIface))
	shipped := shippedFiles.payload()
	expectNone(t, "shipped logo", manifestParity(t, parseObject(t, shippedFiles[manifestPath]), "crw", shipped))
	for _, row := range []struct{ key, value, fragment string }{
		{"websiteURL", "http://insecure.invalid", "https URL"},
		{"brandColor", "red", "#RRGGBB"},
		{"brandColor", "#D7010F\n", ""},
		{"logo", "./missing.png", "does not ship"},
		{"logo", "assets/logo.png", "./ relative path"},
	} {
		errs := manifestParity(t, withIface(row.key, row.value), "crw", shipped)
		if row.fragment != "" {
			expectFragment(t, row.key, errs, row.fragment)
		}
	}
	for _, value := range []string{"https://", "https:///missing-host", "http://example.invalid", "example.invalid",
		"https://@", "https://[", "https://:443", "https://[1.2.3.4]/", "https://[zz]/", "https://a]b/"} {
		expectFragment(t, value, manifestParity(t, withIface("websiteURL", value), "crw", nil), "https URL")
		expectFragment(t, value, manifestParity(t, testManifest().set("author", jsonObject{{"name", "a"}, {"url", value}}), "crw", nil), "author.url")
	}
	for _, value := range []string{"https://[::1]:8", "https://[v1.x]/", "HTTPS://Example.invalid"} {
		expectNone(t, value, manifestParity(t, withIface("websiteURL", value), "crw", nil))
	}
	expectNone(t, "author", manifestParity(t, testManifest().set("author",
		jsonObject{{"name", "a"}, {"email", "a@example.invalid"}, {"url", "https://example.invalid"}}), "crw", nil))
	expectFragment(t, "email", manifestParity(t, testManifest().set("author", jsonObject{{"name", "a"}, {"email", 5}}), "crw", nil), "author.email")
}

// parseObject turns manifest text back into a jsonObject fixture with the same key order.
func parseObject(t *testing.T, text string) jsonObject {
	t.Helper()
	var convert func(any) any
	convert = func(v any) any {
		switch x := v.(type) {
		case *pyDict:
			var o jsonObject
			for _, key := range x.keys {
				o = append(o, jsonKV{key, convert(x.vals[key])})
			}
			return o
		case []any:
			out := make([]any, len(x))
			for i := range x {
				out[i] = convert(x[i])
			}
			return out
		}
		return v
	}
	return convert(parse(t, text)).(jsonObject)
}

func Test47_PLG_7_SkillsPathStaysInside(t *testing.T) {
	for _, row := range []struct{ value, message string }{
		{"../../skills/", "manifest must declare skills as a ./ relative path"},
		{"./../skills/", "declared skills path must stay inside the plugin root"},
		{"/abs/skills/", "manifest must declare skills as a ./ relative path"},
		{"skills/", "manifest must declare skills as a ./ relative path"},
		{"./", "declared skills path must stay inside the plugin root"},
		{"./skills/current/", ""},
	} {
		m := testManifest().set("skills", row.value)
		var want [2]string
		callPython(t, map[string]any{"fn": "declared_skills_path", "manifest": dumps(m)}, &want)
		got, err := declaredSkillsPath(parse(t, dumps(m)))
		gotPair := [2]string{"ok", got}
		if err != nil {
			gotPair = [2]string{"error", err.Error()}
		}
		expectEqual(t, row.value, gotPair, want)
		if row.message != "" {
			expectEqual(t, row.value, gotPair, [2]string{"error", row.message})
		}
	}
}

func Test47_PLG_8_MarketplaceEntryIsPinned(t *testing.T) {
	expectNone(t, "good", marketplaceParity(t, testCatalog()))
	expectFragment(t, "name", marketplaceParity(t, testCatalog().set("name", "other")), "name")
	expectFragment(t, "displayName", marketplaceParity(t, testCatalog().set("interface", jsonObject{{"displayName", "Other"}})), "displayName")
	source := testEntry().get("source").(jsonObject)
	policy := testEntry().get("policy").(jsonObject)
	for _, row := range []struct {
		entry    jsonObject
		fragment string
	}{
		{testEntry().set("source", source.set("path", "./plugins/other")), "source.path"},
		{testEntry().set("source", source.set("source", "git")), "source.source"},
		{testEntry().set("policy", policy.set("authentication", "ON_INSTALL")), "ON_USE"},
		{testEntry().set("policy", policy.set("installation", "NOT_AVAILABLE")), "AVAILABLE"},
		{testEntry().del("category"), "category"},
		{testEntry().set("source", "string"), "entry source must be an object"},
		{testEntry().set("policy", "string"), "entry policy must be an object"},
	} {
		expectFragment(t, row.fragment, marketplaceParity(t, catalogWithEntry(row.entry)), row.fragment)
	}
	for _, spelling := range []string{"../../plugins/crw", "/plugins/crw", "plugins/crw", "./plugins/crw/", "./plugins/./crw"} {
		expectFragment(t, spelling, marketplaceParity(t, catalogWithEntry(testEntry().set("source", source.set("path", spelling)))), "source.path")
	}
	marketplaceParity(t, testCatalog().set("plugins", []any{testEntry(), testEntry()}))
	marketplaceParity(t, testCatalog().set("plugins", []any{"x", 5}))
	// Read from the revision: a committed wrong path fails the CLI even with a clean tree.
	r := pluginRepo(t, goodFiles(t), "plugins/crw/skills")
	broken := catalogWithEntry(testEntry().set("source", source.set("path", "./plugins/elsewhere")))
	r.write(marketplacePath, dumps(broken))
	r.gitCommit("break")
	got := pluginCLIParity(t, r.root)
	if got.code != 1 || !strings.Contains(got.stderr, "source.path") {
		t.Errorf("CLI marketplace: %+v", got)
	}
}

func Test47_PLG_9_PayloadHygiene(t *testing.T) {
	good := goodFiles(t)
	expectNone(t, "clean", hygieneParity(t, good, testManifest()))
	for _, missing := range []string{manifestPath, "LICENSE"} {
		expectFragment(t, missing, hygieneParity(t, good.without(missing), testManifest()), missing+" must ship with the package")
	}
	expectFragment(t, "allowlist", hygieneParity(t, good.with("packages/relay.py", "x"), testManifest()),
		"t packages/relay.py: only .codex-plugin, LICENSE, skills may ship in the package")
	for _, name := range []string{".codexclaw/sessions/s.json", "skills/crw-run/relay.sqlite3", ".env", ".env.local",
		"skills/id_rsa", "skills/aws-credentials", "skills/client.key", "skills/crw-run/client-secrets.json",
		"skills/secret.yaml", "skills/service-credential.json", "skills/id_ed25519", "skills/id_ecdsa",
		"skills/.netrc", "skills/.npmrc", "skills/authorized_keys", "skills/X.PEM"} {
		expectFragment(t, name, hygieneParity(t, good.with(name, "x"), testManifest()), "operational state and credentials may not ship")
	}
	for _, name := range []string{"skills/crw-run/secretary.md", "skills/crw-run/credentialing.md",
		"skills/crw-run/keyboard.md", "skills/crw-run/database.md"} {
		expectNone(t, name, hygieneParity(t, good.with(name, "x"), testManifest()))
	}
	expectFragment(t, "personal", hygieneParity(t, good.with("skills/crw-run/SKILL.md", "put it in /home/someone/code/x"), testManifest()),
		"contains the personal path '/home/someone/'")
	for _, text := range []string{"C:\\Users\\me\\x", "/Users/me/x", "at /root/x", "x/home/y/z", "use <worktree-root>/<project> or /example/home/x"} {
		hygieneParity(t, good.with("skills/crw-run/SKILL.md", text), testManifest())
	}
	hygieneParity(t, good.with("skills/bin", "\xff\xfe/home/a/"), testManifest())
}

func Test47_PLG_10_DeclaredDirectoryIsTheSkillSet(t *testing.T) {
	errs, found := skillsParity(t, goodFiles(t), testManifest())
	expectNone(t, "good", errs)
	expectEqual(t, "found", found, []string{"crw-run"})
	nested := testManifest().set("skills", "./skills/current/")
	base := files{manifestPath: dumps(nested), "skills/current/crw-run/SKILL.md": testSkill,
		"skills/current/crw-run/agents/openai.yaml": testInterface("crw-run"), "LICENSE": "MIT"}
	errs, found = skillsParity(t, base, nested)
	expectNone(t, "nested", errs)
	expectEqual(t, "nested found", found, []string{"crw-run"})
	errs, _ = skillsParity(t, base.with("skills/old/crw-check/SKILL.md", testSkill), nested)
	expectFragment(t, "outside", errs, "t skills/old/crw-check/SKILL.md: ships outside every declared component path")
	errs, _ = skillsParity(t, goodFiles(t).without("skills/crw-run/agents/openai.yaml"), testManifest())
	expectFragment(t, "yaml", errs, "t skills/crw-run: missing agents/openai.yaml")
	errs, _ = skillsParity(t, files{"LICENSE": "MIT"}, testManifest())
	expectFragment(t, "empty", errs, "t: the declared skills path ships no skill")
	for _, yaml := range []string{testInterface("other"), "interface:\n  display_name: \"x\n", "interface:\n  display_name: a\n  display_name: b\n", "interface:\n  display_name: a\n"} {
		skillsParity(t, goodFiles(t).with("skills/crw-run/agents/openai.yaml", yaml), testManifest())
	}
	skillsParity(t, goodFiles(t), testManifest().set("skills", 5))
}

func Test47_PLG_11_DigestIsFramedAndCoversMode(t *testing.T) {
	good := goodFiles(t).payload()
	var want string
	callPython(t, map[string]any{"fn": "digest", "payload": wire(good)}, &want)
	expectEqual(t, "digest", payloadDigest(good), want)
	changed := goodFiles(t).with("LICENSE", "MIT ").payload()
	if payloadDigest(changed) == payloadDigest(good) {
		t.Error("a changed byte must change the digest")
	}
	executable := maps(good)
	executable["LICENSE"] = entry{"100755", []byte("MIT")}
	callPython(t, map[string]any{"fn": "digest", "payload": wire(executable)}, &want)
	expectEqual(t, "mode digest", payloadDigest(executable), want)
	if payloadDigest(executable) == payloadDigest(good) {
		t.Error("the file mode must change the digest")
	}
	third := sha256Hex([]byte("three"))
	left := payload{"a": {"100644", []byte("one")}, "b\n100644 " + third + " x": {"100644", []byte("two")}}
	right := payload{"a": {"100644", []byte("one")}, "b": {"100644", []byte("two")}, "x": {"100644", []byte("three")}}
	if unframed(left) != unframed(right) {
		t.Fatal("the counterexample must collide without framing")
	}
	if payloadDigest(left) == payloadDigest(right) {
		t.Error("framing must separate the smuggled name")
	}
	callPython(t, map[string]any{"fn": "digest", "payload": wire(left)}, &want)
	expectEqual(t, "framed digest", payloadDigest(left), want)
}

func sha256Hex(data []byte) string { return hexSum(data) }

func hexSum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func unframed(p payload) string {
	var lines []string
	for _, name := range p.names() {
		lines = append(lines, p[name].mode+" "+hexSum(p[name].data)+" "+name)
	}
	return hexSum([]byte(strings.Join(lines, "\n")))
}

func Test47_PLG_12_VersionNamesItsPayload(t *testing.T) {
	good := goodFiles(t)
	version := parse(t, good[manifestPath]).get("version").(string)
	if !regexp.MustCompile(`^0\.1\.0\+[0-9a-f]{12}$`).MatchString(version) {
		t.Fatalf("recorded version %q", version)
	}
	var want [2]string
	callPython(t, map[string]any{"fn": "payload_version", "payload": wire(good.payload()), "version": version}, &want)
	expectEqual(t, "payload_version", want, [2]string{"ok", version})
	expectEqual(t, "settles", recordedFiles(t, good, parseObject(t, good[manifestPath])), good)
	other := recordedFiles(t, good.with("LICENSE", "MIT\n"), parseObject(t, good[manifestPath]).set("version", "0.1.0"))
	if other[manifestPath] == good[manifestPath] {
		t.Error("one changed file must change the version")
	}
	elided, err := versionPayload(good.payload(), version)
	if err != nil {
		t.Fatal(err)
	}
	expectEqual(t, "elided manifest", string(elided[manifestPath].data), dumps(testManifest()))
	stale := parseObject(t, good[manifestPath]).set("version", "0.1.0+000000000000")
	errs := manifestParity(t, stale, "crw", good.with(manifestPath, dumps(stale)).payload())
	expectFragment(t, "stale", errs, "does not name this payload. Record "+pyRepr(version))
	suffix := strings.SplitN(version, "+", 2)[1]
	errs = manifestParity(t, parseObject(t, good[manifestPath]), "crw", good.with("skills/crw-run/references/built.md", "built from "+suffix).payload())
	expectFragment(t, "repeat", errs, "t skills/crw-run/references/built.md: a shipped file repeats the payload suffix")
	twice := parseObject(t, good[manifestPath]).set("description", version)
	errs = manifestParity(t, twice, "crw", good.with(manifestPath, dumps(twice)).payload())
	expectFragment(t, "twice", errs, "spells "+pyRepr(version)+" 2 times; the suffix has to be elided")
	errs = manifestParity(t, testManifest(), "crw", good.with(manifestPath, dumps(testManifest())).payload())
	expectFragment(t, "plain", errs, "version '0.1.0' does not name this payload")
	// CLI: a committed skill edited after recording; an installed LICENSE edited.
	r := pluginRepo(t, good.with("skills/crw-run/SKILL.md", testSkill+"\nAn extra paragraph.\n"), "plugins/crw/skills")
	got := pluginCLIParity(t, r.root)
	if got.code != 1 || !strings.Contains(got.stderr, "does not name this payload") {
		t.Errorf("CLI release digest: %+v", got)
	}
	dir := writePayload(t, good.with("LICENSE", "MIT, with a later edit"))
	got = pluginPayloadParity(t, dir)
	if got.code != 1 || !strings.Contains(got.stderr, "does not name this payload") {
		t.Errorf("CLI installed digest: %+v", got)
	}
}

func Test47_PLG_13_RecordVersionWritesAndSettles(t *testing.T) {
	plain := goodFiles(t).with(manifestPath, dumps(testManifest()))
	pyRepo := pluginRepo(t, plain, "plugins/crw/skills")
	goRepo := pluginRepo(t, plain, "plugins/crw/skills")
	written := func(r *fixtureRepo) string {
		data, err := os.ReadFile(filepath.Join(r.root, "plugins/crw", manifestPath))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	py := runCommand(t, pyRepo.root, nil, "python3", "scripts/ci/plugin.py", "--record-version")
	got := goCheck(t, goRepo.root, nil, "plugin", "--record-version")
	sameResult(t, "record", py, got)
	expectEqual(t, "written manifest", written(goRepo), written(pyRepo))
	if v := parse(t, written(goRepo)).get("version"); !regexp.MustCompile(`^0\.1\.0\+[0-9a-f]{12}$`).MatchString(v.(string)) {
		t.Errorf("recorded %v", v)
	}
	settled := written(goRepo)
	py = runCommand(t, pyRepo.root, nil, "python3", "scripts/ci/plugin.py", "--record-version")
	got = goCheck(t, goRepo.root, nil, "plugin", "--record-version")
	sameResult(t, "again", py, got)
	if got.code != 0 || !strings.Contains(got.stdout, "already recorded") || written(goRepo) != settled {
		t.Errorf("second record: %+v", got)
	}
	twice := parseObject(t, goodFiles(t)[manifestPath])
	twice = twice.set("description", twice.get("version"))
	r := pluginRepo(t, goodFiles(t).with(manifestPath, dumps(twice)), "plugins/crw/skills")
	py = runCommand(t, r.root, nil, "python3", "scripts/ci/plugin.py", "--record-version")
	got = goCheck(t, r.root, nil, "plugin", "--record-version")
	sameResult(t, "cannot derive", py, got)
	if got.code != 1 || !strings.Contains(got.stderr, "elided") || strings.Contains(got.stderr, "Traceback") || strings.Contains(got.stderr, "panic") {
		t.Errorf("cannot derive: %+v", got)
	}
}

func Test47_PLG_14_RepositoryCheckReadsTheRevision(t *testing.T) {
	good := goodFiles(t)
	expectEqual(t, "healthy", pluginCLIParity(t, pluginRepo(t, good, "plugins/crw/skills").root).code, 0)
	for _, row := range []struct {
		f        files
		fragment string
	}{
		{good.with(manifestPath, "not-json"), "plugin.json"},
		{good.without("LICENSE"), "LICENSE"},
		{good.with("LICENSE", "Apache"), "repository license"},
		{good.with(manifestPath, dumps(testManifest().set("license", "Apache-2.0"))), "license"},
	} {
		got := pluginCLIParity(t, pluginRepo(t, row.f, "plugins/crw/skills").root)
		if got.code != 1 || !strings.Contains(got.stderr, row.fragment) {
			t.Errorf("%s: %+v", row.fragment, got)
		}
	}
}

func Test47_PLG_15_RootSkillsLinkIsExact(t *testing.T) {
	for _, row := range []struct{ link, fragment string }{
		{"", "skills: the repository root must keep a link to the packaged skills"},
		{"plugins/crw", "skills: the root link points at 'plugins/crw' instead of 'plugins/crw/skills'"},
		{"plugins/crw/skills ", "root link points at"},
	} {
		got := pluginCLIParity(t, pluginRepo(t, goodFiles(t), row.link).root)
		if got.code != 1 || !strings.Contains(got.stderr, row.fragment) {
			t.Errorf("link %q: %+v", row.link, got)
		}
	}
}

func Test47_PLG_16_WorkingTreeGetsTheSameChecks(t *testing.T) {
	r := pluginRepo(t, goodFiles(t), "plugins/crw/skills")
	r.write("plugins/crw/"+manifestPath, "not-json")
	got := pluginCLIParity(t, r.root)
	if got.code != 1 || !strings.Contains(got.stderr, "working tree") {
		t.Errorf("working manifest: %+v", got)
	}
	withPlan := goodFiles(t).with("skills/crw-plan/SKILL.md", "---\nname: crw-plan\ndescription: d\n---\n").
		with("skills/crw-plan/agents/openai.yaml", testInterface("crw-plan"))
	withPlan = recordedFiles(t, withPlan, parseObject(t, withPlan[manifestPath]).set("version", "0.1.0"))
	r = pluginRepo(t, withPlan, "plugins/crw/skills")
	if err := os.RemoveAll(filepath.Join(r.root, "plugins/crw/skills/crw-plan")); err != nil {
		t.Fatal(err)
	}
	got = pluginCLIParity(t, r.root)
	if got.code != 1 || !strings.Contains(got.stderr, "working tree: crw-plan ships in the revision but is missing here") {
		t.Errorf("missing skill: %+v", got)
	}
	r = pluginRepo(t, goodFiles(t), "plugins/crw/skills")
	r.write("plugins/crw/notes.txt", "local")
	got = pluginCLIParity(t, r.root)
	if got.code != 1 || !strings.Contains(got.stderr, "working tree plugins/crw/notes.txt: untracked or ignored files") {
		t.Errorf("untracked: %+v", got)
	}
}

func Test47_PLG_17_DirectoryPayloadHasNoEmptyDirOrSymlink(t *testing.T) {
	r := pluginRepo(t, goodFiles(t), "plugins/crw/skills")
	if err := os.Mkdir(filepath.Join(r.root, "plugins/crw/extra-empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := pluginCLIParity(t, r.root)
	if got.code != 1 || !strings.Contains(got.stderr, "extra-empty: an empty directory still ships; remove it") {
		t.Errorf("working empty dir: %+v", got)
	}
	dir := writePayload(t, goodFiles(t))
	if err := os.Mkdir(filepath.Join(dir, "leftover"), 0o755); err != nil {
		t.Fatal(err)
	}
	got = pluginPayloadParity(t, dir)
	if got.code != 1 || !strings.Contains(got.stderr, "leftover: an empty directory still ships") {
		t.Errorf("installed empty dir: %+v", got)
	}
	dir = writePayload(t, goodFiles(t))
	if err := os.Symlink(filepath.Join(dir, "skills/crw-run"), filepath.Join(dir, "skills/crw-plan")); err != nil {
		t.Fatal(err)
	}
	got = pluginPayloadParity(t, dir)
	if got.code != 1 || !strings.Contains(got.stderr, "installed skills/crw-plan: the installer drops symlinks") {
		t.Errorf("installed symlink: %+v", got)
	}
}

func Test47_PLG_18_JSONReport(t *testing.T) {
	root := repoRoot()
	py := runCommand(t, root, nil, "python3", "scripts/ci/plugin.py", "--json")
	got := goCheck(t, root, nil, "plugin", "--json")
	sameResult(t, "repository --json", py, got)
	var r map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &r); err != nil {
		t.Fatalf("report: %v", err)
	}
	names := []any{"crw-check", "crw-define", "crw-logic", "crw-loop", "crw-next", "crw-plan", "crw-refactor", "crw-run", "crw-status", "crw-tidy"}
	var expected []any
	for _, n := range names {
		expected = append(expected, "crw:"+n.(string))
	}
	expectEqual(t, "skills", r["skills"], names)
	expectEqual(t, "expectedSkillNames", r["expectedSkillNames"], expected)
	head := strings.TrimSpace(runCommand(t, root, nil, "git", "rev-parse", "HEAD").stdout)
	expectEqual(t, "resolved", r["resolved"], head)
	expectEqual(t, "source", r["source"], "revision")
	again := goCheck(t, root, nil, "plugin", "--json")
	expectEqual(t, "stable", again.stdout, got.stdout)
	sameResult(t, "repository text", runCommand(t, root, nil, "python3", "scripts/ci/plugin.py"), goCheck(t, root, nil, "plugin"))
	dir := writePayload(t, goodFiles(t))
	py = python(t, t.TempDir(), nil, "scripts/ci/plugin.py", "--payload", dir, "--json")
	got = goCheck(t, t.TempDir(), nil, "plugin", "--payload", dir, "--json")
	sameResult(t, "payload --json", py, got)
	if err := json.Unmarshal([]byte(got.stdout), &r); err != nil {
		t.Fatal(err)
	}
	expectEqual(t, "payload names", r["expectedSkillNames"], []any{"crw:crw-run"})
	expectEqual(t, "payload source", r["source"], "payload")
	got = pluginPayloadParity(t, writePayload(t, goodFiles(t).without("LICENSE")))
	if got.code != 1 || !strings.Contains(got.stderr, "LICENSE") {
		t.Errorf("payload without LICENSE: %+v", got)
	}
}

// pluginRepo is test_plugin.py's SyntheticRepositoryTests.build: plugin.py copied to
// scripts/ci (its root is its own checkout), the package, LICENSE, marketplace and link.
func pluginRepo(t *testing.T, f files, link string) *fixtureRepo {
	t.Helper()
	r := newRepo(t)
	script, err := os.ReadFile(filepath.Join(repoRoot(), "scripts/ci/plugin.py"))
	if err != nil {
		t.Fatal(err)
	}
	r.write("scripts/ci/plugin.py", string(script))
	for name, text := range f {
		r.write("plugins/crw/"+name, text)
	}
	r.write("LICENSE", "MIT")
	r.write(marketplacePath, dumps(testCatalog()))
	if link != "" {
		if err := os.Symlink(link, filepath.Join(r.root, "skills")); err != nil {
			t.Fatal(err)
		}
	}
	r.gitCommit("package")
	return r
}

func (r *fixtureRepo) gitCommit(message string) {
	r.git("add", "-A")
	r.git("commit", "-q", "-m", message)
}

// pluginCLIParity runs the repository's copy of plugin.py and `crw-dev ci plugin` in root.
func pluginCLIParity(t *testing.T, root string, args ...string) result {
	t.Helper()
	py := runCommand(t, root, nil, "python3", append([]string{"scripts/ci/plugin.py"}, args...)...)
	got := goCheck(t, root, nil, "plugin", args...)
	sameResult(t, "plugin "+strings.Join(args, " "), py, got)
	return got
}

func pluginPayloadParity(t *testing.T, dir string) result {
	t.Helper()
	py := python(t, t.TempDir(), nil, "scripts/ci/plugin.py", "--payload", dir)
	got := goCheck(t, t.TempDir(), nil, "plugin", "--payload", dir)
	sameResult(t, "plugin --payload", py, got)
	return got
}

func writePayload(t *testing.T, f files) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "crw")
	for name, text := range f {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// An installed payload's file mode reaches the digest: an executable bit read from disk is
// mode 100755, so a manifest recorded for the 100644 payload no longer names it, in both
// implementations, and the digest of the executable payload is the one Python derives.
func Test47_PLG_11_InstalledPayloadModeIsRead(t *testing.T) {
	good := goodFiles(t)
	dir := writePayload(t, good)
	expectEqual(t, "plain payload", pluginPayloadParity(t, dir).code, 0)
	if err := os.Chmod(filepath.Join(dir, "LICENSE"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := pluginPayloadParity(t, dir)
	if got.code != 1 || !strings.Contains(got.stderr, "does not name this payload") {
		t.Errorf("executable LICENSE: %+v", got)
	}
	read, errs := directoryPayload(dir)
	expectNone(t, "directory payload", errs)
	expectEqual(t, "LICENSE mode", read["LICENSE"].mode, "100755")
	expectEqual(t, "SKILL.md mode", read["skills/crw-run/SKILL.md"].mode, "100644")
	var want string
	callPython(t, map[string]any{"fn": "digest", "payload": wire(read)}, &want)
	expectEqual(t, "digest", payloadDigest(read), want)
}
