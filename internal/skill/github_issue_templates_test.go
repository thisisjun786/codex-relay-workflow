package skill

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// CRW-1607: the draft GitHub label catalog and issue templates are a document contract — the
// files themselves, the canonical tracking model and the release gate's real reader all agree.
// Nothing here writes to GitHub; the catalog is the agreed draft, not an applied inventory.

var crw1607LabelColor = regexp.MustCompile(`^[0-9a-fA-F]{6}$`)
var crw1607PriorityLabel = regexp.MustCompile(`^P[0-9]+$`)
var crw1607CanonPriority = regexp.MustCompile("`P[0-9]+`")

// crw1607Read reads a repository file; a missing artifact is a failure, not a skip.
func crw1607Read(t *testing.T, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repositoryRoot(), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return data
}

// crw1607DecodeLabels strictly decodes the catalog: unknown fields and trailing JSON are errors.
func crw1607DecodeLabels(t *testing.T, data []byte) []struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
} {
	t.Helper()
	var labels []struct {
		Name        string `json:"name"`
		Color       string `json:"color"`
		Description string `json:"description"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&labels); err != nil {
		t.Fatalf("labels.json does not decode: %v", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		t.Fatalf("labels.json has trailing content: %v", err)
	}
	return labels
}

// crw1607CanonSection returns the '## Tracking operating model' section, up to the next
// level-2 heading (subheadings stay inside).
func crw1607CanonSection(t *testing.T, doc []byte) string {
	t.Helper()
	_, rest, found := strings.Cut(string(doc), "## Tracking operating model")
	if !found {
		t.Fatal("tracking-model.md has no '## Tracking operating model' section")
	}
	section, _, _ := strings.Cut(rest, "\n## ")
	return section
}

func TestCRW1607LabelCatalog(t *testing.T) {
	labels := crw1607DecodeLabels(t, crw1607Read(t, ".github/labels.json"))
	if len(labels) == 0 {
		t.Fatal("labels.json is an empty catalog")
	}
	seen := map[string]string{}
	var priorities []string
	byName := map[string]bool{}
	var areas []string
	for _, label := range labels {
		name := label.Name
		if name == "" || name != strings.TrimSpace(name) || len([]rune(name)) > 50 {
			t.Errorf("label name %q is empty, padded or over 50 runes", name)
		}
		if strings.TrimSpace(label.Description) == "" || len([]rune(label.Description)) > 100 {
			t.Errorf("label %q description is empty or over 100 runes", name)
		}
		if !crw1607LabelColor.MatchString(label.Color) {
			t.Errorf("label %q color %q is not six hex digits", name, label.Color)
		}
		folded := strings.ToLower(name)
		if first, dup := seen[folded]; dup {
			t.Errorf("labels %q and %q differ only by case", first, name)
		}
		seen[folded] = name
		byName[name] = true
		if crw1607PriorityLabel.MatchString(name) {
			priorities = append(priorities, name)
		}
		if suffix, ok := strings.CutPrefix(name, "area:"); ok {
			areas = append(areas, suffix)
		}
	}
	sort.Strings(priorities)

	section := crw1607CanonSection(t, crw1607Read(t,
		"plugins/crw/skills/crw-canon/references/tracking-model.md"))
	// The canonical priorities are the backtick names the tracking model itself declares.
	canonSet := map[string]bool{}
	for _, match := range crw1607CanonPriority.FindAllString(section, -1) {
		canonSet[strings.Trim(match, "`")] = true
	}
	if len(canonSet) == 0 {
		t.Fatal("the tracking model section names no priorities")
	}
	var canon []string
	for name := range canonSet {
		canon = append(canon, name)
	}
	sort.Strings(canon)
	if strings.Join(priorities, ",") != strings.Join(canon, ",") {
		t.Errorf("priority labels %v do not equal the canonical %v", priorities, canon)
	}

	// The canonical kind and marker labels exist in the catalog and in the canon section.
	for _, want := range []string{
		"defect", "feature", "test", "docs",
		"blocks-real-use", "real-use-caveat", "found-in-trial",
		"설계 먼저", "from-linear",
	} {
		if !byName[want] {
			t.Errorf("the canonical label %q is missing from labels.json", want)
		}
		if !strings.Contains(section, "`"+want+"`") {
			t.Errorf("the canonical label %q is not a backtick name in the tracking model", want)
		}
	}

	// Every area label names a real tree directory.
	if len(areas) == 0 {
		t.Fatal("labels.json names no area labels")
	}
	dirs := map[string]string{"dev-ci": "internal/dev/ci", "install": "internal/runtime/install",
		"skills": "plugins/crw/skills"}
	for _, suffix := range areas {
		dir := dirs[suffix]
		if dir == "" {
			dir = "internal/" + suffix
		}
		info, err := os.Stat(filepath.Join(repositoryRoot(), filepath.FromSlash(dir)))
		if err != nil || !info.IsDir() {
			t.Errorf("area:%s names no existing directory %s", suffix, dir)
		}
	}
}

var crw1607WorkHeadings = []string{
	"## 배경", "## 기준", "## 편집 영역", "## 정한 답", "## 먼저 실패하게 쓸 시험", "## 끝 조건",
}

func TestCRW1607WorkTemplateReleaseGate(t *testing.T) {
	raw := crw1607Read(t, ".github/ISSUE_TEMPLATE/work.md")
	if !strings.HasPrefix(string(raw), "---\n") {
		t.Fatal("work.md does not open with YAML frontmatter")
	}
	_, body, found := strings.Cut(string(raw)[len("---\n"):], "\n---\n")
	if !found {
		t.Fatal("work.md frontmatter never closes")
	}
	skill := string(crw1607Read(t, "plugins/crw/skills/crw-add-issue/SKILL.md"))
	for _, heading := range crw1607WorkHeadings {
		if !strings.Contains(skill, "`"+heading+"`") {
			t.Errorf("crw-add-issue does not name the heading `%s`", heading)
		}
	}
	// Each required heading is a standalone body line, exactly once — the projection drops
	// comments, and a line that merely contains the heading's text is not a standalone heading.
	visible := regexp.MustCompile(`(?s)<!--.*?-->`).ReplaceAllString(body, "")
	lines := strings.Split(visible, "\n")
	for _, heading := range crw1607WorkHeadings {
		count := 0
		for _, line := range lines {
			if line == heading {
				count++
			}
		}
		if count != 1 {
			t.Errorf("body has %d standalone %q lines, want exactly 1", count, heading)
		}
	}

	// The unchanged template body is not ready: the real release gate reports all five missing
	// sections, in no assumed order.
	blank, err := CheckIssueReady(ReadyIssue{
		ID: "SYN-1607", Title: "작업 양식 계약", Description: body,
	})
	if err != nil {
		t.Fatal(err)
	}
	if blank.Ready || blank.Decision != "not_ready" {
		t.Fatalf("blank template: ready=%t decision=%q, want not_ready", blank.Ready, blank.Decision)
	}
	missing := append([]string(nil), blank.Missing...)
	sort.Strings(missing)
	wantMissing := []string{"criteria", "decided_answer", "done_condition", "edit_region", "red_test"}
	if strings.Join(missing, ",") != strings.Join(wantMissing, ",") {
		t.Fatalf("blank template missing = %v, want %v", missing, wantMissing)
	}

	// Filling the actual template sections makes the gate's reader call it ready.
	filled := body
	for _, entry := range []struct{ heading, entry string }{
		{"## 배경", "작업 양식의 준비 항목을 검사한다."},
		{"## 기준", "* 다섯 준비 항목을 해제 검사기가 읽는다."},
		{"## 편집 영역", "* `.github/ISSUE_TEMPLATE/work.md`"},
		{"## 정한 답", "작업 양식은 준비 항목의 제목을 그대로 유지한다."},
		{"## 먼저 실패하게 쓸 시험", "* 작업 양식이 없으면 계약 시험이 실패한다."},
		{"## 끝 조건", "* `go test -count=1 ./internal/skill -run '^TestCRW1607WorkTemplateReleaseGate$'`가 통과한다."},
	} {
		filled = strings.Replace(filled, entry.heading+"\n", entry.heading+"\n\n"+entry.entry+"\n", 1)
	}
	filledReady, err := CheckIssueReady(ReadyIssue{
		ID: "SYN-1607", Title: "작업 양식 계약", Description: filled,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !filledReady.Ready || filledReady.Decision != "ready" {
		t.Fatalf("filled template: ready=%t decision=%q reasons=%v",
			filledReady.Ready, filledReady.Decision, filledReady.Reasons)
	}
	if len(filledReady.Missing) != 0 || len(filledReady.Warnings) != 0 {
		t.Errorf("filled template missing=%v warnings=%v, want none",
			filledReady.Missing, filledReady.Warnings)
	}
	want := "go test -count=1 ./internal/skill -run '^TestCRW1607WorkTemplateReleaseGate$'"
	if len(filledReady.EndCondition) != 1 || filledReady.EndCondition[0] != want {
		t.Errorf("end condition = %v, want [%q]", filledReady.EndCondition, want)
	}
}
