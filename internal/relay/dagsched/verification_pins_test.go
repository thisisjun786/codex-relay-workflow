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

// declaredPinsOf reads the pins of in-memory declaration files (a file not in the map is absent).
func declaredPinsOf(files map[string]string) (map[string]string, error) {
	return DeclaredToolPins(func(path string) ([]byte, bool, error) {
		body, ok := files[path]
		return []byte(body), ok, nil
	})
}

// refusedPinRecord asserts that a record with these tools and pins is refused as disposition_conflict naming the tool.
func refusedPinRecord(t *testing.T, repo *gitRepo, head string, keys VerificationKeys, record map[string]string, tool string) {
	t.Helper()
	_, err := JudgeVerificationRecord(sealPinned(t, repo, head, keys, copyPins(record), copyPins(record)), keys)
	if refusalReasonOf(err) != "disposition_conflict" || !strings.Contains(err.Error(), tool) {
		t.Fatalf("a record %v must be refused as disposition_conflict naming %s, got %v", record, tool, err)
	}
}

// A setup-node step whose uses value is a quoted scalar is the same step as an unquoted one: its node-version is required,
// and a record without the node pin, or with another, is refused (verification round 2, finding 1).
func TestDeclaredNodePinsOfAQuotedSetupNodeStep(t *testing.T) {
	for name, uses := range map[string]string{
		"double quotes":        `"actions/setup-node@v4"`,
		"single quotes":        `'actions/setup-node@v4'`,
		"quotes and a comment": `"actions/setup-node@49933ea5288caeca8642d1e84afbd3f7d6820020" # v4`,
	} {
		t.Run(name, func(t *testing.T) {
			files := fullDeclaration()
			files[".github/workflows/ci.yml"] = "name: ci\non: [push]\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n" +
				"      - name: Set up Node\n        uses: " + uses + "\n        with:\n          node-version: '24.20.0'\n      - run: make test\n"
			repo, head := pinnedCommit(t, files)
			keys := pinnedKeys(t, repo, head)
			if keys.Pins["node"] != "24.20.0" {
				t.Fatalf("a quoted setup-node step declares node 24.20.0, the keys read %v", keys.Pins)
			}
			noNode := declaredAll()
			delete(noNode, "node")
			refusedPinRecord(t, repo, head, keys, noNode, "node")
			wrong := declaredAll()
			wrong["node"] = "20.0.0"
			refusedPinRecord(t, repo, head, keys, wrong, "node")
			if _, err := JudgeVerificationRecord(sealPinned(t, repo, head, keys, copyPins(declaredAll()), copyPins(declaredAll())), keys); err != nil {
				t.Fatalf("the record that equals the declaration is reusable: %v", err)
			}
		})
	}
}

// go.mod declares the toolchain and the staticcheck require in every form the go command reads: a single-line require, a
// require block entry with a comment, a CRLF file, a quoted module path. Each form yields the pins, and a record without
// them, or with other versions, is refused (verification round 2, finding 2).
func TestDeclaredGoModPinsInEveryRequireForm(t *testing.T) {
	for name, goMod := range map[string]string{
		"single-line require": "module example.com/m\n\ngo 1.27\n\ntoolchain go1.27.1\n\nrequire honnef.co/go/tools v0.8.1\n",
		"indirect in a block": "module example.com/m\n\ngo 1.27\n\ntoolchain go1.27.1\n\nrequire (\n\tgolang.org/x/sys v0.47.0\n\thonnef.co/go/tools v0.8.1 // indirect\n)\n",
		"CRLF":                "module example.com/m\r\n\r\ngo 1.27\r\n\r\ntoolchain go1.27.1\r\n\r\nrequire (\r\n\thonnef.co/go/tools v0.8.1\r\n)\r\n",
		"comments":            "module example.com/m // the module\n\ngo 1.27\n\ntoolchain go1.27.1 // pinned\n\nrequire ( // tools\n\thonnef.co/go/tools v0.8.1 // staticcheck\n)\n",
		"quoted module path":  "module example.com/m\n\ngo 1.27\n\ntoolchain go1.27.1\n\nrequire \"honnef.co/go/tools\" v0.8.1\n",
	} {
		t.Run(name, func(t *testing.T) {
			files := fullDeclaration()
			files["go.mod"] = goMod
			repo, head := pinnedCommit(t, files)
			keys := pinnedKeys(t, repo, head)
			if keys.Pins["go"] != "1.27.1" || keys.Pins["staticcheck"] != "0.8.1" {
				t.Fatalf("go.mod declares go 1.27.1 and staticcheck 0.8.1, the keys read %v", keys.Pins)
			}
			for _, tool := range []string{"go", "staticcheck"} {
				missing := declaredAll()
				delete(missing, tool)
				refusedPinRecord(t, repo, head, keys, missing, tool)
				wrong := declaredAll()
				wrong[tool] = "9.9.9"
				refusedPinRecord(t, repo, head, keys, wrong, tool)
			}
			if _, err := JudgeVerificationRecord(sealPinned(t, repo, head, keys, copyPins(declaredAll()), copyPins(declaredAll())), keys); err != nil {
				t.Fatalf("the record that equals the declaration is reusable: %v", err)
			}
		})
	}
	// a declaration the reader cannot read is an error, never "the tool is not declared"
	for name, goMod := range map[string]string{
		"a toolchain that is no go version": "module example.com/m\n\ntoolchain banana\n",
		"two toolchains":                    "module example.com/m\n\ntoolchain go1.27.1\ntoolchain go1.26.0\n",
		"a require with no version":         "module example.com/m\n\nrequire honnef.co/go/tools\n",
		"a require that is no version":      "module example.com/m\n\nrequire (\n\thonnef.co/go/tools latest\n)\n",
		"two staticcheck versions":          "module example.com/m\n\nrequire honnef.co/go/tools v0.8.1\nrequire honnef.co/go/tools v0.7.0\n",
		"an open block":                     "module example.com/m\n\nrequire (\n\thonnef.co/go/tools v0.8.1\n",
	} {
		if pins, err := declaredPinsOf(map[string]string{"go.mod": goMod}); err == nil {
			t.Errorf("%s: an unreadable go.mod declaration must be an error, got %v", name, pins)
		}
	}
	// a go.mod that declares neither still reads an empty set
	if pins, err := declaredPinsOf(map[string]string{"go.mod": "module example.com/m\n\ngo 1.21\n\nrequire golang.org/x/sys v0.47.0\n"}); err != nil || len(pins) != 0 {
		t.Fatalf("a go.mod with no toolchain and no staticcheck declares nothing, got %v, %v", pins, err)
	}
}

// A setup-node step ends with the step: the with block of a later job (a reusable workflow call), of a later step or of the
// job itself never stands in for the node-version the setup-node step lacks (verification round 2, finding 3).
func TestSetupNodeCannotTakeTheWithOfAnotherStepOrJob(t *testing.T) {
	for name, ci := range map[string]string{
		"the next job's with": "jobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/setup-node@v4\n" +
			"  b:\n    uses: ./.github/workflows/reusable.yml\n    with:\n      node-version: '24.20.0'\n",
		"a job key after the steps":           "jobs:\n  a:\n    steps:\n      - uses: actions/setup-node@v4\n    with:\n      node-version: '24.20.0'\n",
		"the next step's with, compact steps": "jobs:\n  a:\n    steps:\n    - uses: actions/setup-node@v4\n    - uses: actions/cache@v4\n      with:\n        node-version: '24.20.0'\n",
		"a sibling key of the steps list":     "jobs:\n  a:\n    steps:\n      - uses: actions/setup-node@v4\n    strategy:\n      matrix:\n        node-version: ['24.20.0']\n",
		"a flow step":                         "jobs:\n  a:\n    steps:\n      - {uses: actions/setup-node@v4, with: {node-version: '24.20.0'}}\n",
		"a job-level setup-node":              "jobs:\n  a:\n    uses: actions/setup-node@v4\n    with:\n      node-version: '24.20.0'\n",
		"a structure the reader cannot read": "jobs:\n  a:\n    steps:\n      - uses: actions/setup-node@v4\n        with:\n          node-version: '24.20.0'\n" +
			"       broken: indentation\n",
	} {
		if pins, err := declaredPinsOf(map[string]string{pinWorkflowFile: ci}); err == nil {
			t.Errorf("%s: a setup-node step with no node-version of its own must be an error, got %v", name, pins)
		}
	}
	// the same shapes with the node-version on the step itself read it, and the other with blocks add nothing
	for name, ci := range map[string]string{
		"the next job's with": "jobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/setup-node@v4\n        with:\n          node-version: '24.20.0'\n" +
			"  b:\n    uses: ./.github/workflows/reusable.yml\n    with:\n      node-version: '22.0.0'\n",
		"compact steps":                        "jobs:\n  a:\n    steps:\n    - uses: actions/setup-node@v4\n      with:\n        node-version: 24.20.0 # exact\n    - uses: actions/cache@v4\n      with:\n        node-version: '22.0.0'\n",
		"a literal run block that mentions it": "jobs:\n  a:\n    steps:\n      - run: |\n          echo uses: actions/setup-node@v4\n          echo with:\n      - uses: actions/setup-node@v4\n        with:\n          node-version: \"24.20.0\"\n",
		"a step name that mentions it":         "jobs:\n  a:\n    steps:\n      - name: actions/setup-node@v4\n        uses: actions/setup-node@v4\n        with:\n          node-version: '24.20.0'\n",
	} {
		pins, err := declaredPinsOf(map[string]string{pinWorkflowFile: ci})
		if err != nil || pins["node"] != "24.20.0" {
			t.Errorf("%s: want node 24.20.0, got %v, %v", name, pins, err)
		}
	}
}

// A setup-node use written as a YAML block scalar is the same step as a plain one: the reader either reads its node-version
// or refuses the declaration, so a record without the node pin, or with a self-consistent wrong one, is never judged against
// a declaration that silently lost the step (verification round 3, d2).
func TestDeclaredNodePinsRefuseABlockScalarSetupNodeUse(t *testing.T) {
	for name, header := range map[string]string{
		"folded, stripped": ">-",
		"folded":           ">",
		"literal":          "|",
		"literal, kept":    "|+",
		"indicator":        ">2-",
	} {
		ci := "jobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: " + header + "\n          actions/setup-node@v4\n        with:\n          node-version: '24.20.0'\n      - run: make test\n"
		if pins, err := declaredPinsOf(map[string]string{pinWorkflowFile: ci}); refusalReasonOf(err) != "disposition_conflict" {
			t.Errorf("%s: a block scalar uses that names setup-node must be refused, got %v, %v", name, pins, err)
		}
	}
	// the judge refuses the commit's keys instead of judging a record against a lost declaration
	files := fullDeclaration()
	files[pinWorkflowFile] = "jobs:\n  build:\n    steps:\n      - uses: >-\n          actions/setup-node@v4\n        with:\n          node-version: '24.20.0'\n"
	repo, head := pinnedCommit(t, files)
	if _, err := CommitVerificationKeys(context.Background(), repo.path, head); refusalReasonOf(err) != "disposition_conflict" {
		t.Fatalf("keys of a commit whose setup-node use is a block scalar must be refused, got %v", err)
	}
	// a block scalar that is another action, or a run body that mentions setup-node, is not a setup-node use
	for name, ci := range map[string]string{
		"another action":         "jobs:\n  a:\n    steps:\n      - uses: >-\n          actions/checkout@v4\n",
		"a run body that quotes": "jobs:\n  a:\n    steps:\n      - run: |\n          uses: >-\n            actions/setup-node@v4\n",
	} {
		if pins, err := declaredPinsOf(map[string]string{pinWorkflowFile: ci}); err != nil || len(pins) != 0 {
			t.Errorf("%s: declares no node, got %v, %v", name, pins, err)
		}
	}
}

// secrets.sh declares the Gitleaks version by assigning scan_version. Any equivalent spelling of the assignment is read, and
// an assignment the reader cannot read is an error: a real scan_version never reads as "the tool is not declared"
// (verification round 3, d3).
func TestDeclaredGitleaksPinInEveryAssignmentForm(t *testing.T) {
	for name, line := range map[string]string{
		"plain":                     "scan_version=8.30.1\n",
		"single quotes":             "scan_version='8.30.1'\n",
		"double quotes":             "scan_version=\"8.30.1\"\n",
		"a trailing comment":        "scan_version=8.30.1 # pinned\n",
		"quotes and a comment":      "scan_version='8.30.1'   # pinned\n",
		"export":                    "export scan_version=8.30.1\n",
		"readonly":                  "readonly scan_version=8.30.1\n",
		"indented":                  "  scan_version=8.30.1\n",
		"CRLF":                      "scan_version=8.30.1\r\n",
		"the same value twice":      "scan_version=8.30.1\nscan_version=8.30.1\n",
		"no newline at end of file": "scan_version=8.30.1",
	} {
		pins, err := declaredPinsOf(map[string]string{pinSecretsFile: "#!/usr/bin/env bash\nset -eu\n" + line})
		if err != nil || pins["gitleaks"] != "8.30.1" {
			t.Errorf("%s: want gitleaks 8.30.1, got %v, %v", name, pins, err)
		}
	}
	for name, line := range map[string]string{
		"a command substitution": "scan_version=$(cat VERSION)\n",
		"a parameter expansion":  "scan_version=\"${GITLEAKS_VERSION:-8.30.1}\"\n",
		"a variable":             "scan_version=$pinned\n",
		"an append":              "scan_version+=.1\n",
		"an empty value":         "scan_version=\n",
		"two versions":           "scan_version=8.30.1\nscan_version=8.29.0\n",
		"a tail after the value": "scan_version=8.30.1 && true\n",
		"a mismatched quote":     "scan_version='8.30.1\n",
		"a declare":              "declare -r scan_version=\"$X\"\n",
	} {
		if pins, err := declaredPinsOf(map[string]string{pinSecretsFile: "#!/usr/bin/env bash\n" + line}); refusalReasonOf(err) != "disposition_conflict" {
			t.Errorf("%s: an unreadable scan_version assignment must be refused, got %v, %v", name, pins, err)
		}
	}
	// mentions of the name that assign nothing declare nothing and are not errors
	for name, body := range map[string]string{
		"a comment":        "# scan_version=9.9.9\n",
		"a use":            "echo \"${scan_version}\" scan_version=1\n",
		"no scan_version":  "echo hello\n",
		"another variable": "my_scan_version=1.2.3\n",
	} {
		if pins, err := declaredPinsOf(map[string]string{pinSecretsFile: "#!/usr/bin/env bash\n" + body}); err != nil || len(pins) != 0 {
			t.Errorf("%s: declares nothing, got %v, %v", name, pins, err)
		}
	}
	// the judge refuses a record that omits the pin or carries a self-consistent wrong one, for a quoted assignment
	files := fullDeclaration()
	files[pinSecretsFile] = "#!/usr/bin/env bash\nscan_version='8.30.1'\n"
	repo, head := pinnedCommit(t, files)
	keys := pinnedKeys(t, repo, head)
	if keys.Pins["gitleaks"] != "8.30.1" {
		t.Fatalf("the keys of a quoted assignment read %v", keys.Pins)
	}
	missing := declaredAll()
	delete(missing, "gitleaks")
	refusedPinRecord(t, repo, head, keys, missing, "gitleaks")
	wrong := declaredAll()
	wrong["gitleaks"] = "8.29.0"
	refusedPinRecord(t, repo, head, keys, wrong, "gitleaks")
}
