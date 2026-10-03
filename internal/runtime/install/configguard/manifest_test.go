package configguard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestActivationPreservesAcceptedSurrogatesAndCutsUTF16(t *testing.T) {
	home := activationHome(t)
	path := filepath.Join(home, "config.toml")
	activationWrite(t, path, "[memories]\ndedicated_tools = true\n")
	activationWrite(t, manifestPath(home), `{"version":2,"configPath":"x","flags":{},"tableKeys":{"memories.dedicated_tools":{"table":"memories","key":"dedicated_tools","priorValue":"\ud800","appliedValue":"true","setByCodexclaw":true}}}`)
	var calls [][]string
	deps := activationDeps(t, home, allActivationFlags(), &calls)
	base := deps.Run
	deps.Run = func(args []string) CodexRunResult {
		if args[1] == "list" {
			return CodexRunResult{Stdout: "multi_agent true\ngoals true\nhooks true\ndefault_mode_request_user_input false"}
		}
		if args[2] == "default_mode_request_user_input" {
			return CodexRunResult{ExitCode: 1, Stderr: " \ufeff" + strings.Repeat("a", 499) + "😀tail "}
		}
		return base(args)
	}
	m, e := Activate(deps)
	if e != nil {
		t.Fatal(e)
	}
	raw := activationRead(t, manifestPath(home))
	if !strings.Contains(raw, `"priorValue": "\ud800"`) || !strings.Contains(raw, strings.Repeat("a", 499)+`\ud83d"`) {
		t.Fatalf("lone surrogate bytes lost: %s", raw)
	}
	parsed := parseInstallManifest(raw)
	if parsed == nil || *parsed.TableKeys["memories.dedicated_tools"].PriorValue != *m.TableKeys["memories.dedicated_tools"].PriorValue {
		t.Fatal("lossless carried value changed")
	}
	if got := activationFailureMessage(strings.Repeat("a", 498) + "😀tail"); got != strings.Repeat("a", 498)+"😀" {
		t.Fatalf("full pair cut=%q", got)
	}
}

// These two recorded cases change intentionally to avoid losing settings or restoration records.
func TestActivationIntentionallyChangedCases(t *testing.T) {
	var cases []struct {
		ID, Classification, Reason, Config string
		Go                                 struct {
			Error             bool
			Config            string
			ManifestPreserved bool
		}
	}
	b, err := os.ReadFile("testdata/activation-changes.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.ID, func(t *testing.T) {
			home := activationHome(t)
			path := filepath.Join(home, "config.toml")
			activationWrite(t, path, c.Config)
			prior := `{"version":2,"configPath":"x","flags":{},"tableKeys":{"memories.dedicated_tools":{"table":"memories","key":"dedicated_tools","priorValue":"false","appliedValue":"true","setByCodexclaw":true}}}` + "\n"
			if c.Go.ManifestPreserved {
				activationWrite(t, manifestPath(home), prior)
				if err := os.Chmod(manifestPath(home), 0200); err != nil {
					t.Fatal(err)
				}
				defer os.Chmod(manifestPath(home), 0600)
				if _, err := os.ReadFile(manifestPath(home)); err == nil {
					t.Skip("effective privileges allow reading mode0200")
				}
			} else {
				if err := os.Chmod(home, 0500); err != nil {
					t.Fatal(err)
				}
				probe, err := os.CreateTemp(home, ".probe-")
				os.Chmod(home, 0700)
				if err == nil {
					probe.Close()
					os.Remove(probe.Name())
					t.Skip("effective privileges allow creating in mode0500")
				}
				defer os.Chmod(home, 0700)
			}
			run := func(args []string) CodexRunResult {
				if args[1] == "list" {
					hooks := "true"
					if !c.Go.ManifestPreserved {
						hooks = "false"
					}
					return CodexRunResult{Stdout: "multi_agent true\ngoals true\nhooks " + hooks + "\ndefault_mode_request_user_input true"}
				}
				if err := os.Chmod(home, 0500); err != nil {
					t.Fatal(err)
				}
				return CodexRunResult{}
			}
			_, err := Activate(ActivateDeps{Run: run, CodexHome: home, Now: func() string { return "2026-06-30T00:00:00.000Z" }})
			if (err != nil) != c.Go.Error || activationRead(t, path) != c.Go.Config {
				t.Fatalf("case %s: error=%v config=%q", c.ID, err, activationRead(t, path))
			}
			if c.Go.ManifestPreserved {
				if err := os.Chmod(manifestPath(home), 0600); err != nil {
					t.Fatal(err)
				}
				if activationRead(t, manifestPath(home)) != prior {
					t.Fatal("unreadable restoration record replaced")
				}
			}
		})
	}
}

func TestActivateMalformedReadableManifestIsAbsent(t *testing.T) {
	home := activationHome(t)
	activationWrite(t, manifestPath(home), "truncated {")
	var calls [][]string
	m, e := Activate(activationDeps(t, home, allActivationFlags(), &calls))
	if e != nil || m == nil {
		t.Fatalf("result=%+v error=%v", m, e)
	}
	if parseInstallManifest(activationRead(t, manifestPath(home))) == nil {
		t.Fatal("new manifest missing")
	}
}

func TestActivateUnreadableDestinationsAreRefused(t *testing.T) {
	for _, target := range []string{"config", "manifest", "backup"} {
		t.Run(target, func(t *testing.T) {
			home := activationHome(t)
			path := filepath.Join(home, "config.toml")
			activationWrite(t, path, "# preserve\n")
			dest := path
			if target == "manifest" {
				dest = manifestPath(home)
			}
			if target == "backup" {
				dest = path + ".crw-2026-06-30T00-00-00-000Z.bak"
			}
			activationWrite(t, dest, "original secret-free bytes\n")
			if e := os.Chmod(dest, 0200); e != nil {
				t.Fatal(e)
			}
			defer os.Chmod(dest, 0600)
			if _, e := os.ReadFile(dest); e == nil {
				t.Skip("effective privileges allow reading mode0200")
			}
			var calls [][]string
			m, e := Activate(activationDeps(t, home, allActivationFlags(), &calls))
			if e == nil || m != nil || !strings.Contains(e.Error(), "could not read") {
				t.Fatalf("result=%+v error=%v", m, e)
			}
			if e = os.Chmod(dest, 0600); e != nil {
				t.Fatal(e)
			}
			if activationRead(t, dest) != "original secret-free bytes\n" {
				t.Fatal("unreadable file overwritten")
			}
			if len(calls) != 1 {
				t.Fatal("enable called after read refusal")
			}
		})
	}
}

func TestActivationBackupModesAndExistingTargetFailures(t *testing.T) {
	for _, mode := range []os.FileMode{0600, 0640, 0444} {
		t.Run(mode.String(), func(t *testing.T) {
			home := activationHome(t)
			path := filepath.Join(home, "config.toml")
			body := "[memories]\ndedicated_tools = true\n"
			activationWrite(t, path, body)
			if e := os.Chmod(path, mode); e != nil {
				t.Fatal(e)
			}
			var calls [][]string
			m, e := Activate(activationDeps(t, home, allActivationFlags(), &calls))
			if e != nil {
				t.Fatal(e)
			}
			info, e := os.Stat(*m.BackupPath)
			if e != nil || info.Mode().Perm() != mode || activationRead(t, *m.BackupPath) != body {
				t.Fatalf("backup mode/content=%v %v", info, e)
			}
		})
	}
	for _, kind := range []string{"directory", "read-only"} {
		t.Run(kind, func(t *testing.T) {
			home := activationHome(t)
			path := filepath.Join(home, "config.toml")
			activationWrite(t, path, "# original\n")
			backup := path + ".crw-2026-06-30T00-00-00-000Z.bak"
			if kind == "directory" {
				if e := os.Mkdir(backup, 0700); e != nil {
					t.Fatal(e)
				}
			} else {
				activationWrite(t, backup, "old backup\n")
				if e := os.Chmod(backup, 0444); e != nil {
					t.Fatal(e)
				}
				defer os.Chmod(backup, 0600)
				f, e := os.OpenFile(backup, os.O_WRONLY, 0)
				if e == nil {
					f.Close()
					t.Skip("effective privileges allow writing mode0444")
				}
			}
			var calls [][]string
			if _, e := Activate(activationDeps(t, home, allActivationFlags(), &calls)); e == nil {
				t.Fatal("bad backup destination accepted")
			}
			if activationRead(t, path) != "# original\n" {
				t.Fatal("config changed after backup failure")
			}
			if kind == "read-only" && activationRead(t, backup) != "old backup\n" {
				t.Fatal("backup overwritten")
			}
			files, e := os.ReadDir(home)
			if e != nil {
				t.Fatal(e)
			}
			for _, f := range files {
				if strings.HasSuffix(f.Name(), ".tmp") {
					t.Fatal("staging leaked")
				}
			}
		})
	}
}

func TestActivateFollowsConfigAndBackupSymlinks(t *testing.T) {
	home := activationHome(t)
	target := filepath.Join(home, "target.toml")
	path := filepath.Join(home, "config.toml")
	original := "# linked\n"
	activationWrite(t, target, original)
	if e := os.Symlink(target, path); e != nil {
		t.Fatal(e)
	}
	backup := path + ".crw-2026-06-30T00-00-00-000Z.bak"
	backupTarget := filepath.Join(home, "backup-target")
	activationWrite(t, backupTarget, "previous\n")
	if e := os.Symlink(backupTarget, backup); e != nil {
		t.Fatal(e)
	}
	var calls [][]string
	deps := activationDeps(t, home, allActivationFlags(), &calls)
	if _, e := Activate(deps); e != nil {
		t.Fatal(e)
	}
	for _, p := range []string{path, backup} {
		i, e := os.Lstat(p)
		if e != nil || i.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("link replaced:%s %v", p, e)
		}
	}
	if activationRead(t, backupTarget) != original || !strings.Contains(activationRead(t, target), "dedicated_tools = true") {
		t.Fatal("symlink target content")
	}
}

func TestActivateRefusesDanglingConfigLink(t *testing.T) {
	home := activationHome(t)
	path := filepath.Join(home, "config.toml")
	missing := filepath.Join(home, "absent")
	if e := os.Symlink(missing, path); e != nil {
		t.Fatal(e)
	}
	var calls [][]string
	if _, e := Activate(activationDeps(t, home, allActivationFlags(), &calls)); e == nil {
		t.Fatal("dangling config treated as absent")
	}
	if _, e := os.Stat(missing); !os.IsNotExist(e) {
		t.Fatal("dangling target created")
	}
}

func TestActivateFailedConfigPublicationPreservesBytes(t *testing.T) {
	home := activationHome(t)
	path := filepath.Join(home, "config.toml")
	activationWrite(t, path, "# original\n")
	var calls [][]string
	state := allActivationFlags()
	state["hooks"] = false
	deps := activationDeps(t, home, state, &calls)
	deps.Run = func(a []string) CodexRunResult {
		if a[1] == "list" {
			return CodexRunResult{Stdout: "multi_agent true\ngoals true\nhooks false\ndefault_mode_request_user_input true"}
		}
		if e := os.Chmod(path, 0444); e != nil {
			t.Fatal(e)
		}
		return CodexRunResult{}
	}
	defer os.Chmod(path, 0600)
	f, e := os.OpenFile(path, os.O_WRONLY, 0)
	if e != nil {
		t.Fatal(e)
	}
	f.Close()
	// Establish that mode0444 actually blocks the primitive on this effective user.
	if e = os.Chmod(path, 0444); e != nil {
		t.Fatal(e)
	}
	f, e = os.OpenFile(path, os.O_WRONLY, 0)
	os.Chmod(path, 0600)
	if e == nil {
		f.Close()
		t.Skip("effective privileges allow writing mode0444")
	}
	if _, e = Activate(deps); e == nil {
		t.Fatal("read-only config rewritten")
	}
	if activationRead(t, path) != "# original\n" {
		t.Fatal("config truncated")
	}
	if _, e = os.Stat(manifestPath(home)); !os.IsNotExist(e) {
		t.Fatal("manifest published after failed config write")
	}
}
