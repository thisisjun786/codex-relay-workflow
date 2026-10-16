package publication

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/publishpolicy"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

// cliWorld is `crw publish` over a local fake forge: the NewGitHub seam points at it, the context
// names the test host, and no token or real network exists.
type cliWorld struct {
	url      string
	requests []string
	bodies   []string
	comments []string
	stdout   *bytes.Buffer
	stderr   *bytes.Buffer
	d        Deps
}

func newCLIWorld(t *testing.T) *cliWorld {
	t.Helper()
	w := &cliWorld{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /repos/o/r/issues", func(wr http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.requests = append(w.requests, "POST "+r.URL.Path)
		w.bodies = append(w.bodies, string(b))
		var in map[string]any
		_ = json.Unmarshal(b, &in)
		_ = json.NewEncoder(wr).Encode(map[string]any{"number": 7, "title": in["title"], "body": in["body"],
			"state": "open", "html_url": "https://github.com/o/r/issues/7"})
	})
	mux.HandleFunc("POST /repos/o/r/milestones", func(wr http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.requests = append(w.requests, "POST "+r.URL.Path)
		w.bodies = append(w.bodies, string(b))
		var in map[string]any
		_ = json.Unmarshal(b, &in)
		_ = json.NewEncoder(wr).Encode(map[string]any{"number": 3, "title": in["title"], "description": in["description"], "state": "open"})
	})
	mux.HandleFunc("POST /repos/o/r/issues/1/comments", func(wr http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.requests = append(w.requests, "POST "+r.URL.Path)
		w.bodies = append(w.bodies, string(b))
		var in map[string]any
		_ = json.Unmarshal(b, &in)
		w.comments = append(w.comments, fmt.Sprint(in["body"]))
		_ = json.NewEncoder(wr).Encode(map[string]any{"id": 9})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	w.url = srv.URL
	w.d = Deps{
		Getenv:  func(string) string { return "" },
		Context: &publishpolicy.Context{Paths: []string{"/home/me"}, Hostnames: []string{"lina-os"}},
		NewGitHub: func(repo string, p publishpolicy.Policy, c publishpolicy.Context, rec func(publishpolicy.Event)) (*tracking.GitHub, error) {
			return tracking.NewGitHub(repo, tracking.StaticToken("ghp_test"),
				tracking.WithBaseURL(w.url), tracking.WithPublication(p, c, rec))
		},
	}
	return w
}

func (w *cliWorld) run(args ...string) (int, string, string) {
	w.stdout.Reset()
	w.stderr.Reset()
	code := RunWith(context.Background(), args, strings.NewReader(""), w.stdout, w.stderr, w.d)
	return code, w.stdout.String(), w.stderr.String()
}

// `crw publish issue` posts the sanitized title and body; milestone and labels pass through, the
// audit line names only the kinds changed, and the forge never sees the private values.
func TestPublishIssueScrubsAndAudits(t *testing.T) {
	w := newCLIWorld(t)
	code, out, errOut := w.run("issue", "--repo", "o/r", "--title", "work in /home/me/build on lina-os",
		"--body", "log at 10.2.3.4, dev@ex.com", "--milestone", "3", "--label", "bug")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var res struct {
		Repo  string         `json:"repo"`
		Issue map[string]any `json:"issue"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Repo != "o/r" {
		t.Fatalf("answer %q: %v", out, err)
	}
	if res.Issue["number"] != float64(7) {
		t.Errorf("issue number %v", res.Issue["number"])
	}
	// the forge saw sanitized text
	if len(w.requests) != 1 || w.requests[0] != "POST /repos/o/r/issues" {
		t.Fatalf("requests %v", w.requests)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(w.bodies[0]), &sent)
	if strings.Contains(sent["title"].(string), "/home/me") || strings.Contains(sent["title"].(string), "lina-os") ||
		!strings.Contains(sent["title"].(string), "[path]") || !strings.Contains(sent["title"].(string), "[hostname]") {
		t.Errorf("the sent title: %q", sent["title"])
	}
	if strings.Contains(sent["body"].(string), "10.2.3.4") || strings.Contains(sent["body"].(string), "dev@ex.com") {
		t.Errorf("the sent body: %q", sent["body"])
	}
	if sent["milestone"] != float64(3) {
		t.Errorf("milestone lost: %v", sent["milestone"])
	}
	if labels, ok := sent["labels"].([]any); !ok || len(labels) != 1 || labels[0] != "bug" {
		t.Errorf("labels lost: %v", sent["labels"])
	}
	for _, raw := range []string{"/home/me", "lina-os", "10.2.3.4", "dev@ex.com"} {
		if strings.Contains(w.stderr.String(), raw) {
			t.Errorf("the audit echoes %q", raw)
		}
	}
	if !strings.Contains(errOut, `"surface":"github"`) || !strings.Contains(errOut, `"field":"title"`) {
		t.Errorf("no audit event: %q", errOut)
	}
}

// A token in the title, the body or the comment header is refused before anything is sent: zero
// requests, empty stdout, exit 2, and the error names ErrSecret not the value.
func TestPublishSecretBlocksBeforeHTTP(t *testing.T) {
	secret := "ghp_" + strings.Repeat("A", 24)
	for name, args := range map[string][]string{
		"issue title":    {"issue", "--repo", "o/r", "--title", "t " + secret},
		"issue body":     {"issue", "--repo", "o/r", "--title", "t", "--body", secret},
		"milestone":      {"milestone", "--repo", "o/r", "--title", "m", "--body", secret},
		"comment body":   {"comment", "--issue", "o/r#1", "--header", "[l · s]", "--body", secret},
		"comment header": {"comment", "--issue", "o/r#1", "--header", "[l · " + secret + "]"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newCLIWorld(t)
			code, out, errOut := w.run(args...)
			if code != 2 || out != "" {
				t.Fatalf("exit %d stdout %q", code, out)
			}
			if !strings.Contains(errOut, "public_text_secret") || strings.Contains(errOut, secret) {
				t.Errorf("error %q", errOut)
			}
			if reqs := w.requests; len(reqs) != 0 {
				t.Errorf("requests sent: %v", reqs)
			}
		})
	}
}

// Milestone description and comment body pass the same boundary.
func TestPublishMilestoneAndCommentScrub(t *testing.T) {
	w := newCLIWorld(t)
	if code, out, errOut := w.run("milestone", "--repo", "o/r", "--title", "M1", "--body", "work /home/me/m1"); code != 0 {
		t.Fatalf("milestone: %d %s %s", code, out, errOut)
	}
	if code, out, errOut := w.run("comment", "--issue", "o/r#1", "--header", "[l · s]", "--body", "ran on lina-os"); code != 0 {
		t.Fatalf("comment: %d %s %s", code, out, errOut)
	}
	reqs := w.requests
	if len(reqs) != 2 {
		t.Fatalf("requests %v", reqs)
	}
	// the comment is posted sanitized (the fake keeps comment bodies)
	found := false
	for _, c := range w.comments {
		if strings.Contains(c, "[hostname]") && !strings.Contains(c, "lina-os") {
			found = true
		}
	}
	if !found {
		t.Errorf("the comment body was not scrubbed: %v", w.comments)
	}
}

// prepare writes the sanitized bytes exactly (no added newline), mode 0600, in place; a blocked
// draft leaves an existing output file untouched and creates none that was absent.
func TestPublishPrepare(t *testing.T) {
	w := newCLIWorld(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "draft.md")
	out := filepath.Join(dir, "out.md")
	text := "see /home/me/x — 한국어 prose\nsecond line\n"
	if err := os.WriteFile(in, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	code, outS, _ := w.run("prepare", "--repo", "o/r", "--kind", "issue", "--input", in, "--output", out)
	if code != 0 {
		t.Fatalf("prepare: %d", code)
	}
	if !strings.Contains(outS, `"output"`) {
		t.Errorf("answer %q", outS)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "see [path] — 한국어 prose\nsecond line\n" {
		t.Errorf("output %q", data)
	}
	if fi, _ := os.Stat(out); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %o", fi.Mode().Perm())
	}
	// blocked: an existing output keeps its bytes, nothing new is made
	bad := filepath.Join(dir, "draft2.md")
	if err := os.WriteFile(bad, []byte("ghp_"+strings.Repeat("A", 24)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, outS, _ = w.run("prepare", "--repo", "o/r", "--kind", "commit", "--input", bad, "--output", out)
	if code != 2 || outS != "" {
		t.Fatalf("blocked prepare: %d %q", code, outS)
	}
	if b, _ := os.ReadFile(out); string(b) != "old" {
		t.Errorf("the blocked draft overwrote the output")
	}
	absent := filepath.Join(dir, "never.md")
	if code, _, _ := w.run("prepare", "--repo", "o/r", "--kind", "pr", "--input", bad, "--output", absent); code != 2 {
		t.Fatal("blocked prepare did not exit 2")
	}
	if _, err := os.Lstat(absent); !os.IsNotExist(err) {
		t.Errorf("a blocked prepare created %s", absent)
	}
	// stdin input: an empty draft is allowed and produces a zero-byte file
	w.stdout.Reset()
	stdinOut := filepath.Join(dir, "s.md")
	code, _, _ = w.run("prepare", "--repo", "o/r", "--kind", "issue", "--input", "-", "--output", stdinOut)
	if code != 0 {
		t.Fatalf("stdin prepare: %d", code)
	}
	if b, err := os.ReadFile(stdinOut); err != nil || len(b) != 0 {
		t.Errorf("stdin prepare output: %d bytes, %v", len(b), err)
	}
}

// Flag errors, a bad ref and a bad repository name stop before any call.
func TestPublishFlagAndRefErrors(t *testing.T) {
	for name, args := range map[string][]string{
		"mutual body":     {"issue", "--repo", "o/r", "--title", "t", "--body", "", "--body-file", "x"},
		"bad repo":        {"issue", "--repo", "nope", "--title", "t"},
		"no title":        {"issue", "--repo", "o/r"},
		"bad issue ref":   {"comment", "--issue", "o/r", "--header", "[l · s]"},
		"milestone label": {"milestone", "--repo", "o/r", "--title", "m", "--label", "x"},
		"unknown verb":    {"wat"},
		"extra arg":       {"issue", "--repo", "o/r", "--title", "t", "pos"},
		"bad kind":        {"prepare", "--repo", "o/r", "--kind", "x", "--input", "-", "--output", "o"},
		"missing io":      {"prepare", "--repo", "o/r", "--kind", "issue"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newCLIWorld(t)
			code, out, _ := w.run(args...)
			if code != 2 || out != "" {
				t.Errorf("exit %d stdout %q", code, out)
			}
			if len(w.requests) != 0 {
				t.Errorf("requests sent on a usage error")
			}
		})
	}
	// help needs no forge
	w := newCLIWorld(t)
	if code, out, _ := w.run("--help"); code != 0 || !strings.Contains(out, "publish") {
		t.Errorf("help: %d %q", code, out)
	}
}

// A repository's configured policy is what the live command runs under: disabled lets the repo's
// own paths through while a token still stops, and a custom host list masks what the defaults miss.
func TestPublishUsesConfiguredPolicy(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(cfg, []byte(`{"publication":{"repositories":{"o/r":{"disabled":true}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	w := newCLIWorld(t)
	w.d.Getenv = func(k string) string {
		if k == "CRW_CONFIG" {
			return cfg
		}
		return ""
	}
	code, _, _ := w.run("issue", "--repo", "o/r", "--title", "work in /home/me/build", "--body", "ok")
	if code != 0 {
		t.Fatalf("disabled policy: exit %d", code)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(w.bodies[0]), &sent)
	if !strings.Contains(sent["title"].(string), "/home/me/build") {
		t.Errorf("a disabled policy still masked the path: %q", sent["title"])
	}
	// the token gate is unaffected by disabled
	w2 := newCLIWorld(t)
	w2.d.Getenv = w.d.Getenv
	code, out, _ := w2.run("issue", "--repo", "o/r", "--title", "ghp_"+strings.Repeat("A", 24))
	if code != 2 || out != "" || len(w2.requests) != 0 {
		t.Errorf("disabled policy published a secret: %d %q", code, out)
	}
	// a configured host list masks
	if err := os.WriteFile(cfg, []byte(`{"publication":{"repositories":{"o/r":{"hostnames":["custom-box"],"paths":["/work/private"]}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	w3 := newCLIWorld(t)
	w3.d.Getenv = w.d.Getenv
	if code, _, _ := w3.run("issue", "--repo", "o/r", "--title", "on custom-box /work/private/x"); code != 0 {
		t.Fatalf("configured policy: exit %d", code)
	}
	_ = json.Unmarshal([]byte(w3.bodies[0]), &sent)
	if strings.Contains(sent["title"].(string), "custom-box") || strings.Contains(sent["title"].(string), "/work/private") {
		t.Errorf("configured literals not applied: %q", sent["title"])
	}
}

// Each subcommand's --help prints the usage to stdout and asks for nothing.
func TestPublishSubcommandHelp(t *testing.T) {
	for _, verb := range []string{"issue", "milestone", "comment", "prepare"} {
		w := newCLIWorld(t)
		code, out, errOut := w.run(verb, "--help")
		if code != 0 || !strings.Contains(out, "crw publish") || errOut != "" {
			t.Errorf("%s --help: %d out=%q err=%q", verb, code, out, errOut)
		}
		if len(w.requests) != 0 {
			t.Errorf("%s --help sent requests", verb)
		}
	}
}

// A stdout that cannot take the answer is a 1, not a silent success.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, os.ErrPermission }

func TestPublishAnswerWriterFailure(t *testing.T) {
	w := newCLIWorld(t)
	var stderr bytes.Buffer
	code := RunWith(context.Background(), []string{"issue", "--repo", "o/r", "--title", "t"}, strings.NewReader(""), failWriter{}, &stderr, w.d)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
}
