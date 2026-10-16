//go:build dev

package ci

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// CRW-1579 (D8): every place the program decides a temporary location goes through internal/tmploc,
// the one place. The check greps every tracked file for the direct deciders: os.TempDir(), a literal
// /tmp, /var/tmp or /dev/shm, MkdirTemp or CreateTemp with an empty parent (which is TMPDIR, decided
// inside os), and the direct reads of TMPDIR. Every hit must be an exception with its reason or a
// pending site (still to move, with the unit that moves it), and every exception and pending entry
// must still match a hit, so a stale entry fails too.
//
// The pattern is written with bracket expressions so that its own text is no hit. This file's
// tables quote the lines they allow; it is test code (exception E2).
const tmpDecisionPattern = `os[.]TempDir[(][)]|"/[t]mp|"/var/[t]mp|"/dev/[s]hm|MkdirTemp[(]""|CreateTemp[(]""|[(]"TMPDIR"[)]|"TMPDIR", "TM[P]"`

// tmpParentPattern finds the os.MkdirTemp and os.CreateTemp calls whose parent the variable-parent
// rule judges.
const tmpParentPattern = `os[.](MkdirTemp|CreateTemp)[(]`

// tmpHit is one line a pattern found: the file, the line number and the line's text, trimmed.
type tmpHit struct {
	file string
	line int
	text string
}

func (h tmpHit) String() string { return fmt.Sprintf("%s:%d: %s", h.file, h.line, h.text) }

// tmpEntry allows one line: the file and the line's exact text (trimmed), with the reason it is
// allowed or the unit that moves it.
type tmpEntry struct {
	file, text, why string
}

func (e tmpEntry) matches(h tmpHit) bool { return e.file == h.file && e.text == h.text }

// tmpGlob is an exception that covers whole files, with its reason.
type tmpGlob struct {
	name   string
	reason string
	covers func(file string) bool
}

// E1: internal/tmploc (non-test) is the one place: it holds DefaultDir, SocketRoot and the read of
// TMPDIR. It is matched by file and exact line text, so a new hit in tmploc still fails.
const tmpE1 = "E1 internal/tmploc is the one place: DefaultDir, SocketRoot and the one read of TMPDIR"

var tmpTmplocLines = []tmpEntry{
	{"internal/tmploc/dir.go", `const DefaultDir = "/tmp"`, tmpE1},
	{"internal/tmploc/dir.go", `if v := getenv("TMPDIR"); v != "" {`, tmpE1},
	{"internal/tmploc/dir.go", `return os.Getenv("TMPDIR")`, tmpE1},
	{"internal/tmploc/doc.go", `// ("/tmp"), reported with source "default" (G-D1 = A, Q63ANS1722). The default is a location only:`, tmpE1},
	{"internal/tmploc/socket.go", `const SocketRoot = "/tmp"`, tmpE1},
}

// E4: the recorded hook payload text in contracttest/hook.go, the same as contract/fixtures/hook.
// It is matched by file and exact line text.
var tmpRecordedPayload = tmpEntry{
	"internal/contracttest/hook.go",
	`payload = map[string]any{"cwd": "/tmp/workspace", "hook_event_name": "Stop", "last_assistant_message": "I finished the task.", "model": "test-model", "permission_mode": "default", "session_id": "01a0b109-1ea5-7fb3-9adc-87f45ed83688", "stop_hook_active": false, "transcript_path": "/tmp/transcript.jsonl", "turn_id": "turn-1"}`,
	"E4 recorded hook payload text, the same as contract/fixtures/hook: not a location this program chooses",
}

// E5: the cxccorpus RecordedScratch line, the recorded oracle condition of the path_length_dependent
// fixtures: their case root <RecordedScratch>/cxc-rec-<16 hex> is the recorded 33 bytes, and the
// recorder's default and the long cases' replay both use it (a replay that cannot use it fails with
// the reason). It is matched by file and exact line text.
var tmpRecordedScratch = tmpEntry{
	"internal/dev/cxccorpus/engine.go",
	`const RecordedScratch = "/var/tmp"`,
	"E5 the recorded case-root parent of the path_length_dependent fixtures (33 bytes): the recorded oracle condition, not a location this program chooses",
}

// E6: port history prose that quotes code or Python literals. These are records of what was, not
// locations; a new docs file with a hit fails.
var tmpPortProse = []string{
	"docs/port-cxc/known-defects.md",
	"docs/port-cxc/known-defects/CRW-1393.md",
	"docs/port/refactor-backlog.d/11-real-use-run-4/_section.md",
	"docs/port/refactor-backlog.d/18-closed-by-the-real-use-run-4-record/_section.md",
	"docs/port/refactor-backlog.md",
}

// tmpRecordedData is E3's file rule: testdata and contract/fixtures.
func tmpRecordedData(file string) bool {
	return strings.Contains("/"+file, "/testdata/") || strings.HasPrefix(file, "contract/fixtures/")
}

// tmpGlobs are the exceptions that cover whole files.
var tmpGlobs = []tmpGlob{
	{"E2", "test code (*_test.go, web/test): pending CRW-1580, which removes this entry", func(file string) bool {
		return strings.HasSuffix(file, "_test.go") || strings.HasPrefix(file, "web/test/")
	}},
	{"E3", "recorded data and the oracle's own programs (testdata, contract/fixtures): not locations this program chooses", tmpRecordedData},
	{"E6", "port history prose that quotes code or Python literals", func(file string) bool {
		return slices.Contains(tmpPortProse, file)
	}},
}

// The units that move the pending sites (plan-r2 of bundle 1566-tmp-arch).
const (
	tmpU8 = "U-A5-8 product implicit MkdirTemp(\"\") sites"
	tmpU9 = "U-A5-9 the last sites"
)

// tmpPending are the sites still to move, each with the unit that moves it. Each A5 unit deletes its
// own entries; U-A5-9 deletes the mechanism and adds TestNoPendingTempDecisions.
var tmpPending = []tmpEntry{
	{"plugins/crw/skills/crw-dev-visualizer/scripts/diagram-to-html.sh", `OUTDIR="/tmp/codex-diagrams"`, tmpU9},
}

// Reasons of the variable-parent table.
const (
	tmpParentBeside   = "the directory of the file the write replaces (a parent spelling or rootDir of the path), which is never \"\": a bare name gives \".\""
	tmpParentRefresh  = "g.dir, the refresh's own directory, made by tmploc in newRefreshGit (base_refresh.go)"
	tmpParentResolved = "a root crwconfig.Resolve answered, which is absolute (strict resolve)"
)

// tmpNeverEmptyParents are the os.MkdirTemp / os.CreateTemp calls whose parent is a variable that
// can never be "" or relative, each with the reason (evidence/mkdirtemp-parents.md, bundle
// 1566-tmp-arch). A call whose parent is filepath.Dir(...) or a non-empty literal needs no entry. A
// new call with any other parent fails until it is listed here with its reason or moves to
// tmploc.MkdirTempIn, which refuses an empty or relative parent.
var tmpNeverEmptyParents = []tmpEntry{
	{"cmd/crw/repomap.go", `f, err := os.CreateTemp(dir, pattern)`, "the marker seam's one caller passes filepath.Dir(path)"},
	{"internal/crwconfig/verifier.go", `tmp, err := os.CreateTemp(dir, ".config-*.json")`, "dir is filepath.Dir of the configuration file's link target"},
	{"internal/dev/ci/local.go", `temp, err := os.MkdirTemp(root, localTempPrefix)`, "root is localWorkRoot's answer, which refuses a work root that is not absolute"},
	{"internal/dev/ci/local.go", `dir, err := os.MkdirTemp(root, localSocketPattern)`, "root is tmploc.SocketRoot, or a test's socketRoot option when it is not \"\""},
	{"internal/dev/cxccorpus/shipped.go", `tmp, err := os.CreateTemp(dir, ".hook-*.tmp")`, "dir is the shipped hooks directory below the checkout, checked to be a plain directory first"},
	{"internal/dev/skillport/stage.go", `if tmp, err = os.MkdirTemp(filepath.Join(root, SkillsRoot), ".stage-"); err != nil {`, "a join below the checkout root with a constant, never \"\""},
	{"internal/flow/report/send.go", `f, err = os.CreateTemp(dir, "crw-notice-*.txt")`, "dir is ManageSender.Dir, used only when it is absolute (the case above); an empty Dir goes to tmploc.CreateTemp"},
	{"internal/flow/trace/trace.go", `tmp, err := os.CreateTemp(dir, ".crw-flow-out-*")`, tmpParentBeside},
	{"internal/hookswitch/write.go", `f, err := os.CreateTemp(dir, ".switch.json.*.tmp")`, "dir is filepath.Dir of the switch file's path"},
	{"internal/integrator/queue.go", `tmp, err := os.CreateTemp(r.Cfg.QueueDir, ".enqueue-"+name+"-")`, "queue_dir is required and must be absolute (integrator Config validation)"},
	{"internal/manage/audit_drafts.go", `tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-")`, tmpParentBeside},
	{"internal/manage/audit_result_copy.go", `tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-")`, tmpParentBeside},
	{"internal/manage/audit_round.go", `tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-")`, tmpParentBeside},
	{"internal/manage/batch_report.go", `file, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")`, tmpParentBeside},
	{"internal/manage/capacity.go", `file, err := os.CreateTemp(dir, pattern)`, "the state-write seam's one caller passes rootDir(path)"},
	{"internal/manage/dagreview_host.go", `file, err := os.CreateTemp(stateDir, dagHostStateFile+".*.tmp")`, "stateDir is the dag review host's state directory, the one its lock and offsets live in"},
	{"internal/manage/deliver.go", `file, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")`, tmpParentBeside},
	{"internal/manage/improve_run.go", `tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-")`, tmpParentBeside},
	{"internal/manage/premerge.go", `tmp, err := os.CreateTemp(dir, ".premerge-*.tmp")`, tmpParentBeside},
	{"internal/pabcd/stateroot/anchor.go", `tmp, err := os.CreateTemp(dir, "."+sessionID+".*.tmp")`, "dir is filepath.Dir of the anchor's path"},
	{"internal/policystore/policy_write.go", `file, err := os.CreateTemp(dir, writeTempPrefix)`, tmpParentBeside},
	{"internal/relay/store/diagnostic_probe.go", `if temp, err := os.CreateTemp(selection.Path, ".probe-"); err == nil {`, "selection.Path is the state directory the probe has just found to exist"},
	{"internal/review/agy/workdir.go", `root, err := os.MkdirTemp(parent, "crw-agy-")`, "parent is filepath.Abs of the work root (the line above), whose default is tmploc.Dir()"},
	{"internal/role/store.go", `f, err := os.CreateTemp(dir, path[strings.LastIndexByte(path, '/')+1:]+".*.tmp")`, tmpParentBeside},
	{"internal/runtime/install/mcp.go", `file, err := os.CreateTemp(dir, ".crw-write-")`, "dir is filepath.Dir of the record's path"},
	{"internal/runtime/pointer/pointer.go", `file, err := os.CreateTemp(parent, ".crw-pointer-")`, "parent is filepath.Dir of the pointer's path"},
	{"internal/skill/base_refresh_generated.go", `dir, err := os.MkdirTemp(g.dir, "witness-")`, tmpParentRefresh},
	{"internal/skill/base_refresh_regenerate.go", `dir, err := os.MkdirTemp(g.dir, "run-")`, tmpParentRefresh},
	{"internal/skill/merge_build_check.go", `work, err := os.MkdirTemp(g.dir, "merge-build-")`, tmpParentRefresh},
	{"internal/skill/merge_build_check_go.go", `made, err := os.MkdirTemp(tools.BuildWork, "merge-build-go-")`, "build_work is used only when it is not \"\" (the check above), and it is " + tmpParentResolved},
	{"internal/testsupport/testsupport.go", `root, err = os.MkdirTemp(top, "h-")`, "top is the inherited isolation root, used only when it is absolute and an existing directory"},
	{"internal/tools/install.go", `dir, err := os.MkdirTemp(toolsRoot, stagingPrefix+pin.Name+"-")`, "toolsRoot is tools_root, " + tmpParentResolved},
	{"internal/tools/tools.go", `return os.MkdirTemp(dir, pattern)`, "the download seam's one caller passes temp_root, " + tmpParentResolved + "; /tmp/crw when TMPDIR is unset (G-D1 = A)"},
}

// tmpPendingParents are the calls whose parent may be "" or relative at run time, so "" would mean
// TMPDIR decided inside os. Each moves to tmploc.MkdirTempIn or tmploc.MkdirTemp in the unit that owns
// its file.
var tmpPendingParents = []tmpEntry{}

// tmpGrep runs git grep over the tracked files and answers the hits.
func tmpGrep(t *testing.T, pattern string, paths ...string) []tmpHit {
	t.Helper()
	out, err := runGit(repoRoot(), append([]string{"grep", "-n", "-I", "-E", pattern, "--"}, paths...)...)
	if err != nil {
		t.Fatalf("git grep %s: %v", pattern, err)
	}
	var hits []tmpHit
	for _, raw := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		file, rest, ok := strings.Cut(raw, ":")
		number, text, ok2 := strings.Cut(rest, ":")
		if !ok || !ok2 {
			t.Fatalf("git grep line %q has no file:line: form", raw)
		}
		var line int
		if _, err := fmt.Sscanf(number, "%d", &line); err != nil {
			t.Fatalf("git grep line %q: %v", raw, err)
		}
		hits = append(hits, tmpHit{file, line, strings.TrimSpace(text)})
	}
	if len(hits) == 0 {
		t.Fatalf("git grep %s found nothing: the check reads no file", pattern)
	}
	return hits
}

// tmpDecisionHits are the hits of tmpDecisionPattern over every tracked file.
func tmpDecisionHits(t *testing.T) []tmpHit {
	return tmpGrep(t, tmpDecisionPattern)
}

// tmpParentHits are the os.MkdirTemp / os.CreateTemp calls the variable-parent rule judges: non-test
// Go outside tmploc (E1) and the recorded data (E3), with comment lines, empty parents (the main check)
// and the parents that are filepath.Dir(...) or a non-empty literal left out.
func tmpParentHits(t *testing.T) []tmpHit {
	call := regexp.MustCompile(tmpParentPattern)
	var judged []tmpHit
	for _, h := range tmpGrep(t, tmpParentPattern, "*.go", ":!*_test.go", ":!internal/tmploc/*.go") {
		if strings.HasPrefix(h.text, "//") || tmpRecordedData(h.file) {
			continue
		}
		loc := call.FindStringIndex(h.text)
		arg := h.text[loc[1]:]
		switch {
		case strings.HasPrefix(arg, `""`):
			continue // an empty parent: tmpDecisionPattern's
		case strings.HasPrefix(arg, `"`), strings.HasPrefix(arg, "filepath.Dir("):
			continue // a non-empty literal, or the directory of a path, which is never ""
		}
		judged = append(judged, h)
	}
	return judged
}

// tmpCovered says which exception or pending entry allows a hit ("" for none).
func tmpCovered(h tmpHit) string {
	for _, g := range tmpGlobs {
		if g.covers(h.file) {
			return g.name
		}
	}
	for _, e := range tmpTmplocLines {
		if e.matches(h) {
			return "E1"
		}
	}
	if tmpRecordedPayload.matches(h) {
		return "E4"
	}
	if tmpRecordedScratch.matches(h) {
		return "E5"
	}
	for _, e := range tmpPending {
		if e.matches(h) {
			return "pending " + e.why
		}
	}
	return ""
}

// CRW-1579 (D8): every temporary-location decision in a tracked file is tmploc's, an exception with
// its reason, or a pending site its unit moves. Anything else is a second place deciding a
// temporary location.
func TestTempLocationsComeFromTmploc(t *testing.T) {
	var loose []string
	for _, h := range tmpDecisionHits(t) {
		if tmpCovered(h) == "" {
			loose = append(loose, h.String())
		}
	}
	if len(loose) > 0 {
		t.Errorf("%d line(s) decide a temporary location outside internal/tmploc and are no exception or pending site; "+
			"use tmploc (Dir, MkdirTemp, CreateTemp, MkdirTempIn) or t.TempDir() / tmploctest:\n%s", len(loose), strings.Join(loose, "\n"))
	}
}

// CRW-1579 (D8, R1-7): a non-test os.MkdirTemp / os.CreateTemp whose parent is a variable needs a
// reason that the variable is never "" or relative (tmpNeverEmptyParents), or is a pending site; ""
// would be TMPDIR decided inside os, and a relative parent the working directory.
func TestTempParentsAreNeverEmpty(t *testing.T) {
	var loose []string
	for _, h := range tmpParentHits(t) {
		if !slices.ContainsFunc(tmpNeverEmptyParents, h.matchedBy) && !slices.ContainsFunc(tmpPendingParents, h.matchedBy) {
			loose = append(loose, h.String())
		}
	}
	if len(loose) > 0 {
		t.Errorf("%d os.MkdirTemp / os.CreateTemp call(s) take a parent that may be \"\" or relative; use tmploc.MkdirTempIn "+
			"(which refuses both) or list the call in tmpNeverEmptyParents with the reason it is never empty:\n%s", len(loose), strings.Join(loose, "\n"))
	}
}

func (h tmpHit) matchedBy(e tmpEntry) bool { return e.matches(h) }

// CRW-1579 (D8): every exception and pending entry still matches a hit, so an entry whose site moved
// or changed is removed rather than left to allow a later line. An entry listed twice fails too.
func TestTempDecisionExceptionsAreLive(t *testing.T) {
	hits := tmpDecisionHits(t)
	for _, g := range tmpGlobs {
		if !slices.ContainsFunc(hits, func(h tmpHit) bool { return g.covers(h.file) }) {
			t.Errorf("exception %s (%s) covers no hit: remove it", g.name, g.reason)
		}
	}
	for _, file := range tmpPortProse {
		if !slices.ContainsFunc(hits, func(h tmpHit) bool { return h.file == file }) {
			t.Errorf("E6 lists %s, which has no hit: remove it", file)
		}
	}
	parentHits := tmpParentHits(t)
	for _, table := range []struct {
		name    string
		entries []tmpEntry
		hits    []tmpHit
	}{
		{"E1", tmpTmplocLines, hits},
		{"E4", []tmpEntry{tmpRecordedPayload}, hits},
		{"E5", []tmpEntry{tmpRecordedScratch}, hits},
		{"the pending list", tmpPending, hits},
		{"tmpNeverEmptyParents", tmpNeverEmptyParents, parentHits},
		{"tmpPendingParents", tmpPendingParents, parentHits},
	} {
		seen := map[[2]string]bool{}
		for _, e := range table.entries {
			key := [2]string{e.file, e.text}
			if seen[key] {
				t.Errorf("%s lists %s: %q twice", table.name, e.file, e.text)
			}
			seen[key] = true
			if !slices.ContainsFunc(table.hits, func(h tmpHit) bool { return e.matches(h) }) {
				t.Errorf("%s lists %s: %q, which no longer matches a line: remove it (or fix its text)", table.name, e.file, e.text)
			}
		}
	}
	for _, e := range tmpNeverEmptyParents {
		if slices.ContainsFunc(tmpPendingParents, func(p tmpEntry) bool { return p.file == e.file && p.text == e.text }) {
			t.Errorf("%s: %q is both never empty and pending", e.file, e.text)
		}
	}
}
