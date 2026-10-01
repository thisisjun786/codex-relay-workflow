package pointer_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func TestMain(m *testing.M) {
	golden.Helper()
	testsupport.Main(m)
}

func dirs(t *testing.T, names ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range names {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func is(v *bool, want bool) bool { return v != nil && *v == want }

// place replaces a link and never appends one; names answers for the environment the link
// resolves into; remove takes away only a pointer that still names what its caller placed, and
// reads the absence back.
func TestPlaceNamesRemove(t *testing.T) {
	root := dirs(t, "envA", "envB")
	path := pointer.Path(root)
	if got := pointer.Read(path); got.State != pointer.NoPointer || !pointer.Usable(got.State) {
		t.Fatalf("before placing: %+v", got)
	}
	if !is(pointer.Names(path, filepath.Join(root, "envA")), false) {
		t.Fatal("no pointer is an established no, not an unknown")
	}
	if err := pointer.Place(path, filepath.Join(root, "envA")); err != nil {
		t.Fatal(err)
	}
	if got := pointer.Read(path); got.State != pointer.Link || got.Target != filepath.Join(root, "envA") || got.Detail != "the pointer names "+got.Target {
		t.Fatalf("after placing: %+v", got)
	}
	if !is(pointer.Names(path, filepath.Join(root, "envA")), true) || !is(pointer.Names(path, filepath.Join(root, "envB")), false) {
		t.Fatal("the pointer names exactly its target")
	}
	if err := pointer.Place(path, filepath.Join(root, "envB")); err != nil {
		t.Fatal(err)
	}
	if !is(pointer.Names(path, filepath.Join(root, "envB")), true) {
		t.Fatal("a pointer is repointed, never appended to")
	}
	if ok, why := pointer.Remove(path, filepath.Join(root, "envA")); ok || !strings.Contains(why, "is not this run's to remove") {
		t.Fatalf("a pointer moved by another run was removed: %v %s", ok, why)
	}
	if ok, why := pointer.Remove(path, filepath.Join(root, "envB")); !ok || why != "the pointer this run placed was removed and its absence was read back" {
		t.Fatalf("removing this run's pointer: %v %s", ok, why)
	}
	if ok, why := pointer.Remove(path, filepath.Join(root, "envB")); !ok || why != "there is no pointer here, which is the state this restores to" {
		t.Fatalf("removing an absent pointer: %v %s", ok, why)
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".crw-pointer-") {
			t.Fatalf("a temporary link was left: %s", e.Name())
		}
	}
}

// Somebody's real directory where the pointer goes is not a pointer: never placed over,
// never removed.
func TestARealDirectoryIsNotAPointer(t *testing.T) {
	root := dirs(t, "current/inside", "env")
	path := pointer.Path(root)
	got := pointer.Read(path)
	if got.State != pointer.NotALink || pointer.Usable(got.State) {
		t.Fatalf("a real directory: %+v", got)
	}
	var refused *pointer.RefusedError
	if err := pointer.Place(path, filepath.Join(root, "env")); !errors.As(err, &refused) || refused.Answer.State != pointer.NotALink {
		t.Fatalf("a real directory was not refused as NOT_A_LINK: %v", err)
	}
	if ok, _ := pointer.Remove(path, filepath.Join(root, "env")); ok {
		t.Fatal("a real directory was removed")
	}
	if _, err := os.Stat(filepath.Join(path, "inside")); err != nil {
		t.Fatal("the real directory's content is gone")
	}
	if !is(pointer.Names(path, filepath.Join(root, "env")), false) {
		t.Fatal("a real directory names nothing, and that is established")
	}
}

// A regular file where the pointer goes is refused by Place itself, before anything is written:
// a rename would replace it as readily as it replaces a link.
func TestARegularFileIsNeverPlacedOver(t *testing.T) {
	root := dirs(t, "env")
	path := pointer.Path(root)
	if err := os.WriteFile(path, []byte("somebody's file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := pointer.Read(path); got.State != pointer.NotALink {
		t.Fatalf("a regular file: %+v", got)
	}
	err := pointer.Place(path, filepath.Join(root, "env"))
	var refused *pointer.RefusedError
	if !errors.As(err, &refused) || refused.Answer.State != pointer.NotALink || refused.Path != path || !strings.Contains(err.Error(), pointer.NotALink) {
		t.Errorf("placing over a regular file was not refused as NOT_A_LINK: %v", err)
	}
	if got := pointer.Read(path); got.State != pointer.NotALink {
		t.Errorf("the regular file was replaced: %+v", got)
	}
	if content, err := os.ReadFile(path); err != nil || string(content) != "somebody's file\n" {
		t.Errorf("the regular file's content is gone: %q %v", content, err)
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".crw-pointer-") {
			t.Errorf("a temporary link was left: %s", e.Name())
		}
	}
}

// A pointer that could not be read answers neither yes nor no.
func TestAnUnreachablePointerNamesNothingEitherWay(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads through a directory without search permission")
	}
	root := dirs(t, "locked", "env")
	path := filepath.Join(root, "locked", "current")
	if err := os.Chmod(filepath.Join(root, "locked"), 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(root, "locked"), 0o755)
	got := pointer.Read(path)
	if got.State != pointer.Unreachable || pointer.Usable(got.State) || !strings.Contains(got.Detail, "permission denied") {
		t.Fatalf("behind a directory without search permission: %+v", got)
	}
	if pointer.Names(path, filepath.Join(root, "env")) != nil {
		t.Fatal("an unread pointer said what it names")
	}
	var refused *pointer.RefusedError
	if err := pointer.Place(path, filepath.Join(root, "env")); !errors.As(err, &refused) || refused.Answer.State != pointer.Unreachable {
		t.Fatalf("placing over an unread path was not refused as UNREACHABLE: %v", err)
	}
}

// A relative target resolves against the link's own directory, following links before "..".
func TestARelativeTargetResolvesFromTheLinkDirectory(t *testing.T) {
	root := dirs(t, "rt/envA", "elsewhere/envB", "elsewhere/sub")
	if err := os.Symlink(filepath.Join(root, "elsewhere", "sub"), filepath.Join(root, "rt", "hop")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "rt", "current")
	if err := pointer.Place(path, "envA"); err != nil {
		t.Fatal(err)
	}
	if !is(pointer.Names(path, filepath.Join(root, "rt", "envA")), true) {
		t.Fatal("a relative target is relative to the link's directory")
	}
	if err := pointer.Place(path, "hop/../envB"); err != nil {
		t.Fatal(err)
	}
	if !is(pointer.Names(path, filepath.Join(root, "elsewhere", "envB")), true) {
		t.Fatal("hop is followed before .. is applied")
	}
}

// Readers never see the pointer missing while it is repointed.
func TestRepointingNeverLeavesTheLinkMissing(t *testing.T) {
	root := dirs(t, "envA", "envB")
	path := pointer.Path(root)
	if err := pointer.Place(path, filepath.Join(root, "envA")); err != nil {
		t.Fatal(err)
	}
	var missing atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if pointer.Read(path).State != pointer.Link {
				missing.Add(1)
			}
		}
	}()
	for i := 0; i < 300; i++ {
		target := "envA"
		if i%2 == 1 {
			target = "envB"
		}
		if err := pointer.Place(path, filepath.Join(root, target)); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	if missing.Load() != 0 {
		t.Fatalf("a reader saw no pointer %d times", missing.Load())
	}
}
