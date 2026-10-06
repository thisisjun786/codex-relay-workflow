package manage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// auditPkgFakeGit answers the git calls the bundle builder makes from canned data, so a
// test never runs git, reads a real repository or reaches the network.
func auditPkgFakeGit(t *testing.T, head string, tree map[string][]string, files map[string]string) {
	t.Helper()
	previousGit, previousBlob := auditPkgGit, auditPkgBlob
	auditPkgBlob = func(_ context.Context, _ string, _ string, path string, w io.Writer) error {
		body, ok := files[path]
		if !ok {
			return errors.New("no such path " + path)
		}
		_, err := io.WriteString(w, body)
		return err
	}
	auditPkgGit = func(_ context.Context, repo string, args ...string) ([]byte, error) {
		if repo != "/checkout" {
			t.Errorf("git ran in %q, want the configured checkout", repo)
		}
		if len(args) == 0 {
			return nil, errors.New("git called with no arguments")
		}
		switch args[0] {
		case "fetch":
			return nil, nil
		case "rev-parse":
			return []byte(head + "\n"), nil
		case "ls-tree":
			path := args[len(args)-1]
			if auditPkgContains(args, "--name-only") {
				return []byte(strings.Join(tree[path], "\n") + "\n"), nil
			}
			var lines []string
			for _, name := range tree[path] {
				body := files[name]
				lines = append(lines, fmt.Sprintf("100644 blob %s %d\t%s", auditPkgBlobID(body), len(body), name))
			}
			return []byte(strings.Join(lines, "\n") + "\n"), nil
		case "show":
			spec := args[1]
			path := spec[strings.IndexByte(spec, ':')+1:]
			body, ok := files[path]
			if !ok {
				return nil, errors.New("no such path " + path)
			}
			return []byte(body), nil
		}
		return nil, errors.New("unexpected git arguments: " + strings.Join(args, " "))
	}
	t.Cleanup(func() { auditPkgGit, auditPkgBlob = previousGit, previousBlob })
}

func auditPkgContains(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

// auditPkgBlobID is a stable object name for the fake, so the ls-tree line looks like the
// real thing without hashing.
func auditPkgBlobID(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:20])
}

// auditPkgFakeCriteria answers the relay criteria reads from a canned map, and records the
// keys it was asked for.
func auditPkgFakeCriteria(t *testing.T, byIssue map[string]string) *[]string {
	t.Helper()
	asked := &[]string{}
	previous := auditPkgRelayCriteria
	auditPkgRelayCriteria = func(_ context.Context, _ *Env, _ *Config, issue string) ([]byte, error) {
		*asked = append(*asked, issue)
		body, ok := byIssue[issue]
		if !ok {
			return nil, errors.New("no assignment for " + issue)
		}
		return []byte(body), nil
	}
	t.Cleanup(func() { auditPkgRelayCriteria = previous })
	return asked
}

// auditPkgConfig is a configuration with a checkout section and the package_criteria the
// test names.
func auditPkgConfig(t *testing.T, stateDir string, criteria map[string]any) *Config {
	t.Helper()
	cfg := auditSectionConfig(t, stateDir, map[string]any{"grader": []string{"x"}, "package_criteria": criteria})
	cfg.raw["checkout"] = auditPkgJSON(t, map[string]any{"repository": "/checkout", "base_ref": "origin/dev"})
	return cfg
}

func auditPkgJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// auditPkgCriteriaJSON is a criteria-show answer.
func auditPkgCriteriaJSON(t *testing.T, id, title string) string {
	t.Helper()
	return string(auditPkgJSON(t, map[string]any{
		"relationshipId": "rel-1",
		"criteria":       []map[string]any{{"id": id, "title": title, "required": true}},
		"setDigest":      "digest",
	}))
}

// auditPkgBuildFixture builds one bundle with the fake git, fake relay and a configuration.
func auditPkgBuildFixture(t *testing.T, state, pkg, head string, criteria map[string]any) string {
	t.Helper()
	e, _, _ := auditTestEnv(t)
	cfg := auditPkgConfig(t, state, criteria)
	section, err := auditPkgSectionOf(cfg)
	if err != nil {
		t.Fatal(err)
	}
	co, err := auditPkgCheckoutOf(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := auditPkgBuild(context.Background(), e, cfg, section, co, pkg, head)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// auditPkgHead is the head every package test builds at.
const auditPkgHead = "0123456789abcdef0123456789abcdef01234567"

// C5: the bundle carries every source and test file of the head, the criteria land in
// criteria.json, and a file past 1MB is replaced by a listing line carrying its path, size
// and sha256.
func TestAuditPackageBuildsTheBundle(t *testing.T) {
	state := t.TempDir()
	big := strings.Repeat("x", auditPkgLargeFileBytes+1)
	auditPkgFakeGit(t, auditPkgHead,
		map[string][]string{"internal/manage": {"internal/manage/a.go", "internal/manage/a_test.go", "internal/manage/big.bin"}},
		map[string]string{
			"internal/manage/a.go":      "package manage\n",
			"internal/manage/a_test.go": "package manage\n",
			"internal/manage/big.bin":   big,
		})
	auditPkgFakeCriteria(t, map[string]string{"CRW-764": auditPkgCriteriaJSON(t, "C1", "one")})
	dir := auditPkgBuildFixture(t, state, "internal/manage", auditPkgHead, map[string]any{
		"internal/manage": map[string]any{"issues": []string{"CRW-764"}},
	})
	for _, rel := range []string{"bundle.json", "criteria.json", "task.md", "src/internal/manage/a.go", "src/internal/manage/a_test.go"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			t.Errorf("the bundle is missing %s: %v", rel, err)
		}
	}
	listing, err := os.ReadFile(filepath.Join(dir, auditPkgSrcDir, auditPkgLargeListName))
	if err != nil {
		t.Fatalf("the large-file listing is missing: %v", err)
	}
	sum := sha256.Sum256([]byte(big))
	want := fmt.Sprintf("internal/manage/big.bin %d sha256:%s", len(big), hex.EncodeToString(sum[:]))
	if !strings.Contains(string(listing), want) {
		t.Errorf("the listing is %q, want it to carry %q", listing, want)
	}
	if _, err := os.Stat(filepath.Join(dir, auditPkgSrcDir, "internal", "manage", "big.bin")); !os.IsNotExist(err) {
		t.Errorf("an oversized file was copied whole: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, auditBundleFile))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Mode    string `json:"mode"`
		Subject string `json:"subject"`
		Head    string `json:"head"`
		Schema  string `json:"schema"`
		Issue   string `json:"issue"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Schema != auditBundleSchema || doc.Mode != auditModePackage || doc.Subject != "internal/manage" || doc.Head != auditPkgHead || doc.Issue != auditPkgIssue {
		t.Errorf("bundle.json = %+v", doc)
	}
	if _, err := auditReadBundle(dir); err != nil {
		t.Errorf("the built bundle is not readable by the grader: %v", err)
	}
	criteria, err := os.ReadFile(filepath.Join(dir, auditPkgCriteriaFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(criteria), "C1") || !strings.Contains(string(criteria), "CRW-764") {
		t.Errorf("criteria.json does not carry the merged criteria: %s", criteria)
	}
	if name := filepath.Base(dir); name != auditPkgBundlePrefix+"internal_manage-"+auditPkgHead[:12] {
		t.Errorf("the bundle directory is %q", name)
	}
}

// C8: a criteria key the relay cannot read lands in criteria_missing and the bundle is
// still built, so grading proceeds.
func TestAuditPackageRecordsUnreadableCriteria(t *testing.T) {
	state := t.TempDir()
	auditPkgFakeGit(t, auditPkgHead, map[string][]string{"pkg": {"pkg/a.go"}}, map[string]string{"pkg/a.go": "package pkg\n"})
	asked := auditPkgFakeCriteria(t, map[string]string{})
	dir := auditPkgBuildFixture(t, state, "pkg", auditPkgHead, map[string]any{
		"pkg": map[string]any{"issues": []string{"CRW-999"}},
	})
	data, err := os.ReadFile(filepath.Join(dir, auditPkgCriteriaFile))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Missing []string `json:"criteria_missing"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Missing) != 1 || doc.Missing[0] != "CRW-999" {
		t.Errorf("criteria_missing = %v, want the unreadable key", doc.Missing)
	}
	if len(*asked) != 1 || (*asked)[0] != "CRW-999" {
		t.Errorf("the relay was asked for %v, want the key the criteria name", *asked)
	}
}

// A package with no criteria source at all still builds, and the bundle says so.
func TestAuditPackageWithNoCriteriaSource(t *testing.T) {
	state := t.TempDir()
	auditPkgFakeGit(t, auditPkgHead, map[string][]string{"pkg": {"pkg/a.go"}}, map[string]string{"pkg/a.go": "package pkg\n"})
	auditPkgFakeCriteria(t, map[string]string{})
	dir := auditPkgBuildFixture(t, state, "pkg", auditPkgHead, nil)
	bundle, err := auditReadBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bundle.CriteriaUnavailable {
		t.Error("a bundle with no criteria source does not say criteria_unavailable")
	}
}

// The three criteria sources land in reference/, known-defects/ and criteria.json, and
// task.md names the package and the known-defects instruction.
func TestAuditPackageCopiesTheCriteriaSources(t *testing.T) {
	state := t.TempDir()
	sourceDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceDir, "origin.go"), []byte("package origin\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	auditPkgFakeGit(t, auditPkgHead,
		map[string][]string{
			"pkg":                {"pkg/a.go"},
			"docs/known-defects": {"docs/known-defects/CRW-1.md"},
		},
		map[string]string{"pkg/a.go": "package pkg\n", "docs/known-defects/CRW-1.md": "known\n"})
	auditPkgFakeCriteria(t, map[string]string{"CRW-1": auditPkgCriteriaJSON(t, "C1", "one")})
	dir := auditPkgBuildFixture(t, state, "pkg", auditPkgHead, map[string]any{
		"pkg": map[string]any{
			"source":        []string{sourceDir},
			"known_defects": []string{"docs/known-defects"},
			"issues":        []string{"CRW-1"},
		},
	})
	if _, err := os.Stat(filepath.Join(dir, auditPkgReferenceDir, filepath.Base(sourceDir), "origin.go")); err != nil {
		t.Errorf("the port source was not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, auditPkgKnownDefectsDir, "docs", "known-defects", "CRW-1.md")); err != nil {
		t.Errorf("the known-defects content was not copied: %v", err)
	}
	task, err := os.ReadFile(filepath.Join(dir, auditPkgTaskFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"pkg", auditPkgKnownDefectsDir, "already-known"} {
		if !strings.Contains(string(task), needle) {
			t.Errorf("task.md does not carry %q: %q", needle, task)
		}
	}
}

// The longest matching package path prefix supplies the criteria source.
func TestAuditPackageSourcePrefixIsTheLongestMatch(t *testing.T) {
	section := auditPkgSection{PackageCriteria: map[string]auditPkgSource{
		"internal/":       {Issues: []string{"wide"}},
		"internal/manage": {Issues: []string{"narrow"}},
	}}
	if got := auditPkgSourceFor(section, "internal/manage"); len(got.Issues) != 1 || got.Issues[0] != "narrow" {
		t.Errorf("the source is %+v, want the longest matching prefix", got)
	}
	if got := auditPkgSourceFor(section, "internal/relay"); len(got.Issues) != 1 || got.Issues[0] != "wide" {
		t.Errorf("the source is %+v, want the shorter matching prefix", got)
	}
	if got := auditPkgSourceFor(section, "cmd/crw"); len(got.Issues) != 0 {
		t.Errorf("an undeclared package got %+v", got)
	}
}

// A package path can never make the builder remove anything outside the bundle root.
func TestAuditPackageResetDirRefusesAnEscape(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bundles")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := auditPkgResetDir(root, root); err == nil {
		t.Error("the bundle root itself was accepted as a bundle directory")
	}
	if err := auditPkgResetDir(root, filepath.Join(root, "..", "elsewhere")); err == nil {
		t.Error("a directory above the bundle root was accepted")
	}
	inside := filepath.Join(root, "pkg-a")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := auditPkgResetDir(root, inside); err != nil {
		t.Errorf("a directory inside the root was refused: %v", err)
	}
}

// The head is the one given, and a missing head is the fetched base ref's tip.
func TestAuditPackageResolveHead(t *testing.T) {
	ctx := context.Background()
	auditPkgFakeGit(t, auditPkgHead, map[string][]string{}, map[string]string{})
	head, err := auditPkgResolveHead(ctx, auditPkgCheckout{Repository: "/checkout", BaseRef: "origin/dev"}, "given")
	if err != nil || head != "given" {
		t.Errorf("head = %q/%v, want the given one", head, err)
	}
	head, err = auditPkgResolveHead(ctx, auditPkgCheckout{Repository: "/checkout", BaseRef: "origin/dev"}, "")
	if err != nil || head != auditPkgHead {
		t.Errorf("head = %q/%v, want the fetched tip", head, err)
	}
	if _, err := auditPkgResolveHead(ctx, auditPkgCheckout{}, ""); err == nil {
		t.Error("a checkout with no repository resolved a head")
	}
}
