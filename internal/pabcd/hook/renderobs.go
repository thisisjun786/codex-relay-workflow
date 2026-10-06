// Package hook contains the ported PABCD hook behavior. Harness owns ingress and
// activation; this package has no package-level working initializers.
package hook

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

const StateDir = crwdir.DirName
const RenderObsFile = "render-observations.jsonl"

type RenderObsKind string

const (
	Observation       RenderObsKind = "observation"
	ArtifactModified  RenderObsKind = "artifact-modified"
	NativeObservation RenderObsKind = "native-observation"
)

// RenderObsRow is render-observations.ts:54-65; empty optional fields are absent.
type RenderObsRow struct {
	TS, Detail, SessionID                  string
	Kind                                   RenderObsKind
	NativeApp, ScreenshotPath, CriterionID string
}

// RenderObservationTools and RenderArtifactExtensions return independent copies.
func RenderObservationTools() []string {
	return []string{"view_image", "browser:control-in-app-browser", "chrome:control-chrome", "computer-use:computer-use"}
}
func RenderArtifactExtensions() []string { return []string{".html", ".svg", ".css", ".jsx", ".tsx"} }

// RenderPayload is the typed ingress supplied by the harness, without an import
// back to harness (which calls this package).
type RenderPayload struct {
	Event, Cwd, SessionID, ToolName string
	Input, Response                 any
}

func ledgerPath(cwd string) string { return filepath.Join(cwd, StateDir, RenderObsFile) }

// confined resolves existing absolute internal links, then Root opens the result:
// an escaping replacement link cannot pass between validation and the open.
func confined(base, path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		parent, parentErr := filepath.EvalSymlinks(filepath.Dir(path))
		if parentErr != nil {
			return "", parentErr
		}
		resolved, err = filepath.Join(parent, filepath.Base(path)), nil
	}
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(base, resolved)
	if err != nil || !filepath.IsLocal(rel) {
		return "", os.ErrPermission
	}
	return rel, nil
}

func workspace(cwd string) (*os.Root, string, error) {
	base, err := filepath.Abs(cwd)
	if err == nil {
		base, err = filepath.EvalSymlinks(base)
	}
	if err != nil {
		return nil, "", err
	}
	root, err := os.OpenRoot(base)
	return root, base, err
}

func ledgerRoot(cwd string, create bool) (*os.Root, string, error) {
	root, base, err := workspace(cwd)
	if err != nil {
		return nil, "", err
	}
	dir := filepath.Join(base, StateDir)
	if _, err = confined(base, dir); err == nil && create {
		_, err = crwdir.EnsureDir(base)
	}
	var path string
	if err == nil {
		path, err = confined(base, filepath.Join(dir, RenderObsFile))
	}
	if err != nil {
		_ = root.Close()
		return nil, "", err
	}
	return root, path, nil
}

func (row RenderObsRow) line() string {
	quote := func(s string) string { return pyjson.Dumps(s, pyjson.Options{Unicode: true}) }
	fields := []string{`"ts":` + quote(row.TS), `"kind":` + quote(string(row.Kind)), `"detail":` + quote(row.Detail), `"sessionId":` + quote(row.SessionID)}
	for _, field := range [][2]string{{"nativeApp", row.NativeApp}, {"screenshotPath", row.ScreenshotPath}, {"criterionId", row.CriterionID}} {
		if field[1] != "" {
			fields = append(fields, quote(field[0])+":"+quote(field[1]))
		}
	}
	return "{" + strings.Join(fields, ",") + "}\n"
}

func appendRow(cwd string, row RenderObsRow) {
	root, path, err := ledgerRoot(cwd, true)
	if err != nil {
		return // best effort, as in the oracle
	}
	defer root.Close()
	f, err := root.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		return
	}
	defer f.Close()
	line := row.line()
	if endsMidLine(root, f, path) {
		line = "\n" + line
	}
	_, _ = f.WriteString(line)
}

// The existing state/interview conservative LF guard, with Root-confined reads.
func endsMidLine(root *os.Root, f *os.File, path string) bool {
	info, err := f.Stat()
	if err != nil {
		return true
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return false
	}
	r, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return true
	}
	defer r.Close()
	readInfo, err := r.Stat()
	if err != nil || !os.SameFile(info, readInfo) {
		return true
	}
	var last [1]byte
	n, _ := r.ReadAt(last[:], info.Size()-1)
	return n != 1 || last[0] != '\n'
}

func ledgerBytes(cwd string) ([]byte, error) {
	root, path, err := ledgerRoot(cwd, false)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.ReadFile(path)
}

func jsonValue(raw string) (any, error) {
	return pyjson.Loads(raw, pyjson.LoadOptions{Map: true, Surrogates: true, Numbers: pyjson.SpelledNumbers})
}

func stringField(o map[string]any, key string) string { s, _ := o[key].(string); return s }

// ReadRenderObsRows skips damaged lines and validates only the oracle's fields.
func ReadRenderObsRows(cwd string) []RenderObsRow {
	out := []RenderObsRow{}
	data, err := ledgerBytes(cwd)
	if err != nil {
		return out
	}
	for _, line := range text.SplitLines(string(data)) {
		value, err := jsonValue(text.Trim(line))
		o, _ := value.(map[string]any)
		kind := RenderObsKind(stringField(o, "kind"))
		detail, valid := o["detail"].(string)
		if err != nil || !valid || (kind != Observation && kind != ArtifactModified && kind != NativeObservation) {
			continue
		}
		row := RenderObsRow{TS: stringField(o, "ts"), Kind: kind, Detail: detail, SessionID: stringField(o, "sessionId")}
		for key, dst := range map[string]*string{"nativeApp": &row.NativeApp, "screenshotPath": &row.ScreenshotPath, "criterionId": &row.CriterionID} {
			if s := stringField(o, key); text.Trim(s) != "" {
				*dst = s
			}
		}
		if kind != NativeObservation || row.NativeApp != "" || row.ScreenshotPath != "" {
			out = append(out, row)
		}
	}
	return out
}

// ResetRenderLedger truncates only the confined ledger; it preserves appenders'
// open inode, unlike a rename, and cannot truncate an outside symlink target.
func ResetRenderLedger(cwd string) {
	root, path, err := ledgerRoot(cwd, true)
	if err != nil {
		return
	}
	defer root.Close()
	if f, err := root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666); err == nil {
		_ = f.Close()
	}
}

func matchingRows(cwd string, kind RenderObsKind, session []string) []RenderObsRow {
	out := []RenderObsRow{}
	for _, row := range ReadRenderObsRows(cwd) {
		if row.Kind == kind && (len(session) == 0 || row.SessionID == session[0]) {
			out = append(out, row)
		}
	}
	return out
}
func HasRenderObservation(cwd string, session ...string) bool {
	return len(matchingRows(cwd, Observation, session)) > 0
}
func HasRenderArtifactModified(cwd string, session ...string) bool {
	return len(matchingRows(cwd, ArtifactModified, session)) > 0
}
func NativeObservationRows(cwd string, session ...string) []RenderObsRow {
	return matchingRows(cwd, NativeObservation, session)
}
func HasNativeObservation(cwd string, session ...string) bool {
	return len(NativeObservationRows(cwd, session...)) > 0
}

func NativeObservationLedgerMalformed(cwd string) bool {
	data, err := ledgerBytes(cwd)
	if err != nil {
		return !errors.Is(err, os.ErrNotExist)
	}
	return RenderObsLedgerMalformed(data)
}

// RenderObsLedgerMalformed is NativeObservationLedgerMalformed over bytes a caller already holds, so a second reader of
// the same ledger (the state-copy preflight, internal/runtime/install/migrate) decides it by this rule rather than by one
// of its own. The oracle's Stop hook asks the same question before it trusts the ledger (hook.ts:1910).
func RenderObsLedgerMalformed(data []byte) bool {
	for _, line := range text.SplitLines(string(data)) {
		if text.Trim(line) == "" {
			continue
		}
		v, err := jsonValue(line) // deliberately untrimmed, unlike the row reader
		if _, ok := v.(map[string]any); err != nil || !ok {
			return true
		}
	}
	return false
}

func structuredField(value any, names ...string) string {
	o, _ := value.(map[string]any)
	for _, name := range names {
		if s := text.Trim(stringField(o, name)); s != "" {
			return s
		}
	}
	return ""
}

func resolve(base, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(base, path)
}

func declaredScreenshot(cwd, session, path string) string {
	if session == "" || session == "." || session == ".." || strings.IndexFunc(session, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-')
	}) >= 0 {
		return ""
	}
	root, base, err := workspace(cwd)
	if err != nil {
		return ""
	}
	defer root.Close()
	logical, err := filepath.Abs(cwd)
	if err != nil {
		return ""
	}
	viewed := resolve(logical, path)
	if rel, err := filepath.Rel(logical, viewed); err != nil || !filepath.IsLocal(rel) {
		return ""
	}
	qa := filepath.Join(logical, StateDir, "evidence", session, "qa")
	qaRel, err := confined(base, qa)
	if err != nil {
		return ""
	}
	found := false
	err = fs.WalkDir(root.FS(), qaRel, func(name string, entry fs.DirEntry, err error) error {
		if err != nil || found {
			return err
		}
		if !entry.Type().IsRegular() || entry.Name() != "verdict.json" {
			return nil
		}
		data, err := root.ReadFile(name)
		if err != nil {
			return err
		}
		value, err := jsonValue(string(data))
		if err != nil || value == nil {
			return fs.ErrInvalid
		}
		o, _ := value.(map[string]any)
		refs, _ := o["artifactRefs"].([]any)
		logicalName, _ := filepath.Rel(qaRel, name)
		for _, ref := range refs {
			if s, ok := ref.(string); ok && resolve(filepath.Dir(filepath.Join(qa, logicalName)), s) == viewed {
				found = true
				return fs.SkipAll
			}
		}
		return nil
	})
	if err == nil && found && nonEmptyFile(root, base, viewed) {
		return viewed
	}
	return ""
}

func nonEmptyFile(root *os.Root, base, path string) bool {
	rel, err := confined(base, path)
	if err != nil {
		return false
	}
	info, err := root.Stat(rel) // statSync parity: never open a FIFO just to inspect its size
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

func asciiWord(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}
func toolResponseFailed(response any) bool {
	if s, ok := response.(string); ok {
		lower := strings.Map(func(r rune) rune {
			if r >= 'A' && r <= 'Z' {
				return r + ('a' - 'A')
			}
			return r
		}, s)
		head := text.Trim(lower)
		for _, prefix := range []string{"error", "failed"} {
			if strings.HasPrefix(head, prefix) && (len(head) == len(prefix) || !asciiWord(head[len(prefix)])) {
				return true
			}
		}
		for i := 0; i+6 <= len(lower); i++ {
			if lower[i:i+6] == "enoent" && (i == 0 || !asciiWord(lower[i-1])) && (i+6 == len(lower) || !asciiWord(lower[i+6])) {
				return true
			}
		}
		return false
	}
	o, _ := response.(map[string]any)
	if o["isError"] == true || o["success"] == false {
		return true
	}
	err, exists := o["error"]
	return exists && err != nil && err != false && err != ""
}

func IsRenderArtifact(path string) bool {
	base := filepath.Base(path)
	if strings.LastIndexByte(base, '.') <= 0 {
		return false
	}
	switch strings.ToLower(filepath.Ext(base)) {
	case ".html", ".svg", ".css", ".jsx", ".tsx":
		return true
	}
	return false
}

func HandleRenderObservationCapture(p RenderPayload) string {
	if p.Event != "PostToolUse" {
		return ""
	}
	found := false
	for _, tool := range RenderObservationTools() {
		if p.ToolName == tool {
			found = true
		}
	}
	if !found {
		return ""
	}
	row := RenderObsRow{TS: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), Kind: Observation, Detail: p.ToolName, SessionID: p.SessionID}
	appendRow(p.Cwd, row)
	row.Kind = NativeObservation
	row.CriterionID = structuredField(p.Input, "criterionId")
	if row.CriterionID == "" {
		row.CriterionID = structuredField(p.Response, "criterionId")
	}
	if p.ToolName == "computer-use:computer-use" {
		row.NativeApp = structuredField(p.Input, "appName", "application", "app")
		if row.NativeApp == "" {
			row.NativeApp = structuredField(p.Response, "appName", "application", "app")
		}
		if row.NativeApp != "" {
			row.TS = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
			appendRow(p.Cwd, row)
		}
	} else if p.ToolName == "view_image" {
		path := structuredField(p.Input, "path")
		if path != "" && !toolResponseFailed(p.Response) {
			row.ScreenshotPath = declaredScreenshot(p.Cwd, p.SessionID, path)
			if row.ScreenshotPath != "" {
				row.TS = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
				appendRow(p.Cwd, row)
			}
		}
	}
	return ""
}

func HandleRenderArtifactCapture(p RenderPayload) string {
	if p.Event != "PostToolUse" || p.ToolName != "apply_patch" {
		return ""
	}
	o, _ := p.Input.(map[string]any)
	command, _ := o["command"].(string)
	for _, shape := range FileEditShapes(command) {
		if IsRenderArtifact(shape.File) {
			appendRow(p.Cwd, RenderObsRow{TS: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), Kind: ArtifactModified, Detail: shape.File, SessionID: p.SessionID})
		}
	}
	return ""
}

func RenderGroundingAdvisory() string {
	return "[crw advisory — C-RENDER-GROUNDING-01] Render-artifact files were modified " +
		"during this cycle, but no render-observation tool call (view_image, " +
		"browser:control-in-app-browser, chrome:control-chrome, computer-use:computer-use) " +
		"was recorded. Before C->D, RUN the artifact in its execution environment, OBSERVE " +
		"the output (read the screenshot back), and FIX any defect. Well-formed (tsc/lint) " +
		"is not correct -- a static parse does not confirm the artifact renders correctly. " +
		"Defaults: 1280x720 viewport; drive stateful artifacts until first interactive " +
		"state change. One clean observation suffices."
}
