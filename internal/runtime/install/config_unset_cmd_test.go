package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1149: the CLI refuses an unset of a key the user changed since crw set it, names the manual next step, and the
// explicit --release drops crw's record without touching config.toml.
func TestConfigUnsetRefusalAndReleaseThroughTheCLI(t *testing.T) {
	t.Parallel()
	h := configCommandHome(t, "[memories]\ngenerate_memories = true\n", true)
	if code, out, err := runConfigCommand(t, h, "set", "memories.dedicated_tools", "true"); code != 0 {
		t.Fatalf("set: %d %q %q", code, out, err)
	}
	edited := strings.Replace(h.read("config.toml"), "dedicated_tools = true", "dedicated_tools = false", 1)
	if err := os.WriteFile(filepath.Join(h.home, "config.toml"), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runConfigCommand(t, h, "unset", "memories.dedicated_tools")
	if code != 1 || out != "" || !strings.Contains(errOut, "(changed)") || !strings.Contains(errOut, "--release") || h.read("config.toml") != edited {
		t.Fatalf("unset: %d %q %q", code, out, errOut)
	}
	code, out, errOut = runConfigCommand(t, h, "unset", "memories.dedicated_tools", "--release")
	if code != 0 || out != "memories.dedicated_tools: crw's record released; config.toml left as it is\n" || h.read("config.toml") != edited {
		t.Fatalf("release: %d %q %q", code, out, errOut)
	}
	if code, _, errOut = runConfigCommand(t, h, "unset", "memories.dedicated_tools"); code != 1 || !strings.Contains(errOut, "is not recorded") {
		t.Fatalf("after release: %d %q", code, errOut)
	}
}
