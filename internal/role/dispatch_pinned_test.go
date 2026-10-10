package role

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests swap a directory or a record in the window between its check and its use, through the after
// callback the pinned walk takes as a parameter (no package-level variable). Points: "crw", "dispatches" and
// "session" (after the component was checked, before it is opened), "save" (before the temporary record is
// created), "sibling" (after the created check listed the records, before one is read) and "read" (after the
// reader looked at a record, before it opens it).

const dispatchPinnedRefusal = "dispatch directory must not be a symlink"

// dispatchPinnedOutside is a directory outside the workspace that holds one file a run must not touch.
func dispatchPinnedOutside(t *testing.T) string {
	t.Helper()
	outside := t.TempDir()
	check(t, os.WriteFile(filepath.Join(outside, "sentinel"), []byte("untouched"), 0o600))
	return outside
}

func dispatchPinnedNames(t *testing.T, dir string) []string {
	t.Helper()
	var names []string
	for _, e := range must(os.ReadDir(dir)) {
		names = append(names, e.Name())
	}
	return names
}

func dispatchPinnedInput(action string, fields map[string]any) map[string]any {
	b := map[string]any{"action": action, "sessionId": "session-test", "dispatchId": "task-test"}
	for k, v := range fields {
		b[k] = v
	}
	return b
}

// Cases 1 and 2, and the other ways a checked component can stop being the checked directory: the walk is refused
// and nothing is created where the replaced name now leads.
func TestDispatchPinnedRefusesSwapAfterCheck(t *testing.T) {
	for _, point := range []string{"crw", "dispatches", "session"} {
		for _, kind := range []string{"outside link", "inside link", "other directory", "named pipe", "link to the moved original"} {
			t.Run(point+"/"+kind, func(t *testing.T) {
				ws, outside := t.TempDir(), dispatchPinnedOutside(t)
				env, _ := home(t)
				inside := filepath.Join(ws, "inside")
				check(t, os.Mkdir(inside, 0o700))
				target := filepath.Join(ws, ".crw")
				if point != "crw" {
					target = filepath.Join(target, "dispatches")
				}
				if point == "session" {
					target = filepath.Join(target, "session-test")
				}
				moved, original, swapped := target+".moved", []string(nil), false
				after := func(p string) {
					if p != point {
						return
					}
					swapped, original = true, dispatchPinnedNames(t, target)
					check(t, os.Rename(target, moved))
					switch kind {
					case "outside link":
						check(t, os.Symlink(outside, target))
					case "inside link":
						check(t, os.Symlink(must(filepath.Rel(filepath.Dir(target), inside)), target))
					case "other directory":
						check(t, os.Mkdir(target, 0o700))
					case "named pipe":
						check(t, syscall.Mkfifo(target, 0o600))
					default:
						check(t, os.Symlink(filepath.Base(moved), target))
					}
				}
				_, err := dispatchPinnedRun(ws, dispatchPinnedInput("start", map[string]any{"role": "executor"}), env, nil, after)
				if !swapped {
					t.Fatal("the swap point was not reached")
				}
				if err == nil || err.Error() != dispatchPinnedRefusal {
					t.Fatalf("error %v, want %q", err, dispatchPinnedRefusal)
				}
				// Where the swapped name leads, and the directory that was checked, are as they were.
				for dir, want := range map[string][]string{outside: {"sentinel"}, inside: nil} {
					if got := dispatchPinnedNames(t, dir); !slices.Equal(got, want) {
						t.Fatalf("%s holds %v, want %v", dir, got, want)
					}
				}
				if kind == "other directory" && len(dispatchPinnedNames(t, target)) != 0 {
					t.Fatalf("the replacing directory holds %v", dispatchPinnedNames(t, target))
				}
				if got := dispatchPinnedNames(t, moved); !slices.Equal(got, original) {
					t.Fatalf("the checked directory holds %v, want %v", got, original)
				}
			})
		}
	}
}

// Case 3: the stopped close takes its own pinned directory (the hook reaches it, not the status pre-call of
// RunDispatch); case 4 is its run without a swap.
func TestDispatchPinnedStoppedCloseRefusesSwap(t *testing.T) {
	for _, swap := range []bool{true, false} {
		t.Run(map[bool]string{true: "swap", false: "no swap"}[swap], func(t *testing.T) {
			ws, env, start, file := dispatchTestFixture(t)
			dispatchTestClaimIssued(t, ws, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
			dispatchTestCall(t, ws, env, map[string]any{"action": "report", "attemptId": start.AttemptID, "outcome": "created", "agentId": "child-a"})
			session, outside := filepath.Dir(file), dispatchPinnedOutside(t)
			// The record the close would rewrite if it followed the link.
			record := must(os.ReadFile(file))
			check(t, os.WriteFile(filepath.Join(outside, "task-test.json"), record, 0o600))
			input := createdCheckInput(start.AttemptID, "stopped")
			input["executionState"], input["reconciliation"] = "stopped", "child stopped; partial work inspected"
			after := func(p string) {
				if swap && p == "session" {
					check(t, os.Rename(session, session+".moved"))
					check(t, os.Symlink(outside, session))
				}
			}
			h := &createdCheckFake{reply: createdCheckReply("session-test", "subagent", "idle")}
			out, err := createdCheckStop(context.Background(), ws, input, env, h, after)
			if !swap {
				check(t, err)
				if stored := must(dispatchRead(file, "session-test", "task-test")); out.Action != "stop" || stored.Status != "stopped" {
					t.Fatalf("close = %+v, stored %v", out, stored.Status)
				}
				if names := dispatchPinnedNames(t, session); !slices.Equal(names, []string{"task-test.json"}) {
					t.Fatalf("temporary file or lock left behind: %v", names)
				}
				return
			}
			if err == nil || err.Error() != dispatchPinnedRefusal {
				t.Fatalf("error %v, want %q", err, dispatchPinnedRefusal)
			}
			if string(must(os.ReadFile(filepath.Join(outside, "task-test.json")))) != string(record) || !slices.Equal(dispatchPinnedNames(t, outside), []string{"sentinel", "task-test.json"}) {
				t.Fatal("the close changed the directory the swapped name leads to")
			}
			if string(must(os.ReadFile(filepath.Join(session+".moved", "task-test.json")))) != string(record) {
				t.Fatal("a refused close changed the checked record")
			}
		})
	}
}

// Case 4: nothing is swapped, so a claim answers, saves and cleans up as it did before, and a held lock still
// fails closed with the same words.
func TestDispatchPinnedWithoutSwapBehavesAsBefore(t *testing.T) {
	ws, env, start, file := dispatchTestFixture(t)
	var points []string
	claim := dispatchPinnedInput("claim", map[string]any{"attemptId": start.AttemptID})
	check(t, os.Mkdir(file+".lock", 0o700))
	_, err := dispatchPinnedRun(ws, claim, env, nil, nil)
	if !errors.Is(err, fs.ErrExist) || err.Error() != "mkdir "+file+".lock: file exists" {
		t.Fatalf("held lock: %v", err)
	}
	check(t, os.Remove(file+".lock"))
	out, err := dispatchPinnedRun(ws, claim, env, nil, func(p string) { points = append(points, p) })
	check(t, err)
	if out.Action != "spawn" || !slices.Equal(points, []string{"crw", "dispatches", "session", "read", "save"}) {
		t.Fatalf("answer %q after points %v", out.Action, points)
	}
	if stored := must(dispatchRead(file, "session-test", "task-test")); !stored.Attempts[0].Claimed || !dispatchIs(stored.Attempts[0].Status, "claimed") {
		t.Fatal("the claim was not saved")
	}
	if names := dispatchPinnedNames(t, filepath.Dir(file)); !slices.Equal(names, []string{"task-test.json"}) {
		t.Fatalf("temporary file or lock left behind: %v", names)
	}
}

// Cases 5 and 6: the session directory is moved aside and replaced by a link to a directory that already holds a
// record, and a lock, of the same names, after the record was read and before the temporary file is created. The
// save and the lock cleanup stay in the directory the lock was taken in.
func TestDispatchPinnedSaveStaysInTheCheckedDirectory(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(map[bool]string{false: "record", true: "record and lock"}[held], func(t *testing.T) {
			ws, env, start, file := dispatchTestFixture(t)
			session, outside := filepath.Dir(file), t.TempDir()
			check(t, os.WriteFile(filepath.Join(outside, "task-test.json"), []byte("outside record"), 0o600))
			want := []string{"task-test.json"}
			if held {
				check(t, os.Mkdir(filepath.Join(outside, "task-test.json.lock"), 0o700))
				check(t, os.WriteFile(filepath.Join(outside, "task-test.json.lock", "owner"), []byte("another run"), 0o600))
				want = append(want, "task-test.json.lock")
			}
			after := func(p string) {
				if p == "save" {
					check(t, os.Rename(session, session+".moved"))
					check(t, os.Symlink(outside, session))
				}
			}
			out, err := dispatchPinnedRun(ws, dispatchPinnedInput("claim", map[string]any{"attemptId": start.AttemptID}), env, nil, after)
			check(t, err)
			if out.Action != "spawn" {
				t.Fatalf("answer %q", out.Action)
			}
			if string(must(os.ReadFile(filepath.Join(outside, "task-test.json")))) != "outside record" {
				t.Fatal("the record the swapped name leads to was overwritten")
			}
			if got := dispatchPinnedNames(t, outside); !slices.Equal(got, want) {
				t.Fatalf("directory the swapped name leads to holds %v, want %v", got, want)
			}
			if held && string(must(os.ReadFile(filepath.Join(outside, "task-test.json.lock", "owner")))) != "another run" {
				t.Fatal("the lock the swapped name leads to was removed")
			}
			moved := session + ".moved"
			if !must(dispatchRead(filepath.Join(moved, "task-test.json"), "session-test", "task-test")).Attempts[0].Claimed {
				t.Fatal("the claim was not saved where the lock was taken")
			}
			if names := dispatchPinnedNames(t, moved); !slices.Equal(names, []string{"task-test.json"}) {
				t.Fatalf("temporary file or lock left in the checked directory: %v", names)
			}
		})
	}
}

// Case 7: a record that is a regular file when the created check lists the directory (or when the reader looks at
// it) and a named pipe when it is opened is refused as unusable, and the dispatch lock is released.
func TestDispatchPinnedSiblingTurnedIntoPipe(t *testing.T) {
	for _, point := range []string{"sibling", "read"} {
		t.Run(point, func(t *testing.T) {
			ws := t.TempDir()
			env, _ := home(t)
			env, _ = createdArchivedHost(t, env, "session-test", "", 1)
			one := createdArchivedSession(t, ws, env, "task-one")
			stray, record := createdArchivedRecord(ws, "a-stray"), createdArchivedRecord(ws, "task-one")
			check(t, os.WriteFile(stray, []byte("{}"), 0o600))
			before := must(os.ReadFile(record))
			swapped, reads := false, 0
			after := func(p string) {
				if p == "read" {
					reads++ // the first read is the dispatch's own record, the second the first sibling
				}
				if (p == "sibling" && point == "sibling" || p == "read" && point == "read" && reads == 2) && !swapped {
					swapped = true
					_ = os.Remove(stray)
					_ = syscall.Mkfifo(stray, 0o600)
				}
			}
			done := make(chan error, 1)
			go func() {
				_, err := dispatchPinnedChecked(context.Background(), ws, createdArchivedReport("task-one", one, "child-a"), env, nil, after)
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "dispatch record a-stray.json is unusable") {
					t.Fatalf("error %v", err)
				}
			case <-time.After(5 * time.Second):
				// Release a reader that is blocked on the pipe, so a failure leaves no goroutine behind.
				if f, err := os.OpenFile(stray, os.O_RDWR, 0); err == nil {
					_ = f.Close()
				}
				t.Fatal("the created check blocked on a sibling record")
			}
			if info := must(os.Lstat(stray)); !swapped || info.Mode()&os.ModeNamedPipe == 0 {
				t.Fatal("the record was not swapped for a pipe")
			}
			if string(before) != string(must(os.ReadFile(record))) {
				t.Fatal("a refused report changed the ledger")
			}
			if _, err := os.Lstat(record + ".lock"); !os.IsNotExist(err) {
				t.Fatalf("dispatch lock left behind: %v", err)
			}
		})
	}
}

// Case 8: a record replaced after it was looked at by a relative link to itself (which resolves to the very file
// that was checked) is still refused as a link.
func TestDispatchPinnedRecordSwappedForLinkToItself(t *testing.T) {
	ws, env, start, file := dispatchTestFixture(t)
	before := must(os.ReadFile(file))
	after := func(p string) {
		if p == "read" {
			check(t, os.Rename(file, file+".moved"))
			check(t, os.Symlink(filepath.Base(file)+".moved", file))
		}
	}
	_, err := dispatchPinnedRun(ws, dispatchPinnedInput("claim", map[string]any{"attemptId": start.AttemptID}), env, nil, after)
	if err == nil || err.Error() != "dispatch state must not be a symlink" {
		t.Fatalf("error %v", err)
	}
	if string(must(os.ReadFile(file+".moved"))) != string(before) || !slices.Equal(dispatchPinnedNames(t, filepath.Dir(file)), []string{"task-test.json", "task-test.json.moved"}) {
		t.Fatal("a refused claim changed the directory")
	}
}

// A rename that fails still reads as the path operation it replaces, with the checked paths.
func TestDispatchPinnedRenameFailureNamesThePaths(t *testing.T) {
	ws, env, start, file := dispatchTestFixture(t)
	var temp string
	_, err := dispatchRun(ws, dispatchPinnedInput("claim", map[string]any{"attemptId": start.AttemptID}), env, func(tmp, final string) error {
		temp = tmp
		check(t, os.Remove(final))
		return os.Mkdir(final, 0o700)
	})
	var link *os.LinkError
	if !errors.As(err, &link) || link.Op != "rename" || link.Old != temp || link.New != file {
		t.Fatalf("error %v", err)
	}
}

// A record that is a directory or a named pipe is named for what it is, and a pipe does not block the read.
func TestDispatchPinnedRecordOfAnotherKind(t *testing.T) {
	for _, kind := range []string{"directory", "named pipe"} {
		t.Run(kind, func(t *testing.T) {
			ws, env, _, file := dispatchTestFixture(t)
			check(t, os.Remove(file))
			want := "read " + file + ": is a directory"
			if kind == "directory" {
				check(t, os.Mkdir(file, 0o700))
			} else {
				check(t, syscall.Mkfifo(file, 0o600))
				want = "dispatch state must be a regular file"
			}
			dispatchTestError(t, ws, env, dispatchPinnedInput("status", nil), want)
		})
	}
}
