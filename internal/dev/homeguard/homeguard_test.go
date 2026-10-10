//go:build dev

package homeguard

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	cleanup, _ := RefuseAccountHome("")
	code := m.Run()
	if err := cleanup(); err != nil {
		os.Stderr.WriteString("homeguard: " + err.Error() + "\n")
		code = 1
	}
	os.Exit(code)
}

func TestRefuseTurnsAwayEveryProtectedDirectoryOfTheAccountHome(t *testing.T) {
	home := t.TempDir()
	defer SetAccountHome(home)()
	for _, rel := range []string{".codex", ".codex/crw/switch.json", ".crw", ".crw/x/y", ".local/share/crw-runtime", ".local/share/crw-runtime/current/bin/crw"} {
		err := Refuse(filepath.Join(home, rel))
		var refusal *Error
		if !errors.As(err, &refusal) {
			t.Errorf("%s: want a refusal, got %v", rel, err)
		}
	}
	for _, rel := range []string{"", "codex", ".codexx", ".local/share", ".local/share/other", "work/.codex-plugin", ".config"} {
		if err := Refuse(filepath.Join(home, rel)); err != nil {
			t.Errorf("%s: want no refusal, got %v", rel, err)
		}
	}
	if err := Refuse(t.TempDir()); err != nil {
		t.Errorf("an unrelated directory: %v", err)
	}
}

func TestRefuseFollowsLinksAndRelativeSpellings(t *testing.T) {
	home := t.TempDir()
	defer SetAccountHome(home)()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	link := filepath.Join(other, "elsewhere")
	if err := os.Symlink(filepath.Join(home, ".codex"), link); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(other, "dangling")
	if err := os.Symlink(filepath.Join(home, ".crw", "not-yet"), dangling); err != nil {
		t.Fatal(err)
	}
	homeLink := filepath.Join(other, "home-link")
	if err := os.Symlink(home, homeLink); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"a link to .codex":                 filepath.Join(link, "crw", "switch.json"),
		"a link that points into .crw":     filepath.Join(dangling, "x"),
		"the home through a link":          filepath.Join(homeLink, ".codex", "crw"),
		"dot-dot back into the home":       filepath.Join(home, "work", "..", ".codex", "crw"),
		"a missing tail below a protected": filepath.Join(home, ".local", "share", "crw-runtime", "a", "b"),
	} {
		var refusal *Error
		if err := Refuse(path); !errors.As(err, &refusal) {
			t.Errorf("%s: want a refusal, got %v", name, err)
		} else if !strings.Contains(refusal.Error(), "account's real home") {
			t.Errorf("%s: the error does not say why: %v", name, refusal)
		}
	}
}

func TestAccountHomeIsThePasswdHomeNotHOME(t *testing.T) {
	account, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil || account.HomeDir == "" {
		t.Skip("no passwd home to compare with")
	}
	t.Setenv("HOME", t.TempDir())
	got, err := AccountHome()
	if err != nil || got != account.HomeDir {
		t.Fatalf("AccountHome = %q, %v; the passwd home is %q", got, err, account.HomeDir)
	}
	if err := Refuse(filepath.Join(account.HomeDir, ".codex", "crw", "switch.json")); err == nil {
		t.Fatal("the real switch file is not refused when HOME names another directory")
	}
	// the guard of this package's own TestMain must not count what this test provoked
	state.Lock()
	state.refused = nil
	state.Unlock()
}

func TestRefusalsOfAFakeHomeAreNotHeldAgainstThePackage(t *testing.T) {
	cleanup, _ := RefuseAccountHome("")
	restore := SetAccountHome(t.TempDir())
	home, _ := AccountHome()
	if Refuse(filepath.Join(home, ".codex")) == nil {
		t.Fatal("the fake home is not refused")
	}
	restore()
	if err := cleanup(); err != nil {
		t.Fatalf("a refusal of a fake home failed the guard: %v", err)
	}
}

func TestGuardNamesWhatTheRealHomeRefused(t *testing.T) {
	account, err := AccountHome()
	if err != nil {
		t.Skip("no account home")
	}
	cleanup, _ := RefuseAccountHome("")
	if Refuse(filepath.Join(account, ".codex", "crw", "switch.json")) == nil {
		t.Fatal("the real switch file is not refused")
	}
	err = cleanup()
	if err == nil || !strings.Contains(err.Error(), "switch.json") {
		t.Fatalf("the guard does not name the refused destination: %v", err)
	}
}

func TestGuardFailsWhenTheRealSwitchFileChanged(t *testing.T) {
	home := t.TempDir()
	defer SetAccountHome(home)()
	cleanup, _ := RefuseAccountHome("")
	file := filepath.Join(home, ".codex", "crw", "switch.json")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(`{"active":"crw"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cleanup(); err == nil || !strings.Contains(err.Error(), "hook switch") {
		t.Fatalf("a switch file that appeared while the tests ran is not reported: %v", err)
	}
}
