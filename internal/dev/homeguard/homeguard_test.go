package homeguard

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
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

// The read-only harnesses write nothing, so they have no destination to refuse (CRW-1186): this
// holds them to it. A writer added to one of them must call Refuse for its destination and move out of this list.
func TestReadOnlyHarnessesCallNoWriter(t *testing.T) {
	writers := map[string]bool{"WriteFile": true, "Create": true, "CreateTemp": true, "Mkdir": true, "MkdirAll": true, "MkdirTemp": true,
		"Rename": true, "Remove": true, "RemoveAll": true, "Symlink": true, "Link": true, "Chmod": true, "Chtimes": true, "Truncate": true, "Lchown": true, "Chown": true}
	for _, dir := range []string{"../stopevents", "../trialledger"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("%s: %v (%d files)", dir, err, len(files))
		}
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(parsed, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "os" {
					return true
				}
				if writers[sel.Sel.Name] {
					t.Errorf("%s calls os.%s: a writer needs homeguard.Refuse", file, sel.Sel.Name)
				}
				if sel.Sel.Name == "OpenFile" && len(call.Args) > 1 {
					flags := types.ExprString(call.Args[1])
					if !strings.Contains(flags, "O_RDONLY") || strings.Contains(flags, "O_WRONLY") || strings.Contains(flags, "O_RDWR") || strings.Contains(flags, "O_CREATE") {
						t.Errorf("%s calls os.OpenFile(%s): not a read", file, flags)
					}
				}
				return true
			})
		}
	}
}
