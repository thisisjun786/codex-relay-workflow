package manage

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// auditPRReview752TextPatch is a one-file patch, the shape gh answers `pr diff` with.
const auditPRReview752TextPatch = "diff --git a/internal/a.go b/internal/a.go\n" +
	"--- a/internal/a.go\n" +
	"+++ b/internal/a.go\n" +
	"@@ -1 +1 @@\n" +
	"-old\n" +
	"+new\n"

// auditPRReview752Listing is one git diff-tree -z --name-status answer: each record followed
// by the NUL byte that delimits it.
func auditPRReview752Listing(records ...string) []byte {
	var out []byte
	for _, record := range records {
		out = append(out, []byte(record)...)
		out = append(out, 0)
	}
	return out
}

// C2: the changed paths come from git's own listing, read positionally: a status, then its
// path, or, for a rename or a copy, the old path and then the new one. A deletion contributes
// nothing, a path that carries a space arrives as itself because the listing is NUL-delimited,
// and a record that does not fit the grammar is refused rather than half-read.
func TestAuditPRReview752NameStatusPaths(t *testing.T) {
	listing := auditPRReview752Listing(
		"M", "internal/a.go",
		"A", "docs/new.md",
		"D", "docs/gone.md",
		"R100", "old.go", "new.go",
		"C100", "src/from.go", "src/to.go",
		"T", "link",
		"M", "docs/with space.txt",
		"M", "internal/a.go",
	)
	got, err := auditPRNameStatusPaths(listing)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"internal/a.go", "docs/new.md", "new.go", "src/to.go", "link", "docs/with space.txt"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the paths are %v, want %v", got, want)
	}
	if paths, err := auditPRNameStatusPaths(nil); err != nil || len(paths) != 0 {
		t.Errorf("an empty listing gave %v (%v)", paths, err)
	}
	// A listing read half way would build a bundle that looks complete while a file the
	// change carries is missing from it, so a record that does not fit is an error.
	for _, broken := range []string{
		"M\x00",                            // a status with no path
		"R100\x00old.go\x00",               // a rename with no target path
		"internal/a.go\x00M\x00x\x00",      // a path where a status belongs
		"Z\x00x\x00",                       // a status git never writes
		"M\x00a.go",                        // no terminating NUL: the listing is truncated
		"R100\x00\x00old.go\x00new.go\x00", // a rename whose source path is empty
		"M\x00\x00",                        // a present but empty path
		"M\x00a.go\x00\x00b.go\x00",        // an empty path between two records
	} {
		if _, err := auditPRNameStatusPaths([]byte(broken)); err == nil {
			t.Errorf("the listing %q was accepted", broken)
		}
	}
	// A path that merely looks like a status is a path: the records alternate, so a file named
	// `M` is read as the path of the record before it, not as another status.
	if paths, err := auditPRNameStatusPaths(auditPRReview752Listing("M", "M")); err != nil || strings.Join(paths, ",") != "M" {
		t.Errorf("a path named M gave %v (%v)", paths, err)
	}
}

// auditPRReview752Escaped is the form encoding/json writes by default for a name that carries
// one of the three bytes it escapes.
func auditPRReview752Escaped(name string) string {
	return strings.NewReplacer("&", "\\u0026", "<", "\\u003c", ">", "\\u003e").Replace(name)
}

// auditPRReview752Strings collects every string value of a decoded JSON document, so a test
// can ask what a reader of the file sees rather than what the file's bytes spell.
func auditPRReview752Strings(value any, out *[]string) {
	switch typed := value.(type) {
	case string:
		*out = append(*out, typed)
	case []any:
		for _, item := range typed {
			auditPRReview752Strings(item, out)
		}
	case map[string]any:
		for _, item := range typed {
			auditPRReview752Strings(item, out)
		}
	}
}

// auditPRReview752Checkout fakes the git reader the bundle builder uses: the fetch, the
// diff-tree listing of a merge commit, and git show of one of its files. The listing is keyed
// by the merge commit, so one target can fail while another answers.
func auditPRReview752Checkout(t *testing.T, listings map[string][]byte, files map[string]map[string]string) {
	t.Helper()
	previousGit, previousBlob := auditPkgGit, auditPkgBlob
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
		case "diff-tree":
			merge := args[len(args)-1]
			listing, ok := listings[merge]
			if !ok {
				return nil, errors.New("no listing for " + merge)
			}
			return listing, nil
		}
		return nil, errors.New("unexpected git arguments: " + strings.Join(args, " "))
	}
	auditPkgBlob = func(_ context.Context, repo, head, path string, w io.Writer) error {
		if repo != "/checkout" {
			t.Errorf("the blob was read from %q, want the configured checkout", repo)
		}
		byPath, ok := files[head]
		if !ok {
			return errors.New("no files at " + head)
		}
		body, ok := byPath[path]
		if !ok {
			return errors.New("no such path " + path)
		}
		_, err := io.WriteString(w, body)
		return err
	}
	t.Cleanup(func() { auditPkgGit, auditPkgBlob = previousGit, previousBlob })
}

// C1: a registered pair name is scrubbed before the criteria document is encoded. The encoder
// escapes &, < and > as \\u0026 and the like, so a scrub applied only to the encoded bytes
// never matches a name that carries one of them, and a reader of criteria.json sees exactly
// the name the scrub exists to hide.
//
// A name that carries a byte JSON escapes structurally — a quote, a backslash, a line or
// paragraph separator — is the case that separates the two defences: with SetEscapeHTML(false)
// the encoder writes it as \\" or \\\\ or \\u2028, so it never appears literally in the encoded
// bytes and the byte replacement cannot match it. Only the pre-encode walk removes it, which is
// why the HTML trio alone would still pass with the walk deleted.
func TestAuditPRReview752CriteriaScrubbedBeforeEncoding(t *testing.T) {
	for _, tc := range []struct {
		label   string
		name    string
		escaped bool
	}{
		{"ampersand", "Pair&A", true},
		{"angle", "Pair<A>", true},
		{"greater", "Pair>A", true},
		{"quote", `Pair"A`, false},
		{"backslash", `Pair\A`, false},
		{"separator", "Pair\u2028A", false},
	} {
		t.Run(tc.label, func(t *testing.T) {
			name := tc.name
			state := t.TempDir()
			cfg := auditPRSectionFixture(t, state, map[string]any{
				"pr_since": "2026-10-01T00:00:00Z",
				"pairs":    map[string]string{"deepseek": name},
				"scrub":    []string{name},
				"grader":   auditFake(t, "json", auditJSONClean),
			})
			auditPRFakeGh(t, auditPRListJSON(t, auditPRMergeEntry(12, "CRW-12: a change", "2026-10-05T00:00:00Z", "m12")),
				map[int]string{12: auditPRReview752TextPatch})
			auditPRFakeRelay(t, map[string]string{"CRW-12": auditPRAssignmentJSON(t, "rel-1", "child-1")},
				map[string]string{"child-1": auditPRSettingsJSON(t, "inferhub/deepseek-v4.1-flash")},
				map[string]string{"rel-1": auditPRCriteriaJSON(t, "c1", name+" passes")})
			auditPRFakeCheckout(t, "m12", map[string]string{"internal/a.go": "package a\n"})
			e, _, errOut := auditTestEnv(t)
			if code := auditPRRunWith(context.Background(), e, cfg, 9, false); code != 0 {
				t.Fatalf("audit pr: exit %d %q", code, errOut.String())
			}
			path := filepath.Join(state, "audit", "bundles", "pr-12", auditPRCriteriaFile)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var doc any
			if err := json.Unmarshal(data, &doc); err != nil {
				t.Fatalf("%s is not a JSON document: %v", auditPRCriteriaFile, err)
			}
			var values []string
			auditPRReview752Strings(doc, &values)
			for _, value := range values {
				if strings.Contains(value, name) {
					t.Errorf("a reader of %s sees %q in %q", auditPRCriteriaFile, name, value)
				}
			}
			// For the HTML trio the escaped form is what the original defect left behind, because
			// the byte replacement runs on text the encoder has already escaped. For the other
			// cases the encoder writes that form by design, so only the decoded check above can
			// tell the walk apart from the byte replacement.
			if tc.escaped {
				if escaped := auditPRReview752Escaped(name); strings.Contains(string(data), escaped) {
					t.Errorf("%s carries the escaped name %q", auditPRCriteriaFile, escaped)
				}
			}
			if !strings.Contains(string(data), auditPRRedacted) {
				t.Errorf("%s carries no redaction:\n%s", auditPRCriteriaFile, data)
			}
		})
	}
}

// C1: a pull request whose criteria could not be read keeps the shape it had, a null criteria
// list rather than an empty one: the walk must not turn "nothing was registered" into "the
// empty list was registered".
func TestAuditPRReview752EmptyCriteriaKeepsItsShape(t *testing.T) {
	state := t.TempDir()
	cfg := auditPRSectionFixture(t, state, map[string]any{
		"pr_since": "2026-10-01T00:00:00Z", "grader": auditFake(t, "json", auditJSONClean),
	})
	auditPRFakeGh(t, auditPRListJSON(t, auditPRMergeEntry(12, "CRW-12: a change", "2026-10-05T00:00:00Z", "m12")),
		map[int]string{12: auditPRReview752TextPatch})
	auditPRFakeRelay(t, map[string]string{"CRW-12": `{"issueKey":"CRW-12","responsibleChild":null,"responsibleRelationship":null,"assignments":[]}`}, nil, nil)
	auditPRFakeCheckout(t, "m12", map[string]string{"internal/a.go": "package a\n"})
	e, _, errOut := auditTestEnv(t)
	if code := auditPRRunWith(context.Background(), e, cfg, 9, false); code != 0 {
		t.Fatalf("audit pr: exit %d %q", code, errOut.String())
	}
	data, err := os.ReadFile(filepath.Join(state, "audit", "bundles", "pr-12", auditPRCriteriaFile))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Criteria            []any `json:"criteria"`
		CriteriaUnavailable bool  `json:"criteria_unavailable"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Criteria != nil || !doc.CriteriaUnavailable {
		t.Errorf("the document is %s, want a null criteria list and criteria_unavailable", data)
	}
}

// C2: the changed paths come from git, so a path that carries a space reaches the bundle
// unchanged. The fixture is a real temporary git repository whose merge commit changes a text
// file and a binary file whose names both carry a space and renames a third file: the paths come
// from the real `git diff-tree -z --name-status -M` and the file bodies from the real `git show`,
// with only gh and the relay faked. A patch's own header is ambiguous exactly here: git puts a tab
// after a `+++` line whose path holds a space, and a binary block names no path in its body at all.
func TestAuditPRReview752PathsWithSpaces(t *testing.T) {
	repo, merge := auditPRReview752Repo(t)
	state := t.TempDir()
	cfg := auditPRSectionFixture(t, state, map[string]any{
		"pr_since": "2026-10-01T00:00:00Z", "grader": auditFake(t, "json", auditJSONClean),
	})
	cfg.raw["checkout"] = auditPkgJSON(t, map[string]any{"repository": repo, "base_ref": "origin/dev"})
	auditPRFakeGh(t, auditPRListJSON(t, auditPRMergeEntry(12, "CRW-12: a change", "2026-10-05T00:00:00Z", merge)),
		map[int]string{12: "diff --git a/docs/with space.txt b/docs/with space.txt\n"})
	auditPRFakeRelay(t, map[string]string{"CRW-12": auditPRAssignmentJSON(t, "rel-1", "child-1")},
		map[string]string{"child-1": auditPRSettingsJSON(t, "inferhub/deepseek-v4.1-flash")},
		map[string]string{"rel-1": auditPRCriteriaJSON(t, "c1", "it works")})
	e, _, errOut := auditTestEnv(t)
	if code := auditPRRunWith(context.Background(), e, cfg, 9, false); code != 0 {
		t.Fatalf("audit pr: exit %d %q", code, errOut.String())
	}
	dir := filepath.Join(state, "audit", "bundles", "pr-12", auditPRFilesDir)
	for path, want := range map[string]string{
		"docs/with space.txt":  "after\n",
		"img/with space.png":   "\x00\x01binary after\n",
		"src/renamed file.txt": "a line that survives the rename\n",
	} {
		body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(path)))
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if string(body) != want {
			t.Errorf("%s holds %q, want %q", path, body, want)
		}
	}
	// A rename contributes the path it became: the path it came from is not in the bundle.
	if _, err := os.Stat(filepath.Join(dir, "src", "moved file.txt")); !os.IsNotExist(err) {
		t.Errorf("the bundle carries the rename's old path: %v", err)
	}
}

// C3: one target whose bundle cannot be built is skipped, not fatal. Its half-built directory
// is removed and no ledger row is written, so a later run retries it; the rest of the targets
// are still processed and the run reports the failure at the end.
func TestAuditPRReview752OneFailedTargetDoesNotStopTheRest(t *testing.T) {
	state := t.TempDir()
	cfg := auditPRSectionFixture(t, state, map[string]any{
		"pr_since": "2026-10-01T00:00:00Z", "grader": auditFake(t, "json", auditJSONClean),
	})
	auditPRFakeGh(t, auditPRListJSON(t,
		auditPRMergeEntry(12, "CRW-12: the first", "2026-10-05T00:00:00Z", "m12"),
		auditPRMergeEntry(11, "CRW-11: the second", "2026-10-04T00:00:00Z", "m11")),
		map[int]string{12: auditPRReview752TextPatch, 11: auditPRReview752TextPatch})
	auditPRFakeRelay(t,
		map[string]string{
			"CRW-12": auditPRAssignmentJSON(t, "rel-12", "child-12"),
			"CRW-11": auditPRAssignmentJSON(t, "rel-11", "child-11"),
		},
		map[string]string{
			"child-12": auditPRSettingsJSON(t, "inferhub/deepseek-v4.1-flash"),
			"child-11": auditPRSettingsJSON(t, "inferhub/deepseek-v4.1-flash"),
		},
		map[string]string{
			"rel-12": auditPRCriteriaJSON(t, "c1", "it works"),
			"rel-11": auditPRCriteriaJSON(t, "c1", "it works"),
		})
	// The first target's listing cannot be read; the second's can.
	auditPRReview752Checkout(t,
		map[string][]byte{"m11": auditPRReview752Listing("M", "internal/a.go")},
		map[string]map[string]string{"m11": {"internal/a.go": "package a\n"}})
	e, _, errOut := auditTestEnv(t)
	if code := auditPRRunWith(context.Background(), e, cfg, 9, false); code != 1 {
		t.Fatalf("audit pr with one failed target: exit %d, want 1: %q", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "#12") {
		t.Errorf("the skipped target is not named with its number: %q", errOut.String())
	}
	if _, err := os.Stat(filepath.Join(state, "audit", "bundles", "pr-12")); !os.IsNotExist(err) {
		t.Errorf("the failed target's bundle directory was left behind: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, "audit", "bundles", "pr-11", auditBundleFile)); err != nil {
		t.Errorf("the second target's bundle was not built: %v", err)
	}
	rows, err := auditReportLedger(e, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Subject == auditPRSubject(12) {
			t.Errorf("the failed target has a ledger row: %+v", row)
		}
	}
	if len(rows) != 1 || rows[0].Subject != auditPRSubject(11) {
		t.Errorf("the ledger holds %+v, want the second target only", rows)
	}
}

// C3: a run that is cancelled while a target's bundle is being built stops there rather than
// skipping that target. The interrupt is the run's, not the target's, and a skip would let the
// run go on to rewrite the report after the first interrupt.
func TestAuditPRReview752CancelledRunStopsInsteadOfSkipping(t *testing.T) {
	state := t.TempDir()
	cfg := auditPRSectionFixture(t, state, map[string]any{
		"pr_since": "2026-10-01T00:00:00Z", "grader": auditFake(t, "json", auditJSONClean),
	})
	auditPRFakeGh(t, auditPRListJSON(t, auditPRMergeEntry(12, "CRW-12: a change", "2026-10-05T00:00:00Z", "m12")),
		map[int]string{12: auditPRReview752TextPatch})
	auditPRFakeRelay(t, map[string]string{"CRW-12": auditPRAssignmentJSON(t, "rel-12", "child-12")},
		map[string]string{"child-12": auditPRSettingsJSON(t, "inferhub/deepseek-v4.1-flash")},
		map[string]string{"rel-12": auditPRCriteriaJSON(t, "c1", "it works")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	previousGit := auditPkgGit
	auditPkgGit = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "fetch" {
			return nil, nil
		}
		// The listing is interrupted: the context is cancelled and the command reports the
		// interruption the way a killed process does.
		cancel()
		return nil, errors.New("signal: killed")
	}
	t.Cleanup(func() { auditPkgGit = previousGit })
	e, _, errOut := auditTestEnv(t)
	if code := auditPRRunWith(ctx, e, cfg, 9, false); code != 1 {
		t.Fatalf("a cancelled run: exit %d, want 1: %q", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(state, "audit", auditReportFile)); !os.IsNotExist(err) {
		t.Errorf("a cancelled run rewrote the report: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, "audit", "bundles", auditPRSubject(12))); !os.IsNotExist(err) {
		t.Errorf("a cancelled run left a bundle directory behind: %v", err)
	}
}

// auditPRReview752Repo builds a temporary git repository and returns it with the merge commit of
// one real branch merge. The merge changes a text file and a binary file whose names both carry a
// space, and renames a third file, so `git diff-tree -z --name-status -M` answers a two-path R*
// record and two paths a patch header could not name. The repository is the real reader the bundle
// builder uses: auditPkgGit and auditPkgBlob are never replaced here. Every git call runs with its
// own configuration and identity, so the fixture neither reads nor writes the operator's git
// configuration, and `origin` is the repository itself, so the builder's fetch succeeds.
func auditPRReview752Repo(t *testing.T) (repo, merge string) {
	t.Helper()
	repo = filepath.Join(t.TempDir(), "checkout")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		return auditPRReview752Git(t, repo, args...)
	}
	run("init", "--quiet", "-b", "dev")
	run("remote", "add", "origin", repo)
	for _, dir := range []string{"docs", "img", "src"} {
		if err := os.MkdirAll(filepath.Join(repo, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, filepath.FromSlash(name)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("docs/with space.txt", "before\n")
	write("img/with space.png", "\x00\x01binary before\n")
	write("src/moved file.txt", "a line that survives the rename\n")
	run("add", "-A")
	run("commit", "--quiet", "-m", "base")
	run("checkout", "--quiet", "-b", "side")
	write("docs/with space.txt", "after\n")
	write("img/with space.png", "\x00\x01binary after\n")
	run("mv", "src/moved file.txt", "src/renamed file.txt")
	run("add", "-A")
	run("commit", "--quiet", "-m", "change")
	run("checkout", "--quiet", "dev")
	run("merge", "--quiet", "--no-ff", "-m", "merge", "side")
	return repo, run("rev-parse", "HEAD")
}

// auditPRReview752Git runs one git command in a fixture repository with its own configuration
// and identity, so no fixture reads or writes the operator's git configuration.
func auditPRReview752Git(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid",
		"GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
		"LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// C3: a target's own patch read failing skips only that target, exactly as a bundle-build
// failure does: the remaining target is still built and the run exits 1.
func TestAuditPRReview752PatchReadFailureSkipsOnlyThatTarget(t *testing.T) {
	state := t.TempDir()
	cfg := auditPRSectionFixture(t, state, map[string]any{
		"pr_since": "2026-10-01T00:00:00Z", "grader": auditFake(t, "json", auditJSONClean),
	})
	auditPRFakeGh(t, auditPRListJSON(t,
		auditPRMergeEntry(12, "CRW-12: the first", "2026-10-05T00:00:00Z", "m12"),
		auditPRMergeEntry(11, "CRW-11: the second", "2026-10-04T00:00:00Z", "m11")),
		// The first target's patch cannot be read; the second's can.
		map[int]string{11: auditPRReview752TextPatch})
	auditPRFakeRelay(t,
		map[string]string{
			"CRW-12": auditPRAssignmentJSON(t, "rel-12", "child-12"),
			"CRW-11": auditPRAssignmentJSON(t, "rel-11", "child-11"),
		},
		map[string]string{
			"child-12": auditPRSettingsJSON(t, "inferhub/deepseek-v4.1-flash"),
			"child-11": auditPRSettingsJSON(t, "inferhub/deepseek-v4.1-flash"),
		},
		map[string]string{
			"rel-12": auditPRCriteriaJSON(t, "c1", "it works"),
			"rel-11": auditPRCriteriaJSON(t, "c1", "it works"),
		})
	auditPRReview752Checkout(t,
		map[string][]byte{"m11": auditPRReview752Listing("M", "internal/a.go")},
		map[string]map[string]string{"m11": {"internal/a.go": "package a\n"}})
	e, _, errOut := auditTestEnv(t)
	if code := auditPRRunWith(context.Background(), e, cfg, 9, false); code != 1 {
		t.Fatalf("audit pr with one unreadable patch: exit %d, want 1: %q", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "#12") {
		t.Errorf("the skipped target is not named with its number: %q", errOut.String())
	}
	if _, err := os.Stat(filepath.Join(state, "audit", "bundles", "pr-11", auditBundleFile)); err != nil {
		t.Errorf("the second target's bundle was not built: %v", err)
	}
	rows, err := auditReportLedger(e, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Subject != auditPRSubject(11) {
		t.Errorf("the ledger holds %+v, want the second target only", rows)
	}
}

// C3: a target's own criteria read failing skips only that target, the same way.
func TestAuditPRReview752CriteriaReadFailureSkipsOnlyThatTarget(t *testing.T) {
	state := t.TempDir()
	cfg := auditPRSectionFixture(t, state, map[string]any{
		"pr_since": "2026-10-01T00:00:00Z", "grader": auditFake(t, "json", auditJSONClean),
	})
	auditPRFakeGh(t, auditPRListJSON(t,
		auditPRMergeEntry(12, "CRW-12: the first", "2026-10-05T00:00:00Z", "m12"),
		auditPRMergeEntry(11, "CRW-11: the second", "2026-10-04T00:00:00Z", "m11")),
		map[int]string{12: auditPRReview752TextPatch, 11: auditPRReview752TextPatch})
	auditPRFakeRelay(t,
		map[string]string{
			"CRW-12": auditPRAssignmentJSON(t, "rel-12", "child-12"),
			"CRW-11": auditPRAssignmentJSON(t, "rel-11", "child-11"),
		},
		map[string]string{
			"child-12": auditPRSettingsJSON(t, "inferhub/deepseek-v4.1-flash"),
			"child-11": auditPRSettingsJSON(t, "inferhub/deepseek-v4.1-flash"),
		},
		// The first target's criteria cannot be read; the second's can.
		map[string]string{"rel-11": auditPRCriteriaJSON(t, "c1", "it works")})
	auditPRReview752Checkout(t,
		map[string][]byte{"m11": auditPRReview752Listing("M", "internal/a.go")},
		map[string]map[string]string{"m11": {"internal/a.go": "package a\n"}})
	e, _, errOut := auditTestEnv(t)
	if code := auditPRRunWith(context.Background(), e, cfg, 9, false); code != 1 {
		t.Fatalf("audit pr with one unreadable criteria set: exit %d, want 1: %q", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "#12") {
		t.Errorf("the skipped target is not named with its number: %q", errOut.String())
	}
	if _, err := os.Stat(filepath.Join(state, "audit", "bundles", "pr-11", auditBundleFile)); err != nil {
		t.Errorf("the second target's bundle was not built: %v", err)
	}
	rows, err := auditReportLedger(e, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Subject != auditPRSubject(11) {
		t.Errorf("the ledger holds %+v, want the second target only", rows)
	}
}

// C3: the dry run's pre-pass follows the same rule: a target whose relay read fails is named and
// skipped, the other target is still printed, and the run exits 1.
func TestAuditPRReview752DryRunSkipsAnUnreadableTarget(t *testing.T) {
	state := t.TempDir()
	cfg := auditPRSectionFixture(t, state, map[string]any{"pr_since": "2026-10-01T00:00:00Z"})
	auditPRFakeGh(t, auditPRListJSON(t,
		auditPRMergeEntry(12, "CRW-12: the first", "2026-10-05T00:00:00Z", "m12"),
		auditPRMergeEntry(11, "CRW-11: the second", "2026-10-04T00:00:00Z", "m11")), nil)
	auditPRFakeRelay(t,
		// The first target's child cannot be read; the second's can.
		map[string]string{"CRW-11": auditPRAssignmentJSON(t, "rel-11", "child-11")},
		map[string]string{"child-11": auditPRSettingsJSON(t, "inferhub/deepseek-v4.1-flash")}, nil)
	e, out, errOut := auditTestEnv(t)
	if code := auditPRRunWith(context.Background(), e, cfg, 9, true); code != 1 {
		t.Fatalf("a dry run with one unreadable target: exit %d, want 1: %q", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "#12") {
		t.Errorf("the skipped target is not named with its number: %q", errOut.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "pr-11") {
		t.Errorf("the dry run printed %q, want the second target only", out.String())
	}
}
