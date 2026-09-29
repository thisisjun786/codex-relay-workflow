package service_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/service"
)

// The launch declaration is read in one place (service.ResolveLaunchPolicyAt) and answers
// what Python's resolve_launch_policy answers for the same tree, byte for byte: every way the
// declaration can be absent, unreadable, undecodable, too deep, not an object or not naming a
// file, every way the policy file it names can fail, and the environment beside it. The
// oracle is testdata/launch_policy.json, captured once by launch_policy_capture.py, whose
// build() this file's buildLaunchCase mirrors.

type launchContent struct {
	Text     *string `json:"text"`
	Hex      *string `json:"hex"`
	Generate string  `json:"generate"`
	Depth    int     `json:"depth"`
	Special  string  `json:"special"`
	Mode     *int    `json:"mode"`
}

type launchCase struct {
	Launch      *launchContent           `json:"launch"`
	Files       map[string]launchContent `json:"files"`
	Environment *string                  `json:"environment"`
	Resolution  string                   `json:"resolution"`
}

type launchGolden struct {
	Policy string                `json:"policy"`
	Other  string                `json:"other"`
	Cases  map[string]launchCase `json:"cases"`
}

func loadLaunchGolden(t *testing.T) launchGolden {
	t.Helper()
	raw, err := os.ReadFile("testdata/launch_policy.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden launchGolden
	if err = json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	return golden
}

// generateDeep is launch_policy_capture.py generate().
func generateDeep(kind string, depth int) string {
	array := func(n int) string { return strings.Repeat("[", n) + strings.Repeat("]", n) }
	object := func(n int) string { return strings.Repeat(`{"a":`, n) + "1" + strings.Repeat("}", n) }
	switch kind {
	case "array":
		return array(depth)
	case "object":
		return object(depth)
	case "declaration-array":
		return `{"path": "<R>/good.json", "x": ` + array(depth-1) + "}"
	case "policy-array":
		return `{"roles": {}, "x": ` + array(depth-1) + "}"
	case "policy-object":
		return `{"roles": {}, "x": ` + object(depth-1) + "}"
	case "policy-object-in-arrays":
		return `{"roles": {}, "x": ` + strings.Repeat("[", depth-2) + "{}" + strings.Repeat("]", depth-2) + "}"
	case "policy-duplicate-then-deep":
		return `{"x": {"a": 1, "a": 2}, "y": ` + array(depth) + "}"
	}
	panic("unknown generator " + kind)
}

// buildLaunchCase is launch_policy_capture.py build(): the case's tree under root, returning S.
func buildLaunchCase(t *testing.T, root string, golden launchGolden, c launchCase) string {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	content := func(spec launchContent) []byte {
		switch {
		case spec.Generate != "":
			return []byte(strings.ReplaceAll(generateDeep(spec.Generate, spec.Depth), "<R>", root))
		case spec.Hex != nil:
			raw, err := hex.DecodeString(*spec.Hex)
			must(err)
			return raw
		case spec.Text != nil:
			return []byte(strings.ReplaceAll(*spec.Text, "<R>", root))
		}
		t.Fatal("empty content")
		return nil
	}
	must(os.MkdirAll(filepath.Join(root, "home"), 0o700))
	must(os.WriteFile(filepath.Join(root, "good.json"), []byte(golden.Policy), 0o600))
	must(os.WriteFile(filepath.Join(root, "other.json"), []byte(golden.Other), 0o600))
	must(os.WriteFile(filepath.Join(root, "home", "p.json"), []byte(golden.Policy), 0o600))
	must(os.Symlink(filepath.Join(root, "good.json"), filepath.Join(root, "alias.json")))
	state := filepath.Join(root, "S")
	must(os.Mkdir(state, 0o700))
	for name, spec := range c.Files {
		must(os.WriteFile(filepath.Join(root, name), content(spec), 0o600))
	}
	target := filepath.Join(state, "launch-policy.json")
	if c.Launch == nil {
		return state
	}
	switch c.Launch.Special {
	case "directory":
		must(os.Mkdir(target, 0o700))
	case "fifo":
		must(syscall.Mkfifo(target, 0o600))
	case "dangling":
		must(os.Symlink(filepath.Join(state, "nothing"), target))
	case "loop":
		must(os.Symlink(target, target))
	default:
		must(os.WriteFile(target, content(*c.Launch), 0o600))
		if c.Launch.Mode != nil {
			must(os.Chmod(target, os.FileMode(*c.Launch.Mode)))
			t.Cleanup(func() { _ = os.Chmod(target, 0o600) })
		}
	}
	return state
}

func TestLaunchPolicy_resolution_is_pythons_for_every_declaration(t *testing.T) {
	golden := loadLaunchGolden(t)
	if len(golden.Cases) < 60 {
		t.Fatalf("the oracle holds %d cases", len(golden.Cases))
	}
	for name, c := range golden.Cases {
		t.Run(name, func(t *testing.T) {
			if c.Launch != nil && c.Launch.Mode != nil && os.Geteuid() == 0 {
				t.Skip("root reads a mode-0 file")
			}
			root := t.TempDir()
			state := buildLaunchCase(t, root, golden, c)
			t.Chdir(root)
			t.Setenv("HOME", filepath.Join(root, "home"))
			environment := ""
			if c.Environment != nil {
				environment = strings.ReplaceAll(*c.Environment, "<R>", root)
			}
			got := strings.ReplaceAll(emitted(t, service.ResolveLaunchPolicyAt(state, environment)), root, "<R>")
			if got != c.Resolution {
				t.Fatalf("Go\n%s\nPython\n%s", got, c.Resolution)
			}
		})
	}
}

func emitted(t *testing.T, value any) string {
	t.Helper()
	var out bytes.Buffer
	if err := contract.Emit(&out, value); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(out.String(), "\n")
}

// The digest a launch reports for a policy file is the one the bridge's parser computes for it
// (execution.FromFile), the one the relay's role policy carries, and the one Python's
// ExecutionPolicy.from_file gave for the same bytes (the golden "record" case): SHA-256 of
// exactly the bytes read, never of a re-encoding. A synthetic file, never the host's.
func TestLaunchPolicy_digest_is_the_parsers_and_pythons(t *testing.T) {
	golden := loadLaunchGolden(t)
	var python struct{ Digest string }
	if err := json.Unmarshal([]byte(golden.Cases["record"].Resolution), &python); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	state := buildLaunchCase(t, root, golden, golden.Cases["record"])
	path := filepath.Join(root, "good.json")
	parsed, err := execution.FromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(golden.Policy))
	var launch struct{ Digest string }
	if err = json.Unmarshal([]byte(emitted(t, service.ResolveLaunchPolicyAt(state, ""))), &launch); err != nil {
		t.Fatal(err)
	}
	for name, digest := range map[string]any{
		"execution.FromFile": parsed.Summary()["digest"],
		"role policy":        registry.ResolveRolePolicy(map[string]string{execution.EnvPolicy: path}).Digest(),
		"launch resolution":  launch.Digest,
		"sha256 of the file": hex.EncodeToString(sum[:]),
	} {
		if digest != python.Digest {
			t.Errorf("%s digest %v, Python %s", name, digest, python.Digest)
		}
	}
}
