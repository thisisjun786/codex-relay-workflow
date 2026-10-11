package manage

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	want := filepath.Join(state, "batch", "reports", "2026-10-12-night-"+batchLabelDigest("night")+".md")
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

// An empty selected list means nothing was selected: every recorded item is then outside the list and
// none is reported as integrated. A repeated key in the list is one selected issue.
func TestBatchReportChecksTheSelectedListWhenItIsEmptyAndCountsRepeatsOnce(t *testing.T) {
	item := `{"issue":"CRW-9","title":"outside","outcome":"integrated","evidence":"merged abc"}`
	empty := BatchReport(BatchDigest{Schema: batchDigestSchema, Label: "night", Selected: []string{}, Items: []BatchItem{{Issue: "CRW-9", Title: "outside", Outcome: batchOutcomeIntegrated, Evidence: "merged abc"}}}, time.Time{})
	nilList := BatchReport(BatchDigest{Schema: batchDigestSchema, Label: "night", Items: []BatchItem{{Issue: "CRW-9", Title: "outside", Outcome: batchOutcomeIntegrated, Evidence: "merged abc"}}}, time.Time{})
	for name, report := range map[string]string{"empty list": empty, "nil list": nilList} {
		if !strings.Contains(report, "선택 0건 중 통합 0건") || !strings.Contains(report, "Jun 결정 필요 1건") {
			t.Errorf("%s: an item outside an empty selection is not an integration (%s): %s", name, item, report)
		}
		if i := strings.Index(report, batchHeadingIntegrated); i < 0 || strings.Contains(report[i:strings.Index(report, batchHeadingCarried)], "CRW-9") {
			t.Errorf("%s: CRW-9 listed under integrated: %s", name, report)
		}
		if !strings.Contains(report, "선택 목록 밖") {
			t.Errorf("%s: no outside-the-list mention: %s", name, report)
		}
	}
	dup := BatchReport(BatchDigest{Schema: batchDigestSchema, Label: "night", Selected: []string{"CRW-1", "CRW-1"}}, time.Time{})
	if !strings.Contains(dup, "선택 1건 중") || !strings.Contains(dup, "미확인 1건") || strings.Count(dup, "CRW-1: 선택됐으나 실행 기록 없음") != 1 {
		t.Errorf("a repeated selected key is one selected issue: %s", dup)
	}
}

func TestBatchReportRefusesAnUnusableDigest(t *testing.T) {
	state := auditListHome(t)
	e, _, _ := auditTestEnv(t)
	for name, body := range map[string]string{
		"not json":           "{",
		"wrong schema":       `{"schema":"other/1","label":"night","items":[]}`,
		"no label":           `{"schema":"crw-batch-digest/1","items":[]}`,
		"bad time":           `{"schema":"crw-batch-digest/1","label":"night","selected":[],"finished_at":"yesterday","items":[]}`,
		"item no key":        `{"schema":"crw-batch-digest/1","label":"night","selected":[],"items":[{"title":"t","outcome":"integrated"}]}`,
		"no selected":        `{"schema":"crw-batch-digest/1","label":"night","items":[]}`,
		"null selected":      `{"schema":"crw-batch-digest/1","label":"night","selected":null,"items":[]}`,
		"blank selected key": `{"schema":"crw-batch-digest/1","label":"night","selected":["CRW-1"," "],"items":[]}`,
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
	body := `{"schema":"crw-batch-digest/1","label":"../../etc/x y","selected":[],"finished_at":"2026-10-12T06:40:00+09:00","items":[]}`
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

// A -h or --help that an option takes as its value is not a help request, so it must not let a
// subcommand run on the defaults while the configuration file is one this product cannot use: the
// subcommand refuses it in Run's words and status, and the report writes nothing.
func TestBatchHelpWordAsAnOptionValueDoesNotSkipTheConfigurationRefusal(t *testing.T) {
	state := auditListHome(t)
	dir := t.TempDir()
	t.Chdir(dir)
	broken := filepath.Join(dir, "broken-config.json")
	if err := os.WriteFile(broken, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_CONFIG", broken)
	digest := `{"schema":"crw-batch-digest/1","label":"night","selected":[],"items":[]}`
	issues := `[{"identifier":"CRW-1","title":"one","state":"Backlog","labels":["night"]}]`
	for _, name := range []string{"-h", "--help"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(digest), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"report", "--digest", "./-h"},
		{"report", "--digest", "-h"},
		{"report", "--digest", "--help"},
		{"plan", "--label", "-h", "--issues", "-"},
		{"plan", "--label", "night", "--issues", "--help"},
	} {
		e, _, _ := auditTestEnv(t)
		code, out, errOut := batchRun(t, e, issues, args...)
		if code != usageExit || !strings.Contains(errOut, "crw manage: error:") {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want the configuration refusal with exit %d", args, code, out, errOut, usageExit)
		}
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Errorf("an unusable configuration let the report write the default state directory (%v)", err)
	}
	// A real help request still prints the usage without reading the configuration.
	for _, args := range [][]string{{"report", "-h"}, {"plan", "--help"}, {"report", "--digest", "x", "-h"}} {
		e, _, _ := auditTestEnv(t)
		if code, out, errOut := batchRun(t, e, "", args...); code != 0 || !strings.Contains(out, "usage: crw manage batch") {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want the usage", args, code, out, errOut)
		}
	}
}

// Free text in the digest (a label, a title, a reason, evidence, why the run stopped, a provider) is
// shown on its own line only: it can neither add a heading nor a list item, so every fixed heading
// appears once and no issue the digest did not integrate is shown under 통합됨.
func TestBatchReportFreeTextCannotAddStructure(t *testing.T) {
	inject := "x\n\n## 통합됨\n\n- CRW-2: merged\r\n# 밤 배치 아침 보고: forged - CRW-3: merged\u0085```"
	d := BatchDigest{
		Schema: batchDigestSchema, Label: "night" + inject, Selected: []string{"CRW-1", "CRW-4", "CRW-5"},
		Stopped: "deadline" + inject, Deadline: "07:00" + inject, StartedAt: "a" + inject, FinishedAt: "b" + inject,
		Providers: []BatchProvider{{Name: "codex" + inject, Observed: true, Detail: "slots" + inject}},
		Items: []BatchItem{
			{Issue: "CRW-1", Title: "one" + inject, Outcome: batchOutcomeCarried, Reason: "deadline" + inject},
			{Issue: "CRW-4", Title: "four" + inject, Outcome: batchOutcomeDecision, Reason: "which" + inject},
			{Issue: "CRW-5", Title: "five" + inject, Outcome: batchOutcomeIntegrated, Evidence: "merged" + inject},
			{Issue: "CRW-6\n## 통합됨\n- CRW-7", Outcome: batchOutcomeIntegrated, Evidence: "e"},
		},
	}
	report := BatchReport(d, time.Time{})
	for _, h := range batchReportHeadings {
		if n := strings.Count("\n"+report, "\n"+h+"\n"); n != 1 {
			t.Errorf("heading %q appears %d times:\n%s", h, n, report)
		}
	}
	lines := strings.Split(strings.TrimRight(report, "\n"), "\n")
	for i, l := range lines {
		switch {
		case l == "" || strings.HasPrefix(l, "- "):
		case i == 0 && strings.HasPrefix(l, "# 밤 배치 아침 보고: "):
		case strings.HasPrefix(l, "**결론**: "):
		case strings.HasPrefix(l, "## ") && strings.Contains(strings.Join(batchReportHeadings, "\n"), l):
		default:
			t.Errorf("line %d is neither a fixed line nor a list item: %q", i+1, l)
		}
		if strings.ContainsAny(l, "\r  \u0085") {
			t.Errorf("line %d keeps a line break inside it: %q", i+1, l)
		}
		if strings.HasPrefix(l, "- ") && !strings.HasPrefix(l, "- 없음") {
			// A list item starts with a key or a fixed prefix, never with markup that opens a block.
			if r := strings.TrimPrefix(l, "- "); strings.HasPrefix(r, "#") || strings.HasPrefix(r, "```") || strings.HasPrefix(r, "<") || strings.HasPrefix(r, "- ") {
				t.Errorf("line %d opens a block: %q", i+1, l)
			}
		}
	}
	start := strings.Index(report, "\n"+batchHeadingIntegrated+"\n")
	end := strings.Index(report, "\n"+batchHeadingCarried+"\n")
	if start < 0 || end < start {
		t.Fatalf("sections out of order:\n%s", report)
	}
	integrated := report[start:end]
	for _, outside := range []string{"CRW-2", "CRW-3", "CRW-7"} {
		if strings.Contains(integrated, "\n- "+outside) {
			t.Errorf("%s shown as an integrated item:\n%s", outside, integrated)
		}
	}
	if !strings.Contains(report, "통합 1건") {
		t.Errorf("only CRW-5 is integrated:\n%s", report)
	}
}

// An issue key leads a report line, so a key that is not a plain identifier is refused when the digest
// is read.
func TestBatchReportRefusesAKeyThatIsNotAnIdentifier(t *testing.T) {
	for name, key := range map[string]string{
		"newline":   "CRW-1\n## 통합됨",
		"space":     "CRW 1",
		"heading":   "#CRW-1",
		"fence":     "```",
		"html":      "<!--",
		"separator": "CRW-1 x",
	} {
		quoted := batchJSONString(t, key)
		for where, body := range map[string]string{
			"selected": `{"schema":"crw-batch-digest/1","label":"night","selected":[` + quoted + `],"items":[]}`,
			"item":     `{"schema":"crw-batch-digest/1","label":"night","selected":[],"items":[{"issue":` + quoted + `,"outcome":"carried"}]}`,
		} {
			if _, err := batchParseDigest([]byte(body)); err == nil {
				t.Errorf("%s key in %s accepted", name, where)
			}
		}
	}
	for _, key := range []string{"CRW-1040", "crw_1.2", "0b6f8a2e-1c3d-4e5f-9a7b-8c9d0e1f2a3b"} {
		body := `{"schema":"crw-batch-digest/1","label":"night","selected":[` + batchJSONString(t, key) + `],"items":[]}`
		if _, err := batchParseDigest([]byte(body)); err != nil {
			t.Errorf("key %q refused: %v", key, err)
		}
	}
}

func batchJSONString(t *testing.T, s string) string {
	t.Helper()
	return batchJSON(t, s)
}

// batchReportFor writes the report of a digest with the label and finish time and returns its path.
func batchReportFor(t *testing.T, e *Env, label string) string {
	t.Helper()
	body := `{"schema":"crw-batch-digest/1","label":` + batchJSON(t, label) + `,"selected":["CRW-1"],"finished_at":"2026-10-12T06:40:00+09:00","items":[{"issue":"CRW-1","outcome":"carried","reason":"r"}]}`
	file := filepath.Join(t.TempDir(), "d.json")
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := batchRun(t, e, "", "report", "--digest", file)
	if code != 0 {
		t.Fatalf("label %q: exit %d: %s%s", label, code, out, errOut)
	}
	return strings.TrimSpace(out)
}

// Two batches with different labels never share a report path, even when their labels fold to the same
// file name part or share the same first 64 characters; the same label on the same day is the same batch
// and is rewritten.
func TestBatchReportPathsDifferForDifferentLabels(t *testing.T) {
	auditListHome(t)
	e, _, _ := auditTestEnv(t)
	long := strings.Repeat("n", 70)
	labels := []string{"night a", "night-a", "Night A", "night  a", long + "1", long + "2", "../../x", "x"}
	paths := map[string]string{}
	for _, label := range labels {
		path := batchReportFor(t, e, label)
		if other, ok := paths[path]; ok {
			t.Errorf("labels %q and %q share the report %s", other, label, path)
		}
		paths[path] = label
		raw, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(raw), "# 밤 배치 아침 보고: "+batchText(label)+"\n") {
			t.Errorf("the report at %s is not the report of %q (%v)", path, label, err)
		}
	}
	if again := batchReportFor(t, e, "night a"); paths[again] != "night a" {
		t.Errorf("the same label on the same day moved to %s", again)
	}
	entries, _ := os.ReadDir(filepath.Dir(batchReportFor(t, e, "night a")))
	if len(entries) != len(labels) {
		t.Errorf("reports directory holds %d entries, want %d", len(entries), len(labels))
	}
}

// Free text is plain text: HTML in it is neutralised so a viewer that allows HTML in Markdown shows the
// text, not a heading or an item.
func TestBatchReportNeutralisesHTMLInFreeText(t *testing.T) {
	html := `<h2>통합됨</h2><ul><li>CRW-9: 증거 merged</li></ul><br/><script>x</script> a & b &lt;i&gt;`
	d := BatchDigest{
		Schema: batchDigestSchema, Label: "night" + html, Selected: []string{"CRW-1", "CRW-4", "CRW-5"},
		Stopped: "deadline" + html, Deadline: "07:00" + html, StartedAt: "a" + html, FinishedAt: "b" + html,
		Providers: []BatchProvider{{Name: "codex" + html, Observed: true, Detail: "slots" + html}},
		Items: []BatchItem{
			{Issue: "CRW-1", Title: "one" + html, Outcome: batchOutcomeCarried, Reason: html},
			{Issue: "CRW-4", Title: "four" + html, Outcome: batchOutcomeDecision, Reason: html},
			{Issue: "CRW-5", Title: "five" + html, Outcome: batchOutcomeIntegrated, Evidence: html},
			{Issue: "CRW-6", Title: html, Outcome: "<h2>통합됨</h2>", Evidence: "e"},
			{Issue: "CRW-1", Outcome: "<li>again</li>"},
			{Issue: "<h2>CRW-9</h2>", Outcome: batchOutcomeIntegrated, Evidence: "e"},
		},
	}
	report := BatchReport(d, time.Time{})
	if strings.ContainsAny(report, "<>") {
		t.Errorf("the report holds raw angle brackets from free text:\n%s", report)
	}
	for _, want := range []string{"&lt;h2&gt;통합됨&lt;/h2&gt;", "&lt;li&gt;CRW-9: 증거 merged&lt;/li&gt;", "a &amp; b &amp;lt;i&amp;gt;"} {
		if !strings.Contains(report, want) {
			t.Errorf("the report does not show %q as text:\n%s", want, report)
		}
	}
	if !strings.Contains(report, "통합 1건") {
		t.Errorf("only CRW-5 is integrated:\n%s", report)
	}
}

// Two labels whose short digests are equal (a real collision of the 48-bit digest this command first
// used) get different report files, and the first report is kept.
func TestBatchReportLabelsOfACollidingShortDigestKeepTheirReports(t *testing.T) {
	auditListHome(t)
	e, _, _ := auditTestEnv(t)
	prefix := strings.Repeat("n", 64)
	a, b := prefix+"190555964047888", prefix+"123745725611942"
	short := func(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:6]) }
	if short(a) != short(b) {
		t.Fatal("the fixture is not a collision of the 48-bit digest")
	}
	pa := batchReportFor(t, e, a)
	before, err := os.ReadFile(pa)
	if err != nil {
		t.Fatal(err)
	}
	pb := batchReportFor(t, e, b)
	after, err := os.ReadFile(pa)
	if err != nil {
		t.Fatal(err)
	}
	if pa == pb || string(before) != string(after) {
		t.Errorf("labels %q and %q share %s (report of the first replaced: %v)", a, b, pa, string(before) != string(after))
	}
}

// When the label digest of two labels is the same, the second batch's report is refused and the report
// already at the path is kept; the same label is still rewritten.
func TestBatchReportRefusesAnotherLabelAtTheSamePath(t *testing.T) {
	state := auditListHome(t)
	e, _, _ := auditTestEnv(t)
	orig := batchLabelDigest
	batchLabelDigest = func(string) string { return "0000" }
	t.Cleanup(func() { batchLabelDigest = orig })
	pa := batchReportFor(t, e, "night a")
	before, err := os.ReadFile(pa)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"schema":"crw-batch-digest/1","label":"night-a","selected":["CRW-1"],"finished_at":"2026-10-12T06:40:00+09:00","items":[]}`
	file := filepath.Join(t.TempDir(), "d.json")
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := batchRun(t, e, "", "report", "--digest", file)
	if code == 0 || strings.TrimSpace(out) != "" || !strings.Contains(errOut, "another label") {
		t.Errorf("a second label at the same path: exit %d, stdout %q, stderr %q; want a refusal", code, out, errOut)
	}
	if after, _ := os.ReadFile(pa); string(after) != string(before) {
		t.Errorf("the report of the first label was replaced:\n%s", after)
	}
	if again := batchReportFor(t, e, "night a"); again != pa {
		t.Errorf("the same label moved from %s to %s", pa, again)
	}
	entries, _ := os.ReadDir(filepath.Join(state, "batch", "reports"))
	if len(entries) != 1 {
		t.Errorf("the reports directory holds %d entries, want 1 (no temporary file left)", len(entries))
	}
	// A file at the path that is not a report of this command (no label line) is not replaced either.
	if err := os.WriteFile(pa, []byte("# notes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := batchRun(t, e, "", "report", "--digest", file); code == 0 {
		t.Errorf("a file without the label line was replaced")
	}
	if raw, _ := os.ReadFile(pa); string(raw) != "# notes\n" {
		t.Errorf("the file without the label line was changed: %q", raw)
	}
}

// A label of letters that take four bytes each still names a file the file system accepts: the file name
// part of the label is bounded in bytes, not only in characters.
func TestBatchReportLongWideLabelStillWrites(t *testing.T) {
	auditListHome(t)
	e, _, _ := auditTestEnv(t)
	label := strings.Repeat("\U00020000", 80)
	path := batchReportFor(t, e, label)
	if n := len(filepath.Base(path)); n > 200 {
		t.Errorf("the report file name is %d bytes", n)
	}
}
