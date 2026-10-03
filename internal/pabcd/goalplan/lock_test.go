package goalplan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

func TestLockDiagnosticsAndStatus(t *testing.T) {
	cwd, dir := readWorkspace(t)
	lock := filepath.Join(dir, GoalplanLockDir)
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	owner := filepath.Join(lock, GoalplanLockOwnerFile)
	writeReadFile(t, owner, `{"pid":-1,"token":"old","acquiredAt":"2000-01-01"}`)
	var delays []int
	got, err := WithGoalplanWriteLock(cwd, "demo", func(*Goalplan) (string, error) { t.Fatal("entered held lock"); return "", nil }, &GoalplanWriteLockOptions{Sleep: func(ms int) { delays = append(delays, ms) }})
	if err != nil || got.Kind != "locked" || !reflect.DeepEqual(delays, []int{5, 10, 20, 40}) || !strings.Contains(got.Reason, `"token":"old"`) {
		t.Fatalf("%+v %v %v", got, err, delays)
	}
	for _, raw := range []string{" \n", "{not-json\n"} {
		writeReadFile(t, owner, raw)
		when := time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)
		if err := os.Chtimes(lock, when, when); err != nil {
			t.Fatal(err)
		}
		now := float64(when.UnixMilli() + 2500)
		status, err := GoalplanWriteLockStatus(cwd, "demo", &GoalplanLockStatusOptions{NowMs: &now})
		b, _ := os.ReadFile(owner)
		if err != nil || !status.Exists || status.Path != lock || status.AgeMs == nil || *status.AgeMs != 2500 || string(b) != raw {
			t.Fatalf("%+v %v owner %q", status, err, b)
		}
	}
	now := float64(0)
	s, err := GoalplanWriteLockStatus(cwd, "demo", &GoalplanLockStatusOptions{NowMs: &now})
	if err != nil || s.AgeMs == nil || *s.AgeMs != 0 {
		t.Fatalf("%+v %v", s, err)
	}
	s, err = GoalplanWriteLockStatus(cwd, "demo", &GoalplanLockStatusOptions{Stat: func(path string) (float64, error) { return 0, os.ErrNotExist }})
	if err != nil || s.Exists || s.AgeMs != nil {
		t.Fatalf("vanished: %+v %v", s, err)
	}
	_, err = GoalplanWriteLockStatus(cwd, "demo", &GoalplanLockStatusOptions{Stat: func(string) (float64, error) { return 0, os.ErrPermission }})
	if !errors.Is(err, os.ErrPermission) {
		t.Fatal(err)
	}
}

func TestLockReleaseAndMetadata(t *testing.T) {
	cwd, dir := readWorkspace(t)
	lock := filepath.Join(dir, GoalplanLockDir)
	now := "2026-01-01T00:00:00.000Z"
	_, err := WithGoalplanWriteLock(cwd, "demo", func(*Goalplan) (int, error) {
		b, e := os.ReadFile(filepath.Join(lock, GoalplanLockOwnerFile))
		if e != nil || !strings.Contains(string(b), now) {
			t.Fatalf("owner %s %v", b, e)
		}
		info, e := os.Stat(filepath.Join(lock, GoalplanLockOwnerFile))
		if e != nil || info.Mode().Perm() != 0o600 {
			t.Fatal("owner mode", e)
		}
		return 0, errors.New("callback failed")
	}, &GoalplanWriteLockOptions{Now: func() string { return now }})
	if err == nil {
		t.Fatal("callback error swallowed")
	}
	if _, e := os.Stat(lock); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("lock retained", e)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic swallowed")
			}
		}()
		_, _ = WithGoalplanWriteLock(cwd, "demo", func(*Goalplan) (int, error) { panic("callback") }, nil)
	}()
	writeReadFile(t, filepath.Join(dir, GoalplanFile), "{not-json")
	got, err := WithGoalplanWriteLock(cwd, "demo", func(*Goalplan) (int, error) { t.Fatal("unreadable callback"); return 0, nil }, nil)
	if err != nil || got.Kind != "unreadable" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, e := os.Stat(lock); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("lock retained", e)
	}
}

func TestLockRefusesLinksAndDoesNotReadOwnerTarget(t *testing.T) {
	for _, kind := range []string{"lock", "dangling-lock", "owner"} {
		t.Run(kind, func(t *testing.T) {
			cwd, dir := readWorkspace(t)
			outside := t.TempDir()
			lock := filepath.Join(dir, GoalplanLockDir)
			writeReadFile(t, filepath.Join(outside, GoalplanLockOwnerFile), "outside-secret")
			if kind == "owner" {
				if e := os.Mkdir(lock, 0o700); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink(filepath.Join(outside, GoalplanLockOwnerFile), filepath.Join(lock, GoalplanLockOwnerFile)); e != nil {
					t.Fatal(e)
				}
			} else {
				target := outside
				if kind == "dangling-lock" {
					target = filepath.Join(outside, "missing")
				}
				if e := os.Symlink(target, lock); e != nil {
					t.Fatal(e)
				}
			}
			got, err := WithGoalplanWriteLock(cwd, "demo", func(*Goalplan) (int, error) { t.Fatal("callback"); return 0, nil }, &GoalplanWriteLockOptions{RetryDelaysMs: []int{}})
			if kind == "owner" {
				if err != nil || got.Kind != "locked" || strings.Contains(got.Reason, "outside-secret") || !strings.Contains(got.Reason, "unavailable") {
					t.Fatalf("%+v %v", got, err)
				}
			} else {
				if err == nil && got.Kind != "unreadable" {
					t.Fatalf("%+v %v", got, err)
				}
				if _, e := GoalplanWriteLockDir(cwd, "demo"); e == nil {
					t.Fatal("unsafe lock path")
				}
			}
			b, _ := os.ReadFile(filepath.Join(outside, GoalplanLockOwnerFile))
			if string(b) != "outside-secret" {
				t.Fatal("outside changed")
			}
		})
	}
}

func helperWait(t *testing.T, paths ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, p := range paths {
			if _, e := os.Stat(p); e == nil {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("waiting for %v", paths)
}
func TestGoalplanLockProcess(t *testing.T) {
	mode, cwd := os.Getenv("CRW_GOALPLAN_LOCK_HELPER"), os.Getenv("CRW_GOALPLAN_LOCK_CWD")
	if mode == "" {
		t.Skip("owned helper process")
	}
	who := "b"
	if mode == "holder" {
		who = "a"
	}
	signal := func(name string) { writeReadFile(t, filepath.Join(cwd, name), who) }
	var delays []int
	opts := &GoalplanWriteLockOptions{Sleep: func(ms int) {
		delays = append(delays, ms)
		signal("contended")
		if mode == "handoff" {
			helperWait(t, filepath.Join(cwd, "a-done"))
		}
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}}
	got, err := WithGoalplanWriteLock(cwd, "demo", func(p *Goalplan) (string, error) {
		signal(who + "-active")
		defer os.Remove(filepath.Join(cwd, who+"-active"))
		if who == "b" {
			if _, e := os.Stat(filepath.Join(cwd, "a-active")); e == nil {
				signal("overlap")
			}
		}
		p.WorkPhases = append(p.WorkPhases, GoalplanWorkPhase{ID: "wp-" + who, Title: who, Status: WorkPhasePending, Tasks: []GoalplanTask{}, CriteriaIDs: []string{}})
		data, e := json.Marshal(p)
		if e != nil {
			return "", e
		}
		dir, e := GoalplanDir(cwd, "demo")
		if e != nil {
			return "", e
		}
		if e = crwdir.Publish(filepath.Join(dir, GoalplanFile), data); e != nil {
			return "", e
		}
		signal(who + "-entered")
		if who == "a" {
			helperWait(t, filepath.Join(cwd, "release"))
		}
		return who, nil
	}, opts)
	if err != nil {
		t.Fatal(err)
	}
	signal(who + "-done")
	_ = json.NewEncoder(os.Stdout).Encode(struct {
		Result GoalplanWriteLockResult[string]
		Delays []int
	}{got, delays})
	os.Exit(0)
}

type lockChild struct {
	out  bytes.Buffer
	done chan struct{}
	err  error
}

func startLockChild(t *testing.T, cwd, mode string) *lockChild {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c := &lockChild{done: make(chan struct{})}
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGoalplanLockProcess$")
	cmd.Env = append(os.Environ(), "CRW_GOALPLAN_LOCK_HELPER="+mode, "CRW_GOALPLAN_LOCK_CWD="+cwd)
	cmd.Stdout = &c.out
	cmd.Stderr = &c.out
	if e := cmd.Start(); e != nil {
		cancel()
		t.Fatal(e)
	}
	go func() { c.err = cmd.Wait(); close(c.done) }()
	t.Cleanup(func() { cancel(); <-c.done }) // CommandContext kills this exact owned pid; Wait reaps it.
	return c
}
func (c *lockChild) result(t *testing.T) (string, []int) {
	t.Helper()
	<-c.done
	if c.err != nil {
		t.Fatalf("child: %v %s", c.err, c.out.String())
	}
	var r struct {
		Result GoalplanWriteLockResult[string]
		Delays []int
	}
	if e := json.Unmarshal(c.out.Bytes(), &r); e != nil {
		t.Fatalf("%v %s", e, c.out.String())
	}
	return r.Result.Kind, r.Delays
}
func TestLockRealProcesses(t *testing.T) {
	for _, mode := range []string{"handoff", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			cwd, _ := readWorkspace(t)
			a := startLockChild(t, cwd, "holder")
			helperWait(t, filepath.Join(cwd, "a-entered"))
			b := startLockChild(t, cwd, mode)
			if mode == "handoff" {
				helperWait(t, filepath.Join(cwd, "contended"), filepath.Join(cwd, "b-entered"))
				writeReadFile(t, filepath.Join(cwd, "release"), "go")
			}
			kind, delays := b.result(t)
			if mode == "timeout" {
				writeReadFile(t, filepath.Join(cwd, "release"), "go")
				if kind != "locked" || !reflect.DeepEqual(delays, []int{5, 10, 20, 40}) {
					t.Fatalf("kind %s delays %v", kind, delays)
				}
			} else if kind != "ok" {
				t.Fatal(kind)
			}
			if k, _ := a.result(t); k != "ok" {
				t.Fatal(k)
			}
			if _, e := os.Stat(filepath.Join(cwd, "overlap")); !errors.Is(e, os.ErrNotExist) {
				t.Fatal("writers overlap")
			}
			p := ReadGoalplan(cwd, "demo")
			want := 1
			if mode == "handoff" {
				want = 2
			}
			if p == nil || len(p.WorkPhases) != want {
				t.Fatalf("lost update: %+v", p)
			}
			if mode == "timeout" {
				if _, e := os.Stat(filepath.Join(cwd, "b-entered")); !errors.Is(e, os.ErrNotExist) {
					t.Fatal("timeout entered callback")
				}
			}
		})
	}
}
