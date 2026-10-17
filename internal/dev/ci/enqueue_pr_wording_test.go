//go:build dev

package ci

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func enqueuePRMissingFlags(line string) []string {
	var problems []string
	for _, command := range strings.Split(line, "`") {
		if !strings.Contains(command, "--enqueue-pr") {
			continue
		}
		fields := strings.Fields(command)
		for _, flag := range []string{"--head", "--release-impact"} {
			found := false
			for i, field := range fields {
				if field == flag && i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "--") {
					found = true
				}
				if strings.HasPrefix(field, flag+"=") && len(field) > len(flag)+1 {
					found = true
				}
			}
			if !found {
				problems = append(problems, fmt.Sprintf("%s lacks %s and its value", strings.TrimSpace(command), flag))
			}
		}
	}
	return problems
}

func TestEnqueuePRSkillCommandsRequirePinnedHeadAndReleaseImpact(t *testing.T) {
	root := filepath.Join(repoRoot(), skillsDir)
	commands := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(repoRoot(), path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(data), "\n") {
			commands += strings.Count(line, "--enqueue-pr")
			for _, problem := range enqueuePRMissingFlags(line) {
				t.Errorf("%s:%d: %s", filepath.ToSlash(rel), i+1, problem)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if commands == 0 {
		t.Fatal("no enqueue-pr commands were checked")
	}
}

func TestEnqueuePRWordingGuardChecksEachCommand(t *testing.T) {
	for _, tc := range []struct {
		name, line string
		want       int
	}{
		{"complete", "`crw manage integrate --enqueue-pr o/r#1 --head SHA --release-impact TEXT`", 0},
		{"old form", "`crw manage integrate --enqueue-pr o/r#1`", 2},
		{"other command has flags", "`crw flow pr decide --pr o/r#1 --head SHA --release-impact TEXT`; `crw manage integrate --enqueue-pr o/r#1`", 2},
		{"mixed commands", "`crw manage integrate --enqueue-pr o/r#1 --head SHA --release-impact TEXT`; `crw manage integrate --enqueue-pr o/r#2`", 2},
		{"missing impact", "`crw manage integrate --enqueue-pr o/r#1 --head SHA`", 1},
		{"head without value", "`crw manage integrate --enqueue-pr o/r#1 --head --release-impact TEXT`", 1},
		{"impact without value", "`crw manage integrate --enqueue-pr o/r#1 --head SHA --release-impact`", 1},
		{"equals", "`crw manage integrate --enqueue-pr=o/r#1 --head=SHA --release-impact=TEXT`", 0},
		{"plain code line", "crw manage integrate --enqueue-pr o/r#1", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := enqueuePRMissingFlags(tc.line); len(got) != tc.want {
				t.Fatalf("problems = %v; want %d", got, tc.want)
			}
		})
	}
}
