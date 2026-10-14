package metric

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// linkRefuseListing is every entry below dir with its content, so a test can show that nothing outside the workspace changed.
func linkRefuseListing(t *testing.T, dir string) string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && path != dir {
			data, _ := os.ReadFile(path)
			out = append(out, strings.TrimPrefix(path, dir)+"="+string(data))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, "\n")
}

// The metric writers refuse a symbolic link at .crw, at their directory or at their file, naming it, and write nothing to what it
// leads to outside the workspace.
func TestMetricWritersRefuseALinkInTheStateTree(t *testing.T) {
	writers := map[string]func(cwd string) error{
		"RecordObjectiveMetric": func(cwd string) error {
			_, err := RecordObjectiveMetric(cwd, RecordInput{SessionID: "s1", MetricName: "m", Value: 1, Source: OperatorEntered})
			return err
		},
		"WriteObjectiveKind": func(cwd string) error { return WriteObjectiveKind(cwd, "s1", Maximize) },
		"WriteDivergenceMode": func(cwd string) error {
			_, err := WriteDivergenceMode(cwd, ModeInput{SessionID: "s1", Active: true})
			return err
		},
		"RecordDivergenceCandidate": func(cwd string) error {
			_, err := RecordDivergenceCandidate(cwd, CandidateInput{SessionID: "s1", Title: "t", SourceURLs: []string{"https://example.invalid/a"}})
			return err
		},
	}
	cases := []struct {
		writer, link string
		file         bool
	}{
		{"RecordObjectiveMetric", ".crw", false},
		{"RecordObjectiveMetric", ".crw/" + MetricsFile, true},
		{"WriteObjectiveKind", ".crw", false},
		{"WriteObjectiveKind", ".crw/" + ObjectiveKindDir, false},
		{"WriteObjectiveKind", ".crw/" + ObjectiveKindDir + "/s1.json", true},
		{"WriteDivergenceMode", ".crw/" + DivergenceDir, false},
		{"WriteDivergenceMode", ".crw/" + DivergenceDir + "/s1.mode.json", true},
		{"RecordDivergenceCandidate", ".crw/" + DivergenceDir, false},
		{"RecordDivergenceCandidate", ".crw/" + DivergenceDir + "/" + CandidatesFile, true},
	}
	for _, c := range cases {
		t.Run(c.writer+" "+c.link, func(t *testing.T) {
			cwd, outside := t.TempDir(), t.TempDir()
			target := outside
			if c.file {
				target = filepath.Join(outside, "victim")
				metricsMust(t, os.WriteFile(target, []byte("{\"sessionId\":\"s1\"}"), 0o644))
			}
			link := filepath.Join(cwd, c.link)
			metricsMust(t, os.MkdirAll(filepath.Dir(link), 0o777))
			metricsMust(t, os.Symlink(target, link))
			before := linkRefuseListing(t, outside)
			err := writers[c.writer](cwd)
			if err == nil || !strings.Contains(err.Error(), link) || !strings.Contains(err.Error(), "symbolic link") {
				t.Fatalf("err = %v, want a refusal naming %s as a symbolic link", err, link)
			}
			if after := linkRefuseListing(t, outside); after != before {
				t.Fatalf("outside the workspace changed:\n%s\nwas\n%s", after, before)
			}
		})
	}
}
