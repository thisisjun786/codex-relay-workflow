package role

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// The oracle publishes with no lock: two writers that read the same store both publish a replacement of it, and the second rename
// discards the first writer's update. A is held between its read and its rename; B, which changes another role, must meet A's lock.
func TestLockedWritersLoseNoUpdate(t *testing.T) { // criterion c9
	env, dir := home(t)
	must(SetRole(env, Executor, RolePatch{Effort: Some(EffortLow)}))
	inRename, releaseA, bMet, aDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseA) })
	defer release()
	errs := make(chan error, 2)
	go func() {
		_, err := setRole(env, Reviewer, RolePatch{Effort: Some(EffortHigh)}, func(tmp, final string) error {
			close(inRename)
			<-releaseA
			return crwdir.Rename(tmp, final)
		}, time.Sleep)
		close(aDone)
		errs <- err
	}()
	waitFor(t, inRename, "writer A at its rename")
	lock := filepath.Join(dir, StoreFile+".lock") // while A holds it: 0600, holding A's pid
	if info, err := os.Stat(lock); err != nil || info.Mode().Perm() != 0o600 || readText(t, lock) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("lock %v, %v", info, err)
	}
	go func() {
		met := sync.OnceFunc(func() { close(bMet) })
		_, err := resetRole(env, Executor, crwdir.Rename, func(time.Duration) { met(); <-aDone })
		errs <- err
	}()
	waitFor(t, bMet, "writer B meeting the lock")
	release()
	check(t, <-errs)
	check(t, <-errs)
	cfg := must(ReadConfig(env))
	if e := cfg.Roles[Reviewer].Effort; e == nil || *e != EffortHigh || cfg.Roles[Executor].Effort != nil {
		t.Fatalf("reviewer effort %v, executor effort %v: one update was lost", cfg.Roles[Reviewer].Effort, cfg.Roles[Executor].Effort)
	}
}

func TestConcurrentWritersKeepEveryUpdate(t *testing.T) {
	for round := 0; round < 30; round++ {
		env, _ := home(t)
		start, wg := make(chan struct{}), sync.WaitGroup{}
		for _, role := range Roles() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for attempt := 0; ; attempt++ { // a writer refused after its retry schedule tries again
					_, err := SetRole(env, role, RolePatch{Effort: Some(EffortHigh)})
					if err == nil {
						return
					}
					if !errors.Is(err, fs.ErrExist) || attempt > 100 {
						t.Errorf("%s: %v", role, err)
						return
					}
				}
			}()
		}
		close(start)
		wg.Wait()
		for _, role := range Roles() {
			if e := must(ReadConfig(env)).Roles[role].Effort; e == nil {
				t.Fatalf("round %d: the update of %s was lost", round, role)
			}
		}
	}
}

func TestHeldLockRefusesAfterTheRetrySchedule(t *testing.T) {
	ms := time.Millisecond
	for name, hold := range map[string]func(lock, victim string) error{
		"file":    func(lock, _ string) error { return os.WriteFile(lock, []byte("4242"), 0o600) },
		"symlink": func(lock, victim string) error { return os.Symlink(victim, lock) },
	} {
		t.Run(name, func(t *testing.T) {
			env, dir := home(t)
			must(SetRole(env, Reviewer, RolePatch{Effort: Some(EffortLow)}))
			path, victim := filepath.Join(dir, StoreFile), filepath.Join(dir, "victim")
			lock := path + ".lock"
			check(t, os.WriteFile(victim, []byte("precious"), 0o600))
			check(t, hold(lock, victim))
			before := readText(t, path)
			var slept []time.Duration
			_, err := setRole(env, Explorer, RolePatch{Effort: Some(EffortHigh)}, crwdir.Rename, func(d time.Duration) { slept = append(slept, d) })
			if !errors.Is(err, fs.ErrExist) || !strings.HasPrefix(err.Error(), "cannot update subagent config: ") || !strings.Contains(err.Error(), lock) {
				t.Fatalf("err = %v", err)
			}
			if want := []time.Duration{5 * ms, 10 * ms, 15 * ms, 20 * ms, 25 * ms, 30 * ms, 35 * ms, 40 * ms, 35 * ms, 35 * ms}; !reflect.DeepEqual(slept, want) {
				t.Fatalf("slept %v, want %v", slept, want)
			}
			if _, err := os.Lstat(lock); err != nil || readText(t, path) != before || readText(t, victim) != "precious" {
				t.Fatalf("lock %v, store changed %v, victim %q", err, readText(t, path) != before, readText(t, victim))
			}
		})
	}
}

func TestLockIsReleasedOnEveryPath(t *testing.T) {
	paths := map[string]func(t *testing.T, env host.LookupEnv, dir string){
		"set": func(_ *testing.T, env host.LookupEnv, _ string) {
			must(SetRole(env, Explorer, RolePatch{Effort: Some(EffortLow)}))
		},
		"reset existing": func(_ *testing.T, env host.LookupEnv, _ string) {
			must(SetRole(env, Explorer, RolePatch{Effort: Some(EffortLow)}))
			must(ResetRole(env, Explorer))
		},
		"reset missing": func(_ *testing.T, env host.LookupEnv, _ string) { must(ResetRole(env, Explorer)) },
		"refused patch": func(_ *testing.T, env host.LookupEnv, _ string) {
			_, _ = SetRole(env, Explorer, RolePatch{Effort: Some(EffortName("max"))})
		},
		"unparsable store": func(t *testing.T, env host.LookupEnv, dir string) {
			writeStore(t, dir, "{ nope")
			_, _ = ResetRole(env, Explorer)
		},
		"failed rename": func(_ *testing.T, env host.LookupEnv, _ string) {
			_, _ = setRole(env, Explorer, RolePatch{Effort: Some(EffortLow)}, func(string, string) error { return errors.New("refused") }, time.Sleep)
		},
		"panic in the critical section": func(_ *testing.T, env host.LookupEnv, _ string) {
			defer func() { _ = recover() }()
			_, _ = setRole(env, Explorer, RolePatch{Effort: Some(EffortLow)}, func(string, string) error { panic("boom") }, time.Sleep)
		},
	}
	for name, run := range paths {
		t.Run(name, func(t *testing.T) {
			env, dir := home(t)
			run(t, env, dir)
			entries, _ := os.ReadDir(dir) // a reset of a missing store may leave no directory at all
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".lock") || strings.HasSuffix(e.Name(), ".tmp") {
					t.Fatalf("%s was left behind", e.Name())
				}
			}
			release, err := lockStore(filepath.Join(dir, StoreFile), func(time.Duration) { t.Fatal("the next writer met a lock") })
			check(t, err)
			release()
		})
	}
	env, dir := home(t) // a reset of a missing store writes no store
	must(ResetRole(env, Explorer))
	if _, err := os.Stat(filepath.Join(dir, StoreFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a reset of a missing store created it: %v", err)
	}
}
