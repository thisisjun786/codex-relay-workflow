//go:build dev

package ci

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-966: the release verify job runs the repository's local full verification with flags that exist.
// The invocation is read from release.yml, and each flag it passes must be one the local command's usage
// names, so a renamed flag fails here rather than in a release run.
func TestReleaseVerifyInvocationUsesRegisteredLocalFlags(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot(), ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	start := strings.Index(text, "go run -tags dev ./cmd/crw-dev ci local")
	if start < 0 {
		t.Fatal("release.yml does not run crw-dev ci local")
	}
	// The command is its first line and every following line that the previous one continues with a backslash.
	var lines []string
	for _, line := range strings.Split(text[start:], "\n") {
		lines = append(lines, line)
		if !strings.HasSuffix(strings.TrimSpace(line), "\\") {
			break
		}
	}
	invocation := strings.Join(lines, "\n")
	var usage bytes.Buffer
	if code := Local([]string{"-h"}, &usage, &usage); code != 0 {
		t.Fatalf("ci local -h exited %d", code)
	}
	for _, line := range strings.Split(invocation, "\n") {
		for _, field := range strings.Fields(line) {
			if !strings.HasPrefix(field, "--") {
				continue
			}
			name := strings.TrimPrefix(field, "--")
			if !strings.Contains(usage.String(), "  -"+name+" ") {
				t.Errorf("release.yml passes --%s, which crw-dev ci local does not register", name)
			}
		}
	}
}
