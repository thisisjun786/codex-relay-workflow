package manage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// joinRootFixture is a configured root whose spelling mixes a symbolic link and "..": the kernel
// resolves spelled to real, while filepath.Join, Clean and Dir would fold the ".." before the link
// and name wrong, a directory that does not exist. A command that names a path below spelled with
// those functions reads and writes the wrong place; one that joins the text raw reaches real.
type joinRootFixture struct {
	spelled string // <base>/lnk/../<name>
	real    string // <base>/real/<name>, created
	wrong   string // <base>/<name>, never created
}

func newJoinRootFixture(t *testing.T, base, name string) joinRootFixture {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	f := joinRootFixture{
		spelled: resolved + "/lnk/../" + name,
		real:    filepath.Join(resolved, "real", name),
		wrong:   filepath.Join(resolved, name),
	}
	for _, dir := range []string{f.real, filepath.Join(resolved, "real", "sub")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(resolved, "real", "sub"), filepath.Join(resolved, "lnk")); err != nil {
		t.Fatal(err)
	}
	return f
}

func newJoinRoot(t *testing.T) joinRootFixture {
	t.Helper()
	return newJoinRootFixture(t, t.TempDir(), "state")
}

// requireFile fails the test unless path exists below the real directory.
func (f joinRootFixture) requireFile(t *testing.T, rel ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{f.real}, rel...)...)
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("the configured place lacks %s: %v", strings.Join(rel, "/"), err)
	}
	return path
}

// requireNoWrong fails the test when anything was created where a cleaned spelling points.
func (f joinRootFixture) requireNoWrong(t *testing.T) {
	t.Helper()
	if _, err := os.Lstat(f.wrong); err == nil {
		t.Errorf("a command wrote below %s, the place a cleaned spelling names, and not below the configured root", f.wrong)
	}
}

// requireNames fails the test unless the path the code built names the same file the kernel
// reaches at want.
func requireJoinRootNames(t *testing.T, got, want string) {
	t.Helper()
	gotReal, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Errorf("%s does not name the configured place: %v", got, err)
		return
	}
	wantReal, err := filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatal(err)
	}
	if gotReal != wantReal {
		t.Errorf("%s names %s, want %s", got, gotReal, wantReal)
	}
}

func (f joinRootFixture) mkdir(t *testing.T, rel ...string) string {
	t.Helper()
	dir := filepath.Join(append([]string{f.real}, rel...)...)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// plant makes real/<rel> a symbolic link: a path the code builds below the configured root must
// find it there, and refuse to write through it.
func (f joinRootFixture) plant(t *testing.T, rel string) {
	t.Helper()
	target := filepath.Join(filepath.Dir(f.real), "elsewhere")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(f.real, rel)); err != nil {
		t.Fatal(err)
	}
}

// audit: the ledger, the alert queue, the report, the rounds, the drafts directory and the bundle
// roots are all named below the configured state directory.
func TestJoinRootAuditFamilyUsesTheConfiguredRoot(t *testing.T) {
	f := newJoinRoot(t)
	e, _, _ := auditTestEnv(t)
	cfg := auditSectionConfig(t, f.spelled, nil)

	results := []AuditResult{{Mode: auditModePR, Subject: "s", Head: "h", Issue: "CRW-1", Status: auditStatusOK, Score: 5,
		GradedAt: "2026-01-01T00:00:00Z", Bundle: "b", graded: []byte(crw838JSONFirst), Defects: []AuditDefect{{Severity: "P1", What: "w", Where: "f.go:1"}}}}
	if rows, err := auditRecord(e, cfg, results); err != nil || rows != 1 {
		t.Fatalf("auditRecord: %d rows, %v", rows, err)
	}
	f.requireFile(t, "audit", auditLedgerFile)
	f.requireFile(t, "audit", auditAlertFile)

	listing, err := AuditList(context.Background(), e, cfg, AuditListOptions{})
	if err != nil {
		t.Fatalf("AuditList: %v", err)
	}
	if len(listing.Results) != 1 {
		t.Errorf("audit list read %d ledger rows from the configured root, want 1", len(listing.Results))
	}

	if err := auditReportWrite(e, cfg); err != nil {
		t.Fatalf("auditReportWrite: %v", err)
	}
	f.requireFile(t, "audit", auditReportFile)

	if _, err := auditRoundStart(e, cfg, "r1", []string{"pkg/a"}); err != nil {
		t.Fatalf("auditRoundStart: %v", err)
	}
	f.requireFile(t, "audit", "rounds", "r1.json")

	release, err := auditDraftLock(e, cfg)
	if err != nil {
		t.Fatalf("auditDraftLock: %v", err)
	}
	release()
	f.requireFile(t, "drafts", "drafts.lock")
	if err := auditDraftIndexSave(auditDraftDir(e, cfg)); err != nil {
		t.Fatalf("auditDraftIndexSave: %v", err)
	}
	f.requireFile(t, "drafts", auditDraftIndexFile)

	// The bundle roots and the grade marker are named, not created, by a path function; they must
	// name the same directory the kernel gives the configured spelling.
	bundles := f.mkdir(t, "audit", "bundles")
	requireJoinRootNames(t, auditPkgBundleRoot(e, cfg, auditPkgSection{}), bundles)
	requireJoinRootNames(t, auditPRBundleRoot(e, cfg, auditPRSection{}), bundles)
	pending := f.mkdir(t, "audit", auditPendingDir)
	requireJoinRootNames(t, rootDir(auditPendingPath(e, cfg, filepath.Join(f.real, "a-bundle"))), pending)
	f.requireNoWrong(t)
}

// checkpoint: the record is appended below the configured state directory, the reader finds it
// there, and the relay store is opened below the relay state directory as spelled.
func TestJoinRootCheckpointFamilyUsesTheConfiguredRoot(t *testing.T) {
	f := newJoinRoot(t)
	e, _, _ := auditTestEnv(t)
	cfg := auditSectionConfig(t, f.spelled, nil)
	summary := filepath.Join(t.TempDir(), "summary.txt")
	if err := os.WriteFile(summary, []byte("checkpoint summary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := checkpointRecord(e, cfg, "proj", summary); err != nil {
		t.Fatalf("checkpointRecord: %v", err)
	}
	f.requireFile(t, checkpointDirName, "proj.jsonl")
	instants, err := checkpointRecordInstants(f.spelled)
	if err != nil {
		t.Fatalf("checkpointRecordInstants: %v", err)
	}
	if len(instants["proj"]) != 1 {
		t.Errorf("the reader found %d records of proj below the configured root, want 1", len(instants["proj"]))
	}

	relay := newJoinRootFixture(t, t.TempDir(), "relay")
	capacityTestStore(t, filepath.Join(relay.real, checkpointStoreFile))
	handle, err := checkpointOpenStore(context.Background(), relay.spelled)
	if err != nil {
		t.Fatalf("checkpointOpenStore: %v", err)
	}
	handle.Close()
	f.requireNoWrong(t)
}

// capacity: the judgement's state file is written below the configured state directory.
func TestJoinRootCapacityWritesBelowTheConfiguredRoot(t *testing.T) {
	cf := capacityTestFixture(t, nil)
	f := newJoinRootFixture(t, t.TempDir(), "state")
	cf.cfg.StateDir = f.spelled
	if _, err := Capacity(context.Background(), cf.env, cf.cfg, false); err != nil {
		t.Fatalf("Capacity: %v", err)
	}
	f.requireFile(t, capacityStateFile)
	f.requireNoWrong(t)
}

// pump_state: the state document, its lock and its log live below the configured state directory.
func TestJoinRootPumpFilesUseTheConfiguredRoot(t *testing.T) {
	f := newJoinRoot(t)
	cfg := &Config{StateDir: f.spelled}
	if err := os.WriteFile(filepath.Join(f.real, pumpStateFile), []byte(`{"offsets":{"thread-a":7}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := pumpLoadState(cfg)
	if err != nil {
		t.Fatalf("pumpLoadState: %v", err)
	}
	if st.Offsets["thread-a"] != 7 {
		t.Errorf("the pump read offsets %v, want the state below the configured root", st.Offsets)
	}
	st.Offsets["thread-b"] = 9
	if err := st.pumpSave(cfg); err != nil {
		t.Fatalf("pumpSave: %v", err)
	}
	again, err := pumpLoadState(&Config{StateDir: f.real})
	if err != nil || again.Offsets["thread-b"] != 9 {
		t.Errorf("the saved state is not where the configured root points: %v %v", again.Offsets, err)
	}
	release, err := pumpLock(cfg)
	if err != nil {
		t.Fatalf("pumpLock: %v", err)
	}
	release()
	f.requireFile(t, pumpLockFile)
	pumpLog(cfg, "a line")
	f.requireFile(t, pumpLogFile)
	f.requireNoWrong(t)
}

// pump_queue: the queue directory, the outbox listing and the queue refusal of a planted link are
// read below the configured state directory.
func TestJoinRootPumpQueueUsesTheConfiguredRoot(t *testing.T) {
	f := newJoinRoot(t)
	cfg := &Config{StateDir: f.spelled}
	f.mkdir(t, "outbox")
	if err := os.WriteFile(filepath.Join(f.real, "outbox", "id-1.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	ids, err := pumpReview776QueueOutboxIDs(cfg)
	if err != nil {
		t.Fatalf("pumpReview776QueueOutboxIDs: %v", err)
	}
	if !ids["id-1"] {
		t.Errorf("the outbox listing is %v, want id-1 read from the configured root", ids)
	}

	e, _, _ := auditTestEnv(t)
	f.plant(t, pumpQueueDir)
	st := pumpNewState()
	if err := pumpQueueFlush(context.Background(), e, cfg, &st, pumpSettings{}, true); err == nil {
		t.Errorf("a parent queue that is a planted link below the configured root was flushed")
	}
	f.requireNoWrong(t)
}

// deliver: the ledger entry, the outbox check and the per-message lock are below the configured
// state directory.
func TestJoinRootDeliverUsesTheConfiguredRoot(t *testing.T) {
	f := newJoinRoot(t)
	cfg := &Config{StateDir: f.spelled}
	record := deliverRecord{LogicalID: "id-2", RequestID: "req-2", State: "queued"}
	if err := deliverSave(cfg, record); err != nil {
		t.Fatalf("deliverSave: %v", err)
	}
	f.requireFile(t, "outbox", "id-2.json")
	loaded, found, err := deliverLoad(&Config{StateDir: f.real}, "id-2")
	if err != nil || !found || loaded.RequestID != "req-2" {
		t.Errorf("the saved record is not where the configured root points: %+v %v %v", loaded, found, err)
	}
	if again, found, err := deliverLoad(cfg, "id-2"); err != nil || !found || again.RequestID != "req-2" {
		t.Errorf("deliverLoad through the configured spelling: %+v %v %v", again, found, err)
	}
	release, err := deliverLock(cfg, "id-2")
	if err != nil {
		t.Fatalf("deliverLock: %v", err)
	}
	release()
	f.requireFile(t, deliverLockDir, "id-2.lock")

	// A link planted where the outbox belongs is found, not walked around.
	g := newJoinRoot(t)
	g.plant(t, "outbox")
	if err := deliverOutboxDirSafe(&Config{StateDir: g.spelled}); err == nil {
		t.Errorf("an outbox that is a planted link below the configured root was accepted")
	}
	h := newJoinRoot(t)
	h.plant(t, deliverLockDir)
	if _, err := deliverLock(&Config{StateDir: h.spelled}, "id-3"); err == nil {
		t.Errorf("a lock directory that is a planted link below the configured root was accepted")
	}
	f.requireNoWrong(t)
}

// send-parent --queue writes the notice below the configured state directory.
func TestJoinRootSendParentQueuesBelowTheConfiguredRoot(t *testing.T) {
	f := newJoinRoot(t)
	e, _, _ := auditTestEnv(t)
	cfg := &Config{StateDir: f.spelled}
	code := sendParentDispatch(context.Background(), e, cfg, sendParentArgs{thread: "thread-a", logicalID: "note-1", queue: true}, []byte("hello"))
	if code != 0 {
		t.Fatalf("send-parent --queue exited %d", code)
	}
	f.requireFile(t, sendParentQueueDir, "thread-a", "note-1.txt")
	f.requireNoWrong(t)
}

// memlog samples below the configured state directory.
func TestJoinRootMemlogWritesBelowTheConfiguredRoot(t *testing.T) {
	f := newJoinRoot(t)
	proc := t.TempDir()
	memlogWriteTree(t, proc, nil)
	cfg := auditSectionConfig(t, f.spelled, nil)
	var out, errOut strings.Builder
	e := memlogEnv(memlogFixedClock(time.Date(2026, 10, 9, 1, 2, 3, 0, time.UTC)), &out, &errOut)
	if code := memlogRunWith(context.Background(), e, cfg, []string{"--once"}, memlogNewProcSampler(proc)); code != 0 {
		t.Fatalf("memlog exited %d: %s", code, errOut.String())
	}
	f.requireFile(t, "memlog", "20261009.jsonl")
	f.requireNoWrong(t)
}

// dag-review keeps its offsets and its lock below the relay state directory as spelled.
func TestJoinRootDagReviewStateUsesTheConfiguredRoot(t *testing.T) {
	f := newJoinRoot(t)
	ctx := context.Background()
	release, err := dagHostStateLock(ctx, f.spelled)
	if err != nil {
		t.Fatalf("dagHostStateLock: %v", err)
	}
	release()
	f.requireFile(t, dagHostStateLockFile)
	state, err := dagHostLoadOffsets(f.spelled)
	if err != nil {
		t.Fatalf("dagHostLoadOffsets: %v", err)
	}
	state.Offsets["thread-a"] = 11
	if err := dagHostSaveOffsets(ctx, f.spelled, state); err != nil {
		t.Fatalf("dagHostSaveOffsets: %v", err)
	}
	f.requireFile(t, dagHostStateFile)
	again, err := dagHostLoadOffsets(f.spelled)
	if err != nil || again.Offsets["thread-a"] != 11 {
		t.Errorf("the saved offsets are not read back through the configured spelling: %v %v", again.Offsets, err)
	}
	f.requireNoWrong(t)
}

// improve run leaves its bundle, drafts and roadmap below the configured manage state directory.
func TestJoinRootImproveRunWritesBelowTheConfiguredRoot(t *testing.T) {
	s := improveTestSetup(t)
	f := newJoinRootFixture(t, s.root, "manage-state")
	improveTestStore(t, s, func(t *testing.T, db *sql.DB) { improveRoadmapTestSeedRepeatedFriction(t, db) })
	improveRoadmapTestConfigure(t, s, f.spelled, map[string]any{})
	var stdout, stderr strings.Builder
	e := improveRoadmapTestEnv(s, &stdout, &stderr)
	code, _, errOut := improveRoadmapTestRun(t, e, "--boundary", "milestone", "--ref", "M2")
	if code != 0 {
		t.Fatalf("improve run: exit %d, stderr %s", code, errOut)
	}
	if got := improveRoadmapTestBundles(t, f.real, "M2"); len(got) != 1 {
		t.Errorf("the configured root holds bundles %v, want one", got)
	}
	if got := improveRoadmapTestRoadmaps(t, f.real); len(got) != 1 {
		t.Errorf("the configured root holds roadmaps %v, want one", got)
	}
	f.requireNoWrong(t)
}

// auditPkgRelativeTo names a package directory relative to a configured repository whose spelling
// mixes a symbolic link and "..": go list answers the real directory, and the cleaned spelling of
// the repository is a directory that is not the checkout.
func TestJoinRootAuditPackageDirectoriesAreRelativeToTheConfiguredRepository(t *testing.T) {
	f := newJoinRoot(t)
	pkg := f.mkdir(t, "internal", "pkg")
	got, err := auditPkgRelative(auditPkgCheckout{Repository: f.spelled}, []string{pkg})
	if err != nil {
		t.Fatalf("auditPkgRelative: %v", err)
	}
	if len(got) != 1 || got[0] != "internal/pkg" {
		t.Errorf("the package directory is %v, want internal/pkg", got)
	}
}

// rootDir answers what filepath.Dir answers for a plain path and keeps a "..": it cleans nothing.
func TestRootDirTakesTheDirectoryFromTheText(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"/a/b/c", "/a/b"},
		{"/a/b/c/", "/a/b/c"},
		{"/a", "/"},
		{"/", "/"},
		{"a", "."},
		{"a/b", "a"},
		{"/x/lnk/../state/audit/f", "/x/lnk/../state/audit"},
		{"/x//y", "/x"},
	} {
		if got := rootDir(tc.path); got != tc.want {
			t.Errorf("rootDir(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// joinRootCleaningCalls are the calls of path/filepath that clean their result, directly or in the
// names a search or a walk builds from the directory it was given (Glob, Walk, WalkDir). Below a configured
// or environment root they fold a ".." that follows a symbolic link before the filesystem does, so
// a path built that way names a directory the configuration did not. internal/manage builds such a
// path with crwconfig.JoinRoot and takes its directory with rootDir.
var joinRootCleaningCalls = map[string]bool{"Join": true, "Dir": true, "Clean": true, "Abs": true, "Split": true, "Glob": true, "Walk": true, "WalkDir": true}

// joinRootCleaningAllowed names the functions that may still call one, and why: each works on a
// path that is already resolved, or on text that is not a root.
var joinRootCleaningAllowed = map[string]string{
	"audit_grade.go:auditBundleResolvedPath": "cleans the output of EvalSymlinks, which is already clean",
	"audit_grade.go:auditBundleAbs":          "the fallback identity of a bundle the kernel cannot resolve, deliberately its cleaned absolute spelling",
	"upgrade_steps.go:upgradeExtract":        "cleans a tar entry name to refuse one that escapes the extract directory",
	"improve_collect.go:improvePlanOutput":   "takes the directory of the output store.Realpath already resolved",
}

// No internal/manage source calls filepath.Join, Dir, Clean, Abs, Split, Glob, Walk or WalkDir outside the functions
// above, so a new path below a configured root cannot reintroduce the cleaning.
func TestJoinRootNoCleaningPathCallsInManage(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "filepath" || !joinRootCleaningCalls[sel.Sel.Name] {
					return true
				}
				key := name + ":" + fn.Name.Name
				if _, allowed := joinRootCleaningAllowed[key]; !allowed {
					found = append(found, key+" calls filepath."+sel.Sel.Name)
				}
				return true
			})
		}
	}
	sort.Strings(found)
	for _, f := range found {
		t.Errorf("%s: build a path below a configured root with crwconfig.JoinRoot and take its directory with rootDir", f)
	}
}

// A directory listing a command makes below a configured root keeps the spelling too: the
// filepath.Glob and filepath.WalkDir calls join each name they find to the directory with
// filepath.Join, which folds a ".." that follows a link, so a search for the pump's rollout, the
// release archive or the audit reference sources went on in the wrong directory after its first level.

// pump_sources: the rollout of a parent is found below a CODEX_HOME spelled through a link and "..".
func TestJoinRootPumpRolloutIsFoundBelowTheConfiguredHome(t *testing.T) {
	f := newJoinRoot(t)
	day := f.mkdir(t, "sessions", "2026", "10", "09")
	want := filepath.Join(day, "rollout-thread-a.jsonl")
	if err := os.WriteFile(want, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, _, _ := auditTestEnv(t)
	e.Getenv = func(name string) string {
		if name == "CODEX_HOME" {
			return f.spelled
		}
		return ""
	}
	got, err := pumpRolloutPath(e, "thread-a")
	if err != nil {
		t.Fatalf("pumpRolloutPath: %v", err)
	}
	if got == "" {
		t.Fatalf("the rollout below the configured CODEX_HOME was not found")
	}
	requireJoinRootNames(t, got, want)
	if missing, err := pumpRolloutPath(e, "thread-b"); err != nil || missing != "" {
		t.Errorf("a thread without a rollout answered %q, %v", missing, err)
	}
	f.requireNoWrong(t)
}

// upgrade_steps: the release archive is found, copied and verified below a release directory
// spelled through a link and "..".
func TestJoinRootUpgradeFindsTheArchiveInTheReleaseDirectory(t *testing.T) {
	f := newJoinRoot(t)
	archive := []byte("archive bytes")
	name := "crw_1.0.0_linux_amd64.tar.gz"
	if err := os.WriteFile(filepath.Join(f.real, name), archive, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(archive)
	sums := hex.EncodeToString(digest[:]) + "  " + name + "\n"
	if err := os.WriteFile(filepath.Join(f.real, upgradeSumsName), []byte(sums), 0o600); err != nil {
		t.Fatal(err)
	}
	e, _, _ := auditTestEnv(t)
	run := &upgradeRunState{ctx: context.Background(), e: e, opts: upgradeOptions{ReleaseDir: f.spelled}, dir: t.TempDir()}
	pinned, code, reason := run.verifySums()
	if code != 0 {
		t.Fatalf("verifySums exit %d (%s), steps %+v", code, reason, run.steps)
	}
	if got, err := os.ReadFile(pinned); err != nil || string(got) != string(archive) {
		t.Errorf("the pinned copy is %q, %v", got, err)
	}
	f.requireNoWrong(t)
}

// audit_package: a reference source directory spelled through a link and "..", whose same-named
// file also exists in the directory a cleaned spelling names, copies the configured one.
func TestJoinRootAuditCopiesTheConfiguredSourceDirectory(t *testing.T) {
	f := newJoinRoot(t)
	f.mkdir(t, "sub")
	for rel, text := range map[string]string{"a.txt": "configured a", "sub/b.txt": "configured b"} {
		if err := os.WriteFile(filepath.Join(f.real, rel), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(f.wrong, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.wrong, "a.txt"), []byte("decoy a"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := auditPkgCopySources([]string{f.spelled}, dst); err != nil {
		t.Fatalf("auditPkgCopySources: %v", err)
	}
	for rel, want := range map[string]string{"a.txt": "configured a", "sub/b.txt": "configured b"} {
		got, err := os.ReadFile(filepath.Join(dst, "state", rel))
		if err != nil || string(got) != want {
			t.Errorf("reference/state/%s is %q, %v; want %q", rel, got, err, want)
		}
	}
	// a link below the source is still refused
	if err := os.Symlink(f.real, filepath.Join(f.real, "sub", "loop")); err != nil {
		t.Fatal(err)
	}
	if err := auditPkgCopySources([]string{f.spelled}, t.TempDir()); err == nil {
		t.Errorf("a symbolic link below the source was copied")
	}
}

// A literal component of a rootGlob pattern is looked up by name, as filepath.Glob does, not found
// by listing its directory: a CODEX_HOME that may be searched but not listed (mode 0100) still
// yields its rollout, and a literal last component is found in a directory that cannot be listed.
func TestRootGlobLiteralComponentsNeedNoListing(t *testing.T) {
	home := t.TempDir()
	day := filepath.Join(home, "sessions", "2026", "10", "09")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(day, "rollout-thread-a.jsonl")
	if err := os.WriteFile(want, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o100); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(home, 0o700) })
	if _, err := os.ReadDir(home); err == nil {
		t.Skip("the directory can be listed despite mode 0100 (running as root?)")
	}
	old, err := filepath.Glob(filepath.Join(home, "sessions", "*", "*", "*", "*thread-a.jsonl"))
	if err != nil || len(old) != 1 || old[0] != want {
		t.Fatalf("filepath.Glob found %v, %v; want %s", old, err, want)
	}
	e, _, _ := auditTestEnv(t)
	e.Getenv = func(name string) string {
		if name == "CODEX_HOME" {
			return home
		}
		return ""
	}
	got, err := pumpRolloutPath(e, "thread-a")
	if err != nil || got != want {
		t.Errorf("pumpRolloutPath below a searchable, unlistable CODEX_HOME = %q, %v; want %s", got, err, want)
	}
	if got, err := rootGlob(home, "sessions"); err != nil || len(got) != 1 || got[0] != filepath.Join(home, "sessions") {
		t.Errorf("rootGlob(home, sessions) = %v, %v; want the one literal path", got, err)
	}
	if got, err := rootGlob(home, "missing"); err != nil || len(got) != 0 {
		t.Errorf("rootGlob(home, missing) = %v, %v; want no match", got, err)
	}
	if got, err := rootGlob(home, "sessions", "2026", "10", "09", "rollout-thread-a.jsonl", "below-a-file"); err != nil || len(got) != 0 {
		t.Errorf("a literal below a file = %v, %v; want no match", got, err)
	}
}
