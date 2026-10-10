package dagsched

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// CRW-1026 (d1): a verification record must carry the pins of the commit it verified. The required set comes from what the
// commit declares (go.mod toolchain, the staticcheck require, scripts/ci/secrets.sh, and node when its ci.yml has a setup-node
// step), never from the local PATH; each pin must equal its tools value and the tree's declaration. These tests need no
// store: they judge sealed records against a real git commit.

const pinnedGoMod = "module example.com/m\n\ngo 1.27\n\ntoolchain go1.27.1\n\nrequire (\n\thonnef.co/go/tools v0.8.1\n)\n"
const pinnedSecrets = "#!/usr/bin/env bash\nset -euo pipefail\nscan_version=8.30.1\nscan_archive=\"gitleaks_${scan_version}_linux_x64.tar.gz\"\n"
const pinnedCINode = "name: ci\non: [push]\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n" +
	"      - uses: actions/setup-node@v4\n        with:\n          # an exact release\n          node-version: '24.20.0'\n      - run: make test\n"
const pinnedCINoNode = "name: ci\non: [push]\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n      - run: make test\n"

// pinnedCommit commits the declaring files on a branch of a fresh repository and answers the repository and the commit.
func pinnedCommit(t *testing.T, files map[string]string) (*gitRepo, string) {
	t.Helper()
	repo := newGitRepo(t)
	base := repo.git("rev-parse", "dev")
	repo.git("checkout", "-q", "-b", "pinned", base)
	for path, body := range files {
		full := filepath.Join(repo.path, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		repo.git("add", path)
	}
	repo.git("commit", "-q", "-m", "declare the tools")
	return repo, repo.git("rev-parse", "HEAD")
}

func fullDeclaration() map[string]string {
	return map[string]string{"go.mod": pinnedGoMod, "scripts/ci/secrets.sh": pinnedSecrets, ".github/workflows/ci.yml": pinnedCINode}
}

func declaredAll() map[string]string {
	return map[string]string{"go": "1.27.1", "staticcheck": "0.8.1", "gitleaks": "8.30.1", "node": "24.20.0"}
}

func copyPins(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

// sealPinned seals a passing record of the commit with the given tools and pins; a nil map is a JSON null.
func sealPinned(t *testing.T, repo *gitRepo, head string, keys VerificationKeys, tools, pins map[string]string) []byte {
	t.Helper()
	raw, err := SealVerificationRecord(VerificationRecord{Runner: "local", Repository: "owner/repo", BaseCommit: keys.Base, HeadCommit: head,
		TreeHash: keys.Tree, CiDigest: keys.CiDigest, Tools: tools, Pins: pins, GoFlags: "", GoEnv: "", PinMismatch: []string{},
		Dependencies: keys.Dependencies, OS: runtime.GOOS, Arch: runtime.GOARCH, Result: "pass", Jobs: []json.RawMessage{}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// reseal rewrites members of a sealed record (a nil value deletes the member) and fixes the digest, the way a writer that
// produced this shape would have sealed it.
func reseal(t *testing.T, raw []byte, edit func(members map[string]any)) []byte {
	t.Helper()
	var members map[string]any
	if err := json.Unmarshal(raw, &members); err != nil {
		t.Fatal(err)
	}
	edit(members)
	delete(members, "digest")
	body, err := json.Marshal(members)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := VerificationRecordDigest(body)
	if err != nil {
		t.Fatal(err)
	}
	members["digest"] = digest
	out, err := json.Marshal(members)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func pinnedKeys(t *testing.T, repo *gitRepo, head string) VerificationKeys {
	t.Helper()
	keys, err := CommitVerificationKeys(context.Background(), repo.path, head)
	if err != nil {
		t.Fatal(err)
	}
	keys.Base = repo.git("rev-parse", "dev")
	keys.OS, keys.Arch = runtime.GOOS, runtime.GOARCH
	return keys
}

func TestCommitVerificationKeysReadTheDeclaredPinsOfTheCommit(t *testing.T) {
	repo, head := pinnedCommit(t, fullDeclaration())
	keys := pinnedKeys(t, repo, head)
	want := declaredAll()
	if len(keys.Pins) != len(want) {
		t.Fatalf("the commit declares %v; the keys read %v", want, keys.Pins)
	}
	for name, version := range want {
		if keys.Pins[name] != version {
			t.Errorf("pin %s = %q; the commit declares %q", name, keys.Pins[name], version)
		}
	}
	// a commit without setup-node declares no node; a commit declaring nothing still reads an empty, non-nil set
	oldRepo, oldHead := pinnedCommit(t, map[string]string{"go.mod": pinnedGoMod, "scripts/ci/secrets.sh": pinnedSecrets, ".github/workflows/ci.yml": pinnedCINoNode})
	oldKeys := pinnedKeys(t, oldRepo, oldHead)
	if _, ok := oldKeys.Pins["node"]; ok || oldKeys.Pins["go"] != "1.27.1" {
		t.Fatalf("a tree without setup-node declares no node pin, got %v", oldKeys.Pins)
	}
	bareRepo, bareHead := pinnedCommit(t, map[string]string{"x.txt": "x\n"})
	if bare := pinnedKeys(t, bareRepo, bareHead); bare.Pins == nil || len(bare.Pins) != 0 {
		t.Fatalf("a tree declaring nothing reads an empty set that is not nil (nil means unread), got %#v", bare.Pins)
	}
}

func TestDeclaredToolPinsOfTheRepositoryTree(t *testing.T) {
	// the real files of this repository: a format change that stops the reader from finding a pin is caught here
	root := filepath.Join("..", "..", "..")
	read := func(path string) ([]byte, bool, error) {
		body, err := os.ReadFile(filepath.Join(root, path))
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return body, err == nil, err
	}
	pins, err := DeclaredToolPins(read)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"go", "staticcheck", "gitleaks", "node"} {
		if strings.TrimSpace(pins[name]) == "" {
			t.Errorf("this repository's tree declares %s, but the reader found no pin: %v", name, pins)
		}
	}
}

func TestJudgeVerificationRecordRequiresTheDeclaredPins(t *testing.T) {
	repo, head := pinnedCommit(t, fullDeclaration())
	keys := pinnedKeys(t, repo, head)
	good := func() ([]byte, map[string]string) {
		return sealPinned(t, repo, head, keys, copyPins(declaredAll()), copyPins(declaredAll())), declaredAll()
	}
	if raw, _ := good(); true {
		if _, err := JudgeVerificationRecord(raw, keys); err != nil {
			t.Fatalf("a record whose pins and tools equal the commit's declaration is reusable: %v", err)
		}
	}
	refused := func(t *testing.T, raw []byte, want VerificationKeys, names ...string) {
		t.Helper()
		_, err := JudgeVerificationRecord(raw, want)
		if refusalReasonOf(err) != "disposition_conflict" {
			t.Fatalf("want disposition_conflict, got %v", err)
		}
		for _, name := range names {
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("the refusal must name %q: %v", name, err)
			}
		}
	}
	t.Run("pins member missing", func(t *testing.T) {
		raw, _ := good()
		refused(t, reseal(t, raw, func(m map[string]any) { delete(m, "pins") }), keys, "pins")
	})
	t.Run("pins null", func(t *testing.T) {
		raw, _ := good()
		refused(t, reseal(t, raw, func(m map[string]any) { m["pins"] = nil }), keys, "pins")
	})
	t.Run("pins empty while the commit declares tools", func(t *testing.T) {
		refused(t, sealPinned(t, repo, head, keys, copyPins(declaredAll()), map[string]string{}), keys, "go")
	})
	for _, name := range []string{"go", "staticcheck", "gitleaks", "node"} {
		t.Run("required pin missing: "+name, func(t *testing.T) {
			pins := copyPins(declaredAll())
			delete(pins, name)
			refused(t, sealPinned(t, repo, head, keys, copyPins(declaredAll()), pins), keys, name)
		})
		t.Run("required tool missing: "+name, func(t *testing.T) {
			tools := copyPins(declaredAll())
			delete(tools, name)
			refused(t, sealPinned(t, repo, head, keys, tools, copyPins(declaredAll())), keys, name)
		})
		t.Run("tool differs from its pin: "+name, func(t *testing.T) {
			tools := copyPins(declaredAll())
			tools[name] = "0.0.1-other"
			refused(t, sealPinned(t, repo, head, keys, tools, copyPins(declaredAll())), keys, name, "0.0.1-other")
		})
		// the judge compares with the version the verified commit declares, not only the pin names: tools and pins agree
		// with each other and both differ from the tree
		t.Run("tools and pins agree but differ from the tree: "+name, func(t *testing.T) {
			other := copyPins(declaredAll())
			other[name] = "9.9.9"
			refused(t, sealPinned(t, repo, head, keys, copyPins(other), copyPins(other)), keys, name, "9.9.9", declaredAll()[name])
		})
	}
	t.Run("a tool null", func(t *testing.T) {
		raw, _ := good()
		refused(t, reseal(t, raw, func(m map[string]any) { m["tools"] = nil }), keys, "go")
	})
	t.Run("the local PATH is never read", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		raw, _ := good()
		if _, err := JudgeVerificationRecord(raw, keys); err != nil {
			t.Fatalf("the judge must not depend on the PATH: %v", err)
		}
	})
}

// A record of an older tree (its ci.yml has no setup-node) stays valid without a node pin, and a tree that declares nothing
// requires nothing; a node pin the older tree never declared is still a pin the record must keep consistent with its tools.
func TestJudgeVerificationRecordOlderTreesNeedNoNodePin(t *testing.T) {
	repo, head := pinnedCommit(t, map[string]string{"go.mod": pinnedGoMod, "scripts/ci/secrets.sh": pinnedSecrets, ".github/workflows/ci.yml": pinnedCINoNode})
	keys := pinnedKeys(t, repo, head)
	noNode := declaredAll()
	delete(noNode, "node")
	if _, err := JudgeVerificationRecord(sealPinned(t, repo, head, keys, copyPins(noNode), copyPins(noNode)), keys); err != nil {
		t.Fatalf("a record of a tree without setup-node is valid with no node pin: %v", err)
	}
	if _, err := JudgeVerificationRecord(sealPinned(t, repo, head, keys, noNode, map[string]string{"go": "1.27.1"}), keys); refusalReasonOf(err) != "disposition_conflict" || !strings.Contains(err.Error(), "gitleaks") {
		t.Fatalf("the tools the older tree declares are still required, got %v", err)
	}
	bareRepo, bareHead := pinnedCommit(t, map[string]string{"x.txt": "x\n"})
	bare := pinnedKeys(t, bareRepo, bareHead)
	if _, err := JudgeVerificationRecord(sealPinned(t, bareRepo, bareHead, bare, map[string]string{}, map[string]string{}), bare); err != nil {
		t.Fatalf("a tree declaring no tool requires no pin: %v", err)
	}
}

// Keys that were not built from a commit say nothing about what the commit declares: the judge refuses rather than accept
// every record: there is no caller that waives the declaration (CRW-1026).
func TestJudgeVerificationRecordKeysWithoutADeclaration(t *testing.T) {
	repo, head := pinnedCommit(t, fullDeclaration())
	keys := pinnedKeys(t, repo, head)
	raw := sealPinned(t, repo, head, keys, copyPins(declaredAll()), copyPins(declaredAll()))
	unread := keys
	unread.Pins = nil
	if _, err := JudgeVerificationRecord(raw, unread); refusalReasonOf(err) != "disposition_conflict" {
		t.Fatalf("keys with no declared pins must not accept a record, got %v", err)
	}
	for name, edit := range map[string]func(map[string]any){
		"missing": func(m map[string]any) { delete(m, "pins") },
		"null":    func(m map[string]any) { m["pins"] = nil },
	} {
		if _, err := JudgeVerificationRecord(reseal(t, raw, edit), unread); refusalReasonOf(err) != "disposition_conflict" || !strings.Contains(err.Error(), "pins") {
			t.Fatalf("pins %s must be refused, got %v", name, err)
		}
	}
}

// The acceptance path reads the declaration of the commit it accepts: a record without the pins is refused there and a
// record with them is accepted.
func TestCommitAcceptanceRefusesARecordWithoutThePinsOfTheCommit(t *testing.T) {
	k := newCommitAcceptKit(t)
	k.repo.git("checkout", "-q", "-b", "pinned", k.base)
	for path, body := range fullDeclaration() {
		full := filepath.Join(k.repo.path, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		k.repo.git("add", path)
	}
	k.repo.git("commit", "-q", "-m", "declare the tools")
	k.head = k.repo.git("rev-parse", "HEAD")
	k.repo.git("checkout", "-q", "dev")
	k.report("g", "I")
	keys, err := CommitVerificationKeys(context.Background(), k.repo.path, k.head)
	if err != nil {
		t.Fatal(err)
	}
	keys.Base, keys.OS, keys.Arch = k.base, runtime.GOOS, runtime.GOARCH
	write := func(raw []byte) string {
		path := filepath.Join(t.TempDir(), "verification-record.json")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		return "@" + path
	}
	without := sealPinned(t, k.repo, k.head, keys, map[string]string{"go": "1.27.1"}, map[string]string{})
	if _, err := k.acceptCommit(write(without)); refusalReasonOf(err) != "disposition_conflict" || !strings.Contains(err.Error(), "pin for go") {
		t.Fatalf("a record without the commit's pins is refused on the acceptance path, got %v", err)
	}
	with := sealPinned(t, k.repo, k.head, keys, copyPins(declaredAll()), copyPins(declaredAll()))
	if _, err := k.acceptCommit(write(with)); err != nil {
		t.Fatalf("a record with the commit's pins is accepted: %v", err)
	}
}

// A step reads whole: `with:` before `uses:` is the same setup-node step as the reverse order, so its node declaration is
// collected and a record without the node pin, or with another, is refused. A setup-node step whose node-version cannot be
// read is an error, never "no Node declaration".
func TestDeclaredNodePinsDoNotDependOnTheOrderOfAStepsKeys(t *testing.T) {
	withFirst := "name: ci\non: [push]\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - with:\n          node-version: '24.20.0'\n        uses: actions/setup-node@v4\n"
	files := fullDeclaration()
	files[".github/workflows/ci.yml"] = withFirst
	repo, head := pinnedCommit(t, files)
	keys := pinnedKeys(t, repo, head)
	if keys.Pins["node"] != "24.20.0" {
		t.Fatalf("a setup-node step with the with block first lost its declaration: %v", keys.Pins)
	}
	noNode := declaredAll()
	delete(noNode, "node")
	if _, err := JudgeVerificationRecord(sealPinned(t, repo, head, keys, copyPins(noNode), copyPins(noNode)), keys); refusalReasonOf(err) != "disposition_conflict" || !strings.Contains(err.Error(), "node") {
		t.Fatalf("a record without the node pin must be refused for a tree with setup-node, got %v", err)
	}
	wrongNode := declaredAll()
	wrongNode["node"] = "20.0.0"
	if _, err := JudgeVerificationRecord(sealPinned(t, repo, head, keys, copyPins(wrongNode), copyPins(wrongNode)), keys); refusalReasonOf(err) != "disposition_conflict" || !strings.Contains(err.Error(), "node") {
		t.Fatalf("a record with another node pin must be refused, got %v", err)
	}
	if _, err := JudgeVerificationRecord(sealPinned(t, repo, head, keys, copyPins(declaredAll()), copyPins(declaredAll())), keys); err != nil {
		t.Fatalf("the record that equals the declaration is reusable: %v", err)
	}
	for name, ci := range map[string]string{
		"no node-version":         "jobs:\n  build:\n    steps:\n      - uses: actions/setup-node@v4\n        with:\n          node-version-file: .nvmrc\n",
		"no with":                 "jobs:\n  build:\n    steps:\n      - uses: actions/setup-node@v4\n",
		"an empty node-version":   "jobs:\n  build:\n    steps:\n      - with:\n          node-version: ''\n        uses: actions/setup-node@v4\n",
		"the last step, no value": "jobs:\n  build:\n    steps:\n      - run: make\n      - uses: actions/setup-node@v4\n",
	} {
		_, err := DeclaredToolPins(func(path string) ([]byte, bool, error) {
			if path == pinWorkflowFile {
				return []byte(ci), true, nil
			}
			return nil, false, nil
		})
		if err == nil {
			t.Errorf("%s: a setup-node step whose version cannot be read must be an error", name)
		}
	}
}
