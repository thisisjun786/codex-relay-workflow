package command

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/review"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func TestCRW1105PostOnlyRefusesFinalSecretBytes(t *testing.T) {
	secret := "sk-" + strings.Repeat("S", 24)
	for _, where := range []string{"reason", "finding"} {
		for _, update := range []bool{false, true} {
			t.Run(fmt.Sprint(where, update), func(t *testing.T) {
				f := newFixture(t)
				head := f.repo.change(f.base, 2)
				_, sum, errOut := f.run(head)
				if sum.Artifact == "" {
					t.Fatal(errOut)
				}
				data, err := os.ReadFile(sum.Artifact)
				if err != nil {
					t.Fatal(err)
				}
				a, err := review.ParseArtifact(data, head)
				if err != nil {
					t.Fatal(err)
				}
				if where == "reason" {
					a.Status = review.StatusPartial
					a.Reason = secret
				} else {
					a.Findings = []review.Finding{{File: "a.go", Line: 3, Title: secret, Explanation: "example", Severity: "P2", Grade: review.Grade("P2"), Perspective: "test", Reviewers: []int{0}, Support: 1, Verdict: review.Verdict("unverified")}}
				}
				data, err = json.Marshal(a)
				if err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256(data)
				sha := hex.EncodeToString(digest[:])
				if err := os.WriteFile(sum.Artifact, data, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(sum.Artifact+".sha256", []byte(sha+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				recs := f.ledger()
				var ledger strings.Builder
				for _, r := range recs {
					if r.SHA256 == sum.SHA256 {
						r.SHA256 = sha
					}
					b, _ := json.Marshal(r)
					ledger.Write(b)
					ledger.WriteByte('\n')
				}
				if err := os.WriteFile(filepath.Join(f.state, "ledger.jsonl"), []byte(ledger.String()), 0600); err != nil {
					t.Fatal(err)
				}
				dir := t.TempDir()
				gh := filepath.Join(dir, "gh")
				calls := filepath.Join(dir, "calls")
				get := "[]"
				if update {
					b, _ := json.Marshal([]prComment{{ID: 1, Body: markerPrefix + "old", URL: "u"}})
					get = string(b)
				}
				script := "#!/bin/sh\ncase \"$*\" in *'--method GET'*) printf '%s\\n' '" + get + "';; *) printf '%s\\n' \"$*\" >> '" + calls + "'; cat >/dev/null; printf '%s\\n' '{\"id\":1,\"html_url\":\"u\"}';; esac\n"
				if err := testsupport.WriteProgram(gh, []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				code, posted, errOut := f.run(head, "--post-only", "--pr", "7", "--gh", gh)
				if code != 1 || posted.Comment != nil || !strings.Contains(errOut, "secret-in-github-text") || !strings.Contains(errOut, "body:") || strings.Contains(errOut, secret) {
					t.Errorf("posting refusal: code=%d comment=%v err=%s", code, posted.Comment, errOut)
				}
				if b, _ := os.ReadFile(calls); len(b) != 0 {
					t.Error("POST/PATCH reached fake gh")
				}
				if b, _ := os.ReadFile(sum.Artifact); string(b) != string(data) {
					t.Error("local artifact changed")
				}
			})
		}
	}
}

func TestCRW1105SameBodyAtBothPublishPaths(t *testing.T) {
	dir := t.TempDir()
	gh := filepath.Join(dir, "gh")
	stdin := filepath.Join(dir, "stdin")
	script := "#!/bin/sh\ncat > '" + stdin + "'\nprintf '%s\\n' '{\"id\":1,\"html_url\":\"u\"}'\n"
	if err := testsupport.WriteProgram(gh, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	g := ghForge{binary: gh, dir: dir}
	for _, body := range []string{"clean `$(touch forbidden)` body\n", "example sk-" + strings.Repeat("S", 24), "PASSWORD=synthetic\n", "a=1\nb=2\nc=3\n"} {
		path := filepath.Join(dir, "body.md")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "cwd": dir, "tool_input": map[string]any{"command": "gh pr comment 1 --body-file " + path}})
		shellDenied := hook.GitHubPostAnswer(strings.NewReader(string(raw))) != ""
		_, err := g.Create(context.Background(), 1, body)
		if (err != nil) != shellDenied {
			t.Errorf("publish policy differs: native=%v shell=%v", err != nil, shellDenied)
		}
		if !shellDenied {
			if b, _ := os.ReadFile(stdin); string(b) != body {
				t.Error("stdin bytes changed")
			}
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "forbidden")); err == nil {
		t.Error("body was executed")
	}
}
