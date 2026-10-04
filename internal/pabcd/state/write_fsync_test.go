package state

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

func TestEnsureStateFileSyncFailurePreventsPublication(t *testing.T) {
	cwd, id, published, synced := t.TempDir(), "sync-create", false, false
	link := func(tmp, final string) error { published = true; return os.Link(tmp, final) }
	failSync := func(f *os.File) error {
		synced = true
		if !strings.HasSuffix(f.Name(), ".tmp") {
			t.Errorf("file sync on %q", f.Name())
		}
		return syscall.EIO
	}
	created, err := ensureStateWith(cwd, id, at, link, nil, failSync)
	if created || !errors.Is(err, syscall.EIO) || published || !synced {
		t.Fatalf("created=%v err=%v published=%v synced=%v", created, err, published, synced)
	}
	if _, err := os.Lstat(StatePath(cwd, id)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("final path after failed sync: %v", err)
	}
	if files := sessionFiles(cwd); len(files) != 0 {
		t.Fatalf("files after failed sync: %v", files)
	}
}

func TestWriteStateFileSyncFailurePreservesFinalPath(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "replacement"}[existing], func(t *testing.T) {
			cwd, id, published, synced := t.TempDir(), "sync-write", false, false
			const previous = "previous state bytes stay intact"
			if existing {
				putIn(t, cwd, id, previous)
			}
			rename := func(tmp, final string) error { published = true; return crwdir.Rename(tmp, final) }
			failSync := func(f *os.File) error { synced = true; return syscall.EIO }
			err := writeState(cwd, defaultState(id, "next", at()), at(), rename, failSync)
			if !errors.Is(err, syscall.EIO) || published || !synced {
				t.Fatalf("err=%v published=%v synced=%v", err, published, synced)
			}
			if existing {
				if got := fileText(t, StatePath(cwd, id)); got != previous {
					t.Fatalf("previous bytes changed: %q", got)
				}
			} else if _, err := os.Lstat(StatePath(cwd, id)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("final path after failed sync: %v", err)
			}
			if files := tempFiles(cwd); len(files) != 0 {
				t.Fatalf("temp files after failed sync: %v", files)
			}
		})
	}
}

func TestStatePublicationSyncsTheFileBeforeAndDirectoryAfter(t *testing.T) {
	for _, ensure := range []bool{true, false} {
		t.Run(map[bool]string{true: "link", false: "rename"}[ensure], func(t *testing.T) {
			cwd, id, calls := t.TempDir(), "sync-order", []string{}
			final := StatePath(cwd, id)
			const previous = "previous state"
			if !ensure {
				putIn(t, cwd, id, previous)
			}
			checkSync := func(f *os.File) error {
				info, err := f.Stat()
				if err != nil {
					return err
				}
				path := f.Name()
				if info.IsDir() {
					calls = append(calls, "directory")
					if path != filepath.Dir(final) || ReadState(cwd, id).SessionID != id {
						t.Errorf("directory sync before publication or on wrong path: %q", path)
					}
				} else {
					calls = append(calls, "file")
					if !info.Mode().IsRegular() || !strings.HasPrefix(path, final+".") || !strings.HasSuffix(path, ".tmp") {
						t.Errorf("file sync on wrong descriptor: %q %v", path, info.Mode())
					}
					var staged map[string]any
					if err := json.Unmarshal([]byte(fileText(t, path)), &staged); err != nil || staged["sessionId"] != id || staged["phase"] != "IDLE" {
						t.Errorf("incomplete staged state: %v %v", staged, err)
					}
					if ensure {
						if _, err := os.Lstat(final); !errors.Is(err, fs.ErrNotExist) {
							t.Errorf("publication before file sync: %v", err)
						}
					} else if got := fileText(t, final); got != previous {
						t.Errorf("replacement before file sync: %q", got)
					}
				}
				return f.Sync()
			}
			if created, err := runPrimaryPublication(cwd, id, ensure, checkSync, &calls); !created || err != nil {
				t.Fatalf("created=%v err=%v", created, err)
			}
			if want := []string{"file", "publish", "directory"}; !slices.Equal(calls, want) {
				t.Fatalf("calls=%v want=%v", calls, want)
			}
			if files := sessionFiles(cwd); !slices.Equal(files, []string{id + ".json"}) {
				t.Fatalf("files after publication: %v", files)
			}
		})
	}
}

func TestStateDirectorySyncFailureReturnsErrorAndKeepsPublication(t *testing.T) {
	for _, ensure := range []bool{true, false} {
		t.Run(map[bool]string{true: "link", false: "rename"}[ensure], func(t *testing.T) {
			cwd, id, calls := t.TempDir(), "sync-directory", []string{}
			failDirectory := func(f *os.File) error {
				info, err := f.Stat()
				if err != nil {
					return err
				}
				if info.IsDir() {
					return syscall.EIO
				}
				return f.Sync()
			}
			created, err := runPrimaryPublication(cwd, id, ensure, failDirectory, &calls)
			if (ensure && !created) || !errors.Is(err, syscall.EIO) || ReadState(cwd, id).SessionID != id {
				t.Fatalf("created=%v err=%v state=%+v", created, err, ReadState(cwd, id))
			}
			if files := sessionFiles(cwd); !slices.Equal(files, []string{id + ".json"}) {
				t.Fatalf("files after directory sync failure: %v", files)
			}
		})
	}
}

// runPrimaryPublication adds a publication observation to the existing link/rename seams.
func runPrimaryPublication(cwd, id string, ensure bool, syncFile func(*os.File) error, calls *[]string) (bool, error) {
	publish := func(tmp, final string) error {
		*calls = append(*calls, "publish")
		if ensure {
			return os.Link(tmp, final)
		}
		return crwdir.Rename(tmp, final)
	}
	if ensure {
		return ensureStateWith(cwd, id, at, publish, nil, syncFile)
	}
	err := writeState(cwd, defaultState(id, "next", at()), at(), publish, syncFile)
	return err == nil, err
}
