package crwconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// roomy is a probe that answers every path with an ordinary file system and plenty of room.
func roomy() Probe {
	return Probe{Statfs: func(string) (FS, error) { return FS{Type: "ext4", Free: 1 << 50}, nil }, SocketLimit: 4096}
}

// checkWith loads document under a fresh home and checks it.
func checkWith(t *testing.T, document string, probe Probe, extra ...string) (Report, string) {
	t.Helper()
	home := t.TempDir()
	pairs := append([]string{"HOME", home}, extra...)
	env := rootsEnv(pairs...)
	file, err := Load(env, configWrite(t, strings.ReplaceAll(document, "$HOME", home)))
	if err != nil {
		t.Fatal(err)
	}
	return Check(file, env, probe), home
}

// find returns the finding of one check on one root, or nil.
func find(r Report, check, root string) *Finding {
	for i := range r.Findings {
		if r.Findings[i].Check == check && r.Findings[i].Root == root {
			return &r.Findings[i]
		}
	}
	return nil
}

func mustFind(t *testing.T, r Report, check, root string, level Level) *Finding {
	t.Helper()
	f := find(r, check, root)
	if f == nil {
		t.Fatalf("no %s finding on %s; findings: %+v", check, root, r.Findings)
	}
	if f.Level != level {
		t.Errorf("%s on %s is a %s, want %s", check, root, f.Level, level)
	}
	return f
}

// A clean HOME with no configuration file resolves and checks with no finding at all, and the
// check creates nothing.
func TestCheckCleanHomeNoConfigIsClean(t *testing.T) {
	home := t.TempDir()
	env := rootsEnv("HOME", home)
	file, err := Load(env, "")
	if err != nil {
		t.Fatal(err)
	}
	report := Check(file, env, roomy())
	if len(report.Findings) != 0 {
		t.Errorf("findings on a clean HOME: %+v", report.Findings)
	}
	if err := report.Refusal(); err != nil {
		t.Errorf("Refusal() = %v", err)
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 0 {
		t.Errorf("the check left %v in the home (%v)", entries, err)
	}
	if len(report.Roots) != len(RootNames()) {
		t.Errorf("the report lists %d roots, want %d", len(report.Roots), len(RootNames()))
	}
}

// Each root, moved by the configuration file alone, resolves to the new path from config and
// checks clean; no other root moves, except scratch_root, which follows cache_root.
func TestCheckEachRootMovesByConfigurationAlone(t *testing.T) {
	for _, name := range RootNames() {
		home := t.TempDir()
		env := rootsEnv("HOME", home)
		target := filepath.Join(t.TempDir(), "moved")
		file, err := Load(env, configWrite(t, `{"paths":{"`+name+`":"`+target+`"}}`))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		base, err := Resolve(env, nil)
		if err != nil {
			t.Fatal(err)
		}
		for other, root := range file.Roots() {
			switch {
			case other == name:
				if root.Path != target || root.Source != SourceConfig {
					t.Errorf("%s moved to %+v, want %q from config", name, root, target)
				}
			case name == RootCache && other == RootScratch:
				if root.Path != filepath.Join(target, "scratch") {
					t.Errorf("scratch_root did not follow cache_root: %q", root.Path)
				}
			case root != base[other]:
				t.Errorf("moving %s also moved %s: %+v, was %+v", name, other, root, base[other])
			}
		}
		report := Check(file, env, roomy())
		if faults := report.Faults(); len(faults) != 0 {
			t.Errorf("%s: a root moved by config alone is refused: %+v", name, faults)
		}
	}
}

func TestCheckRefusesAPathThatIsAFile(t *testing.T) {
	home := t.TempDir()
	plain := filepath.Join(home, "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	report, _ := checkWith(t, `{"paths":{"worktree_root":"`+plain+`"}}`, roomy())
	mustFind(t, report, CheckNotDirectory, RootWorktree, LevelError)
	if report.Refusal() == nil {
		t.Error("Refusal() = nil for a hard fault")
	}
}

func TestCheckRefusesARootThatCannotBeCreated(t *testing.T) {
	plain := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// the nearest existing ancestor is a file: nothing below it can be made
	report, _ := checkWith(t, `{"paths":{"data_root":"`+plain+`/below/data"}}`, roomy())
	mustFind(t, report, CheckNotCreatable, RootData, LevelError)
	// a missing root below a writable directory is fine
	report, home := checkWith(t, `{"paths":{"data_root":"$HOME/not/yet/there"}}`, roomy())
	if f := find(report, CheckNotCreatable, RootData); f != nil {
		t.Errorf("a creatable root was refused: %+v", f)
	}
	if _, err := os.Stat(filepath.Join(home, "not")); err == nil {
		t.Error("the check created a directory")
	}
}

func TestCheckRefusesADirectoryThatIsNotWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes everywhere")
	}
	dir := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	report, _ := checkWith(t, `{"paths":{"evidence_root":"`+dir+`"}}`, roomy())
	mustFind(t, report, CheckNotWritable, RootEvidence, LevelError)
	// a missing root below a directory that is not writable cannot be created either
	report, _ = checkWith(t, `{"paths":{"evidence_root":"`+dir+`/sub"}}`, roomy())
	mustFind(t, report, CheckNotCreatable, RootEvidence, LevelError)
}

// The relay_state root holds a Unix socket per scope: the whole path, suffix included, is held
// to the bind limit, and a limit exactly met is a refusal because the path needs its NUL.
func TestCheckSocketPathLengthCountsTheSuffix(t *testing.T) {
	suffix := len("/") + 16 + len("/control.sock")
	if SocketSuffixLen != suffix {
		t.Fatalf("SocketSuffixLen = %d, want %d (/<16 hex scope>/control.sock)", SocketSuffixLen, suffix)
	}
	for _, test := range []struct {
		name   string
		rootLn int
		limit  int
		refuse bool
	}{
		{"one byte under the limit", 107 - suffix, 108, false},
		{"exactly the limit", 108 - suffix, 108, true},
		{"over the limit", 120 - suffix, 108, true},
		{"macos limit", 104 - suffix, 104, true},
		{"macos limit under", 103 - suffix, 104, false},
	} {
		root := "/" + strings.Repeat("r", test.rootLn-1)
		report, _ := checkWith(t, `{"paths":{"relay_state":"`+root+`"}}`, Probe{Statfs: roomy().Statfs, SocketLimit: test.limit})
		f := find(report, CheckSocketPath, RootRelayState)
		if (f != nil) != test.refuse {
			t.Errorf("%s: finding %+v, refuse %v", test.name, f, test.refuse)
		}
		if f != nil && (f.Level != LevelError || !strings.Contains(f.Message, "control.sock")) {
			t.Errorf("%s: finding %+v", test.name, f)
		}
	}
}

// temp_root and scratch_root hold the sockets of tests whose own names are not known here: a
// long root is a warning, not a refusal.
func TestCheckSocketPathOfATestRootIsAWarning(t *testing.T) {
	root := t.TempDir() + "/" + strings.Repeat("t", 200)
	report, _ := checkWith(t, `{"paths":{"temp_root":"`+root+`"}}`, Probe{Statfs: roomy().Statfs, SocketLimit: 200})
	mustFind(t, report, CheckSocketPath, RootTemp, LevelWarning)
	if faults := report.Faults(); len(faults) != 0 {
		t.Errorf("a long temp_root refused: %+v", faults)
	}
}

// The relay's store, WAL and sidecars are one bundle: a configuration that points any of them
// apart is refused, and the one key that moves them is relay_state.
func TestCheckRefusesSplitRelayState(t *testing.T) {
	for _, key := range []string{"relay_store", "relay_wal", "relay_sidecar"} {
		report, _ := checkWith(t, `{"paths":{"relay_state":"$HOME/rs","`+key+`":"$HOME/elsewhere"}}`, roomy())
		f := mustFind(t, report, CheckStateSplit, RootRelayState, LevelError)
		if !strings.Contains(f.Message, key) {
			t.Errorf("%s: message %q does not name the key", key, f.Message)
		}
	}
	report, _ := checkWith(t, `{"paths":{"relay_state":"$HOME/rs"}}`, roomy())
	if f := find(report, CheckStateSplit, RootRelayState); f != nil {
		t.Errorf("relay_state alone was refused: %+v", f)
	}
}

// A store whose WAL or sidecar is a link into another directory is split on disk, whatever the
// configuration says.
func TestCheckRefusesAnObservedSplit(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "rs")
	scope := filepath.Join(root, "0123456789abcdef")
	elsewhere := filepath.Join(home, "elsewhere")
	for _, dir := range []string{scope, elsewhere} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(scope, "relay.sqlite3"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(elsewhere, "relay.sqlite3-wal"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	env := rootsEnv("HOME", home)
	file, err := Load(env, configWrite(t, `{"paths":{"relay_state":"`+root+`"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if f := find(Check(file, env, roomy()), CheckStateSplit, RootRelayState); f != nil {
		t.Fatalf("a bundle without a WAL link was refused: %+v", f)
	}
	if err := os.Symlink(filepath.Join(elsewhere, "relay.sqlite3-wal"), filepath.Join(scope, "relay.sqlite3-wal")); err != nil {
		t.Fatal(err)
	}
	f := mustFind(t, Check(file, env, roomy()), CheckStateSplit, RootRelayState, LevelError)
	if !strings.Contains(f.Message, "relay.sqlite3-wal") {
		t.Errorf("message %q does not name the file", f.Message)
	}
}

func TestCheckWarnsOnLowSpaceAndStillWorks(t *testing.T) {
	probe := Probe{Statfs: func(string) (FS, error) { return FS{Type: "ext4", Free: 1 << 20}, nil }, SocketLimit: 4096}
	report, _ := checkWith(t, `{"paths":{"build_cache":"$HOME/bc"}}`, probe)
	mustFind(t, report, CheckLowSpace, RootBuildCache, LevelWarning)
	if faults := report.Faults(); len(faults) != 0 {
		t.Errorf("low space refused: %+v", faults)
	}
	if report.Refusal() != nil {
		t.Error("a warning produced a refusal")
	}
}

func TestCheckWarnsOnAnUnknownFilesystem(t *testing.T) {
	probe := Probe{Statfs: func(string) (FS, error) { return FS{Type: "", Free: 1 << 50}, nil }, SocketLimit: 4096}
	report, _ := checkWith(t, `{"paths":{"tools_root":"$HOME/t"}}`, probe)
	mustFind(t, report, CheckUnknownFilesystem, RootTools, LevelWarning)
}

func TestCheckWarnsWhenTwoUsesShareAPath(t *testing.T) {
	report, _ := checkWith(t, `{"paths":{"build_cache":"$HOME/same","module_cache":"$HOME/same","worktree_root":"$HOME/other"}}`, roomy())
	f := mustFind(t, report, CheckSharedPath, RootModuleCache, LevelWarning)
	if !strings.Contains(f.Message, RootBuildCache) {
		t.Errorf("message %q does not name the other use", f.Message)
	}
	if f := find(report, CheckSharedPath, RootWorktree); f != nil {
		t.Errorf("distinct paths shared: %+v", f)
	}
	// the same directory spelled through a link is the same path
	home := t.TempDir()
	real := filepath.Join(home, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	env := rootsEnv("HOME", home)
	file, err := Load(env, configWrite(t, `{"paths":{"data_root":"`+real+`","tools_root":"`+link+`"}}`))
	if err != nil {
		t.Fatal(err)
	}
	mustFind(t, Check(file, env, roomy()), CheckSharedPath, RootData, LevelWarning)
}

// A configured root that differs from the placement already in use, which holds data, is
// reported against the configuration; the check names both and moves nothing.
func TestCheckReportsDriftFromTheObservedPlacement(t *testing.T) {
	home := t.TempDir()
	env := rootsEnv("HOME", home)
	used := filepath.Join(home, ".local", "share", "crw", "worktrees")
	if err := os.MkdirAll(filepath.Join(used, "lane-a"), 0o700); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(home, "moved-worktrees")
	file, err := Load(env, configWrite(t, `{"paths":{"worktree_root":"`+moved+`"}}`))
	if err != nil {
		t.Fatal(err)
	}
	report := Check(file, env, roomy())
	f := mustFind(t, report, CheckDrift, RootWorktree, LevelWarning)
	if !strings.Contains(f.Message, used) || !strings.Contains(f.Message, moved) {
		t.Errorf("drift message %q names neither the used nor the configured path", f.Message)
	}
	var status RootStatus
	for _, s := range report.Roots {
		if s.Name == RootWorktree {
			status = s
		}
	}
	if status.Observed != used {
		t.Errorf("Observed = %q, want %q", status.Observed, used)
	}
	// an empty default placement is no drift
	empty := filepath.Join(home, ".local", "share", "crw", "data")
	if err := os.MkdirAll(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	file, err = Load(env, configWrite(t, `{"paths":{"data_root":"`+filepath.Join(home, "d")+`"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if f := find(Check(file, env, roomy()), CheckDrift, RootData); f != nil {
		t.Errorf("an empty default directory is drift: %+v", f)
	}
	// the configuration agreeing with the placement in use is no drift either
	file, err = Load(env, configWrite(t, `{"paths":{"worktree_root":"`+used+`"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if f := find(Check(file, env, roomy()), CheckDrift, RootWorktree); f != nil {
		t.Errorf("agreeing placement reported as drift: %+v", f)
	}
}

// The Go caches have no default here: their observed placement is Go's own, and a configured
// root that differs from a populated one is drift.
func TestCheckReportsDriftOfTheGoCaches(t *testing.T) {
	home := t.TempDir()
	goCache := filepath.Join(home, ".cache", "go-build")
	if err := os.MkdirAll(filepath.Join(goCache, "00"), 0o700); err != nil {
		t.Fatal(err)
	}
	env := rootsEnv("HOME", home)
	file, err := Load(env, configWrite(t, `{"paths":{"build_cache":"`+filepath.Join(home, "bc")+`"}}`))
	if err != nil {
		t.Fatal(err)
	}
	f := mustFind(t, Check(file, env, roomy()), CheckDrift, RootBuildCache, LevelWarning)
	if !strings.Contains(f.Message, goCache) {
		t.Errorf("message %q does not name Go's own cache %q", f.Message, goCache)
	}
}

// CODEX_SESSION_RELAY_STATE names one store directory ahead of every configuration, so a
// configured relay_state does not take effect while it is set: that is drift.
func TestCheckReportsTheRelayEnvironmentOverride(t *testing.T) {
	report, _ := checkWith(t, `{"paths":{"relay_state":"$HOME/rs"}}`, roomy(), "CODEX_SESSION_RELAY_STATE", "/elsewhere/store")
	f := mustFind(t, report, CheckDrift, RootRelayState, LevelWarning)
	if !strings.Contains(f.Message, "CODEX_SESSION_RELAY_STATE") {
		t.Errorf("message %q", f.Message)
	}
}

// Only restricts a check to the roots one command uses.
func TestCheckOnlyLimitsTheRoots(t *testing.T) {
	plain := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	probe := roomy()
	probe.Only = []string{RootBuildCache}
	report, _ := checkWith(t, `{"paths":{"data_root":"`+plain+`"}}`, probe)
	if len(report.Findings) != 0 || len(report.Roots) != 1 || report.Roots[0].Name != RootBuildCache {
		t.Errorf("report = %+v", report)
	}
}

// crw config check prints the report; a hard fault exits 1 and a warning alone exits 0.
func TestRunCheck(t *testing.T) {
	// the command's probe answers from the host; the test swaps in a roomy ordinary file system
	// so the verdict does not depend on the disk or on the length of the test's temporary home
	saved := runProbe
	runProbe = roomy()
	t.Cleanup(func() { runProbe = saved })
	home, env := rootsHome(t)
	code, out, errOut := configRun(t, env, "check")
	if code != 0 || errOut != "" {
		t.Fatalf("crw config check on a clean HOME: exit %d stderr %q", code, errOut)
	}
	var report struct {
		Config   map[string]string `json:"config"`
		Roots    []map[string]any  `json:"roots"`
		Findings []Finding         `json:"findings"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	// the one finding of a clean HOME is the warning that no tracking section is set (CRW-1341)
	if len(report.Roots) != len(RootNames()) || len(report.Findings) != 1 || report.Findings[0].Check != CheckTrackingMissing || report.Findings[0].Level != LevelWarning {
		t.Errorf("clean HOME: %d roots, findings %+v", len(report.Roots), report.Findings)
	}
	plain := filepath.Join(home, "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ = configRun(t, env, "check", "--config", configWrite(t, `{"paths":{"tools_root":"`+plain+`"}}`))
	if code != 1 || !strings.Contains(out, CheckNotDirectory) {
		t.Errorf("a hard fault: exit %d stdout %q", code, out)
	}
	// a configuration the command cannot read is a usage error, as for paths and show
	code, _, errOut = configRun(t, env, "check", "--config", configWrite(t, `{"paths":{"tools_root":"rel"}}`))
	if code != usageExit || !strings.Contains(errOut, "not an absolute path") {
		t.Errorf("relative path: exit %d stderr %q", code, errOut)
	}
	code, _, errOut = configRun(t, env, "check", "extra")
	if code != usageExit || !strings.Contains(errOut, "unexpected argument") {
		t.Errorf("extra argument: exit %d stderr %q", code, errOut)
	}
}
