package role

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOwnerOfTellsTheProductsApart(t *testing.T) {
	body := "# codexclaw subagent role: executor\n"
	cxc := []byte(fmt.Sprintf("# codexclaw-managed: %x\n%s", sha256.Sum256([]byte(body)), body))
	crw, err := NativeRoleContent(Executor)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		raw  []byte
		want Owner
	}{
		"absent":                 {nil, OwnerNone},
		"cxc":                    {cxc, OwnerCXC},
		"cxc edited":             {append(append([]byte{}, cxc...), "x\n"...), OwnerOther},
		"crw":                    {crw, OwnerCRW},
		"crw edited":             {append(append([]byte{}, crw...), "# mine\n"...), OwnerOther},
		"user file":              {[]byte("name = \"executor\"\n"), OwnerOther},
		"empty file":             {[]byte{}, OwnerOther},
		"marker with bad digest": {[]byte("# codexclaw-managed: " + string(make([]byte, 0)) + "zz\nbody\n"), OwnerOther},
	} {
		if got := OwnerOf(c.raw); got != c.want {
			t.Errorf("%s: OwnerOf = %q, want %q", name, got, c.want)
		}
	}
}

func TestNativeRoleContentIsWhatRegisterRoleWrites(t *testing.T) {
	home := t.TempDir()
	for _, name := range NativeRoles() {
		res, err := RegisterRole(name, home)
		if err != nil {
			t.Fatal(err)
		}
		want, err := NativeRoleContent(name)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(res.Path)
		if string(got) != string(want) || res.Path != RoleFilePath(home, name) {
			t.Fatalf("%s: file differs from NativeRoleContent or path %q != %q", name, res.Path, RoleFilePath(home, name))
		}
		read, err := ReadRoleFile(home, name)
		if err != nil || string(read) != string(want) {
			t.Fatalf("ReadRoleFile = %q, %v", read, err)
		}
	}
	if _, err := NativeRoleContent("explorer"); err == nil {
		t.Fatal("explorer is not a native role")
	}
	if raw, err := ReadRoleFile(t.TempDir(), Executor); raw != nil || err != nil {
		t.Fatalf("absent role file = %q, %v", raw, err)
	}
}

func TestReadRoleFileRefusesASymlinkedAgentsDirectory(t *testing.T) {
	home, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "executor.toml"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "agents")); err != nil {
		t.Fatal(err)
	}
	if raw, err := ReadRoleFile(home, Executor); raw != nil || err == nil || !strings.Contains(err.Error(), "non-regular agents directory") {
		t.Fatalf("ReadRoleFile through a symlinked agents directory = %q, %v", raw, err)
	}
	if err := CheckAgentsDirectory(filepath.Join(home, "missing")); err != nil {
		t.Fatalf("an absent agents directory: %v", err)
	}
}
