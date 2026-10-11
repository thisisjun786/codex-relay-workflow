package manage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
)

// The morning report of a night batch (CRW-1040). It is made from a run digest, the record the night
// run leaves (CRW-1040 part b), and written in the crw-status report form: the conclusion first, then
// what was integrated, what was cancelled or carried and why, what needs Jun's decision, and last the
// evidence checked and what stays unverified. The form is
// plugins/crw/skills/crw-status/references/night-batch-report.md; the headings below are its headings.

const batchReportUsage = "usage: crw manage batch report --digest FILE"

// batchDigestSchema is the schema of the run digest the report reads.
const batchDigestSchema = "crw-batch-digest/1"

// The four section headings of the report, in order.
const (
	batchHeadingIntegrated = "## 통합됨"
	batchHeadingCarried    = "## 취소·이월"
	batchHeadingDecision   = "## Jun 결정이 필요한 것"
	batchHeadingEvidence   = "## 확인한 증거와 미확인"
)

var batchReportHeadings = []string{batchHeadingIntegrated, batchHeadingCarried, batchHeadingDecision, batchHeadingEvidence}

// What an item of the digest ended as.
const (
	batchOutcomeIntegrated = "integrated"
	batchOutcomeCancelled  = "cancelled"
	batchOutcomeCarried    = "carried"
	batchOutcomeDecision   = "needs_decision"
)

const batchNoReason = "이유 기록 없음"

// BatchProvider is one provider's observation at the end of the run. A provider whose quota or capacity
// could not be observed is recorded as not observed and shown so, never assumed to have room.
type BatchProvider struct {
	Name     string `json:"name"`
	Observed bool   `json:"observed"`
	Detail   string `json:"detail"`
}

// BatchItem is one issue's end state in the run.
type BatchItem struct {
	Issue    string `json:"issue"`
	Title    string `json:"title"`
	Outcome  string `json:"outcome"`
	Reason   string `json:"reason"`
	Evidence string `json:"evidence"`
}

// BatchDigest is the run digest.
type BatchDigest struct {
	Schema     string          `json:"schema"`
	Label      string          `json:"label"`
	Selected   []string        `json:"selected"`
	StartedAt  string          `json:"started_at"`
	FinishedAt string          `json:"finished_at"`
	Deadline   string          `json:"deadline"`
	Stopped    string          `json:"stopped"`
	Providers  []BatchProvider `json:"providers"`
	Items      []BatchItem     `json:"items"`
}

// batchParseDigest reads and checks a digest: the schema, a label, the times that are given, and a key
// on every item. What an item says is judged by the report, not refused here.
func batchParseDigest(raw []byte) (BatchDigest, error) {
	var d BatchDigest
	if err := json.Unmarshal(raw, &d); err != nil {
		return d, fmt.Errorf("the digest is not a JSON object: %v", err)
	}
	if d.Schema != batchDigestSchema {
		return d, fmt.Errorf("the digest schema is %q, want %q", d.Schema, batchDigestSchema)
	}
	if strings.TrimSpace(d.Label) == "" {
		return d, errors.New("the digest has no label")
	}
	for name, value := range map[string]string{"started_at": d.StartedAt, "finished_at": d.FinishedAt, "deadline": d.Deadline} {
		if value == "" {
			continue
		}
		if _, err := time.Parse(time.RFC3339, value); err != nil {
			return d, fmt.Errorf("%s %q is not an RFC 3339 time", name, value)
		}
	}
	if d.Selected == nil {
		return d, errors.New(`the digest has no "selected" list (write [] when nothing was selected)`)
	}
	for i, key := range d.Selected {
		if strings.TrimSpace(key) == "" {
			return d, fmt.Errorf("selected entry %d is blank", i+1)
		}
		if !batchKeyValid(key) {
			return d, fmt.Errorf("selected entry %d %q is not an issue key (letters, digits, '-', '_' and '.', starting and ending with a letter or digit)", i+1, key)
		}
	}
	for i, item := range d.Items {
		if strings.TrimSpace(item.Issue) == "" {
			return d, fmt.Errorf("item %d has no issue key", i+1)
		}
		if !batchKeyValid(item.Issue) {
			return d, fmt.Errorf("item %d issue %q is not an issue key (letters, digits, '-', '_' and '.', starting and ending with a letter or digit)", i+1, item.Issue)
		}
	}
	return d, nil
}

// batchKeyValid is whether an issue key is a plain identifier: a Linear key such as CRW-1040 or an
// issue id. A key leads a line of the report, so it holds nothing Markdown could read as the start of
// a heading, a list, a fence or an HTML block: only letters, digits, '-', '_' and '.', the first and the
// last a letter or a digit.
func batchKeyValid(key string) bool {
	runes := []rune(key)
	if len(runes) == 0 {
		return false
	}
	for i, r := range runes {
		alnum := unicode.IsLetter(r) || unicode.IsDigit(r)
		if (i == 0 || i == len(runes)-1) && !alnum {
			return false
		}
		if !alnum && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	return true
}

// batchKey is an issue key as the report shows it: the key itself, or, for a key a caller of
// BatchReport passed without batchParseDigest's check, the key quoted on one line.
func batchKey(key string) string {
	if batchKeyValid(key) {
		return key
	}
	return batchQuoted(key)
}

// batchText is free text of the digest as the report shows it: one line, every run of white space or
// control characters (newlines, carriage returns, line and paragraph separators) folded to one space,
// and '&', '<' and '>' written as character references. Free text never starts a report line, so on one
// line it can neither add a heading nor a list item nor end the section it stands in; and it holds no
// raw HTML, so a viewer that allows HTML in Markdown shows the text, never a tag of the digest's.
func batchText(text string) string {
	return batchHTMLEscaper.Replace(strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)), " "))
}

var batchHTMLEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// batchQuoted is free text quoted on one line, for a value the report shows inside quotation marks.
func batchQuoted(text string) string { return batchHTMLEscaper.Replace(strconv.Quote(text)) }

// BatchReport renders the morning report of a digest. checkedAt is when the report was made. It reads
// only the digest: an item is placed by what the digest records, and what the record does not show is
// listed as unverified rather than reported as done.
func BatchReport(d BatchDigest, checkedAt time.Time) string {
	// The selected list, each issue once. An empty or missing list selects nothing: every item is then
	// outside it.
	selected := map[string]bool{}
	var selectedKeys []string
	for _, key := range d.Selected {
		if !selected[key] {
			selected[key] = true
			selectedKeys = append(selectedKeys, key)
		}
	}
	var integrated, carried, decision, unverified []string
	counts := map[string]int{}
	seen := map[string]bool{}
	recorded := map[string]bool{}
	for _, item := range d.Items {
		recorded[item.Issue] = true
		key := batchKey(item.Issue)
		name := key
		if t := batchText(item.Title); t != "" {
			name += " " + t
		}
		if seen[item.Issue] {
			unverified = append(unverified, fmt.Sprintf("%s: 같은 이슈가 중복 기록됨 (첫 기록만 사용, 뒤 기록 결과 %s)", key, batchQuoted(item.Outcome)))
			continue
		}
		seen[item.Issue] = true
		reason := batchText(item.Reason)
		if !selected[item.Issue] {
			decision = append(decision, fmt.Sprintf("%s: 선택 목록 밖 이슈가 실행 기록에 있다 (결과 %s). 해제 경위를 확인해야 한다", name, batchQuoted(item.Outcome)))
			counts[batchOutcomeDecision]++
			continue
		}
		withReason := func(label string) string {
			if reason == "" {
				unverified = append(unverified, fmt.Sprintf("%s: %s로 기록됐으나 %s", key, label, batchNoReason))
				return fmt.Sprintf("%s: %s: %s", name, label, batchNoReason)
			}
			return fmt.Sprintf("%s: %s: %s", name, label, reason)
		}
		switch item.Outcome {
		case batchOutcomeIntegrated:
			evidence := batchText(item.Evidence)
			if evidence == "" {
				unverified = append(unverified, fmt.Sprintf("%s: 통합으로 기록됐으나 증거 없음 (통합으로 세지 않음)", key))
				continue
			}
			counts[batchOutcomeIntegrated]++
			integrated = append(integrated, fmt.Sprintf("%s: 증거 %s", name, evidence))
		case batchOutcomeCancelled:
			counts[batchOutcomeCancelled]++
			carried = append(carried, withReason("취소"))
		case batchOutcomeCarried:
			counts[batchOutcomeCarried]++
			carried = append(carried, withReason("이월"))
		case batchOutcomeDecision:
			counts[batchOutcomeDecision]++
			if reason == "" {
				unverified = append(unverified, fmt.Sprintf("%s: 결정 필요로 기록됐으나 %s", key, batchNoReason))
				decision = append(decision, fmt.Sprintf("%s: %s", name, batchNoReason))
			} else {
				decision = append(decision, fmt.Sprintf("%s: %s", name, reason))
			}
		default:
			unverified = append(unverified, fmt.Sprintf("%s: 알 수 없는 결과 %s (분류하지 않음)", key, batchQuoted(item.Outcome)))
		}
	}
	for _, key := range selectedKeys {
		if !recorded[key] {
			unverified = append(unverified, fmt.Sprintf("%s: 선택됐으나 실행 기록 없음", batchKey(key)))
		}
	}

	var b strings.Builder
	scope := fmt.Sprintf("선택 %d건 중", len(selectedKeys))
	fmt.Fprintf(&b, "# 밤 배치 아침 보고: %s\n\n", batchText(d.Label))
	fmt.Fprintf(&b, "**결론**: %s 통합 %d건, 취소 %d건, 이월 %d건, Jun 결정 필요 %d건, 미확인 %d건.", scope,
		counts[batchOutcomeIntegrated], counts[batchOutcomeCancelled], counts[batchOutcomeCarried], counts[batchOutcomeDecision], len(unverified))
	if deadline := batchText(d.Deadline); deadline != "" {
		fmt.Fprintf(&b, " 기한 %s.", deadline)
	}
	if stopped := batchText(d.Stopped); stopped != "" {
		fmt.Fprintf(&b, " 종료 사유 %s.", stopped)
	}
	b.WriteString("\n")
	section := func(heading string, lines []string) {
		fmt.Fprintf(&b, "\n%s\n\n", heading)
		if len(lines) == 0 {
			b.WriteString("- 없음\n")
		}
		for _, l := range lines {
			fmt.Fprintf(&b, "- %s\n", l)
		}
	}
	section(batchHeadingIntegrated, integrated)
	section(batchHeadingCarried, carried)
	section(batchHeadingDecision, decision)

	evidence := []string{"확인 시각: " + checkedAt.UTC().Format(time.RFC3339)}
	times := func(label, v string) {
		if v = batchText(v); v == "" {
			evidence = append(evidence, label+": 기록 없음")
		} else {
			evidence = append(evidence, label+": "+v)
		}
	}
	times("시작", d.StartedAt)
	times("종료", d.FinishedAt)
	if len(d.Providers) == 0 {
		evidence = append(evidence, "제공자 관측: 기록 없음 (미확인)")
	}
	for _, p := range d.Providers {
		state := "관측됨"
		if !p.Observed {
			state = "관측 안 됨"
		}
		line := fmt.Sprintf("제공자 관측: %s: %s", batchText(p.Name), state)
		if detail := batchText(p.Detail); detail != "" {
			line += " (" + detail + ")"
		}
		evidence = append(evidence, line)
	}
	for _, u := range unverified {
		evidence = append(evidence, "미확인: "+u)
	}
	if len(unverified) == 0 {
		evidence = append(evidence, "미확인: 없음")
	}
	evidence = append(evidence, batchLabelIdentity(d.Label))
	section(batchHeadingEvidence, evidence)
	return b.String()
}

// batchSlug is a label as a readable file name part: letters and digits, the rest folded to one dash, so
// a label can never name a path. Many labels fold to one slug; batchReportPath adds the label's digest.
func batchSlug(label string) string {
	var b strings.Builder
	dash := true
	for _, r := range strings.ToLower(label) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteRune('-')
			dash = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if r := []rune(slug); len(r) > 64 {
		slug = string(r[:64])
	}
	// At most 128 bytes, cut between characters, so the file name (with the date, the digest and the
	// temporary suffix of the write) stays within the 255 bytes a file system accepts.
	for len(slug) > 128 {
		_, size := utf8.DecodeLastRuneInString(slug)
		slug = slug[:len(slug)-size]
	}
	slug = strings.Trim(slug, "-")
	if slug == "" {
		return "batch"
	}
	return slug
}

// batchLabelDigest is a stable digest of the real label (as the digest gives it): the first 128 bits of its
// sha256 in hex, so two labels that fold to the same slug, or share its first 64 characters, do not name the
// same report file. The report's last line holds the whole sha256 (batchLabelIdentity), and a report of
// another label already at the path is never replaced, so even a collision of this digest loses no report.
// A variable so that a test can make two labels collide.
var batchLabelDigest = func(label string) string {
	sum := sha256.Sum256([]byte(label))
	return hex.EncodeToString(sum[:16])
}

// batchLabelIdentity is the last item of every report: the whole sha256 of the real label, which says
// which batch the report file belongs to. No free text can make it: free text never starts a line.
func batchLabelIdentity(label string) string {
	sum := sha256.Sum256([]byte(label))
	return "라벨 식별: sha256 " + hex.EncodeToString(sum[:])
}

// batchReportOwned refuses to replace a file at the report path that is not a report of this label: a
// report of another label (two labels whose digests collide) or a file this command did not write. A
// missing file is free.
func batchReportOwned(path, label string) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the file already at the report path: %w", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if lines[len(lines)-1] != "- "+batchLabelIdentity(label) {
		return fmt.Errorf("%s holds the report of another label (or a file that is not a batch report of this label); it is kept and this report is not written", path)
	}
	return nil
}

// batchReportPath is where a digest's report is written:
// <state_dir>/batch/reports/<date>-<label slug>-<label digest>.md, the date being the finish day in the
// digest's own offset (the morning the report is read), or today when the run recorded no finish. The
// same label on the same day is the same batch and its report is rewritten; a file of another label at the
// path is kept and the report is refused (batchReportOwned).
func batchReportPath(e *Env, d BatchDigest) string {
	day := e.Now().UTC().Format("2006-01-02")
	if t, err := time.Parse(time.RFC3339, d.FinishedAt); err == nil {
		day = t.Format("2006-01-02")
	}
	return crwconfig.JoinRoot(coreDefaults(e).StateDir, "batch", "reports", day+"-"+batchSlug(d.Label)+"-"+batchLabelDigest(d.Label)+".md")
}

func batchRunReport(_ context.Context, e *Env, args []string) int {
	if auditOptionsHelpRequested(args) {
		fmt.Fprintln(e.Stdout, batchReportUsage)
		return 0
	}
	if code, refused := batchConfigRefused(e); refused {
		return code
	}
	values, err := auditPkgParseArgs(args, map[string]bool{"digest": true})
	if err != nil {
		return batchRefuse(e, batchReportUsage, "report", err)
	}
	if strings.TrimSpace(values["digest"]) == "" {
		return batchRefuse(e, batchReportUsage, "report", errors.New("the option --digest needs a value"))
	}
	raw, err := batchReadInput(e, values["digest"])
	if err != nil {
		return batchRefuse(e, batchReportUsage, "report", err)
	}
	digest, err := batchParseDigest(raw)
	if err != nil {
		return batchRefuse(e, batchReportUsage, "report", err)
	}
	path := batchReportPath(e, digest)
	if err := batchWriteFile(path, []byte(BatchReport(digest, e.Now())), func() error { return batchReportOwned(path, digest.Label) }); err != nil {
		fmt.Fprintf(e.Stderr, "crw manage batch report: error: %v\n", err)
		return 3
	}
	fmt.Fprintln(e.Stdout, path)
	return 0
}

// batchWriteFile writes a report through a private temporary file in its directory, synced before the
// rename that commits it, so a morning reader sees the old report or the whole new one. check runs just
// before the rename; when it fails the temporary file is removed and the file at path is kept.
func batchWriteFile(path string, data []byte, check func() error) error {
	dir := rootDir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("the report directory: %w", err)
	}
	file, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("the report temporary file: %w", err)
	}
	temporary := file.Name()
	fail := func(step string, err error) error {
		file.Close()
		os.Remove(temporary)
		return fmt.Errorf("%s: %w", step, err)
	}
	if _, err := file.Write(data); err != nil {
		return fail("write the report", err)
	}
	if err := file.Sync(); err != nil {
		return fail("sync the report", err)
	}
	if err := file.Close(); err != nil {
		return fail("close the report", err)
	}
	if err := check(); err != nil {
		os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return fail("commit the report", err)
	}
	return nil
}
