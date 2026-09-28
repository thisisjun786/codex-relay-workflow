package skill

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestSkillEmbeddedContractsEqualCheckout(t *testing.T) {
	// Given the canonical checkout contracts and the data shipped in this build.
	for _, name := range []string{"start-policy.md", "hook-contract.md"} {
		t.Run(name, func(t *testing.T) {
			path := defaultContract(name)
			disk, err := os.ReadFile(filepath.Join(repositoryRoot(), "plugins", path))
			if err != nil {
				t.Fatal(err)
			}
			// When the installed binary's embedded data is read without a checkout lookup.
			bundled, err := fs.ReadFile(bundledSkillFiles, path)
			if err != nil {
				t.Fatal(err)
			}
			// Then stale built assets cannot pass this checkout's verification.
			if !bytes.Equal(bundled, disk) {
				t.Fatalf("embedded %s differs from the canonical checkout", name)
			}
		})
	}
}
