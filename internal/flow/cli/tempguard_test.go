package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// tempDecision is lane crw-1566-tmp-arch's check pattern (its D8): a direct temp location, an implicit one (os.TempDir, an empty
// MkdirTemp or CreateTemp parent) and a direct TMPDIR read. Until that repo-wide check lands, this test holds it over internal/flow
// (CRW-1570 U5, plan D6).
var tempDecision = regexp.MustCompile(`os\.TempDir\(\)|"/tmp|"/var/tmp|"/dev/shm|MkdirTemp\(""|CreateTemp\(""|\("TMPDIR"\)|"TMPDIR", "TMP"`)

// tempDecisionAllowed are the non-test internal/flow lines the pattern hits that decide no temp location, by file (relative to
// internal/flow) and exact line text, each with its reason. A new hit in the same file still fails, and an entry whose line is gone
// fails as stale.
var tempDecisionAllowed = map[string]string{
	"engine/tools.go\x00if t.sets(\"TMPDIR\") {": "looks up TMPDIR among the run's recorded repository tools (RepositoryTools.Env), " +
		"not in the process environment: the value there is the one the run recorded from tmploc at start (CRW-1570 U1)",
}

// No code under internal/flow decides a temporary location itself: every temporary directory or file a flow site makes comes from
// internal/tmploc (MkdirTemp, CreateTemp, Dir, Check, CheckDir), so a TMPDIR the function refuses is refused there and no other
// place is chosen.
func TestFlowTakesItsTempLocationFromTheOneFunction(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var hits []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		for i, line := range strings.Split(string(body), "\n") {
			if !tempDecision.MatchString(line) {
				continue
			}
			key := rel + "\x00" + strings.TrimSpace(line)
			if _, ok := tempDecisionAllowed[key]; ok {
				seen[key] = true
				continue
			}
			hits = append(hits, "internal/flow/"+rel+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(hits)
	for _, h := range hits {
		t.Errorf("a temp location decided outside internal/tmploc: %s", h)
	}
	for key := range tempDecisionAllowed {
		if !seen[key] {
			file, line, _ := strings.Cut(key, "\x00")
			t.Errorf("stale allowed line: internal/flow/%s no longer holds %q", file, line)
		}
	}
}
