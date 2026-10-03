package state

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// The tests are the B-class tests of CXC v0.2.40 pabcd-state/test/state.test.ts that cover the write path, named by the oracle
// test they port and run through the real EnsureState, WriteState and appenders (state_test.go ports the read side with
// hand-written files). The recorded oracle cases are replayed by oracle_writes_test.go and the process cases by
// concurrency_test.go.

func fileText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// sessionFiles lists the names under .crw/sessions, nil when the directory is missing.
func sessionFiles(cwd string) (names []string) {
	entries, _ := os.ReadDir(filepath.Join(cwd, crwdir.DirName, SessionsSubdir))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func linkFails(errno syscall.Errno) func(string, string) error {
	return func(old, created string) error { return &os.LinkError{Op: "link", Old: old, New: created, Err: errno} }
}

func TestEnsureStateFreshSessionCreatesTheExactDefaultStateWithoutTempFiles(t *testing.T) { // oracle 137
	cwd := t.TempDir()
	if created, err := EnsureState(cwd, "session-start-fresh"); err != nil || !created {
		t.Fatalf("created %v, %v", created, err)
	}
	file := fileText(t, StatePath(cwd, "session-start-fresh"))
	got, unreadable := ReadStateStrict(cwd, "session-start-fresh")
	want := DefaultState("session-start-fresh", "")
	if _, err := time.Parse(time.RFC3339Nano, got.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	want.UpdatedAt = got.UpdatedAt
	if unreadable || mustEncode(t, got) != file || file != mustEncode(t, want) || !slices.Equal(sessionFiles(cwd), []string{"session-start-fresh.json"}) {
		t.Fatalf("unreadable %v, files %v\n%s", unreadable, sessionFiles(cwd), file)
	}
}

func TestEnsureStateExistingValidStateIsResumeSafeAndUnchanged(t *testing.T) { // oracle 245
	cwd, resumed := t.TempDir(), DefaultState("session-start-valid", "resume-me")
	resumed.Phase, resumed.OrchestrationActive, resumed.StopBlockPhase, resumed.StopBlockCount = PhaseB, true, &resumed.Phase, 2
	if err := WriteState(cwd, resumed); err != nil {
		t.Fatal(err)
	}
	before := fileText(t, StatePath(cwd, "session-start-valid"))
	if created, err := EnsureState(cwd, "session-start-valid"); created || err != nil || fileText(t, StatePath(cwd, "session-start-valid")) != before {
		t.Fatalf("created %v, %v", created, err)
	}
}

func TestEnsureStateKeepsCorruptBytesAndRejectsNoncanonicalIDs(t *testing.T) { // oracle 266
	cwd, corrupt := t.TempDir(), "{ not valid json \x00"
	putIn(t, cwd, "session-start-corrupt", corrupt)
	if created, err := EnsureState(cwd, "session-start-corrupt"); created || err != nil || fileText(t, StatePath(cwd, "session-start-corrupt")) != corrupt {
		t.Fatalf("created %v, %v", created, err)
	}
	for _, id := range []string{"  padded  ", "../unsafe/session", "세션"} {
		if created, err := EnsureState(cwd, id); created || !errors.Is(err, ErrNonCanonicalSessionID) || err.Error() != "sessionId must be a canonical state key" {
			t.Errorf("%q: created %v, %v", id, created, err)
		}
	}
	if !slices.Equal(sessionFiles(cwd), []string{"session-start-corrupt.json"}) {
		t.Fatalf("files %v", sessionFiles(cwd))
	}
}

func TestEnsureStateWhenTheHardLinkFails(t *testing.T) { // oracle 651, 666, 680, 696
	for _, c := range []struct {
		name    string
		errno   syscall.Errno
		created bool
		err     error
		file    bool
	}{
		{"falls back when linkSync answers EPERM", syscall.EPERM, true, nil, true},
		{"EEXIST from linkSync returns false without touching the fallback", syscall.EEXIST, false, nil, false},
		{"a non-link error still propagates", syscall.EIO, false, syscall.EIO, false},
	} {
		cwd := t.TempDir()
		created, err := ensureState(cwd, "link-case", at, linkFails(c.errno))
		if created != c.created || !errors.Is(err, c.err) || (err == nil) != (c.err == nil) || len(sessionFiles(cwd)) != map[bool]int{true: 1}[c.file] {
			t.Errorf("%s: created %v, %v, files %v", c.name, created, err, sessionFiles(cwd))
		}
	}
	cwd := t.TempDir() // the fallback maps EEXIST to false: someone else won the race
	if created, err := EnsureState(cwd, "fat32-race"); !created || err != nil {
		t.Fatal(created, err)
	}
	if created, err := ensureState(cwd, "fat32-race", at, linkFails(syscall.ENOTSUP)); created || err != nil {
		t.Fatalf("second: created %v, %v", created, err)
	}
	if _, err := ensureState(t.TempDir(), "x", at, os.Link); err != nil || (func() bool {
		f := filepath.Join(t.TempDir(), "f")
		_ = os.WriteFile(f, nil, 0o644)
		_, e := ensureState(f, "x", at, os.Link)
		return !errors.Is(e, syscall.ENOTDIR)
	})() {
		t.Fatal("a .crw that is a file must fail with ENOTDIR")
	}
}

// hook is a fail hook for ensureStateWith.
type hook = func(ensureStep, string) error

// failing answers err at step, on the final path (onFinal) or on any other path, which in these tests is a temp file; every
// other call goes through.
func failing(final string, step ensureStep, onFinal bool, err error) hook {
	return func(s ensureStep, path string) error {
		if s == step && (path == final) == onFinal {
			return err
		}
		return nil
	}
}

// unsupported makes the no-replace publication answer errno, as a filesystem or kernel without the flag does.
func unsupported(final string, errno syscall.Errno) hook {
	return failing(final, stepPublish, false, &os.LinkError{Op: "rename", Err: errno})
}

// both fails whatever either hook fails; a runs first.
func both(a, b hook) hook {
	return func(s ensureStep, path string) error {
		if err := a(s, path); err != nil {
			return err
		}
		return b(s, path)
	}
}

// recording notes each step the fallback takes, as "<step> temp" or "<step> final", and fails none.
func recording(final string, calls *[]string) hook {
	names := [...]string{stepWrite: "write", stepSync: "sync", stepClose: "close", stepPublish: "publish"}
	return func(s ensureStep, path string) error {
		where := "temp"
		if path == final {
			where = "final"
		}
		*calls = append(*calls, names[s]+" "+where)
		return nil
	}
}

// noReplaceWorks reports whether the platform's no-replace rename works on the filesystem of dir, so a test of that route can
// skip where it does not.
func noReplaceWorks(t *testing.T, dir string) bool {
	t.Helper()
	a, b := filepath.Join(dir, "probe-a"), filepath.Join(dir, "probe-b")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return errors.Is(renameNoReplace(a, b), fs.ErrExist)
}

// limitFileSize lowers RLIMIT_FSIZE to 16 bytes, so that a write past them fails after a partial write (EFBIG), as a full disk
// does, and returns the restore; the limit is process-wide, so a test restores it as soon as the call under test returns.
func limitFileSize(t *testing.T) (restore func()) {
	t.Helper()
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Skip(err)
	}
	limited := syscall.Rlimit{Cur: 16, Max: old.Max}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limited); err != nil {
		t.Skip(err)
	}
	restore = func() {
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
			t.Errorf("restoring the file size limit: %v", err)
		}
	}
	t.Cleanup(restore)
	return restore
}

// standing is what is at path, without following a link: the text of a file, or the target of a symlink.
func standing(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			t.Fatal(err)
		}
		return "link to " + target
	}
	if info.IsDir() {
		return "directory"
	}
	return "file " + fileText(t, path)
}

func TestEnsureStateFallbackLeavesNothingAtTheFinalPathWhenAStepFails(t *testing.T) {
	const id = "fallback-fail"
	onTemp := func(step ensureStep, err syscall.Errno) func(string) hook {
		return func(final string) hook { return failing(final, step, false, err) }
	}
	inPlace := func(step ensureStep, err syscall.Errno) func(string) hook {
		return func(final string) hook {
			return both(unsupported(final, syscall.EINVAL), failing(final, step, true, err))
		}
	}
	for _, c := range []struct {
		name string
		fail func(final string) hook
		want syscall.Errno
	}{
		{"the write of the temp file fails", onTemp(stepWrite, syscall.ENOSPC), syscall.ENOSPC},
		{"the fsync of the temp file fails", onTemp(stepSync, syscall.EIO), syscall.EIO},
		{"the close of the temp file fails", onTemp(stepClose, syscall.EIO), syscall.EIO},
		{"the publication fails with an error that is not unsupported", onTemp(stepPublish, syscall.EIO), syscall.EIO},
		{"no rename without replacing, and the in-place write fails", inPlace(stepWrite, syscall.ENOSPC), syscall.ENOSPC},
		{"no rename without replacing, and the in-place fsync fails", inPlace(stepSync, syscall.EIO), syscall.EIO},
		{"no rename without replacing, and the in-place close fails", inPlace(stepClose, syscall.EIO), syscall.EIO},
	} {
		cwd := t.TempDir()
		created, err := ensureStateWith(cwd, id, at, linkFails(syscall.EPERM), c.fail(StatePath(cwd, id)))
		if created || !errors.Is(err, c.want) || len(sessionFiles(cwd)) != 0 {
			t.Errorf("%s: created %v, %v, files %v", c.name, created, err, sessionFiles(cwd))
		}
	}
}

func TestEnsureStateFallbackALimitedFileSizeLeavesNothingAtTheFinalPath(t *testing.T) { // real partial writes (EFBIG), no failing hook
	const id = "fallback-efbig"
	for _, inPlace := range []bool{false, true} {
		cwd, restore := t.TempDir(), func() {}
		lower := func() { restore = limitFileSize(t) }
		link := func(old, created string) error {
			if !inPlace {
				lower() // the temp file of the fallback is the first to be written past the limit
			}
			return &os.LinkError{Op: "link", Old: old, New: created, Err: syscall.EPERM}
		}
		fail := hook(nil)
		if inPlace { // the temp file is written whole, then the publication is unsupported and the in-place file is the one that fails
			fail = func(s ensureStep, path string) error {
				if s != stepPublish {
					return nil
				}
				lower()
				return &os.LinkError{Op: "rename", Err: syscall.EINVAL}
			}
		}
		created, err := ensureStateWith(cwd, id, at, link, fail)
		restore()
		if created || !errors.Is(err, syscall.EFBIG) || len(sessionFiles(cwd)) != 0 {
			t.Errorf("in place %v: created %v, %v, files %v", inPlace, created, err, sessionFiles(cwd))
		}
	}
}

func TestEnsureStateFallbackPublishesTheDefaultStateAndLeavesNoTempFile(t *testing.T) {
	const id = "fallback-ok"
	want := mustEncode(t, defaultState(id, "", at()))
	for _, c := range []struct {
		name  string
		errno syscall.Errno // the no-replace publication's answer; 0 runs the platform's own
	}{{"the platform's own rename", 0}, {"EINVAL", syscall.EINVAL}, {"ENOSYS", syscall.ENOSYS}, {"ENOTSUP", syscall.ENOTSUP}, {"EPERM", syscall.EPERM}} {
		cwd, fail := t.TempDir(), hook(nil)
		if c.errno != 0 {
			fail = unsupported(StatePath(cwd, id), c.errno)
		}
		created, err := ensureStateWith(cwd, id, at, linkFails(syscall.EXDEV), fail)
		if !created || err != nil || fileText(t, StatePath(cwd, id)) != want || !slices.Equal(sessionFiles(cwd), []string{id + ".json"}) {
			t.Errorf("%s: created %v, %v, files %v", c.name, created, err, sessionFiles(cwd))
		}
	}
}

func TestEnsureStateFallbackTakesTheRouteThePlatformOffers(t *testing.T) {
	const id = "fallback-route"
	for _, errno := range []syscall.Errno{0, syscall.EINVAL} {
		if errno == 0 && !noReplaceWorks(t, t.TempDir()) {
			continue // this filesystem has no no-replace rename, so the in-place route is the platform's own
		}
		cwd, calls := t.TempDir(), []string{}
		final := StatePath(cwd, id)
		fail := recording(final, &calls)
		want := []string{"write temp", "sync temp", "close temp", "publish temp"}
		if errno != 0 {
			fail, want = both(fail, unsupported(final, errno)), append(want, "write final", "sync final", "close final")
		}
		if created, err := ensureStateWith(cwd, id, at, linkFails(syscall.EPERM), fail); !created || err != nil || !slices.Equal(calls, want) {
			t.Errorf("rename answer %v: created %v, %v, steps %q, want %q", errno, created, err, calls, want)
		}
	}
}

func TestEnsureStateFallbackNeverTouchesAnExistingFile(t *testing.T) {
	const id = "fallback-existing"
	resumed := DefaultState(id, "resume-me")
	resumed.Phase, resumed.OrchestrationActive = PhaseB, true
	for _, kind := range []string{"valid", "corrupt", "dangling symlink", "directory"} {
		for _, route := range []string{"rename", "unsupported", "failing temp write"} {
			cwd := t.TempDir()
			final := StatePath(cwd, id)
			switch kind {
			case "valid":
				putIn(t, cwd, id, mustEncode(t, resumed))
			case "corrupt":
				putIn(t, cwd, id, "{ not valid json \x00")
			case "directory":
				if err := os.MkdirAll(final, 0o755); err != nil {
					t.Fatal(err)
				}
			default:
				if err := makeSessionsDir(cwd); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(cwd, "nowhere"), final); err != nil {
					t.Fatal(err)
				}
			}
			before, fail := standing(t, final), hook(nil)
			switch route {
			case "unsupported":
				fail = unsupported(final, syscall.EINVAL)
			case "failing temp write": // a resumed session on a full disk is still (false, nil), as the oracle answers
				fail = failing(final, stepWrite, false, syscall.ENOSPC)
			}
			created, err := ensureStateWith(cwd, id, at, linkFails(syscall.EPERM), fail)
			_, nowhere := os.Lstat(filepath.Join(cwd, "nowhere"))
			if created || err != nil || standing(t, final) != before || !slices.Equal(sessionFiles(cwd), []string{id + ".json"}) || !errors.Is(nowhere, fs.ErrNotExist) {
				t.Errorf("%s, %s: created %v, %v, files %v, dangling target %v", kind, route, created, err, sessionFiles(cwd), nowhere)
			}
		}
	}
}

func TestEnsureStateFallbackLosingTheRaceAnswersFalseAndLeavesNoTempFile(t *testing.T) {
	const id = "fallback-lost"
	// another creator publishes its file between our temp file and our publication, through the rename and in place
	for _, inPlace := range []bool{false, true} {
		cwd := t.TempDir()
		final := StatePath(cwd, id)
		other := mustEncode(t, DefaultState(id, "other-creator"))
		fail := func(s ensureStep, path string) error {
			if s != stepPublish {
				return nil
			}
			putIn(t, cwd, id, other)
			if inPlace {
				return &os.LinkError{Op: "rename", Err: syscall.EINVAL}
			}
			return &os.LinkError{Op: "rename", Err: syscall.EEXIST}
		}
		created, err := ensureStateWith(cwd, id, at, linkFails(syscall.EPERM), fail)
		if created || err != nil || fileText(t, final) != other || !slices.Equal(sessionFiles(cwd), []string{id + ".json"}) {
			t.Errorf("in place %v: created %v, %v, files %v", inPlace, created, err, sessionFiles(cwd))
		}
	}
}

func TestRenameNoReplaceMovesToAFreeNameAndRefusesAnyOccupiedOne(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	put := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put(src, "new")
	free := filepath.Join(dir, "free")
	if err := renameNoReplace(src, free); noReplaceUnsupported(err) {
		t.Skip("this platform or filesystem has no rename that refuses to replace: ", err)
	} else if _, statErr := os.Lstat(src); err != nil || fileText(t, free) != "new" || !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("to a free name: %v, source left %v", err, statErr)
	}
	for _, c := range []struct {
		name  string
		setup func(path string)
		same  func(path string) bool
	}{
		{"a file", func(p string) { put(p, "keep") }, func(p string) bool { return fileText(t, p) == "keep" }},
		{"a directory", func(p string) { _ = os.Mkdir(p, 0o755) }, func(p string) bool { i, err := os.Lstat(p); return err == nil && i.IsDir() }},
		{"a dangling symlink", func(p string) { _ = os.Symlink(filepath.Join(dir, "nowhere"), p) }, func(p string) bool {
			target, err := os.Readlink(p)
			_, gone := os.Lstat(filepath.Join(dir, "nowhere"))
			return err == nil && target == filepath.Join(dir, "nowhere") && errors.Is(gone, fs.ErrNotExist)
		}},
	} {
		put(src, "new")
		occupied := filepath.Join(dir, "occupied")
		c.setup(occupied)
		if err := renameNoReplace(src, occupied); !errors.Is(err, fs.ErrExist) || fileText(t, src) != "new" || !c.same(occupied) {
			t.Errorf("onto %s: %v", c.name, err)
		}
		_ = os.RemoveAll(occupied)
	}
}

func TestEnsureStateFallbackKeepsAFileThatReplacedTheOneItCreated(t *testing.T) {
	// another writer's rename replaces the in-place file before the failed write is rolled back: that file is not the fallback's
	cwd, id := t.TempDir(), "fallback-replaced"
	final, other := StatePath(cwd, id), mustEncode(t, DefaultState(id, "other-writer"))
	swap := func(s ensureStep, path string) error {
		if s != stepWrite || path != final {
			return nil
		}
		if err := os.WriteFile(final+".other", []byte(other), 0o644); err != nil {
			return err
		}
		if err := os.Rename(final+".other", final); err != nil {
			return err
		}
		return syscall.ENOSPC
	}
	created, err := ensureStateWith(cwd, id, at, linkFails(syscall.EPERM), both(unsupported(final, syscall.EINVAL), swap))
	if created || !errors.Is(err, syscall.ENOSPC) || fileText(t, final) != other || !slices.Equal(sessionFiles(cwd), []string{id + ".json"}) {
		t.Fatalf("created %v, %v, files %v", created, err, sessionFiles(cwd))
	}
}

func TestWriteNewReportsAFileItCannotRemove(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a read-only directory does not stop root")
	}
	cwd, id := t.TempDir(), "write-new"
	if err := makeSessionsDir(cwd); err != nil {
		t.Fatal(err)
	}
	final := StatePath(cwd, id)
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(final), 0o755) })
	err := writeNew(final, []byte("data"), func(s ensureStep, path string) error {
		if s != stepWrite {
			return nil
		}
		if err := os.Chmod(filepath.Dir(final), 0o500); err != nil { // the file stays, because it cannot be unlinked
			return err
		}
		return syscall.ENOSPC
	})
	if !errors.Is(err, syscall.ENOSPC) || !errors.Is(err, syscall.EACCES) || !slices.Equal(sessionFiles(cwd), []string{id + ".json"}) {
		t.Fatalf("%v, files %v", err, sessionFiles(cwd))
	}
}

func TestEnsureStateFallbackConcurrentCreatorsYieldOneFile(t *testing.T) {
	const id, creators = "fallback-race", 16
	want := mustEncode(t, defaultState(id, "", at()))
	for _, errno := range []syscall.Errno{0, syscall.EINVAL} {
		if errno == 0 && !noReplaceWorks(t, t.TempDir()) {
			continue
		}
		cwd := t.TempDir()
		if err := makeSessionsDir(cwd); err != nil {
			t.Fatal(err)
		}
		final, fail := StatePath(cwd, id), hook(nil)
		if errno != 0 {
			fail = unsupported(final, errno)
		}
		start, ready, done, torn := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan string, 1)
		var readers, creating sync.WaitGroup
		if errno == 0 { // through the rename a reader sees no file or the whole one; in place it may see the file being written
			readers.Add(1)
			go func() {
				defer readers.Done()
				for first := true; ; first = false {
					select {
					case <-done:
						return
					default:
					}
					if b, err := os.ReadFile(final); err == nil && string(b) != want {
						select {
						case torn <- string(b):
						default:
						}
					}
					if first {
						close(ready) // the reader has looked once before any creator starts
					}
				}
			}()
			<-ready
		}
		created, errs := make([]bool, creators), make([]error, creators)
		for i := range creators {
			creating.Add(1)
			go func() {
				defer creating.Done()
				<-start
				created[i], errs[i] = ensureStateWith(cwd, id, at, linkFails(syscall.EPERM), fail)
			}()
		}
		close(start)
		creating.Wait()
		close(done)
		readers.Wait()
		wins := 0
		for i := range creators {
			if created[i] {
				wins++
			}
			if errs[i] != nil {
				t.Errorf("rename answer %v, creator %d: %v", errno, i, errs[i])
			}
		}
		select {
		case b := <-torn:
			t.Errorf("rename answer %v: a reader saw %q", errno, b)
		default:
		}
		if wins != 1 || fileText(t, final) != want || !slices.Equal(sessionFiles(cwd), []string{id + ".json"}) {
			t.Errorf("rename answer %v: %d creators won, files %v", errno, wins, sessionFiles(cwd))
		}
	}
}

func TestEnsureStateLeavesAnExistingStateDirectoryAndIgnoreFileAlone(t *testing.T) { // oracle 111
	cwd := t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, crwdir.DirName), 0o777); err != nil {
		t.Fatal(err)
	}
	ignore := filepath.Join(cwd, crwdir.DirName, ".gitignore")
	if _, err := EnsureState(cwd, "existing-empty"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ignore); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an existing state directory got an ignore file: %v", err)
	}
	if err := os.WriteFile(ignore, []byte("user rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureState(cwd, "existing-ignore"); err != nil || fileText(t, ignore) != "user rules\n" {
		t.Fatalf("%v", err)
	}
}

func TestWriteStateThenReadStateRoundTrips(t *testing.T) {
	marker := func(session, next string) *DcloseRecoveryMarker {
		m := &DcloseRecoveryMarker{SessionID: session, CheckEpoch: "c-1", ClosedWorkPhaseID: "wp-1"}
		if next != "" {
			m.NextWorkPhaseID = &next
		}
		return m
	}
	phaseB := PhaseB
	for _, c := range []struct {
		name  string // the oracle test
		set   func(*State)
		check func(State) bool
	}{
		{"write -> read roundtrip + flags merge (328): the interview flag is derived", func(s *State) { s.Phase, s.Flags.Interview = PhaseP, true },
			func(s State) bool { return s.Phase == PhaseP && !s.Flags.Interview && !s.Flags.AuditPassed }},
		{"stopBlockTurnId round trips (182)", func(s *State) { s.StopBlockTurnID = str("turn-new") }, func(s State) bool { return s.StopBlockTurnID != nil && *s.StopBlockTurnID == "turn-new" }},
		{"stopBlockCapNotified round-trips (194)", func(s *State) { s.StopBlockCapNotified = true }, func(s State) bool { return s.StopBlockCapNotified }},
		{"loopArmSeen/idleEditNudges valid values roundtrip (407)", func(s *State) { s.LoopArmSeen, s.IdleEditNudges = true, 7 }, func(s State) bool { return s.LoopArmSeen && s.IdleEditNudges == 7 }},
		{"injectedTurns roundtrips (454)", func(s *State) { s.InjectedTurns = []string{"t1", "t2"} }, func(s State) bool { return slices.Equal(s.InjectedTurns, []string{"t1", "t2"}) }},
		{"lastInjectedPhase + orchestrationActive roundtrip (489)", func(s *State) { s.Phase, s.LastInjectedPhase, s.OrchestrationActive = PhaseB, &phaseB, true },
			func(s State) bool {
				return s.LastInjectedPhase != nil && *s.LastInjectedPhase == PhaseB && s.OrchestrationActive
			}},
		{"IDLE phase forces orchestrationActive false (523)", func(s *State) { idle := PhaseIdle; s.LastInjectedPhase, s.OrchestrationActive = &idle, true },
			func(s State) bool {
				return s.Phase == PhaseIdle && s.LastInjectedPhase == nil && !s.OrchestrationActive
			}},
		{"a valid D-close marker restores with its IDLE check epoch (707)", func(s *State) { s.CheckEpoch, s.DcloseRecovery = str("c-1"), marker(s.SessionID, "wp-2") },
			func(s State) bool {
				return s.CheckEpoch != nil && *s.CheckEpoch == "c-1" && s.DcloseRecovery != nil && *s.DcloseRecovery.NextWorkPhaseID == "wp-2" && !s.DcloseRecovery.Legacy
			}},
		{"an explicit null successor restores without the legacy flag (786)", func(s *State) { s.CheckEpoch, s.DcloseRecovery = str("c-1"), marker(s.SessionID, "") },
			func(s State) bool {
				return s.DcloseRecovery != nil && s.DcloseRecovery.NextWorkPhaseID == nil && !s.DcloseRecovery.Legacy
			}},
		{"a foreign D-close marker is dropped and cannot retain an IDLE epoch (808)", func(s *State) { s.CheckEpoch, s.DcloseRecovery = str("c-1"), marker("other-session", "wp-2") },
			func(s State) bool { return s.DcloseRecovery == nil && s.CheckEpoch == nil }},
	} {
		cwd, s := t.TempDir(), DefaultState("rt", "")
		c.set(&s)
		if err := WriteState(cwd, s); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got, unreadable := ReadStateStrict(cwd, "rt"); unreadable || !c.check(got) {
			t.Errorf("%s: %+v unreadable=%v", c.name, got, unreadable)
		}
		if names := sessionFiles(cwd); !slices.Equal(names, []string{"rt.json"}) { // writeState: no orphan .tmp (432)
			t.Errorf("%s: files %v", c.name, names)
		}
	}
	// the hand-edited halves of 182 and 194: a malformed value reads as the default
	cwd, s := t.TempDir(), DefaultState("edit", "")
	s.StopBlockTurnID, s.StopBlockCapNotified = str("turn-new"), true
	if err := WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	edited := strings.NewReplacer("\"turn-new\"", "42", "\"stopBlockCapNotified\": true", "\"stopBlockCapNotified\": \"true\"").Replace(fileText(t, StatePath(cwd, "edit")))
	putIn(t, cwd, "edit", edited)
	if got := ReadState(cwd, "edit"); got.StopBlockTurnID != nil || got.StopBlockCapNotified {
		t.Fatalf("%+v", got)
	}
}

func TestSessionIsolationTwoSessionIDsInOneCwdDoNotClobber(t *testing.T) { // oracle 344
	cwd, alpha, beta := t.TempDir(), DefaultState("alpha", ""), DefaultState("beta", "")
	alpha.Phase, beta.Phase = PhaseB, PhaseP
	for _, s := range []State{alpha, beta} {
		if err := WriteState(cwd, s); err != nil {
			t.Fatal(err)
		}
	}
	if a, b := ReadState(cwd, "alpha"), ReadState(cwd, "beta"); a.Phase != PhaseB || b.Phase != PhaseP {
		t.Fatalf("alpha %s beta %s", a.Phase, b.Phase)
	}
}
