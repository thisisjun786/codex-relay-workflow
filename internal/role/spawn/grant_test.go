package spawn

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests replay testdata/grant/oracle.json, recorded by record-grant.mjs from the CXC
// v0.2.40 functions themselves, and add what the oracle has no counterpart for: the directory
// checks, a grant file that is not a regular file, a foreign owner and the exclusive create.
type spawnGrantOracle struct {
	BaseMs    int64
	Constants struct {
		Token string
		TTLMs int64
	}
	Spawner []struct {
		Obj      map[string]any
		Expected bool
	}
	Paths []struct {
		Obj                                       map[string]any
		Nonce, ScopeDigest, File, ExpectedDirName string
		Scope                                     *struct{ Cwd, Session string }
	}
	MintShape struct {
		At                                        int64
		Content, FileMode, KeyDirMode, UIDDirMode string
	}
	Sequences []struct {
		Name  string
		Steps []struct {
			Op, As, Message string
			Obj             map[string]any
			At              int64
			Expected        any
		}
	}
	Planted []struct {
		Content, Nonce string
		Expected       bool
	}
	Unsafe []struct {
		Name     string
		Expected struct{ Minted, Consumed bool }
	}
}

func spawnGrantTestOracle(t *testing.T) spawnGrantOracle {
	t.Helper()
	raw, err := os.ReadFile("testdata/grant/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture spawnGrantOracle
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// spawnGrantTestText expands the recorder's placeholders: {LONE} is a lone surrogate as this
// repository's JSON holds it (WTF-8), {N1}.. a minted nonce with the :upper, :short and :long variants.
func spawnGrantTestText(s string, nonces map[string]string) string {
	pairs := []string{"{LONE}", "\xed\xa0\x80"}
	for key, n := range nonces {
		pairs = append(pairs, "{"+key+":upper}", strings.ToUpper(n), "{"+key+":short}", strings.TrimSuffix(n, n[max(len(n)-1, 0):]), "{"+key+":long}", n+"f", "{"+key+"}", n)
	}
	return strings.NewReplacer(pairs...).Replace(s)
}

func spawnGrantTestObj(obj map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range obj {
		if s, ok := v.(string); ok {
			v = spawnGrantTestText(s, nil)
		}
		out[k] = v
	}
	return out
}

var spawnGrantTestScope = map[string]any{"cwd": "/ws/a", "session_id": "s1"}

// spawnGrantTestValid is a grant body that expires in the year 2286.
const spawnGrantTestValid = "{\"expiresAt\":9999999999999}"

func spawnGrantTestKeyDir(t *testing.T, root string, uid int, obj map[string]any) string {
	t.Helper()
	key, ok := spawnGrantKey(obj)
	if !ok {
		t.Fatal("no key")
	}
	return filepath.Join(root, spawnGrantDirName(uid), key)
}

// spawnGrantTestWithin fails the test instead of hanging when fn blocks, as opening a FIFO would.
func spawnGrantTestWithin(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a grant call blocks on a special file")
	}
}

func spawnGrantTestTree(t *testing.T, dir string) string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		out = append(out, fmt.Sprintf("%s %v", path, info.Mode()))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, "\n")
}

func TestIsSubagentSpawner(t *testing.T) {
	for _, c := range spawnGrantTestOracle(t).Spawner {
		if got := IsSubagentSpawner(c.Obj); got != c.Expected {
			t.Errorf("%v: got %v, want %v", c.Obj, got, c.Expected)
		}
	}
}

func TestGrantOraclePaths(t *testing.T) {
	fixture := spawnGrantTestOracle(t)
	uid := os.Getuid()
	if fixture.Constants.Token != "CXC-SUBSPAWN-ALLOWED" || strings.ReplaceAll(fixture.Constants.Token, "CXC", "CRW") != spawnGrantToken ||
		strings.ReplaceAll("[CXC-SUBSPAWN-GRANT:", "CXC", "CRW") != spawnGrantMarker || fixture.Constants.TTLMs != spawnGrantTTL.Milliseconds() {
		t.Errorf("constants differ from the oracle: %+v", fixture.Constants)
	}
	for _, c := range fixture.Paths {
		obj := spawnGrantTestObj(c.Obj)
		if lone := strings.Contains(fmt.Sprint(c.Obj), "{LONE}"); lone != strings.Contains(fmt.Sprint(obj), "\xed\xa0\x80") {
			t.Fatalf("%v: the lone surrogate must reach the code as WTF-8 bytes", c.Obj)
		}
		key, ok := spawnGrantKey(obj)
		if !ok || key != c.ScopeDigest || spawnGrantFile(c.Nonce) != c.File || spawnGrantDirName(uid) != strings.ReplaceAll(c.ExpectedDirName, "{UID}", strconv.Itoa(uid)) {
			t.Errorf("%v: key %q file %q dir %q, want %q %q %q", c.Obj, key, spawnGrantFile(c.Nonce), spawnGrantDirName(uid), c.ScopeDigest, c.File, c.ExpectedDirName)
		}
		if cwd, session, _ := spawnGrantScope(obj); c.Scope != nil && (cwd != c.Scope.Cwd || session != c.Scope.Session) {
			t.Errorf("%v: scope %q %q, want %+v", c.Obj, cwd, session, *c.Scope)
		}
	}
	if spawnGrantDirName(-1) != "crw-subspawn-user" {
		t.Errorf("no uid reads user: %q", spawnGrantDirName(-1))
	}
}

// A missing or relative cwd resolves against the kernel's working directory, which process.cwd()
// answers; os.Getwd would answer the $PWD link instead. The digests are computed here.
func TestGrantDefaultCwdIsTheKernelDirectory(t *testing.T) {
	realDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	t.Chdir(realDir)
	t.Setenv("PWD", link)
	if wd, _ := os.Getwd(); wd != link {
		t.Fatalf("arrangement failed: os.Getwd answers %q, not %q", wd, link)
	}
	digest := func(cwd, session string) string {
		sum := sha256.Sum256([]byte(cwd + "\x00" + session))
		return hex.EncodeToString(sum[:])
	}
	for obj, want := range map[string]string{`{}`: digest(realDir, "unknown-session"), `{"cwd":"sub/../x","session_id":"s"}`: digest(filepath.Join(realDir, "x"), "s")} {
		var parsed map[string]any
		_ = json.Unmarshal([]byte(obj), &parsed)
		if key, ok := spawnGrantKey(parsed); !ok || key != want {
			t.Errorf("%s: key %q, want %q", obj, key, want)
		}
	}
}

func TestGrantOracleSequences(t *testing.T) {
	fixture := spawnGrantTestOracle(t)
	uid := os.Getuid()
	for _, sequence := range fixture.Sequences {
		t.Run(sequence.Name, func(t *testing.T) {
			root, nonces := t.TempDir(), map[string]string{}
			for i, step := range sequence.Steps {
				now, obj := time.UnixMilli(fixture.BaseMs+step.At), spawnGrantTestObj(step.Obj)
				switch step.Op {
				case "mint":
					nonce, ok := MintRecursionGrant(obj, root, now)
					nonces[step.As] = nonce
					if ok != step.Expected.(bool) {
						t.Errorf("step %d: mint %v, want %v", i+1, ok, step.Expected)
					}
				case "count":
					entries, _ := os.ReadDir(spawnGrantTestKeyDir(t, root, uid, obj))
					if len(entries) != int(step.Expected.(float64)) {
						t.Errorf("step %d: %d files left, want %v", i+1, len(entries), step.Expected)
					}
				default:
					if got := ConsumeRecursionGrant(obj, spawnGrantTestText(step.Message, nonces), root, now); got != step.Expected.(bool) {
						t.Errorf("step %d: consume %v, want %v", i+1, got, step.Expected)
					}
				}
			}
		})
	}
}

func TestGrantOraclePlantedBodies(t *testing.T) {
	fixture := spawnGrantTestOracle(t)
	root := t.TempDir()
	dir := spawnGrantTestKeyDir(t, root, os.Getuid(), spawnGrantTestScope)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Planted {
		if err := os.WriteFile(filepath.Join(dir, spawnGrantFile(c.Nonce)), []byte(c.Content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := ConsumeRecursionGrant(spawnGrantTestScope, spawnGrantMarker+c.Nonce+"]", root, time.UnixMilli(fixture.BaseMs)); got != c.Expected {
			t.Errorf("body %q: got %v, want %v", c.Content, got, c.Expected)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("body %q left %d files, the oracle's finally removes the claimed one", c.Content, len(entries))
		}
	}
}

func TestGrantMintedShape(t *testing.T) {
	fixture := spawnGrantTestOracle(t)
	root := t.TempDir()
	nonce, ok := MintRecursionGrant(spawnGrantTestScope, root, time.UnixMilli(fixture.BaseMs+fixture.MintShape.At))
	if !ok || len(nonce) != 64 {
		t.Fatalf("mint: %q %v", nonce, ok)
	}
	dir := spawnGrantTestKeyDir(t, root, os.Getuid(), spawnGrantTestScope)
	file := filepath.Join(dir, spawnGrantFile(nonce))
	content, err := os.ReadFile(file)
	mode := func(path string) string {
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatal(statErr)
		}
		return strconv.FormatUint(uint64(info.Mode().Perm()), 8)
	}
	if err != nil || string(content) != fixture.MintShape.Content || mode(file) != fixture.MintShape.FileMode ||
		mode(dir) != fixture.MintShape.KeyDirMode || mode(filepath.Dir(dir)) != fixture.MintShape.UIDDirMode {
		t.Errorf("minted shape differs from the oracle: %q %v %+v", content, err, fixture.MintShape)
	}
	if second, _ := MintRecursionGrant(spawnGrantTestScope, root, time.UnixMilli(fixture.BaseMs)); second == nonce {
		t.Error("two grants share a nonce")
	}
}

func TestGrantWriteKeepsAnExistingName(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	nonce := strings.Repeat("c", 64)
	file := filepath.Join(dir, spawnGrantFile(nonce))
	if err := os.WriteFile(file, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if spawnGrantWrite(root, nonce, time.UnixMilli(1)) {
		t.Error("an existing grant name was overwritten")
	}
	if got, err := os.ReadFile(file); err != nil || string(got) != "keep" {
		t.Errorf("the existing file must stay: %q %v", got, err)
	}
}

// Consuming only reads: with no grant tree under the temp root, nothing is created there.
func TestGrantConsumeCreatesNothing(t *testing.T) {
	root := t.TempDir()
	if ConsumeRecursionGrant(spawnGrantTestScope, spawnGrantMarker+strings.Repeat("a", 64)+"]", root, time.UnixMilli(1)) {
		t.Error("consumed a grant that was never minted")
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("consume created %d entries under the temp root", len(entries))
	}
}

func TestGrantFollowsTempRootLink(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(t.TempDir(), "tmp")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	now := time.UnixMilli(1)
	nonce, ok := MintRecursionGrant(spawnGrantTestScope, link, now)
	if !ok || !ConsumeRecursionGrant(spawnGrantTestScope, spawnGrantMarker+nonce+"]", root, now) {
		t.Errorf("a temp root reached through a link (macOS /tmp) must work: %q %v", nonce, ok)
	}
}

// The directory conditions the oracle accepts and the port refuses: Mint yields no grant and
// changes nothing, Consume is false and a planted valid grant stays where it is. The links are
// relative and stay inside the temp root, which os.Root would follow.
func TestGrantRefusesUnsafeDirectories(t *testing.T) {
	fixture := spawnGrantTestOracle(t)
	mkdir := func(path string, mode os.FileMode) {
		if err := os.MkdirAll(path, 0o700); err != nil || os.Chmod(path, mode) != nil {
			t.Fatal("arrange", path, err)
		}
	}
	mkfifo := func(path string) {
		if err := syscall.Mkfifo(path, 0o700); err != nil || os.Chmod(path, 0o700) != nil {
			t.Fatal("arrange", path, err)
		}
	}
	link := func(root, uidDir, keyDir string, key bool) {
		if key {
			mkdir(filepath.Join(uidDir, "elsewhere"), 0o700)
			_ = os.Symlink("elsewhere", keyDir)
			return
		}
		mkdir(filepath.Join(root, "elsewhere", filepath.Base(keyDir)), 0o700)
		_ = os.Symlink("elsewhere", uidDir)
	}
	arrange := map[string]func(root, uidDir, keyDir string){
		"key directory mode 0755":       func(_, u, k string) { mkdir(u, 0o700); mkdir(k, 0o755) },
		"key directory mode 0770":       func(_, u, k string) { mkdir(u, 0o700); mkdir(k, 0o770) },
		"uid directory mode 0755":       func(_, u, k string) { mkdir(k, 0o700); mkdir(u, 0o755) },
		"uid directory mode 0770":       func(_, u, k string) { mkdir(k, 0o700); mkdir(u, 0o770) },
		"uid directory is a symlink":    func(r, u, k string) { link(r, u, k, false) },
		"key directory is a symlink":    func(r, u, k string) { link(r, u, k, true) },
		"uid directory is a plain file": func(_, u, _ string) { _ = os.WriteFile(u, []byte("x"), 0o600) },
		"key directory is a plain file": func(_, u, k string) { mkdir(u, 0o700); _ = os.WriteFile(k, []byte("x"), 0o600) },
		"uid directory is a FIFO":       func(_, u, _ string) { mkfifo(u) },
		"key directory is a FIFO":       func(_, u, k string) { mkdir(u, 0o700); mkfifo(k) },
	}
	if len(fixture.Unsafe) != len(arrange) {
		t.Fatalf("%d recorded cases, %d arranged", len(fixture.Unsafe), len(arrange))
	}
	now, nonce, uid := time.UnixMilli(fixture.BaseMs), strings.Repeat("b", 64), os.Getuid()
	for _, c := range fixture.Unsafe {
		t.Run(c.Name, func(t *testing.T) {
			root := t.TempDir()
			keyDir := spawnGrantTestKeyDir(t, root, uid, spawnGrantTestScope)
			arrange[c.Name](root, filepath.Dir(keyDir), keyDir)
			before := spawnGrantTestTree(t, root)
			spawnGrantTestWithin(t, func() {
				if got, ok := MintRecursionGrant(spawnGrantTestScope, root, now); ok != c.Expected.Minted || got != "" {
					t.Errorf("mint gave %q %v", got, ok)
				}
			})
			if after := spawnGrantTestTree(t, root); after != before {
				t.Errorf("a refused mint changed the tree:\nbefore %s\nafter %s", before, after)
			}
			planted := filepath.Join(keyDir, spawnGrantFile(nonce))
			planting := os.WriteFile(planted, []byte(spawnGrantTestValid), 0o600)
			spawnGrantTestWithin(t, func() {
				if got := ConsumeRecursionGrant(spawnGrantTestScope, spawnGrantMarker+nonce+"]", root, now); got != c.Expected.Consumed {
					t.Errorf("consume = %v, want %v", got, c.Expected.Consumed)
				}
			})
			if _, err := os.Stat(planted); planting == nil && err != nil {
				t.Errorf("consume touched the planted grant: %v", err)
			}
		})
	}
	t.Run("another owner, through the uid seam", func(t *testing.T) {
		root := t.TempDir()
		keyDir := spawnGrantTestKeyDir(t, root, uid+1, spawnGrantTestScope)
		mkdir(keyDir, 0o700)
		planted := filepath.Join(keyDir, spawnGrantFile(nonce))
		if err := os.WriteFile(planted, []byte(spawnGrantTestValid), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, ok := spawnGrantMint(spawnGrantTestScope, root, uid+1, now); ok || got != "" {
			t.Errorf("minted into a directory owned by another uid: %q", got)
		}
		if spawnGrantConsume(spawnGrantTestScope, spawnGrantMarker+nonce+"]", root, uid+1, now) {
			t.Error("consumed from a directory owned by another uid")
		}
		if _, err := os.Stat(planted); err != nil {
			t.Errorf("consume touched the planted grant: %v", err)
		}
	})
	if _, ok := MintRecursionGrant(spawnGrantTestScope, "", now); ok {
		t.Error("an empty temp root must not mint")
	}
}

func TestGrantRefusesNonRegularFile(t *testing.T) {
	now, uid := time.UnixMilli(1), os.Getuid()
	for _, kind := range []string{"fifo", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			nonce, ok := MintRecursionGrant(spawnGrantTestScope, root, now)
			if !ok {
				t.Fatal("mint")
			}
			keyDir := spawnGrantTestKeyDir(t, root, uid, spawnGrantTestScope)
			grant, valid := filepath.Join(keyDir, spawnGrantFile(nonce)), filepath.Join(keyDir, "valid.json")
			if err := os.WriteFile(valid, []byte(spawnGrantTestValid), 0o600); err != nil || os.Remove(grant) != nil {
				t.Fatal("arrange")
			}
			if kind == "fifo" {
				if err := syscall.Mkfifo(grant, 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Symlink("valid.json", grant); err != nil {
				t.Fatal(err)
			}
			spawnGrantTestWithin(t, func() {
				if ConsumeRecursionGrant(spawnGrantTestScope, spawnGrantMarker+nonce+"]", root, now) {
					t.Error("a " + kind + " was accepted as a grant")
				}
			})
			if _, err := os.Stat(valid); err != nil {
				t.Errorf("the link target was touched: %v", err)
			}
		})
	}
}
