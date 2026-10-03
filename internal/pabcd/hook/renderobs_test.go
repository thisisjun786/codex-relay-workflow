package hook

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func put(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}
func payload(cwd, tool string) RenderPayload {
	return RenderPayload{Event: "PostToolUse", Cwd: cwd, SessionID: "s1", ToolName: tool, Input: map[string]any{}, Response: "ok"}
}

func TestRenderExtensionsAndCapture(t *testing.T) {
	for _, ext := range RenderArtifactExtensions() {
		if !IsRenderArtifact("dir/page" + strings.ToUpper(ext)) {
			t.Fatal(ext)
		}
	}
	for _, path := range []string{"server.ts", "config.json", "README.md", "main.py", "Makefile", ".html"} {
		if IsRenderArtifact(path) {
			t.Fatal(path)
		}
	}
	cwd := t.TempDir()
	for _, tool := range RenderObservationTools() {
		if HandleRenderObservationCapture(payload(cwd, tool)) != "" {
			t.Fatal(tool)
		}
	}
	if rows := ReadRenderObsRows(cwd); len(rows) != 4 || rows[3].Kind != Observation {
		t.Fatalf("rows=%v", rows)
	}
	for _, tool := range []string{"apply_patch", "Bash", "request_user_input"} {
		HandleRenderObservationCapture(payload(cwd, tool))
	}
	p := payload(cwd, "view_image")
	p.Event = "UserPromptSubmit"
	HandleRenderObservationCapture(p)
	if len(ReadRenderObsRows(cwd)) != 4 {
		t.Fatal("unexpected rows")
	}
	for _, tool := range []string{"apply_patch", "Bash"} {
		p = payload(cwd, tool)
		p.Input = map[string]any{"command": "*** Update File: web/index.html\n-old\n+new\n*** Add File: web/App.tsx\n+new\n*** Delete File: old.svg\n*** Update File: server.ts\n+new\n*** End Patch"}
		if HandleRenderArtifactCapture(p) != "" {
			t.Fatal("output")
		}
	}
	rows := ReadRenderObsRows(cwd)
	if len(rows) != 6 || rows[4].Detail != "web/index.html" || rows[5].Detail != "web/App.tsx" {
		t.Fatalf("rows=%v", rows)
	}
	p.Input = []any{"command"}
	HandleRenderArtifactCapture(p)
	if len(ReadRenderObsRows(cwd)) != 6 {
		t.Fatal("malformed input recorded")
	}
}

func TestRenderReadersAndQueries(t *testing.T) {
	for _, test := range []struct {
		name, raw string
		rows      int
		malformed bool
	}{
		{"empty", "", 0, false}, {"garbage", "bad\n", 0, true}, {"objects", "{}\n", 0, false}, {"array", "[]", 0, true}, {"null", "null", 0, true},
		{"defaults", `{"kind":"observation","detail":""}`, 1, false}, {"number", `{"kind":"observation","detail":"image","ignored":1e400}`, 1, false},
		{"bad-native", `{"kind":"native-observation","detail":"image","nativeApp":" "}`, 0, false},
		{"native", `{"kind":"native-observation","detail":"image","nativeApp":" Finder ","criterionId":" c-1 "}`, 1, false},
		{"case", `{"Kind":"observation","detail":"image"}`, 0, false},
		{"bom", "\ufeff" + `{"kind":"observation","detail":"image"}`, 1, true},
		{"crlf", "{}\r\n" + `{"kind":"artifact-modified","detail":"a.html"}` + "\r\n", 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cwd := t.TempDir()
			put(t, ledgerPath(cwd), test.raw)
			if rows := ReadRenderObsRows(cwd); len(rows) != test.rows {
				t.Fatalf("rows=%v want%d", rows, test.rows)
			}
			if got := NativeObservationLedgerMalformed(cwd); got != test.malformed {
				t.Fatalf("malformed=%v want%v", got, test.malformed)
			}
		})
	}
	cwd := t.TempDir()
	if ReadRenderObsRows(cwd) == nil || NativeObservationLedgerMalformed(cwd) {
		t.Fatal("missing ledger")
	}
	for _, kind := range []RenderObsKind{Observation, ArtifactModified, NativeObservation} {
		appendRow(cwd, RenderObsRow{Kind: kind, Detail: "d", SessionID: "other", NativeApp: "Finder"})
	}
	if !HasRenderObservation(cwd) || !HasRenderArtifactModified(cwd, "other") || !HasNativeObservation(cwd, "other") || HasRenderObservation(cwd, "") || HasRenderArtifactModified(cwd, "s1") || len(NativeObservationRows(cwd, "s1")) != 0 {
		t.Fatal("session filtering")
	}
	ResetRenderLedger(cwd)
	if HasRenderObservation(cwd) || HasRenderArtifactModified(cwd) || HasNativeObservation(cwd) {
		t.Fatal("reset")
	}
	cwd = t.TempDir()
	if err := os.MkdirAll(ledgerPath(cwd), 0o755); err != nil {
		t.Fatal(err)
	}
	if len(ReadRenderObsRows(cwd)) != 0 || !NativeObservationLedgerMalformed(cwd) {
		t.Fatal("directory ledger")
	}
	ResetRenderLedger(t.TempDir()) // missing/reset is fail-open
}

func TestRenderRowEncodingAndTailGuard(t *testing.T) {
	row := RenderObsRow{TS: "t", Kind: Observation, Detail: "<>&\u2028\u2029", SessionID: "s", CriterionID: "c"}
	want := "{\"ts\":\"t\",\"kind\":\"observation\",\"detail\":\"<>&\u2028\u2029\",\"sessionId\":\"s\",\"criterionId\":\"c\"}\n"
	if row.line() != want {
		t.Fatalf("line=%q want%q", row.line(), want)
	}
	for _, tail := range []string{`{"kind":"artifact-modified","detail":"old.html"}`, "{partial", ""} {
		cwd := t.TempDir()
		put(t, ledgerPath(cwd), tail)
		appendRow(cwd, row)
		data, err := ledgerBytes(cwd)
		prefix := tail
		if tail != "" {
			prefix += "\n"
		}
		if err != nil || string(data) != prefix+want {
			t.Fatalf("bytes=%q err=%v", data, err)
		}
		if rows := ReadRenderObsRows(cwd); len(rows) == 0 || rows[len(rows)-1].Detail != row.Detail {
			t.Fatalf("joined row=%v", rows)
		}
	}
	cwd := t.TempDir()
	appendRow(cwd, row)
	if data, _ := os.ReadFile(filepath.Join(cwd, StateDir, ".gitignore")); !strings.Contains(string(data), "CRW wrote") {
		t.Fatalf("ignore=%q", data)
	}
}

func TestRenderLedgerConfinesRecordWrites(t *testing.T) {
	for _, directory := range []bool{false, true} {
		cwd, outside := t.TempDir(), t.TempDir()
		target := filepath.Join(outside, RenderObsFile)
		put(t, target, "keep\n")
		link, dest := ledgerPath(cwd), target
		if directory {
			link, dest = filepath.Join(cwd, StateDir), outside
		} else {
			if err := os.Mkdir(filepath.Join(cwd, StateDir), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(dest, link); err != nil {
			t.Fatal(err)
		}
		HandleRenderObservationCapture(payload(cwd, "view_image"))
		ResetRenderLedger(cwd)
		if data, _ := os.ReadFile(target); string(data) != "keep\n" {
			t.Fatalf("outside target changed=%q", data)
		}
	}
	cwd := t.TempDir()
	target := filepath.Join(cwd, "record.jsonl")
	put(t, target, "")
	if err := os.Mkdir(filepath.Join(cwd, StateDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, ledgerPath(cwd)); err != nil {
		t.Fatal(err)
	}
	HandleRenderObservationCapture(payload(cwd, "view_image"))
	if !HasRenderObservation(cwd) {
		t.Fatal("absolute internal ledger link rejected")
	}
	ResetRenderLedger(cwd)
	if data, _ := os.ReadFile(target); len(data) != 0 {
		t.Fatal("internal reset")
	}
}

func TestTailGuardRejectsReplacementInode(t *testing.T) {
	cwd := t.TempDir()
	path := ledgerPath(cwd)
	put(t, path, "old")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	root, _, err := workspace(cwd)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.Rename(path, path+".held"); err != nil {
		t.Fatal(err)
	}
	put(t, path, "xx\n")
	if !endsMidLine(root, f, filepath.Join(StateDir, RenderObsFile)) {
		t.Fatal("replacement inode hid the unterminated writer tail")
	}
}

func TestNativeAppStructuredFields(t *testing.T) {
	for _, field := range []string{"appName", "application", "app"} {
		cwd := t.TempDir()
		p := payload(cwd, "computer-use:computer-use")
		p.Input = map[string]any{field: " Finder ", "criterionId": " c-1 "}
		p.Response = map[string]any{field: "other", "isError": true}
		HandleRenderObservationCapture(p)
		rows := NativeObservationRows(cwd, "s1")
		if len(rows) != 1 || rows[0].NativeApp != "Finder" || rows[0].CriterionID != "c-1" {
			t.Fatalf("rows=%v", rows)
		}
	}
	cwd := t.TempDir()
	p := payload(cwd, "computer-use:computer-use")
	p.Response = map[string]any{"application": "Finder", "criterionId": "c-2"}
	HandleRenderObservationCapture(p)
	if rows := NativeObservationRows(cwd); len(rows) != 1 || rows[0].CriterionID != "c-2" {
		t.Fatal(rows)
	}
	p.Response = "Finder app shown"
	p.Input = []any{"Finder"}
	ResetRenderLedger(cwd)
	HandleRenderObservationCapture(p)
	if HasNativeObservation(cwd) {
		t.Fatal("unstructured app counted")
	}
}

func TestDeclaredScreenshotPathsAndResponses(t *testing.T) {
	for _, name := range []string{"declared", "undeclared", "missing", "empty", "fifo", "failed", "bad-session", "bad-verdict", "null-verdict", "relative-link", "absolute-link", "escape-link", "outside", "workspace-sibling", "verdict-link", "response-path", "cwd-link"} {
		t.Run(name, func(t *testing.T) {
			cwd, outside := t.TempDir(), t.TempDir()
			qa := filepath.Join(cwd, StateDir, "evidence", "s1", "qa", "case")
			image := filepath.Join(qa, "image.png")
			put(t, image, "image")
			verdict := filepath.Join(qa, "verdict.json")
			put(t, verdict, `{"artifactRefs":["image.png"]}`)
			p := payload(cwd, "view_image")
			want := true
			switch name {
			case "undeclared":
				image = filepath.Join(qa, "other.png")
				put(t, image, "image")
				want = false
			case "missing":
				if err := os.Remove(image); err != nil {
					t.Fatal(err)
				}
				want = false
			case "empty":
				put(t, image, "")
				want = false
			case "fifo":
				if err := os.Remove(image); err != nil {
					t.Fatal(err)
				}
				if err := exec.Command("mkfifo", image).Run(); err != nil {
					t.Fatal(err)
				}
				want = false
			case "failed":
				p.Response = map[string]any{"error": "cannot decode"}
				want = false
			case "bad-session":
				p.SessionID = "../s1"
				want = false
			case "bad-verdict":
				put(t, verdict, "{bad")
				want = false
			case "null-verdict":
				put(t, verdict, "null")
				want = false
			case "relative-link", "absolute-link", "escape-link":
				target := filepath.Join(qa, "target.png")
				put(t, target, "image")
				dest := "target.png"
				if name == "absolute-link" {
					dest = target
				}
				if name == "escape-link" {
					dest = filepath.Join(outside, "target.png")
					put(t, dest, "image")
					want = false
				}
				if err := os.Remove(image); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(dest, image); err != nil {
					t.Fatal(err)
				}
			case "outside", "workspace-sibling":
				image = filepath.Join(outside, "external.png")
				if name == "workspace-sibling" {
					image = filepath.Join(cwd, "sibling.png")
				} else {
					want = false
				}
				put(t, image, "image")
				data, _ := json.Marshal(map[string]any{"artifactRefs": []string{image}})
				put(t, verdict, string(data))
			case "verdict-link":
				target := filepath.Join(qa, "other.json")
				put(t, target, `{"artifactRefs":["image.png"]}`)
				if err := os.Remove(verdict); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, verdict); err != nil {
					t.Fatal(err)
				}
				want = false
			case "response-path":
				want = false
			case "cwd-link":
				alias := filepath.Join(t.TempDir(), "workspace")
				if err := os.Symlink(cwd, alias); err != nil {
					t.Fatal(err)
				}
				image = filepath.Join(alias, StateDir, "evidence", "s1", "qa", "case", "image.png")
				p.Cwd = alias
			}
			p.Input = map[string]any{"path": " " + image + " ", "criterionId": "c-1"}
			if name == "response-path" {
				p.Input = map[string]any{}
				p.Response = map[string]any{"path": image}
			}
			if HandleRenderObservationCapture(p) != "" || HasNativeObservation(p.Cwd, p.SessionID) != want {
				t.Fatalf("native=%v want%v rows=%v", HasNativeObservation(p.Cwd, p.SessionID), want, ReadRenderObsRows(p.Cwd))
			}
			if !HasRenderObservation(p.Cwd, p.SessionID) {
				t.Fatal("ordinary row lost")
			}
			if want {
				rows := NativeObservationRows(p.Cwd)
				if rows[0].ScreenshotPath != image || rows[0].CriterionID != "c-1" {
					t.Fatal(rows)
				}
			}
		})
	}
}

func TestFailedResponseOracleRules(t *testing.T) {
	for _, test := range []struct {
		input any
		want  bool
	}{
		{"ok", false}, {" Error: no image", true}, {"\ufeffFAILED: decode", true}, {"errors", false}, {"failedness", false}, {"text ENOENT text", true}, {"xENOENT", false},
		{nil, false}, {map[string]any{"isError": true}, true}, {map[string]any{"success": false}, true}, {map[string]any{"error": 0}, true}, {map[string]any{"error": map[string]any{}}, true}, {map[string]any{"error": false}, false}, {map[string]any{"error": ""}, false}, {map[string]any{"error": nil}, false},
	} {
		if got := toolResponseFailed(test.input); got != test.want {
			t.Fatalf("failed(%v)=%v want%v", test.input, got, test.want)
		}
	}
	if got := structuredField(map[string]any{"appName": " ", "application": " Finder "}, "appName", "application"); got != "Finder" {
		t.Fatal(got)
	}
	if !reflect.DeepEqual(ReadRenderObsRows(t.TempDir()), []RenderObsRow{}) {
		t.Fatal("missing reader is nil")
	}
}
