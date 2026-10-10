package manage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func batchDigest(items string) string {
	return `{"schema":"crw-batch-digest/1","label":"night","selected":["CRW-1","CRW-2","CRW-3","CRW-4","CRW-5"],
	 "started_at":"2026-10-11T23:00:00+09:00","finished_at":"2026-10-12T06:40:00+09:00","deadline":"2026-10-12T07:00:00+09:00",
	 "stopped":"finished","providers":[{"name":"codex","observed":true,"detail":"4 of 4 slots"},{"name":"claude","observed":false}],
	 "items":[` + items + `]}`
}

const batchAllItems = `
 {"issue":"CRW-1","title":"one","outcome":"integrated","evidence":"merged 1234abcd into dev"},
 {"issue":"CRW-2","title":"two","outcome":"cancelled","reason":"the premise no longer holds"},
 {"issue":"CRW-3","title":"three","outcome":"carried","reason":"deadline reached before review"},
 {"issue":"CRW-4","title":"four","outcome":"needs_decision","reason":"two designs fit; which one"},
 {"issue":"CRW-5","title":"five","outcome":"integrated"}`

func TestBatchReportWritesTheCrwStatusFormat(t *testing.T) {
	state := auditListHome(t)
	e, _, _ := auditTestEnv(t)
	file := filepath.Join(t.TempDir(), "digest.json")
	if err := os.WriteFile(file, []byte(batchDigest(batchAllItems)), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := batchRun(t, e, "", "report", "--digest", file)
	if code != 0 {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	want := filepath.Join(state, "batch", "reports", "2026-10-12-night.md")
	if strings.TrimSpace(out) != want {
		t.Fatalf("printed %q, want the report path %q", out, want)
	}
	raw, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	report := string(raw)
	// The headings are the crw-status night batch reference's, in its order.
	var at int
	for _, h := range batchReportHeadings {
		i := strings.Index(report[at:], "\n"+h+"\n")
		if i < 0 {
			t.Fatalf("heading %q missing or out of order in:\n%s", h, report)
		}
		at += i + len(h)
	}
	section := func(heading string) string {
		start := strings.Index(report, "\n"+heading+"\n") + len(heading) + 2
		end := strings.Index(report[start:], "\n## ")
		if end < 0 {
			return report[start:]
		}
		return report[start : start+end]
	}
	if s := section(batchHeadingIntegrated); !strings.Contains(s, "CRW-1") || !strings.Contains(s, "merged 1234abcd into dev") || strings.Contains(s, "CRW-5") {
		t.Errorf("integrated section: %q", s)
	}
	if s := section(batchHeadingCarried); !strings.Contains(s, "CRW-2") || !strings.Contains(s, "the premise no longer holds") || !strings.Contains(s, "CRW-3") || !strings.Contains(s, "deadline reached before review") {
		t.Errorf("cancelled and carried section: %q", s)
	}
	if s := section(batchHeadingDecision); !strings.Contains(s, "CRW-4") || !strings.Contains(s, "which one") {
		t.Errorf("decision section: %q", s)
	}
	unverified := section(batchHeadingEvidence)
	if !strings.Contains(unverified, "CRW-5") || !strings.Contains(unverified, "증거") {
		t.Errorf("an integrated item with no evidence is unverified, not integrated: %q", unverified)
	}
	if !strings.Contains(unverified, "claude") || !strings.Contains(unverified, "관측 안 됨") || strings.Contains(unverified, "claude: 관측됨") {
		t.Errorf("a provider that was not observed is shown as unobserved: %q", unverified)
	}
	if !strings.Contains(report, "통합 1건") || !strings.Contains(report, "2026-10-12T07:00:00+09:00") {
		t.Errorf("the conclusion counts and names the deadline:\n%s", report)
	}
	if info, err := os.Stat(want); err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Errorf("report mode %v %v", info, err)
	}
	// Writing again replaces the file and leaves no temporary file behind.
	if code, _, _ := batchRun(t, e, "", "report", "--digest", file); code != 0 {
		t.Fatalf("second write exit %d", code)
	}
	entries, _ := os.ReadDir(filepath.Dir(want))
	if len(entries) != 1 {
		t.Errorf("reports directory holds %d entries, want 1", len(entries))
	}
}

func TestBatchReportDoesNotTrustWhatTheDigestDoesNotShow(t *testing.T) {
	auditListHome(t)
	e, _, _ := auditTestEnv(t)
	digest := batchDigest(`
	 {"issue":"CRW-1","title":"one","outcome":"integrated","evidence":"merged"},
	 {"issue":"CRW-1","title":"one again","outcome":"cancelled","reason":"x"},
	 {"issue":"CRW-2","title":"two","outcome":"carried"},
	 {"issue":"CRW-3","title":"three","outcome":"released"},
	 {"issue":"CRW-9","title":"outside","outcome":"integrated","evidence":"merged"}`)
	file := filepath.Join(t.TempDir(), "digest.json")
	if err := os.WriteFile(file, []byte(digest), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := batchRun(t, e, "", "report", "--digest", file); code != 0 {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	report := batchReadLatestReport(t)
	sec := func(h string) string {
		start := strings.Index(report, "\n"+h+"\n") + len(h) + 2
		end := strings.Index(report[start:], "\n## ")
		if end < 0 {
			return report[start:]
		}
		return report[start : start+end]
	}
	if s := sec(batchHeadingIntegrated); strings.Contains(s, "CRW-9") || strings.Count(s, "CRW-1") != 1 {
		t.Errorf("an issue outside the selected list is not reported as integrated, a repeat is not counted: %q", s)
	}
	if s := sec(batchHeadingDecision); !strings.Contains(s, "CRW-9") || !strings.Contains(s, "선택 목록 밖") {
		t.Errorf("an item outside the selected list needs Jun's attention: %q", s)
	}
	u := sec(batchHeadingEvidence)
	for _, need := range []string{"CRW-1", "중복", "CRW-2", "이유 기록 없음", "CRW-3", "released", "CRW-4", "CRW-5", "실행 기록 없음"} {
		if !strings.Contains(u, need) {
			t.Errorf("unverified section lacks %q: %q", need, u)
		}
	}
}

func TestBatchReportRefusesAnUnusableDigest(t *testing.T) {
	state := auditListHome(t)
	e, _, _ := auditTestEnv(t)
	for name, body := range map[string]string{
		"not json":     "{",
		"wrong schema": `{"schema":"other/1","label":"night","items":[]}`,
		"no label":     `{"schema":"crw-batch-digest/1","items":[]}`,
		"bad time":     `{"schema":"crw-batch-digest/1","label":"night","finished_at":"yesterday","items":[]}`,
		"item no key":  `{"schema":"crw-batch-digest/1","label":"night","items":[{"title":"t","outcome":"integrated"}]}`,
	} {
		file := filepath.Join(t.TempDir(), "d.json")
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if code, _, _ := batchRun(t, e, "", "report", "--digest", file); code != usageExit {
			t.Errorf("%s: exit %d, want %d", name, code, usageExit)
		}
	}
	if code, _, _ := batchRun(t, e, "", "report"); code != usageExit {
		t.Errorf("report without --digest exits %d", code)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Errorf("a refused digest left a state directory (%v)", err)
	}
}

func TestBatchReportFileNameCannotEscapeTheReportsDirectory(t *testing.T) {
	state := auditListHome(t)
	e, _, _ := auditTestEnv(t)
	body := `{"schema":"crw-batch-digest/1","label":"../../etc/x y","finished_at":"2026-10-12T06:40:00+09:00","items":[]}`
	file := filepath.Join(t.TempDir(), "d.json")
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := batchRun(t, e, "", "report", "--digest", file)
	if code != 0 {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	path := strings.TrimSpace(out)
	if filepath.Dir(path) != filepath.Join(state, "batch", "reports") {
		t.Errorf("report written to %q", path)
	}
}

// batchReadLatestReport reads the one report file under the test's state directory.
func batchReadLatestReport(t *testing.T) string {
	t.Helper()
	home := os.Getenv("XDG_STATE_HOME")
	matches, _ := filepath.Glob(filepath.Join(home, "crw", "manage", "batch", "reports", "*.md"))
	if len(matches) != 1 {
		t.Fatalf("reports: %v", matches)
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The headings the command writes are the ones the crw-status night batch reference names.
func TestBatchReportHeadingsAreTheReferenceHeadings(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "plugins", "crw", "skills", "crw-status", "references", "night-batch-report.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range append([]string{"`# 밤 배치 아침 보고: <label>`"}, batchReportHeadings...) {
		want := h
		if strings.HasPrefix(h, "## ") {
			want = "`" + h + "`"
		}
		if !strings.Contains(string(raw), want) {
			t.Errorf("the reference does not name the heading %s", want)
		}
	}
}
