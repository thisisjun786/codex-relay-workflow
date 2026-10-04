package role

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func registrationTestHome(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for key, dir := range map[string]string{"HOME": "user", "CODEX_HOME": "codex", "CRW_HOME": "crw"} {
		path := filepath.Join(root, dir)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, path)
	}
	return filepath.Join(root, "codex")
}

func registrationTestWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func registrationTestRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func registrationTestHash(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }
func registrationTestSigned(body []byte) []byte {
	return append([]byte("# crw-managed: "+registrationTestHash(body)+"\n"), body...)
}

func registrationTestClean(t *testing.T, home string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, "agents"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("orphan: %s", e.Name())
		}
	}
}

func TestRegistrationRolesAndHome(t *testing.T) {
	home := registrationTestHome(t)
	if !reflect.DeepEqual(NativeRoles(), []NativeRoleName{Architect, Executor}) {
		t.Fatal("native roles must be architect/executor only")
	}
	for _, value := range []string{"", home, "  /spaced-codex  "} {
		env := func(key string) (string, bool) { return value, key == "CODEX_HOME" }
		want := value
		if value == "" {
			want = filepath.Join(home, ".codex")
		}
		if got := ResolveNativeRoleHome(env, home); got != want {
			t.Errorf("home = %q, want %q", got, want)
		}
	}
}

func TestRegistrationCreatesCompleteRoleAndPreservesSettings(t *testing.T) {
	for _, role := range []NativeRoleName{Executor, Architect} {
		t.Run(string(role), func(t *testing.T) {
			home := registrationTestHome(t)
			for _, p := range []string{"config.toml", "agents/worker.toml", "subagents.json"} {
				registrationTestWrite(t, filepath.Join(home, p), []byte("user-owned\n"))
			}
			first, err := RegisterRole(role, home)
			if err != nil || !first.Created || first.Updated {
				t.Fatalf("create = %+v, %v", first, err)
			}
			if first.Path != filepath.Join(home, "agents", string(role)+".toml") {
				t.Fatal(first.Path)
			}
			content := registrationTestRead(t, first.Path)
			line, body, ok := bytes.Cut(content, []byte("\n"))
			if !ok || string(line) != "# crw-managed: "+registrationTestHash(body) {
				t.Fatal("invalid managed marker")
			}
			template := registrationTestRead(t, filepath.Join("agents", string(role)+".toml"))
			_, wantPrompt, wantOK := bytes.Cut(template, []byte("developer_instructions = "))
			_, gotPrompt, gotOK := bytes.Cut(body, []byte("developer_instructions = "))
			if !wantOK || !gotOK || !bytes.Equal(gotPrompt, wantPrompt) {
				t.Fatal("complete developer instructions differ from the copied oracle template")
			}
			if !bytes.Contains(body, []byte("name = \""+string(role)+"\"\n")) || bytes.Contains(body, []byte("\nmodel =")) {
				t.Fatal("wrong role or model sentinel")
			}
			if role == Architect && !bytes.Contains(body, []byte("sandbox_mode = \"read-only\"")) {
				t.Fatal("architect sandbox missing")
			}
			if role == Executor && bytes.Contains(body, []byte("sandbox_mode =")) {
				t.Fatal("executor must inherit sandbox")
			}
			info, err := os.Stat(first.Path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("permissions: %v, %v", info, err)
			}
			second, err := RegisterRole(role, home)
			if err != nil || second != (RegistrationResult{Path: first.Path}) {
				t.Fatalf("idempotence = %+v, %v", second, err)
			}
			encoded, err := json.Marshal(second)
			if err != nil || strings.Contains(string(encoded), "updated") || !strings.Contains(string(encoded), `"created":false`) {
				t.Fatal(string(encoded), err)
			}
			for _, p := range []string{"config.toml", "agents/worker.toml", "subagents.json"} {
				if string(registrationTestRead(t, filepath.Join(home, p))) != "user-owned\n" {
					t.Fatalf("changed %s", p)
				}
			}
			registrationTestClean(t, home)
		})
	}
}

func TestRegistrationRefusesForeignAndNonregularFiles(t *testing.T) {
	for _, role := range []NativeRoleName{Architect, Executor} {
		for _, kind := range []string{"foreign", "directory", "dangling", "link", "agents-link"} {
			t.Run(string(role)+"/"+kind, func(t *testing.T) {
				home := registrationTestHome(t)
				outside := filepath.Join(t.TempDir(), "outside")
				registrationTestWrite(t, outside, []byte("outside\n"))
				path := filepath.Join(home, "agents", string(role)+".toml")
				if kind == "agents-link" {
					if err := os.Symlink(filepath.Dir(outside), filepath.Join(home, "agents")); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
						t.Fatal(err)
					}
					switch kind {
					case "foreign":
						registrationTestWrite(t, path, []byte("# custom\n"))
					case "directory":
						if err := os.Mkdir(path, 0700); err != nil {
							t.Fatal(err)
						}
					case "dangling":
						if err := os.Symlink(outside+"-missing", path); err != nil {
							t.Fatal(err)
						}
					case "link":
						if err := os.Symlink(outside, path); err != nil {
							t.Fatal(err)
						}
					}
				}
				_, err := RegisterRole(role, home)
				if err == nil || (!strings.Contains(err.Error(), "differs") && !strings.Contains(err.Error(), "non-regular")) {
					t.Fatalf("refusal = %v", err)
				}
				if string(registrationTestRead(t, outside)) != "outside\n" {
					t.Fatal("outside modified")
				}
				if kind == "foreign" && string(registrationTestRead(t, path)) != "# custom\n" {
					t.Fatal("foreign file modified")
				}
				if kind == "directory" {
					if st, err := os.Stat(path); err != nil || !st.IsDir() {
						t.Fatal("directory modified")
					}
				}
			})
		}
	}
}

func TestRegistrationValidatesBeforeWritesAndUsesDefaultHome(t *testing.T) {
	home := registrationTestHome(t)
	for _, role := range []NativeRoleName{Explorer, Reviewer, "../etc/passwd", ""} {
		if _, err := RegisterRole(role, home); err == nil || !strings.Contains(err.Error(), "unsupported native role") {
			t.Errorf("role %q: %v", role, err)
		}
	}
	for _, homes := range [][]string{{""}, {" \t\uFEFF"}, {home, home + "-other"}} {
		if _, err := RegisterRole(Architect, homes...); err == nil || !strings.Contains(err.Error(), "invalid native role home") {
			t.Errorf("homes %q: %v", homes, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(home, "agents")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("validation wrote agents")
	}
	first, err := RegisterArchitect()
	if err != nil || !first.Created || first.Path != filepath.Join(home, "agents/architect.toml") {
		t.Fatalf("default = %+v %v", first, err)
	}
	second, err := RegisterExecutor(filepath.Join(home, " padded "))
	if err != nil || !second.Created || second.Path != filepath.Join(home, " padded /agents/executor.toml") {
		t.Fatalf("padded = %+v %v", second, err)
	}
	t.Setenv("CODEX_HOME", "")
	fallback, err := RegisterExecutor()
	if err != nil || fallback.Path != filepath.Join(os.Getenv("HOME"), ".codex/agents/executor.toml") {
		t.Fatalf("fallback = %+v %v", fallback, err)
	}
}

func TestRegistrationUpgradesWithBackupAndPreservesEdits(t *testing.T) {
	for _, role := range []NativeRoleName{Architect, Executor} {
		t.Run(string(role), func(t *testing.T) {
			home := registrationTestHome(t)
			path := filepath.Join(home, "agents", string(role)+".toml")
			prior := registrationTestSigned([]byte("name = \"" + string(role) + "\"\ndeveloper_instructions = \"old\"\n"))
			registrationTestWrite(t, path, prior)
			result, err := RegisterRole(role, home)
			if err != nil || !result.Updated || result.Created {
				t.Fatalf("update = %+v %v", result, err)
			}
			backup := path + ".backup-" + registrationTestHash(prior)
			if !bytes.Equal(registrationTestRead(t, backup), prior) {
				t.Fatal("backup differs")
			}
			current := registrationTestRead(t, path)
			if bytes.Equal(current, prior) {
				t.Fatal("old prompt retained")
			}
			result, err = RegisterRole(role, home)
			if err != nil || result.Created || result.Updated {
				t.Fatal(result, err)
			}
			edited := append(current, []byte("# user edit\n")...)
			registrationTestWrite(t, path, edited)
			if _, err := RegisterRole(role, home); err == nil || !strings.Contains(err.Error(), "differs") {
				t.Fatal(err)
			}
			if !bytes.Equal(registrationTestRead(t, path), edited) {
				t.Fatal("edit overwritten")
			}
			registrationTestClean(t, home)
		})
	}
}

func TestRegistrationAdoptsLegacyAndChecksExistingBackup(t *testing.T) {
	for _, kind := range []string{"absent", "identical", "conflict", "link", "lock"} {
		t.Run(kind, func(t *testing.T) {
			home := registrationTestHome(t)
			first, err := RegisterArchitect(home)
			if err != nil {
				t.Fatal(err)
			}
			_, body, _ := bytes.Cut(registrationTestRead(t, first.Path), []byte("\n"))
			registrationTestWrite(t, first.Path, body)
			backup := first.Path + ".backup-" + registrationTestHash(body)
			switch kind {
			case "identical":
				registrationTestWrite(t, backup, body)
			case "conflict":
				registrationTestWrite(t, backup, []byte("other"))
			case "link":
				registrationTestWrite(t, backup+"-target", body)
				if err := os.Symlink(backup+"-target", backup); err != nil {
					t.Fatal(err)
				}
			case "lock":
				if err := os.Mkdir(filepath.Join(home, "agents/.architect-update.lock"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			result, err := RegisterArchitect(home)
			if kind == "absent" || kind == "identical" {
				if err != nil || !result.Updated || result.Created {
					t.Fatal(result, err)
				}
				if !bytes.Equal(registrationTestRead(t, backup), body) {
					t.Fatal("legacy backup lost")
				}
				registrationTestClean(t, home)
			} else {
				if err == nil {
					t.Fatal("accepted conflict")
				}
				if !bytes.Equal(registrationTestRead(t, first.Path), body) {
					t.Fatal("prior changed")
				}
				if kind != "lock" {
					registrationTestClean(t, home)
				}
			}
		})
	}
}

func TestRegistrationConcurrentCreationDoesNotReplace(t *testing.T) {
	for _, role := range []NativeRoleName{Architect, Executor} {
		t.Run(string(role), func(t *testing.T) {
			home := registrationTestHome(t)
			var wg sync.WaitGroup
			start := make(chan struct{})
			results := make(chan RegistrationResult, 2)
			errors := make(chan error, 2)
			for i := 0; i < 2; i++ {
				wg.Go(func() { <-start; r, e := RegisterRole(role, home); results <- r; errors <- e })
			}
			close(start)
			wg.Wait()
			close(results)
			close(errors)
			for err := range errors {
				if err != nil {
					t.Fatal(err)
				}
			}
			created := 0
			for result := range results {
				if result.Created {
					created++
				}
				if result.Updated {
					t.Fatal("unexpected update")
				}
			}
			if created != 1 {
				t.Fatalf("created %d", created)
			}
			registrationTestClean(t, home)
		})
	}
}

func TestRegistrationPreservesInvalidUTF8BackupBytes(t *testing.T) {
	home := registrationTestHome(t)
	path := filepath.Join(home, "agents/architect.toml")
	body := []byte("name = \"architect\"\ndeveloper_instructions = \"old \xff\"\n")
	decoded := bytes.ReplaceAll(body, []byte{255}, []byte("\uFFFD"))
	prior := append([]byte("# crw-managed: "+registrationTestHash(decoded)+"\n"), body...)
	registrationTestWrite(t, path, prior)
	result, err := RegisterArchitect(home)
	if err != nil || !result.Updated {
		t.Fatal(result, err)
	}
	decodedPrior := bytes.ReplaceAll(prior, []byte{255}, []byte("\uFFFD"))
	backup := path + ".backup-" + registrationTestHash(decodedPrior)
	if !bytes.Equal(registrationTestRead(t, backup), prior) {
		t.Fatal("invalid bytes lost in backup")
	}
	registrationTestWrite(t, path, registrationTestSigned(body))
	if _, err := RegisterArchitect(home); err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatal("raw-signed invalid file accepted", err)
	}
	registrationTestClean(t, home)
}

func TestRegistrationFaultsPreservePriorAndReportPublication(t *testing.T) {
	for _, step := range []registrationStep{registrationFileSync, registrationBackupDirSync, registrationBeforeRename, registrationRoleDirSync} {
		t.Run(fmt.Sprint(step), func(t *testing.T) {
			home := registrationTestHome(t)
			path := filepath.Join(home, "agents/architect.toml")
			prior := registrationTestSigned([]byte("name = \"architect\"\ndeveloper_instructions = \"old\"\n"))
			registrationTestWrite(t, path, prior)
			fault := errors.New("injected filesystem failure")
			hit := false
			_, err := registrationRegister(Architect, []string{home}, func(at registrationStep) error {
				if at == step {
					hit = true
					return fault
				}
				return nil
			})
			if !hit || !errors.Is(err, fault) {
				t.Fatalf("fault not exercised: %v %v", hit, err)
			}
			got := registrationTestRead(t, path)
			if step == registrationRoleDirSync {
				if bytes.Equal(got, prior) {
					t.Fatal("post-publication error hid visible replacement")
				}
			} else if !bytes.Equal(got, prior) {
				t.Fatal("prior changed before publication")
			}
			backup := path + ".backup-" + registrationTestHash(prior)
			if step == registrationFileSync {
				if _, err := os.Lstat(backup); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("partial backup published")
				}
			} else if !bytes.Equal(registrationTestRead(t, backup), prior) {
				t.Fatal("backup changed")
			}
			registrationTestClean(t, home)
		})
	}
	home := registrationTestHome(t)
	path := filepath.Join(home, "agents/architect.toml")
	prior := registrationTestSigned([]byte("name = \"architect\"\nold\n"))
	edited := append(append([]byte{}, prior...), []byte("user edit\n")...)
	registrationTestWrite(t, path, prior)
	_, err := registrationRegister(Architect, []string{home}, func(at registrationStep) error {
		if at == registrationAfterBackup {
			registrationTestWrite(t, path, edited)
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "changed during update") {
		t.Fatal("mutation not refused", err)
	}
	if !bytes.Equal(registrationTestRead(t, path), edited) {
		t.Fatal("raced edit overwritten")
	}
	if !bytes.Equal(registrationTestRead(t, path+".backup-"+registrationTestHash(prior)), prior) {
		t.Fatal("backup lost")
	}
	registrationTestClean(t, home)
}

func TestRegistrationRawRecheckRefusesEqualDecodedMutation(t *testing.T) {
	home := registrationTestHome(t)
	path := filepath.Join(home, "agents/architect.toml")
	body := []byte("name = \"architect\"\nold \xff\n")
	decoded := bytes.ReplaceAll(body, []byte{255}, []byte("\uFFFD"))
	prior := append([]byte("# crw-managed: "+registrationTestHash(decoded)+"\n"), body...)
	edited := bytes.ReplaceAll(prior, []byte{255}, []byte{254})
	registrationTestWrite(t, path, prior)
	_, err := registrationRegister(Architect, []string{home}, func(at registrationStep) error {
		if at == registrationAfterBackup {
			registrationTestWrite(t, path, edited)
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "changed during update") {
		t.Fatal("equal decoded mutation lost", err)
	}
	if !bytes.Equal(registrationTestRead(t, path), edited) {
		t.Fatal("raced bytes overwritten")
	}
	decodedPrior := bytes.ReplaceAll(prior, []byte{255}, []byte("\uFFFD"))
	if !bytes.Equal(registrationTestRead(t, path+".backup-"+registrationTestHash(decodedPrior)), prior) {
		t.Fatal("original bytes lost")
	}
	registrationTestClean(t, home)
}

func TestRegistrationCreationSyncFailureAndForeignMarker(t *testing.T) {
	home := registrationTestHome(t)
	fault := errors.New("directory sync failed")
	_, err := registrationRegister(Executor, []string{home}, func(at registrationStep) error {
		if at == registrationRoleDirSync {
			return fault
		}
		return nil
	})
	if !errors.Is(err, fault) {
		t.Fatal("publication sync error hidden", err)
	}
	path := filepath.Join(home, "agents/executor.toml")
	content := registrationTestRead(t, path)
	foreign := bytes.Replace(content, []byte("# crw-managed:"), []byte("# codexclaw-managed:"), 1)
	registrationTestWrite(t, path, foreign)
	if _, err := RegisterExecutor(home); err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatal("foreign marker adopted", err)
	}
	if !bytes.Equal(registrationTestRead(t, path), foreign) {
		t.Fatal("foreign marker changed")
	}
	registrationTestClean(t, home)
}

func TestRegistrationModelStripKeepsOracleFirstMatch(t *testing.T) {
	registrationTestHome(t)
	input := "name = \"executor\"\ndeveloper_instructions = \"\"\"\nmodel = \"default\"\nkeep\n\"\"\"\nmodel = \"default\"\n"
	want := "name = \"executor\"\ndeveloper_instructions = \"\"\"\nkeep\n\"\"\"\nmodel = \"default\"\n"
	if got := registrationBody(input); got != want {
		t.Fatalf("body = %q", got)
	}
}

func TestRegistrationSyncsReusedBackupBeforeReplacement(t *testing.T) {
	home := registrationTestHome(t)
	path := filepath.Join(home, "agents/architect.toml")
	prior := registrationTestSigned([]byte("name = \"architect\"\nold\n"))
	registrationTestWrite(t, path, prior)
	backup := path + ".backup-" + registrationTestHash(prior)
	registrationTestWrite(t, backup, prior)
	fault := errors.New("reused backup sync failed")
	hit := false
	_, err := registrationRegister(Architect, []string{home}, func(at registrationStep) error {
		if at == registrationReusedBackupSync {
			hit = true
			return fault
		}
		return nil
	})
	if !hit || !errors.Is(err, fault) {
		t.Fatal("reused backup was not synced", err)
	}
	if !bytes.Equal(registrationTestRead(t, path), prior) {
		t.Fatal("replacement preceded reused backup sync")
	}
	if !bytes.Equal(registrationTestRead(t, backup), prior) {
		t.Fatal("reused backup changed")
	}
	registrationTestClean(t, home)
}
