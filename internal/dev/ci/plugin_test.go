//go:build dev

package ci

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// files is a payload fixture: text files of mode 100644.
type files map[string]string

func (f files) payload() payload {
	p := payload{}
	for name, text := range f {
		p[name] = entry{"100644", []byte(text)}
	}
	return p
}

func (f files) with(name, text string) files {
	out := maps.Clone(f)
	out[name] = text
	return out
}

func (f files) without(name string) files {
	out := maps.Clone(f)
	delete(out, name)
	return out
}

// doc is a JSON object fixture; set and del return changed copies.
type doc map[string]any

func (o doc) set(key string, value any) doc {
	out := maps.Clone(o)
	out[key] = value
	return out
}

func (o doc) del(key string) doc {
	out := maps.Clone(o)
	delete(out, key)
	return out
}

func iface() doc {
	return doc{"displayName": "CRW", "shortDescription": "s", "longDescription": "l",
		"developerName": "a", "category": "Developer Tools",
		"capabilities": []string{"Skills"}, "defaultPrompt": []string{"p"}}
}

func testManifest() doc {
	return doc{"name": "crw", "version": "0.1.0", "description": "d",
		"author": doc{"name": "a"}, "repository": "https://example.invalid/repo",
		"license": "MIT", "keywords": []string{"codex"}, "skills": "./skills/", "interface": iface()}
}

func withIface(key string, value any) doc {
	return testManifest().set("interface", iface().set(key, value))
}

func testCatalog() doc {
	return doc{"name": "crw", "interface": doc{"displayName": "CRW"}, "plugins": []any{testEntry()}}
}

func testEntry() doc {
	return doc{"name": "crw", "source": doc{"source": "local", "path": "./plugins/crw"},
		"policy": doc{"installation": "AVAILABLE", "authentication": "ON_USE"}, "category": "Developer Tools"}
}

func catalogWithEntry(e doc) doc {
	return testCatalog().set("plugins", []any{e})
}

const testSkill = "---\nname: crw-run\ndescription: d\n---\n"

func testInterface(name string) string {
	return "interface:\n  display_name: \"A skill\"\n  short_description: \"What it does\"\n  default_prompt: \"$" + name + " do the thing\"\n"
}

// dumps is value as compact JSON text.
func dumps(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(data)
}

// recordedFiles is f with the manifest m recorded under the version that names its payload.
func recordedFiles(t *testing.T, f files, m doc) files {
	t.Helper()
	version, err := payloadVersion(f.with(manifestPath, dumps(m)).payload(), m["version"].(string))
	if err != nil {
		t.Fatal(err)
	}
	return f.with(manifestPath, dumps(m.set("version", version)))
}

func goodFiles(t *testing.T) files {
	return recordedFiles(t, files{"skills/crw-run/SKILL.md": testSkill,
		"skills/crw-run/agents/openai.yaml": testInterface("crw-run"), "LICENSE": "MIT"}, testManifest())
}

// parse is a JSON object as the checks decode it.
func parse(t *testing.T, text string) map[string]any {
	t.Helper()
	value, err := decodeJSON([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return value.(map[string]any)
}

// manifestErrs is manifestErrors of m, read as the check reads it from the manifest's text.
func manifestErrs(t *testing.T, m doc, root string, p payload) []string {
	t.Helper()
	return manifestErrors(parse(t, dumps(m)), root, "t", p)
}

func hygieneErrs(t *testing.T, f files, m doc) []string {
	t.Helper()
	return hygiene(f.payload(), parse(t, dumps(m)), "t")
}

func skillErrs(t *testing.T, f files, m doc) ([]string, []string) {
	t.Helper()
	errs, found := skillSet(f.payload(), parse(t, dumps(m)), "t")
	return errs, sortedKeys(found)
}

func marketplaceErrs(t *testing.T, c doc) []string {
	t.Helper()
	return marketplaceErrors(parse(t, dumps(c)), parse(t, dumps(testManifest())))
}

func anyContains(errs []string, fragment string) bool {
	for _, e := range errs {
		if strings.Contains(e, fragment) {
			return true
		}
	}
	return false
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
	expectNone(t, "valid", manifestErrs(t, testManifest(), "crw", nil))
	for _, row := range []struct {
		key      string
		value    any
		fragment string
	}{
		{"author", doc{"url": "u"}, "author.name"},
		{"keywords", []string{}, "keywords"},
		{"repository", 5, "repository"},
		{"skills", "", "skills"},
	} {
		expectFragment(t, row.key, manifestErrs(t, testManifest().set(row.key, row.value), "crw", nil), row.fragment)
	}
	typed := testManifest().set("author", doc{"name": 1}).set("keywords", []any{1}).
		set("interface", iface().set("displayName", 1).set("capabilities", []any{1}).set("defaultPrompt", []string{""}))
	errs := manifestErrs(t, typed, "crw", nil)
	for _, fragment := range []string{"author.name", "keywords", "interface.displayName", "interface.capabilities", "interface.defaultPrompt"} {
		expectFragment(t, "typed", errs, fragment)
	}
	blank := testManifest().set("description", "   ").set("author", doc{"name": " "}).set("keywords", []string{" "}).
		set("interface", iface().set("displayName", "  ").set("capabilities", []string{"  "}))
	errs = manifestErrs(t, blank, "crw", nil)
	for _, fragment := range []string{"description", "author.name", "keywords", "interface.displayName", "interface.capabilities"} {
		expectFragment(t, "blank", errs, fragment)
	}
	expectFragment(t, "no defaultPrompt", manifestErrs(t, testManifest().set("interface", iface().del("defaultPrompt")), "crw", nil),
		"interface.defaultPrompt must be a nonempty string list")
	// Non-object manifest members and odd JSON values are reported, never a crash.
	errs = manifestErrs(t, testManifest().set("interface", "x").set("author", []string{"a"}).set("version", 1.5), "crw", nil)
	for _, fragment := range []string{"interface must be an object", "author.name is required", "version 1.5 is not a semantic version"} {
		expectFragment(t, "odd values", errs, fragment)
	}
	errs = manifestErrs(t, testManifest().set("version", nil).set("name", nil).set("license", true), "crw", nil)
	for _, fragment := range []string{"version null is not a semantic version", "name null must match the plugin directory \"crw\"",
		`license true must be "MIT"`} {
		expectFragment(t, "null values", errs, fragment)
	}
}

func Test47_PLG_2_VersionIsSemVer(t *testing.T) {
	for _, good := range []string{"1.0.0", "0.1.0-alpha.1", "1.2.3+build.5", "1.0.0-rc.1+exp.sha.5114f85"} {
		expectNone(t, good, manifestErrs(t, testManifest().set("version", good), "crw", nil))
	}
	for _, bad := range []string{"1.0", "01.0.0", "1.0.0-01", "1.0.0-..", "1.0.0-", "1.0.0+", "1.0.0\n"} {
		expectFragment(t, bad, manifestErrs(t, testManifest().set("version", bad), "crw", nil),
			"version "+show(bad)+" is not a semantic version")
	}
	r := pluginRepo(t, goodFiles(t).with(manifestPath, dumps(testManifest().set("version", "1.0.0\n"))))
	got := pluginCLI(t, r.root)
	if got.code != 1 || !strings.Contains(got.stderr, "semantic version") {
		t.Errorf("CLI trailing newline: %+v", got)
	}
}

func Test47_PLG_3_NameMatchesThePluginDirectory(t *testing.T) {
	expectFragment(t, "other", manifestErrs(t, testManifest(), "other", nil),
		`t manifest: name "crw" must match the plugin directory "other"`)
	expectNone(t, "installed", manifestErrs(t, testManifest(), "", nil))
}

func Test47_PLG_4_KeysAreAllowListed(t *testing.T) {
	expectFragment(t, "manifest", manifestErrs(t, testManifest().set("unsupported", "x"), "crw", nil),
		`t manifest: "unsupported" is not a supported manifest key`)
	expectFragment(t, "interface", manifestErrs(t, withIface("unsupported", "x"), "crw", nil),
		"t manifest: interface.unsupported is not a supported interface key")
	expectFragment(t, "author", manifestErrs(t, testManifest().set("author", doc{"name": "a", "unsupported": "x"}), "crw", nil),
		`t manifest: author carries unsupported keys ["unsupported"]`)
	expectFragment(t, "apps", manifestErrs(t, testManifest().set("apps", "./x.json"), "crw", nil),
		"t manifest: apps is not declared by this package")
}

func Test47_PLG_5_DeclaredComponentsMustShip(t *testing.T) {
	good := goodFiles(t).payload()
	for _, field := range []string{"hooks", "mcpServers"} {
		expectFragment(t, field, manifestErrs(t, testManifest().set(field, "./x.json"), "crw", good),
			field+` names "./x.json", which the package does not ship`)
	}
	// Shipped component documents are read with the rules a host loads them by.
	hooks := files{"hooks/hooks.json": `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "x", "timeout": 11}, {"type": "prompt"}, {"type": "command", "timeout": true}, {"type": "command", "command": "y", "timeout": 5.0}]}], "Start": []}}`}
	mcp := files{"mcp.json": `{"mcpServers": {"codex-thread-bridge": {"command": "$HOME/x", "args": ["/abs", "./missing", "%X%"], "tools": {"create_thread": {"approval_mode": "never"}, "t2": {"x": 1}}}, "b": 5, "c": {"command": "./ok", "cwd": ".", "args": []}}}`}
	m := testManifest().set("hooks", []string{"./hooks/hooks.json", "../out.json"}).set("mcpServers", "./mcp.json")
	all := goodFiles(t)
	for name, text := range hooks {
		all = all.with(name, text)
	}
	for name, text := range mcp {
		all = all.with(name, text)
	}
	errs := manifestErrs(t, m, "crw", all.payload())
	for _, fragment := range []string{"a hook timeout may not exceed 10 seconds", "every hook must be a command hook",
		"every hook needs a positive integer timeout", "Start must hold a nonempty list", "carries a variable",
		"is an absolute path", "names a file the package does not ship",
		`approval_mode "never" is not one of`, `must gate send_message_to_thread with "approve"`, "a server must be an object",
		"cwd must be", `tools.t2: carries "x"; only approval_mode is checked here`,
		`hooks "../out.json" must be a ./ relative path inside the plugin root`} {
		expectFragment(t, "components", errs, fragment)
	}
	errs = manifestErrs(t, testManifest().set("mcpServers", doc{"x": 1}).set("hooks", 5), "crw", all.payload())
	expectFragment(t, "inline mcpServers", errs, "mcpServers must name a ./ relative file")
	expectFragment(t, "hooks number", errs, "hooks must be a ./ relative path or a list of them")
	badArgs := all.with("mcp.json", `{"mcpServers": {"s": {"command": 1, "cwd": ".", "args": [1], "tools": {}}}}`)
	errs = manifestErrs(t, testManifest().set("mcpServers", "./mcp.json"), "crw", badArgs.payload())
	for _, fragment := range []string{"args must be a list of strings", "a server needs a command", "tools must be a nonempty object"} {
		expectFragment(t, "bad server", errs, fragment)
	}
	expectFragment(t, "not JSON", manifestErrs(t, testManifest().set("hooks", "./bad.json"), "crw", all.with("bad.json", "{not json").payload()),
		"t bad.json: invalid character")
}

func Test47_PLG_6_OptionalValuesFollowIngestionRules(t *testing.T) {
	expectNone(t, "good optional", manifestErrs(t, testManifest().
		set("interface", iface().set("websiteURL", "https://example.invalid").set("screenshots", []string{"./assets/one.png"})), "crw", nil))
	for _, row := range []struct {
		key   string
		value any
	}{{"websiteURL", 5}, {"screenshots", []string{}}, {"screenshots", []string{""}}} {
		expectFragment(t, row.key, manifestErrs(t, withIface(row.key, row.value), "crw", nil), "not a usable value")
	}
	goodIface := iface().set("websiteURL", "https://example.invalid").set("brandColor", "#D7010F").set("logo", "./assets/logo.png")
	shippedFiles := recordedFiles(t, goodFiles(t).with("assets/logo.png", "png"), testManifest().set("interface", goodIface))
	shipped := shippedFiles.payload()
	expectNone(t, "shipped logo", manifestErrors(parse(t, shippedFiles[manifestPath]), "crw", "t", shipped))
	for _, row := range []struct{ key, value, fragment string }{
		{"websiteURL", "http://insecure.invalid", "https URL"},
		{"brandColor", "red", "#RRGGBB"},
		{"brandColor", "#D7010F\n", "#RRGGBB"},
		{"logo", "./missing.png", "does not ship"},
		{"logo", "assets/logo.png", "./ relative path"},
	} {
		expectFragment(t, row.key, manifestErrs(t, withIface(row.key, row.value), "crw", shipped), row.fragment)
	}
	for _, value := range []string{"https://", "https:///missing-host", "http://example.invalid", "example.invalid",
		"https://@", "https://[", "https://:443", "https://[1.2.3.4]/", "https://[zz]/"} {
		expectFragment(t, value, manifestErrs(t, withIface("websiteURL", value), "crw", nil), "https URL")
		expectFragment(t, value, manifestErrs(t, testManifest().set("author", doc{"name": "a", "url": value}), "crw", nil), "author.url")
	}
	for _, value := range []string{"https://[::1]:8", "HTTPS://Example.invalid"} {
		expectNone(t, value, manifestErrs(t, withIface("websiteURL", value), "crw", nil))
	}
	expectNone(t, "author", manifestErrs(t, testManifest().set("author",
		doc{"name": "a", "email": "a@example.invalid", "url": "https://example.invalid"}), "crw", nil))
	expectFragment(t, "email", manifestErrs(t, testManifest().set("author", doc{"name": "a", "email": 5}), "crw", nil), "author.email")
}

func Test47_PLG_7_SkillsPathStaysInside(t *testing.T) {
	for _, row := range []struct{ value, relative, message string }{
		{"../../skills/", "", "manifest must declare skills as a ./ relative path"},
		{"./../skills/", "", "declared skills path must stay inside the plugin root"},
		{"/abs/skills/", "", "manifest must declare skills as a ./ relative path"},
		{"skills/", "", "manifest must declare skills as a ./ relative path"},
		{"./", "", "declared skills path must stay inside the plugin root"},
		{"./skills/current/", "skills/current", ""},
	} {
		got, err := declaredSkillsPath(parse(t, dumps(testManifest().set("skills", row.value))))
		message := ""
		if err != nil {
			message = err.Error()
		}
		if got != row.relative || message != row.message {
			t.Errorf("%s: %q, %q; want %q, %q", row.value, got, message, row.relative, row.message)
		}
	}
}

func Test47_PLG_8_MarketplaceEntryIsPinned(t *testing.T) {
	expectNone(t, "good", marketplaceErrs(t, testCatalog()))
	expectFragment(t, "name", marketplaceErrs(t, testCatalog().set("name", "other")), "name")
	expectFragment(t, "displayName", marketplaceErrs(t, testCatalog().set("interface", doc{"displayName": "Other"})), "displayName")
	source := testEntry()["source"].(doc)
	policy := testEntry()["policy"].(doc)
	for _, row := range []struct {
		entry    doc
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
		expectFragment(t, row.fragment, marketplaceErrs(t, catalogWithEntry(row.entry)), row.fragment)
	}
	for _, spelling := range []string{"../../plugins/crw", "/plugins/crw", "plugins/crw", "./plugins/crw/", "./plugins/./crw"} {
		expectFragment(t, spelling, marketplaceErrs(t, catalogWithEntry(testEntry().set("source", source.set("path", spelling)))), "source.path")
	}
	expectFragment(t, "two entries", marketplaceErrs(t, testCatalog().set("plugins", []any{testEntry(), testEntry()})),
		`marketplace: expected exactly one entry named "crw"`)
	expectFragment(t, "no entry", marketplaceErrs(t, testCatalog().set("plugins", []any{"x", 5})), "expected exactly one entry")
	expectFragment(t, "plugins not a list", marketplaceErrs(t, testCatalog().set("plugins", 5)), "marketplace: plugins must be a list")
	// Read from the revision: a committed wrong path fails the CLI even with a clean tree.
	r := pluginRepo(t, goodFiles(t))
	broken := catalogWithEntry(testEntry().set("source", source.set("path", "./plugins/elsewhere")))
	r.write(marketplacePath, dumps(broken))
	r.gitCommit("break")
	got := pluginCLI(t, r.root)
	if got.code != 1 || !strings.Contains(got.stderr, "source.path") {
		t.Errorf("CLI marketplace: %+v", got)
	}
}

func Test47_PLG_9_PayloadHygiene(t *testing.T) {
	good := goodFiles(t)
	expectNone(t, "clean", hygieneErrs(t, good, testManifest()))
	for _, missing := range []string{manifestPath, "LICENSE"} {
		expectFragment(t, missing, hygieneErrs(t, good.without(missing), testManifest()), missing+" must ship with the package")
	}
	expectFragment(t, "allowlist", hygieneErrs(t, good.with("packages/relay.py", "x"), testManifest()),
		"t packages/relay.py: only .codex-plugin, LICENSE, skills may ship in the package")
	for _, name := range []string{".codexclaw/sessions/s.json", "skills/crw-run/relay.sqlite3", ".env", ".env.local",
		"skills/id_rsa", "skills/aws-credentials", "skills/client.key", "skills/crw-run/client-secrets.json",
		"skills/secret.yaml", "skills/service-credential.json", "skills/id_ed25519", "skills/id_ecdsa",
		"skills/.netrc", "skills/.npmrc", "skills/authorized_keys", "skills/X.PEM"} {
		expectFragment(t, name, hygieneErrs(t, good.with(name, "x"), testManifest()), "operational state and credentials may not ship")
	}
	for _, name := range []string{"skills/crw-run/secretary.md", "skills/crw-run/credentialing.md",
		"skills/crw-run/keyboard.md", "skills/crw-run/database.md"} {
		expectNone(t, name, hygieneErrs(t, good.with(name, "x"), testManifest()))
	}
	for _, row := range []struct{ text, found string }{
		{"put it in /home/someone/code/x", "/home/someone/"},
		{"C:\\Users\\me\\x", "C:\\Users\\me"},
		{"/Users/me/x", "/Users/me/"},
		{"at /root/x", "/root/"},
		{"x/home/y/z", ""},
		{"use <worktree-root>/<project> or /example/home/x", ""},
		{"\xff\xfe/home/a/", ""}, // not text, so not read for paths
	} {
		errs := hygieneErrs(t, good.with("skills/crw-run/SKILL.md", row.text), testManifest())
		if row.found == "" {
			expectNone(t, row.text, errs)
			continue
		}
		expectFragment(t, row.text, errs, "contains the personal path "+show(row.found))
	}
}

func Test47_PLG_10_DeclaredDirectoryIsTheSkillSet(t *testing.T) {
	errs, found := skillErrs(t, goodFiles(t), testManifest())
	expectNone(t, "good", errs)
	expectEqual(t, "found", found, []string{"crw-run"})
	nested := testManifest().set("skills", "./skills/current/")
	base := files{manifestPath: dumps(nested), "skills/current/crw-run/SKILL.md": testSkill,
		"skills/current/crw-run/agents/openai.yaml": testInterface("crw-run"), "LICENSE": "MIT"}
	errs, found = skillErrs(t, base, nested)
	expectNone(t, "nested", errs)
	expectEqual(t, "nested found", found, []string{"crw-run"})
	errs, _ = skillErrs(t, base.with("skills/old/crw-check/SKILL.md", testSkill), nested)
	expectFragment(t, "outside", errs, "t skills/old/crw-check/SKILL.md: ships outside every declared component path")
	errs, _ = skillErrs(t, goodFiles(t).without("skills/crw-run/agents/openai.yaml"), testManifest())
	expectFragment(t, "yaml", errs, "t skills/crw-run: missing agents/openai.yaml")
	errs, _ = skillErrs(t, files{"LICENSE": "MIT"}, testManifest())
	expectFragment(t, "empty", errs, "t: the declared skills path ships no skill")
	where := "t skills/crw-run/agents/openai.yaml: "
	for _, row := range []struct{ yaml, fragment string }{
		{testInterface("other"), where + "default_prompt must name $crw-run"},
		{"interface:\n  display_name: \"x\n", where + "display_name is not a readable scalar"},
		{"interface:\n  display_name: a\n  display_name: b\n", where + "malformed interface metadata"},
		{"interface:\n  display_name: a\n", where + "missing interface default_prompt, short_description"},
	} {
		errs, _ = skillErrs(t, goodFiles(t).with("skills/crw-run/agents/openai.yaml", row.yaml), testManifest())
		expectFragment(t, row.yaml, errs, row.fragment)
	}
	errs, _ = skillErrs(t, goodFiles(t), testManifest().set("skills", 5))
	expectEqual(t, "skills not a path", errs, []string{"t: manifest must declare skills as a ./ relative path"})
}

// The digest frames each file as "<len>:<name> <mode> <sha256>" in name order, so it covers the
// mode and no name can smuggle a line in; its first twelve digits are the version suffix a
// manifest records, so the golden holds the digests of fixed payloads.
func Test47_PLG_11_DigestIsFramedAndCoversMode(t *testing.T) {
	plain := payload{"LICENSE": {"100644", []byte("MIT")}, "skills/crw-run/SKILL.md": {"100644", []byte(testSkill)}}
	changed := maps.Clone(plain)
	changed["LICENSE"] = entry{"100644", []byte("MIT ")}
	executable := maps.Clone(plain)
	executable["LICENSE"] = entry{"100755", []byte("MIT")}
	third := hexSum([]byte("three"))
	left := payload{"a": {"100644", []byte("one")}, "b\n100644 " + third + " x": {"100644", []byte("two")}}
	right := payload{"a": {"100644", []byte("one")}, "b": {"100644", []byte("two")}, "x": {"100644", []byte("three")}}
	if unframed(left) != unframed(right) {
		t.Fatal("the counterexample must collide without framing")
	}
	digests := map[string]string{"plain": payloadDigest(plain), "changed": payloadDigest(changed),
		"executable": payloadDigest(executable), "left": payloadDigest(left), "right": payloadDigest(right)}
	golden.CheckJSON(t, "digests", digests)
	for _, pair := range [][2]string{{"plain", "changed"}, {"plain", "executable"}, {"left", "right"}} {
		if digests[pair[0]] == digests[pair[1]] {
			t.Errorf("%s and %s share a digest", pair[0], pair[1])
		}
	}
}

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
	version := parse(t, good[manifestPath])["version"].(string)
	if !regexp.MustCompile(`^0\.1\.0\+[0-9a-f]{12}$`).MatchString(version) {
		t.Fatalf("recorded version %q", version)
	}
	if again, err := payloadVersion(good.payload(), version); err != nil || again != version {
		t.Errorf("payloadVersion of the recorded payload: %q, %v; want %q", again, err, version)
	}
	recorded := doc(parse(t, good[manifestPath]))
	expectEqual(t, "settles", recordedFiles(t, good, recorded), good)
	other := recordedFiles(t, good.with("LICENSE", "MIT\n"), recorded.set("version", "0.1.0"))
	if other[manifestPath] == good[manifestPath] {
		t.Error("one changed file must change the version")
	}
	elided, err := versionPayload(good.payload(), version)
	if err != nil {
		t.Fatal(err)
	}
	expectEqual(t, "elided manifest", string(elided[manifestPath].data), dumps(testManifest()))
	stale := recorded.set("version", "0.1.0+000000000000")
	errs := manifestErrs(t, stale, "crw", good.with(manifestPath, dumps(stale)).payload())
	expectFragment(t, "stale", errs, "does not name this payload. Record "+show(version))
	suffix := strings.SplitN(version, "+", 2)[1]
	errs = manifestErrs(t, recorded, "crw", good.with("skills/crw-run/references/built.md", "built from "+suffix).payload())
	expectFragment(t, "repeat", errs, "t skills/crw-run/references/built.md: a shipped file repeats the payload suffix")
	twice := recorded.set("description", version)
	errs = manifestErrs(t, twice, "crw", good.with(manifestPath, dumps(twice)).payload())
	expectFragment(t, "twice", errs, "spells "+show(version)+" 2 times; the suffix has to be elided")
	errs = manifestErrs(t, testManifest(), "crw", good.with(manifestPath, dumps(testManifest())).payload())
	expectFragment(t, "plain", errs, `version "0.1.0" does not name this payload`)
	// CLI: a committed skill edited after recording; an installed LICENSE edited.
	r := pluginRepo(t, good.with("skills/crw-run/SKILL.md", testSkill+"\nAn extra paragraph.\n"))
	got := pluginCLI(t, r.root)
	if got.code != 1 || !strings.Contains(got.stderr, "does not name this payload") {
		t.Errorf("CLI release digest: %+v", got)
	}
	dir := writePayload(t, good.with("LICENSE", "MIT, with a later edit"))
	got = pluginPayload(t, dir)
	if got.code != 1 || !strings.Contains(got.stderr, "does not name this payload") {
		t.Errorf("CLI installed digest: %+v", got)
	}
}

func Test47_PLG_13_RecordVersionWritesAndSettles(t *testing.T) {
	plain := goodFiles(t).with(manifestPath, dumps(testManifest()))
	r := pluginRepo(t, plain)
	written := func(r *fixtureRepo) string {
		data, err := os.ReadFile(filepath.Join(r.root, "plugins/crw", manifestPath))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	got := goCheck(t, r.root, nil, "plugin", "--record-version")
	want := goodFiles(t)[manifestPath]
	version := parse(t, want)["version"].(string)
	expectEqual(t, "record", got, result{0, "Version " + version + " in " + manifestPath +
		", recorded from 4 shipped files. Commit it: the release payload is read from the revision, not from this tree\n", ""})
	expectEqual(t, "written manifest", written(r), want)
	got = goCheck(t, r.root, nil, "plugin", "--record-version")
	expectEqual(t, "again", got, result{0, "Version " + version + " in " + manifestPath + ": already recorded\n", ""})
	expectEqual(t, "settled", written(r), want)
	twice := doc(parse(t, goodFiles(t)[manifestPath]))
	twice = twice.set("description", twice["version"])
	r = pluginRepo(t, goodFiles(t).with(manifestPath, dumps(twice)))
	got = goCheck(t, r.root, nil, "plugin", "--record-version")
	if got.code != 1 || !strings.Contains(got.stderr, "elided") || strings.Contains(got.stderr, "panic") {
		t.Errorf("cannot derive: %+v", got)
	}
}

func Test47_PLG_14_RepositoryCheckReadsTheRevision(t *testing.T) {
	good := goodFiles(t)
	expectEqual(t, "healthy", pluginCLI(t, pluginRepo(t, good).root).code, 0)
	for _, row := range []struct {
		f        files
		fragment string
	}{
		{good.with(manifestPath, "not-json"), "plugin.json"},
		{good.without("LICENSE"), "LICENSE"},
		{good.with("LICENSE", "Apache"), "repository license"},
		{good.with(manifestPath, dumps(testManifest().set("license", "Apache-2.0"))), "license"},
	} {
		got := pluginCLI(t, pluginRepo(t, row.f).root)
		if got.code != 1 || !strings.Contains(got.stderr, row.fragment) {
			t.Errorf("%s: %+v", row.fragment, got)
		}
	}
}

func Test47_PLG_15_RootSkillsEntryIsNeitherRequiredNorRead(t *testing.T) {
	// The root `skills` compatibility link left in todo 44: nothing installs through it, so a
	// checkout without it passes, and so does one whose root entry names something else.
	expectEqual(t, "no link", pluginCLI(t, pluginRepo(t, goodFiles(t)).root).code, 0)
	r := pluginRepo(t, goodFiles(t))
	if err := os.Symlink("plugins/crw", filepath.Join(r.root, "skills")); err != nil {
		t.Fatal(err)
	}
	r.gitCommit("link")
	expectEqual(t, "link", pluginCLI(t, r.root).code, 0)
}

func Test47_PLG_16_WorkingTreeGetsTheSameChecks(t *testing.T) {
	r := pluginRepo(t, goodFiles(t))
	r.write("plugins/crw/"+manifestPath, "not-json")
	got := pluginCLI(t, r.root)
	if got.code != 1 || !strings.Contains(got.stderr, "working tree") {
		t.Errorf("working manifest: %+v", got)
	}
	withPlan := goodFiles(t).with("skills/crw-plan/SKILL.md", "---\nname: crw-plan\ndescription: d\n---\n").
		with("skills/crw-plan/agents/openai.yaml", testInterface("crw-plan"))
	withPlan = recordedFiles(t, withPlan, doc(parse(t, withPlan[manifestPath])).set("version", "0.1.0"))
	r = pluginRepo(t, withPlan)
	if err := os.RemoveAll(filepath.Join(r.root, "plugins/crw/skills/crw-plan")); err != nil {
		t.Fatal(err)
	}
	got = pluginCLI(t, r.root)
	if got.code != 1 || !strings.Contains(got.stderr, "working tree: crw-plan ships in the revision but is missing here") {
		t.Errorf("missing skill: %+v", got)
	}
	r = pluginRepo(t, goodFiles(t))
	r.write("plugins/crw/notes.txt", "local")
	got = pluginCLI(t, r.root)
	if got.code != 1 || !strings.Contains(got.stderr, "working tree plugins/crw/notes.txt: untracked or ignored files") {
		t.Errorf("untracked: %+v", got)
	}
}

func Test47_PLG_17_DirectoryPayloadHasNoEmptyDirOrSymlink(t *testing.T) {
	r := pluginRepo(t, goodFiles(t))
	if err := os.Mkdir(filepath.Join(r.root, "plugins/crw/extra-empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := pluginCLI(t, r.root)
	if got.code != 1 || !strings.Contains(got.stderr, "extra-empty: an empty directory still ships; remove it") {
		t.Errorf("working empty dir: %+v", got)
	}
	dir := writePayload(t, goodFiles(t))
	if err := os.Mkdir(filepath.Join(dir, "leftover"), 0o755); err != nil {
		t.Fatal(err)
	}
	got = pluginPayload(t, dir)
	if got.code != 1 || !strings.Contains(got.stderr, "leftover: an empty directory still ships") {
		t.Errorf("installed empty dir: %+v", got)
	}
	dir = writePayload(t, goodFiles(t))
	if err := os.Symlink(filepath.Join(dir, "skills/crw-run"), filepath.Join(dir, "skills/crw-plan")); err != nil {
		t.Fatal(err)
	}
	got = pluginPayload(t, dir)
	if got.code != 1 || !strings.Contains(got.stderr, "installed skills/crw-plan: the installer drops symlinks") {
		t.Errorf("installed symlink: %+v", got)
	}
}

// The --json report prints each top-level member on its own line, two spaces in, which is how
// docs/plugin-packaging.md reads stopHooks and hooksDigest out of it with sed.
func Test47_PLG_18_JSONReport(t *testing.T) {
	root := repoRoot()
	got := goCheck(t, root, nil, "plugin", "--json")
	var r map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &r); err != nil {
		t.Fatalf("report: %v: %+v", err, got)
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
	text := goCheck(t, root, nil, "plugin")
	if text.code != 0 || !strings.Contains(text.stdout, "Package "+r["version"].(string)+" at "+head+": ") {
		t.Errorf("repository text: %+v", text)
	}
	dir := writePayload(t, goodFiles(t))
	got = goCheck(t, t.TempDir(), nil, "plugin", "--payload", dir, "--json")
	golden.CheckJSON(t, "payload --json", map[string]any{"code": got.code, "stdout": got.stdout, "stderr": got.stderr},
		golden.Substitute(dir, "$PAYLOAD"))
	field := regexp.MustCompile(`(?m)^  "(stopHooks|hooksDigest)": (.*[^,]),?$`)
	if matches := field.FindAllStringSubmatch(got.stdout, -1); len(matches) != 2 {
		t.Errorf("the report's stopHooks and hooksDigest lines: %q", matches)
	}
	if err := json.Unmarshal([]byte(got.stdout), &r); err != nil {
		t.Fatal(err)
	}
	expectEqual(t, "payload names", r["expectedSkillNames"], []any{"crw:crw-run"})
	expectEqual(t, "payload source", r["source"], "payload")
	got = pluginPayload(t, writePayload(t, goodFiles(t).without("LICENSE")))
	if got.code != 1 || !strings.Contains(got.stderr, "LICENSE") {
		t.Errorf("payload without LICENSE: %+v", got)
	}
}

// The report counts the Stop hooks a host runs: those listed under hooks.Stop in the files the
// manifest names under hooks. The check passes a payload whose Stop list sits outside a declared
// file's hooks object or in a file the manifest does not name, and the report counts neither.
// hooksDigest follows the declared files, their order and their bytes.
func Test47_PLG_19_ReportCountsTheStopHooksAHostRuns(t *testing.T) {
	stop := `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "crw hook", "timeout": 10}]}]}}`
	start := `{"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "true", "timeout": 10}]}]}}`
	outside := `{"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "true", "timeout": 10}]}]},` +
		` "notes": {"Stop": [{"hooks": [{"type": "command", "command": "crw hook", "timeout": 10}]}]}}`
	report := func(label string, hooks any, shipped files) map[string]any {
		t.Helper()
		m := testManifest()
		if hooks != nil {
			m = m.set("hooks", hooks)
		}
		f := files{"skills/crw-run/SKILL.md": testSkill, "skills/crw-run/agents/openai.yaml": testInterface("crw-run"), "LICENSE": "MIT"}
		for name, text := range shipped {
			f = f.with(name, text)
		}
		dir := writePayload(t, recordedFiles(t, f, m))
		got := goCheck(t, t.TempDir(), nil, "plugin", "--payload", dir, "--json")
		var r map[string]any
		if err := json.Unmarshal([]byte(got.stdout), &r); err != nil {
			t.Fatalf("%s: %v: %+v", label, err, got)
		}
		return r
	}
	one := report("declared", "./hooks/stop.json", files{"hooks/stop.json": stop})
	expectEqual(t, "declared Stop", one["stopHooks"], float64(1))
	listed := report("listed", []string{"./hooks/stop.json"}, files{"hooks/stop.json": stop})
	expectEqual(t, "one path or a list of it", listed["hooksDigest"], one["hooksDigest"])
	outsideHooks := report("outside the hooks object", "./hooks/stop.json", files{"hooks/stop.json": outside})
	expectEqual(t, "Stop outside the hooks object", outsideHooks["stopHooks"], float64(0))
	undeclared := report("undeclared", "./hooks/start.json", files{"hooks/start.json": start, "hooks/stop.json": stop})
	expectEqual(t, "Stop in a file the manifest does not name", undeclared["stopHooks"], float64(0))
	none := report("no hooks", nil, nil)
	expectEqual(t, "no hooks key", none["stopHooks"], float64(0))
	empty := sha256.Sum256(nil)
	expectEqual(t, "no declared file", none["hooksDigest"], hex.EncodeToString(empty[:]))
	edited := report("command changed", "./hooks/stop.json", files{"hooks/stop.json": strings.Replace(stop, "crw hook", "crw hook -v", 1)})
	expectEqual(t, "changed Stop", edited["stopHooks"], float64(1))
	if edited["hooksDigest"] == one["hooksDigest"] {
		t.Errorf("a changed command kept hooksDigest %v", one["hooksDigest"])
	}
	both := files{"hooks/start.json": start, "hooks/stop.json": stop}
	forward := report("forward", []string{"./hooks/start.json", "./hooks/stop.json"}, both)
	backward := report("backward", []string{"./hooks/stop.json", "./hooks/start.json"}, both)
	expectEqual(t, "two files", backward["stopHooks"], float64(1))
	if forward["hooksDigest"] == backward["hooksDigest"] {
		t.Errorf("reordering the declared files kept hooksDigest %v", forward["hooksDigest"])
	}
}

// pluginRepo is a scratch checkout holding the package f under plugins/crw, the repository
// LICENSE and the marketplace entry.
func pluginRepo(t *testing.T, f files) *fixtureRepo {
	t.Helper()
	r := newRepo(t)
	for name, text := range f {
		r.write("plugins/crw/"+name, text)
	}
	r.write("LICENSE", "MIT")
	r.write(marketplacePath, dumps(testCatalog()))
	r.gitCommit("package")
	return r
}

func (r *fixtureRepo) gitCommit(message string) {
	r.git("add", "-A")
	r.git("commit", "-q", "-m", message)
}

// pluginCLI runs `crw-dev ci plugin` in root.
func pluginCLI(t *testing.T, root string, args ...string) result {
	t.Helper()
	return goCheck(t, root, nil, "plugin", args...)
}

// pluginPayload runs `crw-dev ci plugin --payload dir` outside any checkout.
func pluginPayload(t *testing.T, dir string) result {
	t.Helper()
	return goCheck(t, t.TempDir(), nil, "plugin", "--payload", dir)
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
// mode 100755, so a manifest recorded for the 100644 payload no longer names it.
func Test47_PLG_11_InstalledPayloadModeIsRead(t *testing.T) {
	good := goodFiles(t)
	dir := writePayload(t, good)
	expectEqual(t, "plain payload", pluginPayload(t, dir).code, 0)
	if err := os.Chmod(filepath.Join(dir, "LICENSE"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := pluginPayload(t, dir)
	if got.code != 1 || !strings.Contains(got.stderr, "does not name this payload") {
		t.Errorf("executable LICENSE: %+v", got)
	}
	read, errs := directoryPayload(dir)
	expectNone(t, "directory payload", errs)
	expectEqual(t, "LICENSE mode", read["LICENSE"].mode, "100755")
	expectEqual(t, "SKILL.md mode", read["skills/crw-run/SKILL.md"].mode, "100644")
	executable := good.payload()
	executable["LICENSE"] = entry{"100755", []byte("MIT")}
	expectEqual(t, "digest", payloadDigest(read), payloadDigest(executable))
}
