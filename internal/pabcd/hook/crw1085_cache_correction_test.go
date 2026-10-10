package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCRW1085ImportCacheWithoutSelectedSources(t *testing.T) {
	for _, cmd := range []string{"PYTHONPYCACHEPREFIX=MEMORY python3 -m unittest", "PYTHONPYCACHEPREFIX=MEMORY python3 -m json.tool"} {
		cwd, root, env := gateScene(t)
		if HandleMemoryWriteGate(gateBash(t, cwd, strings.ReplaceAll(cmd, "MEMORY", root)), env) == "" {
			t.Errorf("implicit module cache write allowed: %s", cmd)
		}
	}
}

func TestCRW1085ImportCacheIgnoredEnvironment(t *testing.T) {
	cwd, root, env := gateScene(t)
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "test_safe.py"), []byte("import unittest"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := "cd " + root + "; PYTHONDONTWRITEBYTECODE=1 python3 -E -m unittest test_safe.py"
	if HandleMemoryWriteGate(gateBash(t, cwd, cmd), env) == "" {
		t.Fatal("-E restored a hidden import bytecode write")
	}
}
