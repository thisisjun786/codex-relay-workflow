package manage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// auditPRFakeGh answers the gh calls the pull request mode makes from canned data, so a test
// never runs gh, reaches GitHub or touches the network. It records every call it was given.
func auditPRFakeGh(t *testing.T, list string, diffs map[int]string) *[][]string {
	t.Helper()
	previous := auditPRGh
	calls := &[][]string{}
	auditPRGh = func(_ context.Context, args ...string) ([]byte, error) {
		*calls = append(*calls, args)
		if len(args) > 0 && args[0] == "pr" && len(args) > 1 && args[1] == "list" {
			return []byte(list), nil
		}
		if len(args) > 0 && args[0] == "pr" && len(args) > 1 && args[1] == "diff" {
			number, err := strconv.Atoi(args[2])
			if err != nil {
				return nil, err
			}
			body, ok := diffs[number]
			if !ok {
				return nil, errors.New("no such pull request " + args[2])
			}
			return []byte(body), nil
		}
		return nil, errors.New("unexpected gh arguments: " + strings.Join(args, " "))
	}
	t.Cleanup(func() { auditPRGh = previous })
	return calls
}

// auditPRFakeRelay answers the relay reads from canned maps, keyed by issue, task and
// relationship, and records the argument lists it was asked for.
func auditPRFakeRelay(t *testing.T, assignments, settings, criteria map[string]string) *[][]string {
	t.Helper()
	previous := auditPRRelay
	calls := &[][]string{}
	auditPRRelay = func(_ context.Context, _ *Env, _ *Config, args ...string) ([]byte, error) {
		*calls = append(*calls, args)
		if len(args) < 3 {
			return nil, errors.New("unexpected relay arguments: " + strings.Join(args, " "))
		}
		key := args[2]
		switch args[0] {
		case "assignment-find":
			body, ok := assignments[key]
			if !ok {
				return nil, errors.New("no assignment for " + key)
			}
			return []byte(body), nil
		case "settings-show":
			body, ok := settings[key]
			if !ok {
				return nil, errors.New("no settings for " + key)
			}
			return []byte(body), nil
		case "criteria-show":
			body, ok := criteria[key]
			if !ok {
				return nil, errors.New("no criteria for " + key)
			}
			return []byte(body), nil
		}
		return nil, errors.New("unexpected relay command: " + strings.Join(args, " "))
	}
	t.Cleanup(func() { auditPRRelay = previous })
	return calls
}

// auditPRFakeCheckout fakes the git reader the bundle builder uses and the fetch, so a test
// never runs git or reads a real repository.
func auditPRFakeCheckout(t *testing.T, merge string, files map[string]string) {
	t.Helper()
	previousGit, previousBlob := auditPkgGit, auditPkgBlob
	auditPkgGit = func(_ context.Context, repo string, args ...string) ([]byte, error) {
		if repo != "/checkout" {
			t.Errorf("git ran in %q, want the configured checkout", repo)
		}
		if len(args) == 0 {
			return nil, errors.New("git called with no arguments")
		}
		if args[0] == "fetch" {
			return nil, nil
		}
		return nil, errors.New("unexpected git arguments: " + strings.Join(args, " "))
	}
	auditPkgBlob = func(_ context.Context, repo, head, path string, w io.Writer) error {
		if repo != "/checkout" || head != merge {
			t.Errorf("the blob was read from %q at %q, want the configured checkout at %q", repo, head, merge)
		}
		body, ok := files[path]
		if !ok {
			return errors.New("no such path " + path)
		}
		_, err := io.WriteString(w, body)
		return err
	}
	t.Cleanup(func() { auditPkgGit, auditPkgBlob = previousGit, previousBlob })
}

// auditPRListJSON is a gh pull request list answer.
func auditPRListJSON(t *testing.T, entries ...map[string]any) string {
	t.Helper()
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// auditPRMergeEntry is one gh list entry.
func auditPRMergeEntry(number int, title, mergedAt, merge string) map[string]any {
	return map[string]any{
		"number": number, "title": title, "body": "body of " + title, "mergedAt": mergedAt,
		"mergeCommit": map[string]any{"oid": merge},
	}
}

// auditPRListEntryOf is one decoded gh list entry, for the selector tests that do not go
// through the gh seam.
func auditPRListEntryOf(number int, title, mergedAt, merge string) auditPRListEntry {
	entry := auditPRListEntry{Number: number, Title: title, MergedAt: mergedAt}
	entry.MergeCommit.OID = merge
	return entry
}

// auditPRAssignmentJSON is an assignment-find answer with one assignment.
func auditPRAssignmentJSON(t *testing.T, relationship, child string) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"issueKey": "CRW-1", "responsibleChild": child, "responsibleRelationship": relationship,
		"assignments": []map[string]any{{"relationshipId": relationship, "childTaskId": child, "state": "merged"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// auditPRSettingsJSON is a settings-show answer carrying one model.
func auditPRSettingsJSON(t *testing.T, model string) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{"task": "t", "settings": map[string]any{"model": model}, "usable": true, "deliverable": true})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// auditPRCriteriaJSON is a criteria-show answer.
func auditPRCriteriaJSON(t *testing.T, id, title string) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"relationshipId": "rel-1", "criteria": []map[string]any{{"id": id, "title": title, "required": true}}, "setDigest": "d",
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// auditPRSectionFixture is a configuration with a checkout section and the pull request
// keys the test names.
func auditPRSectionFixture(t *testing.T, state string, section map[string]any) *Config {
	t.Helper()
	cfg := &Config{StateDir: state, Repository: "example/repository", raw: map[string]json.RawMessage{}}
	data, err := json.Marshal(section)
	if err != nil {
		t.Fatal(err)
	}
	cfg.raw["audit"] = data
	cfg.raw["checkout"] = auditPkgJSON(t, map[string]any{"repository": "/checkout", "base_ref": "origin/dev"})
	return cfg
}

// auditPRPatch is a two-file patch: one modified file and one added one.
const auditPRPatch = "diff --git a/internal/a.go b/internal/a.go\n" +
	"index 111..222 100644\n" +
	"--- a/internal/a.go\n" +
	"+++ b/internal/a.go\n" +
	"@@ -1 +1 @@\n" +
	"-old\n" +
	"+new\n" +
	"diff --git a/docs/b.md b/docs/b.md\n" +
	"new file mode 100644\n" +
	"index 000..333\n" +
	"--- /dev/null\n" +
	"+++ b/docs/b.md\n" +
	"@@ -0,0 +1 @@\n" +
	"+hello\n"

// C1: the targets are the merged pull requests whose title carries an issue key, that are
// not already in the ledger as a pull request audit, and that merged at or after pr_since,
// newest first and cut to --max.
func TestAuditPRSelectsOnlyNewKeyedPullRequests(t *testing.T) {
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	pattern, err := auditPRPattern(auditPRSection{})
	if err != nil {
		t.Fatal(err)
	}
	entries := []auditPRListEntry{
		auditPRListEntryOf(12, "CRW-12: newest", "2026-10-05T00:00:00Z", "m12"),
		auditPRListEntryOf(11, "CRW-11: already audited", "2026-10-04T00:00:00Z", "m11"),
		auditPRListEntryOf(10, "no issue key here", "2026-10-04T00:00:00Z", "m10"),
		auditPRListEntryOf(9, "CRW-9: too old", "2026-09-30T00:00:00Z", "m9"),
		auditPRListEntryOf(8, "CRW-8: no merge commit", "2026-10-04T00:00:00Z", ""),
		auditPRListEntryOf(7, "CRW-7: older but new", "2026-10-03T00:00:00Z", "m7"),
	}
	audited := map[string]bool{auditPRSubject(11): true}
	targets, err := auditPRSelect(entries, pattern, since, audited, 9)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatalf("selected %d targets, want 2: %+v", len(targets), targets)
	}
	if targets[0].Number != 12 || targets[1].Number != 7 {
		t.Errorf("the targets are %d and %d, want the newest first", targets[0].Number, targets[1].Number)
	}
	if targets[0].Issue != "CRW-12" || targets[0].Merge != "m12" {
		t.Errorf("the first target carries issue %q and merge %q", targets[0].Issue, targets[0].Merge)
	}
	capped, err := auditPRSelect(entries, pattern, since, audited, 1)
	if err != nil || len(capped) != 1 || capped[0].Number != 12 {
		t.Errorf("--max 1 selected %+v (%v), want the newest target only", capped, err)
	}
	// A merge time that is not a timestamp is an error, not a silent skip: the pull request
	// would otherwise drop out of the audit with nothing said.
	broken := []auditPRListEntry{auditPRListEntryOf(12, "CRW-12: a change", "yesterday", "m12")}
	if _, err := auditPRSelect(broken, pattern, since, map[string]bool{}, 9); err == nil {
		t.Error("a merge time that is not a timestamp was accepted")
	}
}

// C1: a dry run writes the targets only, one line each, and grades nothing.
func TestAuditPRDryRunPrintsOnlyTheTargets(t *testing.T) {
	state := t.TempDir()
	cfg := auditPRSectionFixture(t, state, map[string]any{
		"pr_since": "2026-10-01T00:00:00Z",
		"pairs":    map[string]string{"deepseek": "if-deepseek"},
	})
	auditPRFakeGh(t, auditPRListJSON(t, auditPRMergeEntry(12, "CRW-12: a change", "2026-10-05T00:00:00Z", "m12")), nil)
	auditPRFakeRelay(t, map[string]string{"CRW-12": auditPRAssignmentJSON(t, "rel-1", "child-1")},
		map[string]string{"child-1": auditPRSettingsJSON(t, "inferhub/deepseek-v4.1-flash")}, nil)
	e, out, errOut := auditTestEnv(t)
	if code := auditPRRunWith(context.Background(), e, cfg, 9, true); code != 0 {
		t.Fatalf("audit pr --dry-run: exit %d %q", code, errOut.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("the dry run wrote %d lines, want one: %q", len(lines), out.String())
	}
	var entry auditPRDryRun
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Subject != "pr-12" || entry.Issue != "CRW-12" || entry.Head != "m12" || entry.Pair != "if-deepseek" || entry.Phase != auditPRPhaseLive {
		t.Errorf("the dry run entry is %+v", entry)
	}
	if strings.Contains(out.String(), "inferhub/deepseek-v4.1-flash") || strings.Contains(out.String(), "child-1") {
		t.Errorf("the dry run named the model or the child: %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(state, "audit", "bundles")); !os.IsNotExist(err) {
		t.Errorf("the dry run built a bundle: %v", err)
	}
}

// The since the issue fixes is required: a configuration without pr_since is refused by
// name at exit 2 and nothing is read from gh.
func TestAuditPRRefusesAnUnsetSince(t *testing.T) {
	cfg := auditPRSectionFixture(t, t.TempDir(), map[string]any{"pairs": map[string]string{"x": "y"}})
	calls := auditPRFakeGh(t, "[]", nil)
	e, _, errOut := auditTestEnv(t)
	if code := auditPRRunWith(context.Background(), e, cfg, 9, false); code != usageExit {
		t.Fatalf("audit pr without pr_since: exit %d, want %d", code, usageExit)
	}
	if !strings.Contains(errOut.String(), "audit_since_unset") {
		t.Errorf("the refusal is %q, want audit_since_unset", errOut.String())
	}
	if len(*calls) != 0 {
		t.Errorf("gh was called %v before the refusal", *calls)
	}
}

// C2: a built bundle carries no model, pair or child task id, even when the description and
// the patch name them, because the configured scrub strings replace them everywhere.
func TestAuditPRBundleCarriesNoBlindMetadata(t *testing.T) {
	state := t.TempDir()
	section := map[string]any{
		"pr_since": "2026-10-01T00:00:00Z",
		"pairs":    map[string]string{"deepseek": "if-deepseek"},
		"scrub":    []string{"inferhub/deepseek-v4.1-flash", "if-deepseek", "child-1", "deepseek"},
	}
	cfg := auditPRSectionFixture(t, state, section)
	patch := "diff --git a/internal/a.go b/internal/a.go\n" +
		"--- a/internal/a.go\n" +
		"+++ b/internal/a.go\n" +
		"@@ -1 +1 @@\n" +
		"-the child-1 run used inferhub/deepseek-v4.1-flash for the if-deepseek pair\n" +
		"+the same, renamed\n"
	auditPRFakeGh(t, auditPRListJSON(t, auditPRMergeEntry(12, "CRW-12: a change", "2026-10-05T00:00:00Z", "m12")), map[int]string{12: patch})
	auditPRFakeRelay(t, map[string]string{"CRW-12": auditPRAssignmentJSON(t, "rel-1", "child-1")},
		map[string]string{"child-1": auditPRSettingsJSON(t, "inferhub/deepseek-v4.1-flash")},
		map[string]string{"rel-1": auditPRCriteriaJSON(t, "c1", "the change works")})
	auditPRFakeCheckout(t, "m12", map[string]string{"internal/a.go": "package a\n// child-1 used inferhub/deepseek-v4.1-flash\n"})
	grader := auditFake(t, "json", auditJSONClean)
	cfg.raw["audit"] = auditPkgJSON(t, map[string]any{
		"pr_since": "2026-10-01T00:00:00Z",
		"pairs":    map[string]string{"deepseek": "if-deepseek"},
		"scrub":    []string{"inferhub/deepseek-v4.1-flash", "if-deepseek", "child-1", "deepseek"},
		"grader":   grader,
	})
	e, out, errOut := auditTestEnv(t)
	if code := auditPRRunWith(context.Background(), e, cfg, 9, false); code != 0 {
		t.Fatalf("audit pr: exit %d %q", code, errOut.String())
	}
	dir := filepath.Join(state, "audit", "bundles", "pr-12")
	// The whole bundle is walked, not a hand-picked list of names: the criterion is that no
	// file it holds carries the metadata, so a file this issue adds later cannot escape the
	// check by not being listed here.
	seen := map[string]bool{}
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		seen[rel] = true
		lower := strings.ToLower(string(data))
		for _, needle := range []string{"deepseek", "inferhub", "if-deepseek", "child-1", "model"} {
			if strings.Contains(lower, needle) {
				t.Errorf("%s carries %q:\n%s", path, needle, data)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The five files this issue fixes are the ones the walk must have covered; the grader's
	// own prompt.md and grade.json are written by the engine, not by this mode.
	for _, name := range []string{auditBundleFile, auditPRTaskFile, auditPRCriteriaFile, auditPRDiffFile, filepath.Join(auditPRFilesDir, "internal", "a.go")} {
		if !seen[name] {
			t.Errorf("the bundle does not hold %q; it holds %v", name, seen)
		}
	}
	// The metadata the grader is blind to still reaches the ledger row.
	rows, err := auditReportLedger(e, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Pair != "if-deepseek" || rows[0].Subject != "pr-12" || rows[0].Mode != auditModePR {
		t.Errorf("the ledger row is %+v, want the pair and the subject the caller attached", rows)
	}
	if rows[0].Status != auditStatusOK {
		t.Errorf("the graded status is %q, want ok", rows[0].Status)
	}
	_ = out
}

// C2: the scrub replaces the configured strings, longest first, so a shorter string cannot
// cut into a longer one and leave a fragment behind.
func TestAuditPRScrubsTheConfiguredStrings(t *testing.T) {
	got := string(auditPRScrub([]byte("the if-deepseek pair ran inferhub/deepseek-v4.1-flash and deepseek"),
		[]string{"deepseek", "if-deepseek", "inferhub/deepseek-v4.1-flash"}))
	if strings.Contains(got, "deepseek") || strings.Contains(got, "if-deepseek") {
		t.Errorf("the scrub left a configured string behind: %q", got)
	}
	want := "the [redacted] pair ran [redacted] and [redacted]"
	if got != want {
		t.Errorf("the scrub produced %q, want %q", got, want)
	}
	if got := string(auditPRScrub([]byte("nothing to hide"), nil)); got != "nothing to hide" {
		t.Errorf("an empty scrub list changed the text: %q", got)
	}
}

// C3: a pull request whose issue has no assignment still gets a bundle, and bundle.json
// records that the relationship and the criteria were unavailable.
func TestAuditPRBundleWithoutRelationshipOrCriteria(t *testing.T) {
	state := t.TempDir()
	cfg := auditPRSectionFixture(t, state, map[string]any{"pr_since": "2026-10-01T00:00:00Z"})
	cfg.raw["audit"] = auditPkgJSON(t, map[string]any{
		"pr_since": "2026-10-01T00:00:00Z", "grader": auditFake(t, "json", auditJSONClean),
	})
	patch := "diff --git a/internal/a.go b/internal/a.go\n--- a/internal/a.go\n+++ b/internal/a.go\n@@ -1 +1 @@\n-old\n+new\n"
	auditPRFakeGh(t, auditPRListJSON(t, auditPRMergeEntry(12, "CRW-12: a change", "2026-10-05T00:00:00Z", "m12")), map[int]string{12: patch})
	relayCalls := auditPRFakeRelay(t, map[string]string{"CRW-12": `{"issueKey":"CRW-12","responsibleChild":null,"responsibleRelationship":null,"assignments":[]}`}, nil, nil)
	auditPRFakeCheckout(t, "m12", map[string]string{"internal/a.go": "package a\n"})
	e, _, errOut := auditTestEnv(t)
	if code := auditPRRunWith(context.Background(), e, cfg, 9, false); code != 0 {
		t.Fatalf("audit pr: exit %d %q", code, errOut.String())
	}
	data, err := os.ReadFile(filepath.Join(state, "audit", "bundles", "pr-12", auditBundleFile))
	if err != nil {
		t.Fatal(err)
	}
	var bundle struct {
		Mode                    string `json:"mode"`
		Subject                 string `json:"subject"`
		Head                    string `json:"head"`
		Issue                   string `json:"issue"`
		CriteriaUnavailable     bool   `json:"criteria_unavailable"`
		RelationshipUnavailable bool   `json:"relationship_unavailable"`
	}
	if err := json.Unmarshal(data, &bundle); err != nil {
		t.Fatal(err)
	}
	if bundle.Mode != auditModePR || bundle.Subject != "pr-12" || bundle.Head != "m12" || bundle.Issue != "CRW-12" {
		t.Errorf("the bundle is %+v", bundle)
	}
	if !bundle.CriteriaUnavailable || !bundle.RelationshipUnavailable {
		t.Errorf("the bundle does not record the missing relationship and criteria: %+v", bundle)
	}
	for _, call := range *relayCalls {
		if call[0] == "criteria-show" {
			t.Errorf("the criteria were read for a pull request with no relationship: %v", call)
		}
	}
	rows, err := auditReportLedger(e, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Pair != auditPRPairUnknown {
		t.Errorf("the ledger row is %+v, want the unknown pair", rows)
	}
}

// The pair is the longest configured key the model contains, and an unknown model is the
// unknown pair; the phase is the greatest configured start at or before the merge time.
func TestAuditPRPairAndPhaseRules(t *testing.T) {
	pairs := map[string]string{"sol": "sol", "gpt-6-sol": "sol-xhigh", "deepseek": "if-deepseek"}
	for _, tc := range []struct{ model, want string }{
		{"gpt-6-sol", "sol-xhigh"},
		{"inferhub/deepseek-v4.1-flash", "if-deepseek"},
		{"anthropic/claude", auditPRPairUnknown},
		{"", auditPRPairUnknown},
	} {
		if got := auditPRPairOf(pairs, tc.model); got != tc.want {
			t.Errorf("pair of %q = %q, want %q", tc.model, got, tc.want)
		}
	}
	section := auditPRSection{Phases: map[string]string{
		"2026-10-05T06:22:00+09:00": "live",
		"2026-10-01T00:00:00Z":      "baseline",
	}}
	for _, tc := range []struct {
		merged string
		want   string
	}{
		{"2026-10-06T00:00:00Z", "live"},
		{"2026-10-03T00:00:00Z", "baseline"},
		{"2026-09-30T00:00:00Z", auditPRPhaseLive},
	} {
		merged, err := time.Parse(time.RFC3339, tc.merged)
		if err != nil {
			t.Fatal(err)
		}
		got, err := auditPRPhaseOf(section, merged)
		if err != nil || got != tc.want {
			t.Errorf("phase of %s = %q (%v), want %q", tc.merged, got, err, tc.want)
		}
	}
	if _, err := auditPRPhaseOf(auditPRSection{Phases: map[string]string{"yesterday": "live"}}, time.Now()); err == nil {
		t.Error("a phase start that is not a timestamp was accepted")
	}
	if _, err := auditPRPhaseOf(auditPRSection{Phases: map[string]string{"2026-10-01T00:00:00Z": ""}}, time.Now()); err == nil {
		t.Error("a phase with no name was accepted")
	}
}

// The changed-file list is read from the patch's own headers: a modified file, an added
// file, a renamed file's target and a binary block's path, and never a deletion.
func TestAuditPRDiffPathsReadsThePatchHeaders(t *testing.T) {
	patch := "diff --git a/internal/a.go b/internal/a.go\n--- a/internal/a.go\n+++ b/internal/a.go\n@@ -1 +1 @@\n-old\n+new\n" +
		"diff --git a/docs/gone.md b/docs/gone.md\n--- a/docs/gone.md\n+++ /dev/null\n@@ -1 +0,0 @@\n-gone\n" +
		"diff --git a/old.go b/new.go\nsimilarity index 90%\nrename from old.go\nrename to new.go\n" +
		"diff --git a/img.png b/img.png\nindex 111..222 100644\nBinary files a/img.png and b/img.png differ\n" +
		"diff --git \"a/with space.txt\" \"b/with space.txt\"\n--- \"a/with space.txt\"\n+++ \"b/with space.txt\"\n@@ -1 +1 @@\n-a\n+b\n"
	got := auditPRDiffPaths([]byte(patch))
	want := []string{"internal/a.go", "new.go", "img.png", "with space.txt"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the paths are %v, want %v", got, want)
	}
	if paths := auditPRDiffPaths([]byte(auditPRPatch)); strings.Join(paths, ",") != "internal/a.go,docs/b.md" {
		t.Errorf("the two-file patch gave %v", paths)
	}
	// A hunk's content is data. A patch that adds a line whose own text begins with "++ "
	// renders it as "+++ ...", and reading that as a path would send the bundle to a file
	// that does not exist.
	withHunk := "diff --git a/internal/a.go b/internal/a.go\n--- a/internal/a.go\n+++ b/internal/a.go\n@@ -1,2 +1,3 @@\n context\n+++ this line is content, not a header\n"
	if paths := auditPRDiffPaths([]byte(withHunk)); strings.Join(paths, ",") != "internal/a.go" {
		t.Errorf("a hunk whose content starts with pluses gave %v, want the real path only", paths)
	}
}

// The fetch brings the branch target discovery queries, not the checkout section's
// configurable base_ref, so a checkout configured for another branch still has the dev merge
// objects the bundle builder reads.
func TestAuditPRFetchUsesTheIntegrationBranch(t *testing.T) {
	var calls [][]string
	previous := auditPkgGit
	auditPkgGit = func(_ context.Context, repo string, args ...string) ([]byte, error) {
		if repo != "/checkout" {
			t.Errorf("git ran in %q", repo)
		}
		calls = append(calls, args)
		return nil, nil
	}
	t.Cleanup(func() { auditPkgGit = previous })
	if err := auditPRFetch(context.Background(), auditPkgCheckout{Repository: "/checkout", BaseRef: "origin/release"}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || strings.Join(calls[0], " ") != "fetch origin dev" {
		t.Errorf("the fetch was %v, want the integration branch", calls)
	}
	if err := auditPRFetch(context.Background(), auditPkgCheckout{Repository: "/checkout"}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || strings.Join(calls[1], " ") != "fetch origin dev" {
		t.Errorf("a checkout with no base_ref fetched %v", calls[1:])
	}
}

// The prompt the pr mode writes names the files its bundle actually holds, and the shared
// template no longer names a layout neither mode has.
func TestAuditPRPromptNamesTheBundleItWrites(t *testing.T) {
	prompt := auditPrompt(&auditBundle{Mode: auditModePR})
	for _, needle := range []string{auditPRDiffFile, auditPRTaskFile, auditPRFilesDir} {
		if !strings.Contains(prompt, needle) {
			t.Errorf("the pr prompt does not name %q", needle)
		}
	}
	for _, stale := range []string{"candidate/", "criteria.md", "`inputs/`"} {
		if strings.Contains(prompt, stale) {
			t.Errorf("the pr prompt names %q, which the bundle does not hold:\n%s", stale, prompt)
		}
	}
	if unavailable := auditPrompt(&auditBundle{Mode: auditModePR, CriteriaUnavailable: true}); strings.Contains(unavailable, "issue text") {
		t.Errorf("a criteria_unavailable pr bundle is sent to an issue text it does not hold:\n%s", unavailable)
	}
}

// A path that would leave the bundle is refused before anything is written, and a file the
// merge commit does not hold is an error rather than a silently empty bundle file.
func TestAuditPRRefusesAPathOutsideTheBundle(t *testing.T) {
	state := t.TempDir()
	cfg := auditPRSectionFixture(t, state, map[string]any{"pr_since": "2026-10-01T00:00:00Z"})
	patch := "diff --git a/../../escape.go b/../../escape.go\n--- a/../../escape.go\n+++ b/../../escape.go\n@@ -1 +1 @@\n-a\n+b\n"
	auditPRFakeCheckout(t, "m12", map[string]string{})
	e, _, _ := auditTestEnv(t)
	co, err := auditPkgCheckoutOf(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = auditPRBuild(context.Background(), e, cfg, auditPRSection{}, co, auditPRSource{
		Target: auditPRTarget{Number: 12, Issue: "CRW-12", Merge: "m12"}, Patch: []byte(patch),
	})
	if err == nil {
		t.Fatal("a path leaving the bundle was accepted")
	}
	if _, err := os.Stat(filepath.Join(state, "audit", "bundles", "pr-12", auditBundleFile)); !os.IsNotExist(err) {
		t.Errorf("a bundle was completed for a refused path: %v", err)
	}
}

// The pr command's flags: the help flags print the usage, an unknown option, a missing
// value, a stray positional and a non-numeric cap are usage errors at exit 2.
func TestAuditPRCommandUsageAndArgumentErrors(t *testing.T) {
	e, out, errOut := auditTestEnv(t)
	for _, arg := range []string{"-h", "--help", "help"} {
		out.Reset()
		errOut.Reset()
		if code := auditPRRun(context.Background(), e, []string{arg}); code != 0 || errOut.Len() != 0 {
			t.Errorf("%s: exit %d %q %q", arg, code, out.String(), errOut.String())
		}
		if !strings.Contains(out.String(), auditPRUsage) {
			t.Errorf("%s: the usage is missing: %q", arg, out.String())
		}
	}
	for _, args := range [][]string{{"nope"}, {"--max"}, {"--nope", "1"}, {"--max", "x"}, {"--max=-1"}} {
		out.Reset()
		errOut.Reset()
		if code := auditPRRun(context.Background(), e, args); code != usageExit {
			t.Errorf("%v: exit %d, want %d", args, code, usageExit)
		}
		if !strings.Contains(errOut.String(), auditPRUsage) {
			t.Errorf("%v: the usage is missing: %q", args, errOut.String())
		}
	}
	if max, dry, _, err := auditPRParseArgs([]string{"--max=3", "--dry-run"}); err != nil || max != 3 || !dry {
		t.Errorf("--max=3 --dry-run parsed as %d %v %v", max, dry, err)
	}
}

// A gh failure is an error and no report is written, so a run that could not read its
// targets never looks like a run that found none.
func TestAuditPRRefusesAFailedList(t *testing.T) {
	state := t.TempDir()
	cfg := auditPRSectionFixture(t, state, map[string]any{"pr_since": "2026-10-01T00:00:00Z"})
	previous := auditPRGh
	auditPRGh = func(_ context.Context, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("gh %s: exit status 1", strings.Join(args, " "))
	}
	t.Cleanup(func() { auditPRGh = previous })
	e, _, errOut := auditTestEnv(t)
	if code := auditPRRunWith(context.Background(), e, cfg, 9, false); code != 1 {
		t.Fatalf("a failed list: exit %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "gh pr list") {
		t.Errorf("the error is %q", errOut.String())
	}
	if _, err := os.Stat(filepath.Join(state, "audit", auditReportFile)); !os.IsNotExist(err) {
		t.Errorf("a report was written after a failed list: %v", err)
	}
}

// The bundle directory name is the ledger row's subject, so the audited set the selector
// consults is the same name a previous run wrote.
func TestAuditPRSubjectMatchesTheBundleDirectory(t *testing.T) {
	if got := auditPRSubject(123); got != "pr-123" {
		t.Errorf("auditPRSubject(123) = %q", got)
	}
	rows := []auditLedgerRow{{Mode: auditModePR, Subject: "pr-123"}, {Mode: auditModePackage, Subject: "pr-124"}}
	audited := auditPRAudited(rows)
	if !audited["pr-123"] || audited["pr-124"] {
		t.Errorf("the audited set is %v", audited)
	}
}

// The pull request section is read from the configuration, and an issue_pattern that is not
// a pattern is refused rather than silently matching nothing.
func TestAuditPRSectionAndPattern(t *testing.T) {
	cfg := auditPRSectionFixture(t, t.TempDir(), map[string]any{
		"pr_since": "2026-10-01T00:00:00Z", "issue_pattern": "CRW-\\d+",
		"pairs": map[string]string{"a": "b"}, "phases": map[string]string{"2026-10-01T00:00:00Z": "live"},
		"scrub": []string{"x"},
	})
	section, err := auditPRSectionOf(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if section.PRSince != "2026-10-01T00:00:00Z" || section.IssuePattern != "CRW-\\d+" || section.Pairs["a"] != "b" || len(section.Scrub) != 1 {
		t.Errorf("the section is %+v", section)
	}
	pattern, err := auditPRPattern(section)
	if err != nil {
		t.Fatal(err)
	}
	if got := pattern.FindString("a CRW-42 change"); got != "CRW-42" {
		t.Errorf("the configured pattern matched %q", got)
	}
	if _, err := auditPRPattern(auditPRSection{IssuePattern: "("}); err == nil {
		t.Error("an invalid issue_pattern was accepted")
	}
}

// The gh list carries the repository, the base, the since and the field list the issue
// fixes, and the diff call names the pull request.
func TestAuditPRGhCallShape(t *testing.T) {
	calls := auditPRFakeGh(t, "[]", map[int]string{12: auditPRPatch})
	cfg := &Config{Repository: "example/repository"}
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if _, err := auditPRList(context.Background(), nil, cfg, since); err != nil {
		t.Fatal(err)
	}
	if _, err := auditPRDiff(context.Background(), cfg, 12); err != nil {
		t.Fatal(err)
	}
	// The whole argument list is pinned, not a substring of it: a duplicated token or a
	// dropped flag is a gh usage error at run time, and a containment check would not see it.
	want := []string{"pr", "list", "--repo", "example/repository", "--state", "merged", "--base", "dev",
		"--search", "merged:>=2026-10-01T00:00:00Z", "--json", "number,title,body,mergeCommit,mergedAt", "--limit", "200"}
	if got := strings.Join((*calls)[0], " "); got != strings.Join(want, " ") {
		t.Errorf("the list call is %q, want %q", got, strings.Join(want, " "))
	}
	withoutRepo := auditPRFakeGh(t, "[]", map[int]string{12: auditPRPatch})
	if _, err := auditPRList(context.Background(), nil, &Config{}, since); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join((*withoutRepo)[0], " "); got != "pr list --state merged --base dev --search merged:>=2026-10-01T00:00:00Z --json number,title,body,mergeCommit,mergedAt --limit 200" {
		t.Errorf("a configuration without a repository gave %q", got)
	}
	if diff := strings.Join((*calls)[1], " "); diff != "pr diff 12 --repo example/repository" {
		t.Errorf("the diff call is %q", diff)
	}
}

// The relay reads are the ones the issue fixes, in order, and a relationship's criteria are
// read once.
func TestAuditPRRelayCallShape(t *testing.T) {
	calls := auditPRFakeRelay(t,
		map[string]string{"CRW-12": auditPRAssignmentJSON(t, "rel-1", "child-1")},
		map[string]string{"child-1": auditPRSettingsJSON(t, "inferhub/deepseek-v4.1-flash")},
		map[string]string{"rel-1": auditPRCriteriaJSON(t, "c1", "it works")})
	e, _, _ := auditTestEnv(t)
	child, err := auditPRChildOf(context.Background(), e, &Config{}, "CRW-12")
	if err != nil {
		t.Fatal(err)
	}
	if child.Relationship != "rel-1" || child.Task != "child-1" || child.Model != "inferhub/deepseek-v4.1-flash" {
		t.Errorf("the child is %+v", child)
	}
	criteria, unavailable, err := auditPRCriteria(context.Background(), e, &Config{}, child.Relationship)
	if err != nil {
		t.Fatal(err)
	}
	if unavailable || len(criteria) != 1 || criteria[0]["id"] != "c1" {
		t.Errorf("the criteria are %v (unavailable %v)", criteria, unavailable)
	}
	want := [][]string{
		{"assignment-find", "--issue", "CRW-12"},
		{"settings-show", "--task", "child-1"},
		{"criteria-show", "--relationship", "rel-1"},
	}
	if len(*calls) != len(want) {
		t.Fatalf("the relay saw %v, want %v", *calls, want)
	}
	for i := range want {
		if strings.Join((*calls)[i], " ") != strings.Join(want[i], " ") {
			t.Errorf("relay call %d is %v, want %v", i, (*calls)[i], want[i])
		}
	}
	// A relationship with no registered criteria leaves them unavailable, and the bundle is
	// still built; a relationship the relay could not answer at all is an error.
	if _, unavailable, err := auditPRCriteria(context.Background(), e, &Config{}, ""); err != nil || !unavailable {
		t.Error("an empty relationship did not report the criteria unavailable")
	}
	if _, _, err := auditPRCriteria(context.Background(), e, &Config{}, "rel-none"); err == nil {
		t.Error("a criteria read the relay refused was reported as no criteria")
	}
}

// A model the relay never recorded leaves the pair unknown and the bundle is still built.
func TestAuditPRChildWithoutAModel(t *testing.T) {
	auditPRFakeRelay(t,
		map[string]string{"CRW-12": auditPRAssignmentJSON(t, "rel-1", "child-1")},
		map[string]string{"child-1": `{"task":"child-1","settings":null,"usable":false}`}, nil)
	e, _, _ := auditTestEnv(t)
	child, err := auditPRChildOf(context.Background(), e, &Config{}, "CRW-12")
	if err != nil {
		t.Fatal(err)
	}
	if child.Model != "" || auditPRPairOf(map[string]string{"deepseek": "if-deepseek"}, child.Model) != auditPRPairUnknown {
		t.Errorf("a child without a model is %+v", child)
	}
}

// A relay failure is an error, never an empty child, so a run that could not ask the relay
// does not silently grade a pull request it knows nothing about.
func TestAuditPRChildRefusesARelayFailure(t *testing.T) {
	previous := auditPRRelay
	auditPRRelay = func(_ context.Context, _ *Env, _ *Config, args ...string) ([]byte, error) {
		return nil, errors.New("relay_state_unresolved")
	}
	t.Cleanup(func() { auditPRRelay = previous })
	e, _, _ := auditTestEnv(t)
	if _, err := auditPRChildOf(context.Background(), e, &Config{}, "CRW-12"); err == nil {
		t.Fatal("a relay failure was accepted as an empty child")
	}
}

// The bundle is assembled with the names the issue fixes, and bundle.json is written last,
// so a bundle that failed part way leaves no complete-looking bundle behind.
func TestAuditPRBuildWritesTheFixedLayout(t *testing.T) {
	state := t.TempDir()
	cfg := auditPRSectionFixture(t, state, map[string]any{"pr_since": "2026-10-01T00:00:00Z"})
	auditPRFakeCheckout(t, "m12", map[string]string{"internal/a.go": "package a\n", "docs/b.md": "hello\n"})
	e, _, _ := auditTestEnv(t)
	co, err := auditPkgCheckoutOf(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := auditPRBuild(context.Background(), e, cfg, auditPRSection{}, co, auditPRSource{
		Target:   auditPRTarget{Number: 12, Issue: "CRW-12", Title: "CRW-12: a change", Body: "why", Merge: "m12"},
		Patch:    []byte(auditPRPatch),
		Criteria: []map[string]any{{"id": "c1", "title": "it works"}}, Relationship: "rel-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(state, "audit", "bundles", "pr-12") {
		t.Errorf("the bundle is at %q", dir)
	}
	for _, name := range []string{auditBundleFile, auditPRTaskFile, auditPRCriteriaFile, auditPRDiffFile,
		filepath.Join(auditPRFilesDir, "internal", "a.go"), filepath.Join(auditPRFilesDir, "docs", "b.md")} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	task, err := os.ReadFile(filepath.Join(dir, auditPRTaskFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"CRW-12", "m12", "why", "c1: it works", auditPRDiffFile, auditPRFilesDir} {
		if !strings.Contains(string(task), needle) {
			t.Errorf("task.md does not carry %q:\n%s", needle, task)
		}
	}
	patch, err := os.ReadFile(filepath.Join(dir, auditPRDiffFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(patch, []byte(auditPRPatch)) {
		t.Error("diff.patch is not the patch gh answered with")
	}
}

// A pull request already in the ledger as a pull request audit is not selected again, and a
// package row for the same subject does not hide it.
func TestAuditPRSelectIgnoresAPackageRow(t *testing.T) {
	pattern := regexp.MustCompile(auditPRDefaultPattern)
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	entries := []auditPRListEntry{auditPRListEntryOf(12, "CRW-12: a change", "2026-10-05T00:00:00Z", "m12")}
	if got, err := auditPRSelect(entries, pattern, since, auditPRAudited([]auditLedgerRow{{Mode: auditModePackage, Subject: "pr-12"}}), 9); err != nil || len(got) != 1 {
		t.Errorf("a package row hid the pull request: %v (%v)", got, err)
	}
	if got, err := auditPRSelect(entries, pattern, since, auditPRAudited([]auditLedgerRow{{Mode: auditModePR, Subject: "pr-12"}}), 9); err != nil || len(got) != 0 {
		t.Errorf("an audited pull request was selected again: %v (%v)", got, err)
	}
}

// The grader is run through the existing engine, so a bundle the pr mode built carries the
// ledger row the report reads: mode pr, the subject pr-<N> and the caller's pair and phase.
func TestAuditPRGradesThroughTheEngine(t *testing.T) {
	state := t.TempDir()
	grader := auditFake(t, "json", auditJSONClean)
	cfg := auditPRSectionFixture(t, state, map[string]any{
		"pr_since": "2026-10-01T00:00:00Z", "grader": grader,
		"pairs":  map[string]string{"deepseek": "if-deepseek"},
		"phases": map[string]string{"2026-10-01T00:00:00Z": "live"},
	})
	patch := "diff --git a/internal/a.go b/internal/a.go\n--- a/internal/a.go\n+++ b/internal/a.go\n@@ -1 +1 @@\n-old\n+new\n"
	auditPRFakeGh(t, auditPRListJSON(t, auditPRMergeEntry(12, "CRW-12: a change", "2026-10-05T00:00:00Z", "m12")), map[int]string{12: patch})
	auditPRFakeRelay(t, map[string]string{"CRW-12": auditPRAssignmentJSON(t, "rel-1", "child-1")},
		map[string]string{"child-1": auditPRSettingsJSON(t, "inferhub/deepseek-v4.1-flash")},
		map[string]string{"rel-1": auditPRCriteriaJSON(t, "c1", "it works")})
	auditPRFakeCheckout(t, "m12", map[string]string{"internal/a.go": "package a\n"})
	e, _, errOut := auditTestEnv(t)
	if code := auditPRRunWith(context.Background(), e, cfg, 9, false); code != 0 {
		t.Fatalf("audit pr: exit %d %q", code, errOut.String())
	}
	rows, err := auditReportLedger(e, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("the ledger has %d rows, want 1", len(rows))
	}
	row := rows[0]
	if row.Mode != auditModePR || row.Subject != "pr-12" || row.Issue != "CRW-12" || row.Head != "m12" || row.Pair != "if-deepseek" || row.Phase != "live" {
		t.Errorf("the ledger row is %+v", row)
	}
	if row.Status != auditStatusOK || row.Score == nil || *row.Score != 9 {
		t.Errorf("the graded row is %+v", row)
	}
	report, err := os.ReadFile(auditReportPath(e, cfg))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(report), "| if-deepseek | live | 1 | 9.00 | 0 | 0 |") {
		t.Errorf("the report does not carry the run:\n%s", report)
	}
}

// A run that finds no target still writes the report from the ledger, so an empty run is
// visible rather than silent.
func TestAuditPRWithNoTargetsWritesTheReport(t *testing.T) {
	state := t.TempDir()
	cfg := auditPRSectionFixture(t, state, map[string]any{"pr_since": "2026-10-01T00:00:00Z"})
	auditPRFakeGh(t, "[]", nil)
	e, _, errOut := auditTestEnv(t)
	if code := auditPRRunWith(context.Background(), e, cfg, 9, false); code != 0 {
		t.Fatalf("audit pr with no targets: exit %d %q", code, errOut.String())
	}
	data, err := os.ReadFile(filepath.Join(state, "audit", auditReportFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "| pair | phase |") {
		t.Errorf("the empty report has no table:\n%s", data)
	}
}
