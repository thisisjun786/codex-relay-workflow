package manage

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
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
func TestAuditPRReview752CriteriaScrubbedBeforeEncoding(t *testing.T) {
	for _, name := range []string{"Pair&A", "Pair<A>", "Pair>A"} {
		t.Run(name, func(t *testing.T) {
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
			// The escaped form is what the defect leaves behind, because the byte replacement
			// runs on text the encoder has already escaped.
			if escaped := auditPRReview752Escaped(name); strings.Contains(string(data), escaped) {
				t.Errorf("%s carries the escaped name %q", auditPRCriteriaFile, escaped)
			}
			if !strings.Contains(string(data), auditPRRedacted) {
				t.Errorf("%s carries no redaction:\n%s", auditPRCriteriaFile, data)
			}
		})
	}
}

// C2: the changed paths come from git, so a path that carries a space reaches the bundle
// unchanged. The patch's own header is ambiguous there: git puts a tab after a +++ line whose
// path holds a space, and a binary block names no path in its body at all.
func TestAuditPRReview752PathsWithSpaces(t *testing.T) {
	state := t.TempDir()
	cfg := auditPRSectionFixture(t, state, map[string]any{
		"pr_since": "2026-10-01T00:00:00Z", "grader": auditFake(t, "json", auditJSONClean),
	})
	patch := "diff --git a/docs/with space.txt b/docs/with space.txt\n" +
		"index 111..222 100644\n" +
		"--- a/docs/with space.txt\t\n" +
		"+++ b/docs/with space.txt\t\n" +
		"@@ -1 +1 @@\n" +
		"-old\n" +
		"+new\n" +
		"diff --git a/img/with space.png b/img/with space.png\n" +
		"index 111..222 100644\n" +
		"Binary files a/img/with space.png and b/img/with space.png differ\n"
	auditPRFakeGh(t, auditPRListJSON(t, auditPRMergeEntry(12, "CRW-12: a change", "2026-10-05T00:00:00Z", "m12")),
		map[int]string{12: patch})
	auditPRFakeRelay(t, map[string]string{"CRW-12": auditPRAssignmentJSON(t, "rel-1", "child-1")},
		map[string]string{"child-1": auditPRSettingsJSON(t, "inferhub/deepseek-v4.1-flash")},
		map[string]string{"rel-1": auditPRCriteriaJSON(t, "c1", "it works")})
	auditPRReview752Checkout(t,
		map[string][]byte{"m12": auditPRReview752Listing("M", "docs/with space.txt", "M", "img/with space.png")},
		map[string]map[string]string{"m12": {"docs/with space.txt": "text\n", "img/with space.png": "\x00\x01binary\n"}})
	e, _, errOut := auditTestEnv(t)
	if code := auditPRRunWith(context.Background(), e, cfg, 9, false); code != 0 {
		t.Fatalf("audit pr: exit %d %q", code, errOut.String())
	}
	dir := filepath.Join(state, "audit", "bundles", "pr-12", auditPRFilesDir)
	for path, want := range map[string]string{
		"docs/with space.txt": "text\n",
		"img/with space.png":  "\x00\x01binary\n",
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

// The listing git answers with is read positionally: a status, then its path, or, for a
// rename or a copy, the old path and then the new one. A deletion contributes nothing, and a
// record that does not fit the shape is refused rather than half-read.
func TestAuditPRReview752NameStatusPaths(t *testing.T) {
	listing := auditPRReview752Listing("M", "internal/a.go", "A", "docs/new.md", "D", "docs/gone.md",
		"R100", "old.go", "new.go", "C100", "src/from.go", "src/to.go", "T", "link",
		"M", "docs/with space.txt", "M", "img/with space.png")
	got, err := auditPRNameStatusPaths(listing)
	if err != nil {
		t.Fatal(err)
	}
	want := "internal/a.go,docs/new.md,new.go,src/to.go,link,docs/with space.txt,img/with space.png"
	if strings.Join(got, ",") != want {
		t.Errorf("the paths are %v, want %s", got, want)
	}
	if paths, err := auditPRNameStatusPaths(nil); err != nil || len(paths) != 0 {
		t.Errorf("an empty listing gave %v (%v)", paths, err)
	}
	// A record that does not fit is refused: a listing read half way would build a bundle
	// that looks complete while a changed file is missing from it.
	for _, broken := range []string{"M\x00", "R100\x00old.go\x00", "internal/a.go\x00M\x00x\x00", "Z\x00x\x00"} {
		if _, err := auditPRNameStatusPaths([]byte(broken)); err == nil {
			t.Errorf("the listing %q was accepted", broken)
		}
	}
}
